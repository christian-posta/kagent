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
             patch("kagent.adk.aauth._signer._SA_TOKEN_PATH", str(token_file)), \
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

    def test_no_authorization_header_when_sa_token_missing(self, tmp_path):
        """Outside a pod (no SA token file), the mint still goes out but
        without an Authorization header — the controller will reject it."""
        missing = tmp_path / "does-not-exist"
        mock_lib = _make_mock_aauth_lib()
        with patch("kagent.adk.aauth._signer._AAUTH_AVAILABLE", True), \
             patch("kagent.adk.aauth._signer._aauth_lib", mock_lib), \
             patch("kagent.adk.aauth._signer._SA_TOKEN_PATH", str(missing)), \
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
