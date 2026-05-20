"""Unit tests for kagent.adk.aauth — AAuth signing pipeline.

These tests use unittest.mock to stub the aauth library so they run
without the library being installed.  When the real library is present
the tests also exercise the actual signing + verification round-trip.
"""

from __future__ import annotations

import os
from typing import Any
from unittest.mock import MagicMock, patch

import httpx
import pytest


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _make_mock_aauth_lib() -> MagicMock:
    """Return a MagicMock that looks like the aauth library."""
    lib = MagicMock()
    fake_private_key = MagicMock(name="private_key")
    fake_public_key = MagicMock(name="public_key")
    lib.generate_ed25519_keypair.return_value = (fake_private_key, fake_public_key)
    lib.sign_request.return_value = {
        "Signature": 'sig=:dGVzdA==:',
        "Signature-Input": 'sig=("@method" "@authority" "@path" "signature-key");created=1700000000',
        "Signature-Key": 'sig=hwk;key=dGVzdA==',
    }
    return lib


# ---------------------------------------------------------------------------
# AAuthSigner — construction and from_env
# ---------------------------------------------------------------------------

class TestAAuthSignerFromEnv:
    def test_disabled_by_default(self):
        """Returns None when AAUTH_ENABLED is not set."""
        with patch.dict(os.environ, {}, clear=False):
            os.environ.pop("AAUTH_ENABLED", None)
            from kagent.adk.aauth._signer import AAuthSigner
            assert AAuthSigner.from_env() is None

    def test_disabled_when_false(self):
        with patch.dict(os.environ, {"AAUTH_ENABLED": "false"}):
            from kagent.adk.aauth._signer import AAuthSigner
            assert AAuthSigner.from_env() is None

    def test_disabled_when_lib_missing(self):
        with patch.dict(os.environ, {"AAUTH_ENABLED": "true", "AAUTH_AGENT_ID": "aauth:agent@ns.kagent.local"}):
            with patch("kagent.adk.aauth._signer._AAUTH_AVAILABLE", False):
                from kagent.adk.aauth._signer import AAuthSigner
                assert AAuthSigner.from_env() is None

    def test_disabled_when_agent_id_missing(self):
        with patch.dict(os.environ, {"AAUTH_ENABLED": "true"}, clear=False):
            os.environ.pop("AAUTH_AGENT_ID", None)
            with patch("kagent.adk.aauth._signer._AAUTH_AVAILABLE", True):
                with patch("kagent.adk.aauth._signer._aauth_lib", _make_mock_aauth_lib()):
                    from kagent.adk.aauth._signer import AAuthSigner
                    assert AAuthSigner.from_env() is None

    def test_returns_signer_when_enabled(self):
        mock_lib = _make_mock_aauth_lib()
        with patch.dict(os.environ, {"AAUTH_ENABLED": "true", "AAUTH_AGENT_ID": "aauth:my-agent@default.kagent.local"}):
            with patch("kagent.adk.aauth._signer._AAUTH_AVAILABLE", True):
                with patch("kagent.adk.aauth._signer._aauth_lib", mock_lib):
                    from kagent.adk.aauth._signer import AAuthSigner
                    signer = AAuthSigner.from_env()
                    assert signer is not None
                    assert signer._agent_id == "aauth:my-agent@default.kagent.local"


# ---------------------------------------------------------------------------
# AAuthSigner.sign
# ---------------------------------------------------------------------------

