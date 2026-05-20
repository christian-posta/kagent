"""AAuth inbound HTTP Message Signature verifier (Phase 3, log-only by default).

This module installs an ASGI middleware in front of the agent's FastAPI app
that runs ``aauth.RequestVerifier.verify_request`` against every inbound
request, then either:

* logs the outcome and continues (``log`` mode, the default);
* rejects with 401 + AAuth challenge headers (``enforce`` mode);
* does nothing (``off`` mode).

Configuration is via environment variables, mirroring the controller-side
verifier:

``AAUTH_VERIFY_MODE``  — ``off | log | enforce`` (default ``log`` when the
                        signer has a controller URL; ``off`` otherwise).
``AAUTH_VERIFY_AUTHORITIES`` — comma-separated list of additional Host
                              values to accept as canonical authorities.
                              In a Kind demo the verifier needs to accept
                              the agent's port-forward (``localhost:8080``,
                              ``localhost:18080``) in addition to the
                              cluster-internal service DNS.

The verifier fetches the controller's JWKS via ``AAUTH_CONTROLLER_URL``
(``$AAUTH_CONTROLLER_URL/.well-known/jwks.json``) and uses the standard
``aauth.JWKSFetcher`` cache.
"""

from __future__ import annotations

import logging
import os
import threading
import time
from typing import Any

import httpx

logger = logging.getLogger(__name__)

try:
    import aauth as _aauth_lib

    _AAUTH_AVAILABLE = True
except ImportError:
    _aauth_lib = None  # type: ignore[assignment]
    _AAUTH_AVAILABLE = False


# Paths the verifier always skips. These are unauthenticated by design (the
# A2A AgentCard endpoint is public discovery, health checks need to work
# even when AAuth is off).
_SKIP_PATHS = frozenset(
    [
        "/health",
        "/thread_dump",
        "/.well-known/agent-card.json",  # A2A AgentCard discovery
        "/.well-known/agent.json",
    ]
)


def _parse_mode(s: str | None) -> str:
    """Normalize the AAUTH_VERIFY_MODE env var to one of off|log|enforce."""
    if not s:
        return "log"
    s = s.strip().lower()
    if s in ("off", "disabled", "false"):
        return "off"
    if s in ("enforce", "strict"):
        return "enforce"
    return "log"  # unknown → log (safe default)


def _default_canonical_authorities() -> list[str]:
    """Best-effort guesses for the agent's own canonical authority.

    Derived from KAGENT_NAME / KAGENT_NAMESPACE so the verifier accepts
    in-cluster Service DNS. Operators add port-forward / proxy authorities
    via AAUTH_VERIFY_AUTHORITIES.
    """
    name = os.getenv("KAGENT_NAME", "").strip()
    namespace = os.getenv("KAGENT_NAMESPACE", "").strip()
    out: list[str] = []
    if name and namespace:
        # Common forms a client could put in the Host header.
        out.append(f"{name}.{namespace}.svc.cluster.local:8080")
        out.append(f"{name}.{namespace}.svc:8080")
        out.append(f"{name}.{namespace}:8080")
        out.append(f"{name}.{namespace}.svc.cluster.local")
        out.append(f"{name}.{namespace}")
    out.append("localhost:8080")
    return out


def _canonical_authorities() -> list[str]:
    extra = [
        a.strip()
        for a in os.getenv("AAUTH_VERIFY_AUTHORITIES", "").split(",")
        if a.strip()
    ]
    # Deduplicate while preserving order.
    seen: set[str] = set()
    result: list[str] = []
    for a in (*_default_canonical_authorities(), *extra):
        if a in seen:
            continue
        seen.add(a)
        result.append(a)
    return result


def _parse_rewrites(env_value: str) -> dict[str, str]:
    """Parse ``AAUTH_VERIFY_ISSUER_REWRITE`` (``from1=to1,from2=to2``).

    A common case in our Kind demo: the JWT's ``iss`` is the canonical
    ``http://localhost:8083`` (set on the controller) but in-cluster the
    verifier must reach the controller via ``http://kagent-controller.kagent:8083``.
    """
    out: dict[str, str] = {}
    for pair in env_value.split(","):
        pair = pair.strip()
        if not pair:
            continue
        if "=" not in pair:
            continue
        src, dst = pair.split("=", 1)
        src = src.strip().rstrip("/")
        dst = dst.strip().rstrip("/")
        if src and dst:
            out[src] = dst
    return out


