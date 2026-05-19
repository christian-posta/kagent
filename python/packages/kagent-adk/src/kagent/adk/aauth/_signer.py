"""AAuth signer for outbound HTTP request signing (RFC 9421 + hwk/jwt schemes).

Two modes:

* **Phase 1 (hwk / pseudonymous):** when ``AAUTH_CONTROLLER_URL`` is unset,
  the signer publishes its public key inline on every request. Identity is
  pseudonymous — the verifier sees a stable but unattested public key.

* **Phase 2 (jwt / identified):** when ``AAUTH_CONTROLLER_URL`` is set, the
  signer fetches an ``aa-agent+jwt`` from the kagent controller's Agent
  Provider endpoint at signer-init time and refreshes it lazily before
  expiry. The JWT binds the agent's ephemeral signing key (via ``cnf.jwk``)
  to its kagent identity, signed by the controller's issuer key. The signer
  uses ``sig_scheme="jwt"`` and embeds the JWT in the ``Signature-Key``
  header.

The signing keypair is regenerated each pod restart; the JWT is fetched on
init and re-minted automatically before its ``exp`` is reached.
If the controller is unreachable at init, the signer logs and falls back to
hwk for the life of the process (we do not retroactively upgrade).
"""

from __future__ import annotations

import asyncio
import logging
import os
import time

import httpx

logger = logging.getLogger(__name__)

try:
    import aauth as _aauth_lib

    _AAUTH_AVAILABLE = True
except ImportError:
    _aauth_lib = None  # type: ignore[assignment]
    _AAUTH_AVAILABLE = False


# Path on the controller that mints aa-agent+jwt tokens.
_AGENT_JWT_PATH = "/aauth/agent-jwt"
# Connect/read timeout for the JWT fetch — kept short because it blocks pod startup.
_FETCH_TIMEOUT_SECONDS = 5.0
# Refresh the JWT when this much time is left before expiry.
_REFRESH_WINDOW_SECONDS = 5 * 60
# Minimum gap between refresh attempts after a failure — prevents hammering
# a broken controller on every outbound request.
_MIN_RETRY_SECONDS = 30.0

# Default location of the projected ServiceAccount token in a Kubernetes pod.
# The kubelet rotates this file periodically, so it is re-read on every mint
# request (not cached).
_SA_TOKEN_PATH = "/var/run/secrets/kubernetes.io/serviceaccount/token"


def _read_sa_token() -> str | None:
    """Return the pod's projected ServiceAccount token, or None if unavailable.

    Returning None lets the signer surface a clean controller-side rejection
    (the controller will reply 401, the signer logs and stays on the previous
    JWT or falls back to hwk on first start) rather than crashing the runtime
    when running outside a K8s pod (tests, local dev, etc.).

    The path is looked up at call time (not bound as a default arg) so tests
    can patch ``_SA_TOKEN_PATH`` to point at a fixture file.
    """
    try:
        with open(_SA_TOKEN_PATH) as fh:
            return fh.read().strip() or None
    except OSError:
        return None