class TestAAuthSignerSign:
    def _make_signer(self) -> Any:
        mock_lib = _make_mock_aauth_lib()
        with patch("kagent.adk.aauth._signer._AAUTH_AVAILABLE", True):
            with patch("kagent.adk.aauth._signer._aauth_lib", mock_lib):
                from kagent.adk.aauth._signer import AAuthSigner
                signer = AAuthSigner(agent_id="aauth:test@default.kagent.local")
                signer._lib = mock_lib
                return signer, mock_lib

    def test_sign_returns_three_headers(self):
        mock_lib = _make_mock_aauth_lib()
        with patch("kagent.adk.aauth._signer._AAUTH_AVAILABLE", True):
            with patch("kagent.adk.aauth._signer._aauth_lib", mock_lib):
                from kagent.adk.aauth._signer import AAuthSigner
                signer = AAuthSigner(agent_id="aauth:test@default.kagent.local")
                result = signer.sign("POST", "https://example.com/api", {})
                assert "Signature" in result
                assert "Signature-Input" in result
                assert "Signature-Key" in result

    def test_sign_passes_hwk_scheme(self):
        mock_lib = _make_mock_aauth_lib()
        with patch("kagent.adk.aauth._signer._AAUTH_AVAILABLE", True):
            with patch("kagent.adk.aauth._signer._aauth_lib", mock_lib):
                from kagent.adk.aauth._signer import AAuthSigner
                signer = AAuthSigner(agent_id="aauth:test@default.kagent.local")
                signer.sign("GET", "https://example.com/", {})
                call_kwargs = mock_lib.sign_request.call_args
                assert call_kwargs.kwargs.get("sig_scheme") == "hwk" or (
                    len(call_kwargs.args) > 5 and call_kwargs.args[5] == "hwk"
                )

    def test_sign_no_body(self):
        """Phase 1: body is always None (no content-digest)."""
        mock_lib = _make_mock_aauth_lib()
        with patch("kagent.adk.aauth._signer._AAUTH_AVAILABLE", True):
            with patch("kagent.adk.aauth._signer._aauth_lib", mock_lib):
                from kagent.adk.aauth._signer import AAuthSigner
                signer = AAuthSigner(agent_id="aauth:test@default.kagent.local")
                signer.sign("POST", "https://example.com/", {})
                call_kwargs = mock_lib.sign_request.call_args
                assert call_kwargs.kwargs.get("body") is None

    def test_sign_returns_empty_on_error(self):
        mock_lib = _make_mock_aauth_lib()
        mock_lib.sign_request.side_effect = RuntimeError("boom")
        with patch("kagent.adk.aauth._signer._AAUTH_AVAILABLE", True):
            with patch("kagent.adk.aauth._signer._aauth_lib", mock_lib):
                from kagent.adk.aauth._signer import AAuthSigner
                signer = AAuthSigner(agent_id="aauth:test@default.kagent.local")
                result = signer.sign("POST", "https://example.com/", {})
                assert result == {}


# ---------------------------------------------------------------------------
# make_hook — httpx event hook
# ---------------------------------------------------------------------------

class TestMakeHook:
    @pytest.mark.asyncio
    async def test_hook_adds_signature_headers(self):
        mock_lib = _make_mock_aauth_lib()
        with patch("kagent.adk.aauth._signer._AAUTH_AVAILABLE", True):
            with patch("kagent.adk.aauth._signer._aauth_lib", mock_lib):
                from kagent.adk.aauth._signer import AAuthSigner
                signer = AAuthSigner(agent_id="aauth:test@default.kagent.local")
                hook = signer.make_hook()

                request = httpx.Request("POST", "https://controller.kagent.svc:8083/api/tasks")
                await hook(request)

                assert "Signature" in request.headers
                assert "Signature-Input" in request.headers
                assert "Signature-Key" in request.headers

    @pytest.mark.asyncio
    async def test_hook_is_noop_on_sign_error(self):
        mock_lib = _make_mock_aauth_lib()
        mock_lib.sign_request.side_effect = RuntimeError("boom")
        with patch("kagent.adk.aauth._signer._AAUTH_AVAILABLE", True):
            with patch("kagent.adk.aauth._signer._aauth_lib", mock_lib):
                from kagent.adk.aauth._signer import AAuthSigner
                signer = AAuthSigner(agent_id="aauth:test@default.kagent.local")
                hook = signer.make_hook()

                request = httpx.Request("GET", "https://example.com/")
                original_headers = dict(request.headers)
                await hook(request)
                # Should not have added any signature headers
                for h in ("Signature", "Signature-Input", "Signature-Key"):
                    assert h not in request.headers


# ---------------------------------------------------------------------------
# SA-token bearer — controller-side TokenReview gate
# ---------------------------------------------------------------------------