class _SyncJWKSFetcher:
    """Sync issuer → JWKS lookup with a short in-memory TTL cache.

    The upstream aauth library exposes an async ``JWKSFetcher.fetch``, but
    ``aauth.verify_signature`` is sync and calls ``jwks_fetcher(iss)``
    expecting a plain dict back. This helper does the two-step well-known
    discovery (``/.well-known/aauth-agent.json`` → ``jwks_uri`` → JWKS) over
    sync httpx with caching, suitable for use from any thread the verifier
    runs in.

    The ``rewrites`` dict maps canonical issuer URLs (as embedded in the
    JWT's ``iss`` claim) to URLs the verifier can actually reach. In the
    Kind demo the controller publishes ``iss=http://localhost:8083`` but
    is only reachable in-cluster at ``http://kagent-controller.kagent:8083``.
    """

    DEFAULT_TTL_SECONDS = 300.0
    DEFAULT_TIMEOUT_SECONDS = 5.0

    def __init__(
        self,
        ttl_seconds: float = DEFAULT_TTL_SECONDS,
        rewrites: dict[str, str] | None = None,
    ) -> None:
        self._ttl = ttl_seconds
        self._cache: dict[str, tuple[float, dict[str, Any]]] = {}
        self._lock = threading.Lock()
        self._rewrites = dict(rewrites or {})

    def __call__(self, identifier: str, dwk: str | None = None, kid: str | None = None) -> dict[str, Any]:
        # The aauth lib calls this as ``jwks_fetcher(iss, dwk, kid)`` (see
        # aauth_signing.verifier._fetch_jwks). dwk is the well-known metadata
        # filename (e.g. "aauth-agent.json"); kid is used by the caller for
        # key selection within the returned JWKS, not for the fetch URL.
        metadata_path = dwk or "aauth-agent.json"
        target = self._rewrite(identifier)
        cache_key = f"{target}|{metadata_path}"
        now = time.time()
        with self._lock:
            cached = self._cache.get(cache_key)
            if cached and cached[0] > now:
                return cached[1]
        jwks = self._fetch_uncached(target, metadata_path)
        with self._lock:
            self._cache[cache_key] = (now + self._ttl, jwks)
        return jwks

    def _rewrite(self, identifier: str) -> str:
        canonical = identifier.rstrip("/")
        return self._rewrites.get(canonical, identifier)

    def _fetch_uncached(self, identifier: str, metadata_path: str) -> dict[str, Any]:
        meta_url = identifier.rstrip("/") + "/.well-known/" + metadata_path.lstrip("/")
        with httpx.Client(timeout=self.DEFAULT_TIMEOUT_SECONDS) as client:
            meta_resp = client.get(meta_url)
            meta_resp.raise_for_status()
            metadata = meta_resp.json()
            jwks_uri = metadata.get("jwks_uri")
            if not jwks_uri:
                raise RuntimeError(f"metadata at {meta_url} missing jwks_uri")
            # If the metadata's jwks_uri is the canonical (unreachable) URL,
            # apply the same rewrite so we hit the in-cluster endpoint.
            jwks_uri_target = self._rewrite_url(jwks_uri, identifier)
            jwks_resp = client.get(jwks_uri_target)
            jwks_resp.raise_for_status()
            return jwks_resp.json()

    def _rewrite_url(self, url: str, base_identifier: str) -> str:
        """Rewrite the host portion of jwks_uri to match base_identifier."""
        # Best-effort: if jwks_uri shares the canonical issuer's prefix, swap it.
        for src, dst in self._rewrites.items():
            if url.startswith(src):
                return dst + url[len(src):]
        return url