class AAuthSigner:
    """Signs outbound HTTP requests using an ephemeral Ed25519 key.

    With ``controller_url`` set, the signer mints an ``aa-agent+jwt`` at init,
    uses the ``jwt`` Signature-Key scheme on every request, and lazily refreshes
    the JWT inside :meth:`ensure_fresh_jwt` before each sign.
    Without it, the signer publishes the bare public key (``hwk`` scheme) —
    identity remains pseudonymous but the wire format is still spec-conformant.
    """

    def __init__(self, agent_id: str, controller_url: str | None = None) -> None:
        if not _AAUTH_AVAILABLE:
            raise RuntimeError(
                "aauth library is not installed. "
                "Add 'aauth' to your dependencies or set AAUTH_ENABLED=false."
            )
        self._agent_id = agent_id
        self._private_key, self._public_key = _aauth_lib.generate_ed25519_keypair()
        self._jwt: str | None = None
        self._jwt_exp: float = 0.0
        self._sig_scheme = "hwk"
        self._controller_url = controller_url
        self._refresh_lock: asyncio.Lock | None = None
        self._last_refresh_attempt: float = 0.0

        if controller_url:
            self._fetch_jwt_sync(controller_url)

        logger.info(
            "AAuth signing enabled — agent_id=%s scheme=%s",
            agent_id,
            self._sig_scheme,
        )

    def _build_jwt_request(self) -> tuple[str, dict[str, object], dict[str, str]]:
        """Return (url, body, headers) for a JWT mint request to the controller.

        The headers include an ``Authorization: Bearer <sa-token>`` derived from
        the pod's projected ServiceAccount token. The controller validates this
        token via TokenReview and derives the canonical sub from the SA's
        identity — the body's sub is only a sanity-check.
        """
        url = self._controller_url.rstrip("/") + _AGENT_JWT_PATH  # type: ignore[union-attr]
        pub_jwk = _aauth_lib.public_key_to_jwk(self._public_key)
        body: dict[str, object] = {"sub": self._agent_id, "public_key_jwk": pub_jwk}
        headers: dict[str, str] = {}
        sa_token = _read_sa_token()
        if sa_token:
            headers["Authorization"] = f"Bearer {sa_token}"
        else:
            logger.warning(
                "AAuth: no ServiceAccount token found at %s — the controller will reject this mint request",
                _SA_TOKEN_PATH,
            )
        return url, body, headers

    def _apply_mint_response(self, data: object, *, refresh: bool) -> bool:
        """Update state from a controller mint response. Returns True on success."""
        if not isinstance(data, dict):
            logger.error("AAuth: controller response was not a JSON object: %r", data)
            return False
        token = data.get("token")
        if not token:
            logger.error("AAuth: controller response missing 'token' field: %r", data)
            return False
        expires_in = float(data.get("expires_in") or 0)
        self._jwt = token
        self._jwt_exp = time.time() + expires_in
        self._sig_scheme = "jwt"
        verb = "refreshed" if refresh else "minted"
        logger.info("AAuth: %s aa-agent+jwt — expires_in=%s", verb, data.get("expires_in"))
        return True

    def _fetch_jwt_sync(self, controller_url: str) -> None:
        """Mint an aa-agent+jwt at the controller. Falls back to hwk on error.

        Used only at signer construction time (synchronous; blocks pod startup).
        """
        url, body, headers = self._build_jwt_request()
        try:
            with httpx.Client(timeout=_FETCH_TIMEOUT_SECONDS) as client:
                resp = client.post(url, json=body, headers=headers)
                resp.raise_for_status()
                data = resp.json()
        except Exception:
            logger.exception(
                "AAuth: failed to fetch aa-agent+jwt from %s — falling back to hwk", url
            )
            return
        self._apply_mint_response(data, refresh=False)

    async def _fetch_jwt_async(self) -> None:
        """Async re-mint of the aa-agent+jwt. Keeps the old token on failure."""
        url, body, headers = self._build_jwt_request()
        try:
            async with httpx.AsyncClient(timeout=_FETCH_TIMEOUT_SECONDS) as client:
                resp = await client.post(url, json=body, headers=headers)
                resp.raise_for_status()
                data = resp.json()
        except Exception:
            logger.exception(
                "AAuth: failed to refresh aa-agent+jwt from %s — keeping previous token", url
            )
            return
        self._apply_mint_response(data, refresh=True)

    async def ensure_fresh_jwt(self) -> None:
        """Re-mint the JWT if it is within the refresh window.

        Called from async sign hooks before each request. No-op when the signer
        is in hwk mode or when the current JWT is still comfortably valid.
        Refreshes are serialised with an asyncio.Lock so a burst of concurrent
        requests results in a single mint, and a recent failure is rate-limited
        by ``_MIN_RETRY_SECONDS``.
        """
        if self._sig_scheme != "jwt" or not self._controller_url:
            return
        now = time.time()
        if self._jwt_exp - now > _REFRESH_WINDOW_SECONDS:
            return
        if now - self._last_refresh_attempt < _MIN_RETRY_SECONDS:
            return
        if self._refresh_lock is None:
            self._refresh_lock = asyncio.Lock()
        async with self._refresh_lock:
            # Re-check after acquiring the lock: another task may have refreshed.
            if self._jwt_exp - time.time() > _REFRESH_WINDOW_SECONDS:
                return
            self._last_refresh_attempt = time.time()
            await self._fetch_jwt_async()

    def sign(self, method: str, url: str, headers: dict[str, str]) -> dict[str, str]:
        """Return AAuth signature headers to add to the request.

        Covered components: @method, @authority, @path, signature-key, created.
        Body content-digest is not covered in Phase 1/2 (sidesteps streaming).
        """
        kwargs: dict[str, object] = {}
        if self._sig_scheme == "jwt":
            kwargs["jwt"] = self._jwt
        try:
            return _aauth_lib.sign_request(
                method=method,
                target_uri=url,
                headers=headers,
                body=None,
                private_key=self._private_key,
                sig_scheme=self._sig_scheme,
                **kwargs,
            )
        except Exception:
            logger.exception("AAuth: failed to sign request — url=%s", url)
            return {}

    def make_hook(self) -> object:
        """Return an async httpx event hook that signs every request."""
        signer = self

        async def _sign_request(request: httpx.Request) -> None:
            await signer.ensure_fresh_jwt()
            sig_headers = signer.sign(
                method=request.method,
                url=str(request.url),
                headers=dict(request.headers),
            )
            request.headers.update(sig_headers)

        return _sign_request

    @classmethod
    def from_env(cls) -> AAuthSigner | None:
        """Return an AAuthSigner if AAUTH_ENABLED=true, else None (no-op mode)."""
        if os.getenv("AAUTH_ENABLED", "false").lower() != "true":
            return None
        if not _AAUTH_AVAILABLE:
            logger.warning(
                "AAUTH_ENABLED=true but the 'aauth' library is not installed; "
                "AAuth signing is disabled. Install it or unset AAUTH_ENABLED."
            )
            return None
        agent_id = os.getenv("AAUTH_AGENT_ID", "")
        if not agent_id:
            logger.warning(
                "AAUTH_ENABLED=true but AAUTH_AGENT_ID is not set; "
                "AAuth signing is disabled."
            )
            return None
        controller_url = os.getenv("AAUTH_CONTROLLER_URL", "").strip() or None
        return cls(agent_id=agent_id, controller_url=controller_url)