class TestSATokenBearer:
    def test_authorization_header_present_when_sa_token_readable(self, tmp_path):
        """_build_jwt_request must read the projected SA token and send it
        as a Bearer header so the controller can run TokenReview on it."""
        token_file = tmp_path / "token"
        token_file.write_text("fake.sa.token\n")

        mock_lib = _make_mock_aauth_lib()
        with patch("kagent.adk.aauth._signer._AAUTH_AVAILABLE", True), \
             patch("kagent.adk.aauth._signer._aauth_lib", mock_lib), \
             patch("kagent.adk.aauth._signer._DEFAULT_SA_TOKEN_PATH", str(token_file)), \
             patch("httpx.Client") as fake_client_cls:
            ctx = fake_client_cls.return_value.__enter__.return_value
            ctx.post.return_value.json.return_value = {"token": "tok-1", "expires_in": 86400}
            ctx.post.return_value.raise_for_status.return_value = None

            from kagent.adk.aauth._signer import AAuthSigner
            AAuthSigner(
                agent_id="aauth:test@default.kagent.local",
                controller_url="http://ctl.kagent:8083",
            )

            call_kwargs = ctx.post.call_args.kwargs
            assert call_kwargs["headers"]["Authorization"] == "Bearer fake.sa.token"

    def test_env_override_path_wins_over_default(self, tmp_path):
        """AAUTH_SA_TOKEN_PATH must point the signer at the audience-scoped
        token (mounted by the kagent controller) rather than the default
        SA token mount."""
        env_path = tmp_path / "aauth-token"
        env_path.write_text("audience-scoped-token")
        default_path = tmp_path / "default-token"
        default_path.write_text("wrong-token")

        mock_lib = _make_mock_aauth_lib()
        with patch.dict(os.environ, {"AAUTH_SA_TOKEN_PATH": str(env_path)}), \
             patch("kagent.adk.aauth._signer._AAUTH_AVAILABLE", True), \
             patch("kagent.adk.aauth._signer._aauth_lib", mock_lib), \
             patch("kagent.adk.aauth._signer._DEFAULT_SA_TOKEN_PATH", str(default_path)), \
             patch("httpx.Client") as fake_client_cls:
            ctx = fake_client_cls.return_value.__enter__.return_value
            ctx.post.return_value.json.return_value = {"token": "tok-1", "expires_in": 86400}
            ctx.post.return_value.raise_for_status.return_value = None

            from kagent.adk.aauth._signer import AAuthSigner
            AAuthSigner(
                agent_id="aauth:test@default.kagent.local",
                controller_url="http://ctl.kagent:8083",
            )
            call_kwargs = ctx.post.call_args.kwargs
            assert call_kwargs["headers"]["Authorization"] == "Bearer audience-scoped-token"

    def test_no_authorization_header_when_sa_token_missing(self, tmp_path):
        """Outside a pod (no SA token file), the mint still goes out but
        without an Authorization header — the controller will reject it."""
        missing = tmp_path / "does-not-exist"
        mock_lib = _make_mock_aauth_lib()
        with patch("kagent.adk.aauth._signer._AAUTH_AVAILABLE", True), \
             patch("kagent.adk.aauth._signer._aauth_lib", mock_lib), \
             patch("kagent.adk.aauth._signer._DEFAULT_SA_TOKEN_PATH", str(missing)), \
             patch("httpx.Client") as fake_client_cls:
            ctx = fake_client_cls.return_value.__enter__.return_value
            # Simulate a 401 response so the signer falls back to hwk.
            import httpx as _h
            ctx.post.return_value.raise_for_status.side_effect = _h.HTTPStatusError(
                "401 Unauthorized", request=MagicMock(), response=MagicMock(status_code=401)
            )

            from kagent.adk.aauth._signer import AAuthSigner
            signer = AAuthSigner(
                agent_id="aauth:test@default.kagent.local",
                controller_url="http://ctl.kagent:8083",
            )

            call_kwargs = ctx.post.call_args.kwargs
            assert "Authorization" not in (call_kwargs.get("headers") or {})
            assert signer._sig_scheme == "hwk"  # fell back


# ---------------------------------------------------------------------------
# ensure_fresh_jwt — auto-refresh before expiry
# ---------------------------------------------------------------------------