class AAuthVerifier:
    """Wraps ``aauth.RequestVerifier`` with kagent-flavored defaults."""

    def __init__(self, mode: str, canonical_authorities: list[str]) -> None:
        if not _AAUTH_AVAILABLE:
            raise RuntimeError("aauth library is not installed")
        self._mode = mode
        self._canonical_authorities = canonical_authorities
        # verify_signature wants a sync callable (issuer URL → JWKS dict).
        # The lib's own JWKSFetcher.fetch is async, so we provide our own,
        # with a canonical-iss → in-cluster-URL rewrite map for the demo.
        rewrites = _parse_rewrites(os.getenv("AAUTH_VERIFY_ISSUER_REWRITE", ""))
        self._fetcher = _SyncJWKSFetcher(rewrites=rewrites)
        self._inner = _aauth_lib.RequestVerifier(
            canonical_authorities=canonical_authorities,
            jwks_fetcher=self._fetcher,
        )

    @property
    def mode(self) -> str:
        return self._mode

    def verify(
        self,
        *,
        method: str,
        target_uri: str,
        headers: dict[str, str],
    ) -> dict[str, Any]:
        """Run the underlying lib verifier and return its dict result."""
        return self._inner.verify_request(
            method=method,
            target_uri=target_uri,
            headers=headers,
            body=None,
        )

    @classmethod
    def from_env(cls) -> AAuthVerifier | None:
        """Build a verifier from env if AAuth is enabled, else None.

        Returns None when:
          - ``AAUTH_ENABLED`` is false (no signer running → no inbound traffic
            we'd verify).
          - ``AAUTH_CONTROLLER_URL`` is unset (no issuer → nothing to verify
            against).
          - The aauth library is not installed.
          - ``AAUTH_VERIFY_MODE=off``.
        """
        if os.getenv("AAUTH_ENABLED", "false").lower() != "true":
            return None
        if not os.getenv("AAUTH_CONTROLLER_URL", "").strip():
            return None
        if not _AAUTH_AVAILABLE:
            logger.warning(
                "AAUTH_ENABLED=true but the 'aauth' library is not installed; "
                "inbound verification is disabled."
            )
            return None
        mode = _parse_mode(os.getenv("AAUTH_VERIFY_MODE"))
        if mode == "off":
            return None
        return cls(mode=mode, canonical_authorities=_canonical_authorities())


def _should_skip(path: str) -> bool:
    return path in _SKIP_PATHS


def _challenge_headers() -> dict[str, bytes]:
    """Headers returned with a 401 in enforce mode."""
    return {
        b"content-type": b"application/json",
        b"www-authenticate": b"AAuth",
        b"aauth-requirement": b"requirement=auth-token",
    }


def install_middleware(app: Any) -> AAuthVerifier | None:
    """Install the inbound AAuth verification middleware on a FastAPI/Starlette app.

    Returns the verifier instance (useful for tests). Returns None if the
    verifier is disabled, in which case no middleware is installed.
    """
    verifier = AAuthVerifier.from_env()
    if verifier is None:
        logger.info("AAuth inbound verification disabled (mode=off or AAuth not configured)")
        return None
    logger.info(
        "AAuth inbound verification enabled — mode=%s authorities=%s",
        verifier.mode,
        verifier._canonical_authorities,
    )

    @app.middleware("http")
    async def aauth_verify_middleware(request: Any, call_next: Any) -> Any:
        path = request.url.path
        if _should_skip(path):
            return await call_next(request)

        # Build a header dict the aauth lib understands. httpx/Starlette
        # gives us a multi-value mapping; the lib accepts the lowercase form.
        headers = {k.lower(): v for k, v in request.headers.items()}
        # str(request.url) preserves scheme + authority + path, which is what
        # verify_request expects.
        result = verifier.verify(
            method=request.method,
            target_uri=str(request.url),
            headers=headers,
        )

        if result.get("valid"):
            logger.info(
                "aauth: verified caller=%s method=%s path=%s",
                result.get("agent_id"),
                request.method,
                path,
            )
            # Attach the result so handlers downstream can read it from request.state.
            request.state.aauth_identity = result
            return await call_next(request)

        reason = result.get("error") or "verification failed"
        logger.warning(
            "aauth: unverified reason=%r method=%s path=%s",
            reason,
            request.method,
            path,
        )
        if verifier.mode == "enforce":
            # Starlette response — small, no body validation needed.
            from starlette.responses import JSONResponse

            return JSONResponse(
                status_code=401,
                content={"error": "aauth_verification_failed", "reason": reason},
                headers={
                    "WWW-Authenticate": "AAuth",
                    "AAuth-Requirement": "requirement=auth-token",
                },
            )
        # log mode: passthrough.
        return await call_next(request)

    return verifier