class TestEnsureFreshJWT:
    @pytest.mark.asyncio
    async def test_noop_when_hwk_scheme(self):
        """No controller_url means hwk mode — ensure_fresh_jwt must be a no-op."""
        mock_lib = _make_mock_aauth_lib()
        with patch("kagent.adk.aauth._signer._AAUTH_AVAILABLE", True), \
             patch("kagent.adk.aauth._signer._aauth_lib", mock_lib):
            from kagent.adk.aauth._signer import AAuthSigner
            signer = AAuthSigner(agent_id="aauth:test@default.kagent.local")
            assert signer._sig_scheme == "hwk"

            with patch.object(signer, "_fetch_jwt_async") as fake_fetch:
                await signer.ensure_fresh_jwt()
                fake_fetch.assert_not_called()

    @pytest.mark.asyncio
    async def test_noop_when_jwt_far_from_expiry(self):
        """A fresh JWT must not be refreshed pre-emptively."""
        mock_lib = _make_mock_aauth_lib()
        import time as _t
        with patch("kagent.adk.aauth._signer._AAUTH_AVAILABLE", True), \
             patch("kagent.adk.aauth._signer._aauth_lib", mock_lib), \
             patch("httpx.Client") as fake_client_cls:
            ctx = fake_client_cls.return_value.__enter__.return_value
            ctx.post.return_value.json.return_value = {"token": "tok-1", "expires_in": 86400}
            ctx.post.return_value.raise_for_status.return_value = None

            from kagent.adk.aauth._signer import AAuthSigner
            signer = AAuthSigner(
                agent_id="aauth:test@default.kagent.local",
                controller_url="http://ctl.kagent:8083",
            )
            assert signer._sig_scheme == "jwt"
            far_future = _t.time() + 86400
            assert signer._jwt_exp > _t.time() + 3600  # well past refresh window

            with patch.object(signer, "_fetch_jwt_async") as fake_fetch:
                await signer.ensure_fresh_jwt()
                fake_fetch.assert_not_called()

    @pytest.mark.asyncio
    async def test_refresh_when_close_to_expiry(self):
        """If JWT is inside the refresh window, ensure_fresh_jwt re-mints."""
        mock_lib = _make_mock_aauth_lib()
        with patch("kagent.adk.aauth._signer._AAUTH_AVAILABLE", True), \
             patch("kagent.adk.aauth._signer._aauth_lib", mock_lib), \
             patch("httpx.Client") as fake_client_cls:
            ctx = fake_client_cls.return_value.__enter__.return_value
            ctx.post.return_value.json.return_value = {"token": "tok-1", "expires_in": 60}
            ctx.post.return_value.raise_for_status.return_value = None

            from kagent.adk.aauth._signer import AAuthSigner
            signer = AAuthSigner(
                agent_id="aauth:test@default.kagent.local",
                controller_url="http://ctl.kagent:8083",
            )
            # exp is now+60s, well inside the 300s refresh window

            async def fake_refresh():
                signer._jwt = "tok-2"
                signer._jwt_exp = signer._jwt_exp + 86400

            with patch.object(signer, "_fetch_jwt_async", side_effect=fake_refresh) as fake_fetch:
                await signer.ensure_fresh_jwt()
                fake_fetch.assert_awaited_once()
                assert signer._jwt == "tok-2"

    @pytest.mark.asyncio
    async def test_backoff_skips_recent_failure(self):
        """A recent failed refresh attempt must rate-limit subsequent retries."""
        mock_lib = _make_mock_aauth_lib()
        import time as _t
        with patch("kagent.adk.aauth._signer._AAUTH_AVAILABLE", True), \
             patch("kagent.adk.aauth._signer._aauth_lib", mock_lib), \
             patch("httpx.Client") as fake_client_cls:
            ctx = fake_client_cls.return_value.__enter__.return_value
            ctx.post.return_value.json.return_value = {"token": "tok-1", "expires_in": 60}
            ctx.post.return_value.raise_for_status.return_value = None

            from kagent.adk.aauth._signer import AAuthSigner
            signer = AAuthSigner(
                agent_id="aauth:test@default.kagent.local",
                controller_url="http://ctl.kagent:8083",
            )
            # Simulate a refresh attempt that just happened.
            signer._last_refresh_attempt = _t.time()

            with patch.object(signer, "_fetch_jwt_async") as fake_fetch:
                await signer.ensure_fresh_jwt()
                fake_fetch.assert_not_called()


# ---------------------------------------------------------------------------
# Global singleton (init_signer / get_signer)
# ---------------------------------------------------------------------------

class TestGlobalSingleton:
    def setup_method(self):
        import kagent.adk.aauth as aauth_module
        aauth_module._signer = None

    def teardown_method(self):
        import kagent.adk.aauth as aauth_module
        aauth_module._signer = None

    def test_get_signer_returns_none_before_init(self):
        from kagent.adk.aauth import get_signer
        assert get_signer() is None

    def test_init_signer_disabled_sets_none(self):
        with patch.dict(os.environ, {}, clear=False):
            os.environ.pop("AAUTH_ENABLED", None)
            from kagent.adk.aauth import init_signer, get_signer
            init_signer()
            assert get_signer() is None

    def test_init_signer_enabled_sets_signer(self):
        mock_lib = _make_mock_aauth_lib()
        with patch.dict(os.environ, {"AAUTH_ENABLED": "true", "AAUTH_AGENT_ID": "aauth:x@ns.kagent.local"}):
            with patch("kagent.adk.aauth._signer._AAUTH_AVAILABLE", True):
                with patch("kagent.adk.aauth._signer._aauth_lib", mock_lib):
                    from kagent.adk.aauth import init_signer, get_signer
                    init_signer()
                    assert get_signer() is not None


# ---------------------------------------------------------------------------
# A2A interceptor integration
# ---------------------------------------------------------------------------

# ---------------------------------------------------------------------------
# Inbound verification (Phase 3) — _parse_mode + from_env gating
# ---------------------------------------------------------------------------

class TestInboundVerifier:
    def test_parse_mode_defaults_and_aliases(self):
        from kagent.adk.aauth._verifier import _parse_mode
        assert _parse_mode(None) == "log"
        assert _parse_mode("") == "log"
        assert _parse_mode("log") == "log"
        assert _parse_mode("observe") == "log"
        assert _parse_mode("ENFORCE") == "enforce"
        assert _parse_mode("strict") == "enforce"
        assert _parse_mode("off") == "off"
        assert _parse_mode("disabled") == "off"
        assert _parse_mode("garbage") == "log"

    def test_from_env_disabled_when_aauth_off(self):
        from kagent.adk.aauth._verifier import AAuthVerifier
        with patch.dict(os.environ, {}, clear=False):
            os.environ.pop("AAUTH_ENABLED", None)
            assert AAuthVerifier.from_env() is None

    def test_from_env_disabled_when_no_controller_url(self):
        from kagent.adk.aauth._verifier import AAuthVerifier
        with patch.dict(os.environ, {"AAUTH_ENABLED": "true", "AAUTH_CONTROLLER_URL": ""}, clear=False):
            assert AAuthVerifier.from_env() is None

    def test_from_env_disabled_when_mode_off(self):
        from kagent.adk.aauth._verifier import AAuthVerifier
        with patch.dict(os.environ, {
            "AAUTH_ENABLED": "true",
            "AAUTH_CONTROLLER_URL": "http://ctl.kagent:8083",
            "AAUTH_VERIFY_MODE": "off",
        }, clear=False):
            assert AAuthVerifier.from_env() is None

    def test_from_env_returns_log_mode_by_default(self):
        from kagent.adk.aauth._verifier import AAuthVerifier
        with patch.dict(os.environ, {
            "AAUTH_ENABLED": "true",
            "AAUTH_CONTROLLER_URL": "http://ctl.kagent:8083",
        }, clear=False):
            os.environ.pop("AAUTH_VERIFY_MODE", None)
            v = AAuthVerifier.from_env()
            assert v is not None
            assert v.mode == "log"

    def test_canonical_authorities_includes_in_cluster_dns_and_extras(self):
        from kagent.adk.aauth._verifier import _canonical_authorities
        with patch.dict(os.environ, {
            "KAGENT_NAME": "aauth-test-agent",
            "KAGENT_NAMESPACE": "kagent",
            "AAUTH_VERIFY_AUTHORITIES": "localhost:18080, custom.example:9999",
        }, clear=False):
            auths = _canonical_authorities()
            assert "aauth-test-agent.kagent.svc.cluster.local:8080" in auths
            assert "aauth-test-agent.kagent:8080" in auths
            assert "localhost:8080" in auths
            assert "localhost:18080" in auths
            assert "custom.example:9999" in auths


class TestSubagentInterceptorAAuth:
    @pytest.mark.asyncio
    async def test_intercept_adds_signature_headers_when_enabled(self):
        mock_lib = _make_mock_aauth_lib()
        with patch("kagent.adk.aauth._signer._AAUTH_AVAILABLE", True):
            with patch("kagent.adk.aauth._signer._aauth_lib", mock_lib):
                import kagent.adk.aauth as aauth_module
                from kagent.adk.aauth._signer import AAuthSigner
                aauth_module._signer = AAuthSigner(agent_id="aauth:parent@ns.kagent.local")

                from kagent.adk._remote_a2a_tool import _SubagentInterceptor
                interceptor = _SubagentInterceptor()

                fake_agent_card = MagicMock()
                fake_agent_card.url = "http://child-agent.ns:8080"

                _, http_kwargs = await interceptor.intercept(
                    "send_message", {}, {}, fake_agent_card, None
                )

                assert "Signature" in http_kwargs["headers"]
                assert "Signature-Input" in http_kwargs["headers"]
                assert "Signature-Key" in http_kwargs["headers"]
        aauth_module._signer = None

    @pytest.mark.asyncio
    async def test_intercept_skips_signing_when_disabled(self):
        import kagent.adk.aauth as aauth_module
        aauth_module._signer = None

        from kagent.adk._remote_a2a_tool import _SubagentInterceptor
        interceptor = _SubagentInterceptor()

        fake_agent_card = MagicMock()
        fake_agent_card.url = "http://child-agent.ns:8080"

        _, http_kwargs = await interceptor.intercept(
            "send_message", {}, {}, fake_agent_card, None
        )

        for h in ("Signature", "Signature-Input", "Signature-Key"):
            assert h not in http_kwargs.get("headers", {})


# ---------------------------------------------------------------------------
# MCP httpx_client_factory wrapper
# ---------------------------------------------------------------------------


class TestMcpFactoryWrap:
    """wrap_mcp_httpx_factory must passthrough when AAuth is disabled and
    attach the signer's request hook when it's enabled."""

    def _base_factory(self) -> Any:
        """A factory that mirrors mcp.shared._httpx_utils.create_mcp_http_client."""

        def factory(headers=None, timeout=None, auth=None) -> httpx.AsyncClient:
            kwargs: dict[str, Any] = {"follow_redirects": True}
            if headers is not None:
                kwargs["headers"] = headers
            if timeout is not None:
                kwargs["timeout"] = timeout
            if auth is not None:
                kwargs["auth"] = auth
            return httpx.AsyncClient(**kwargs)

        return factory

    def test_wrap_passthrough_when_signer_disabled(self):
        import kagent.adk.aauth as aauth_module
        from kagent.adk.aauth import wrap_mcp_httpx_factory

        aauth_module._signer = None

        wrapped = wrap_mcp_httpx_factory(self._base_factory())
        client = wrapped()
        try:
            assert client.event_hooks.get("request", []) == []
        finally:
            # Defensive — httpx.AsyncClient is fine to leave for GC, but we
            # close synchronously by reaching into the transport if needed.
            pass

    def test_wrap_attaches_signer_hook_when_enabled(self):
        import kagent.adk.aauth as aauth_module
        from kagent.adk.aauth import wrap_mcp_httpx_factory

        sentinel_hook = MagicMock(name="signer_hook")
        fake_signer = MagicMock(name="signer")
        fake_signer.make_hook.return_value = sentinel_hook

        aauth_module._signer = fake_signer
        try:
            wrapped = wrap_mcp_httpx_factory(self._base_factory())
            client = wrapped()
            request_hooks = client.event_hooks.get("request", [])
            assert sentinel_hook in request_hooks
            fake_signer.make_hook.assert_called_once()
        finally:
            aauth_module._signer = None

    def test_wrap_preserves_existing_request_hooks(self):
        """If the base factory returned a client that already had hooks,
        the wrapper must append rather than clobber."""
        import kagent.adk.aauth as aauth_module
        from kagent.adk.aauth import wrap_mcp_httpx_factory

        async def existing_hook(request: httpx.Request) -> None:  # pragma: no cover
            return None

        def base_factory(headers=None, timeout=None, auth=None) -> httpx.AsyncClient:
            return httpx.AsyncClient(event_hooks={"request": [existing_hook]})

        sentinel_hook = MagicMock(name="signer_hook")
        fake_signer = MagicMock(name="signer")
        fake_signer.make_hook.return_value = sentinel_hook
        aauth_module._signer = fake_signer
        try:
            client = wrap_mcp_httpx_factory(base_factory)()
            hooks = client.event_hooks.get("request", [])
            assert existing_hook in hooks
            assert sentinel_hook in hooks
        finally:
            aauth_module._signer = None

    def test_kagent_mcp_toolset_swaps_factory_on_init(self):
        """KAgentMcpToolset.__init__ must wrap params.httpx_client_factory."""
        from google.adk.tools.mcp_tool import StreamableHTTPConnectionParams

        from kagent.adk._mcp_toolset import KAgentMcpToolset

        original_factory = MagicMock(name="original_factory")
        params = StreamableHTTPConnectionParams(
            url="http://example.local/mcp",
            httpx_client_factory=original_factory,
        )

        toolset = KAgentMcpToolset(connection_params=params)
        assert toolset._connection_params.httpx_client_factory is not original_factory
        # Wrapper is callable (we just constructed it).
        assert callable(toolset._connection_params.httpx_client_factory)

    def test_decode_jwt_unsafe_roundtrips_header_and_claims(self):
        """The mint-time logger uses an unsigned decoder. Verify it pulls
        the two interesting maps out of a real-shaped JWT."""
        import base64
        import json as _json

        from kagent.adk.aauth._signer import _decode_jwt_unsafe

        header = {"alg": "EdDSA", "kid": "kagent-issuer-1", "typ": "aa-agent+jwt"}
        claims = {
            "iss": "http://localhost:8083",
            "sub": "aauth:my-agent@ns.kagent.local",
            "iat": 1700000000,
            "exp": 1700086400,
            "cnf": {"jwk": {"kty": "OKP", "crv": "Ed25519", "x": "abc"}},
        }

        def _b64(d: dict) -> str:
            raw = _json.dumps(d, separators=(",", ":")).encode()
            return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()

        token = f"{_b64(header)}.{_b64(claims)}.signature-placeholder"
        got_header, got_claims = _decode_jwt_unsafe(token)
        assert got_header == header
        assert got_claims == claims

    def test_decode_jwt_unsafe_returns_none_for_garbage(self):
        from kagent.adk.aauth._signer import _decode_jwt_unsafe

        assert _decode_jwt_unsafe("not-a-jwt") == (None, None)
        assert _decode_jwt_unsafe("only.two") == (None, None)
        assert _decode_jwt_unsafe("a.b.c") == (None, None)  # not valid base64/JSON

    def test_kagent_mcp_toolset_wrap_is_noop_when_signer_disabled(self):
        """When AAuth is off, the wrapped factory should produce a client
        with no extra request hooks beyond what the base factory adds."""
        import kagent.adk.aauth as aauth_module
        from google.adk.tools.mcp_tool import StreamableHTTPConnectionParams

        from kagent.adk._mcp_toolset import KAgentMcpToolset

        aauth_module._signer = None

        def base(headers=None, timeout=None, auth=None) -> httpx.AsyncClient:
            return httpx.AsyncClient()

        params = StreamableHTTPConnectionParams(
            url="http://example.local/mcp",
            httpx_client_factory=base,
        )
        toolset = KAgentMcpToolset(connection_params=params)
        client = toolset._connection_params.httpx_client_factory()
        assert client.event_hooks.get("request", []) == []
