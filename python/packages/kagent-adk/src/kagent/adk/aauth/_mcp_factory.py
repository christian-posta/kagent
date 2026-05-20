"""AAuth-signing wrapper for MCP httpx_client_factory.

google-adk's MCP connection params (``StreamableHTTPConnectionParams``,
``SseConnectionParams``) expose ``httpx_client_factory``, which the MCP
SDK calls to construct the underlying ``httpx.AsyncClient``. We wrap
that factory so the returned client carries our AAuth signing hook.

When AAuth is disabled (``get_signer()`` returns None), the wrapper is a
pure passthrough — the client comes back exactly as MCP would have built
it. When enabled, every outbound MCP HTTP call to the built-in
controller MCP endpoint (or any other Streamable/SSE MCP server) is
signed with RFC 9421 HTTP Message Signatures.

This is the cleanest available injection point: no monkey-patching, no
upstream PR. The factory is invoked once per MCP session, and the hook
re-runs for every request the session makes — including JWT refresh,
since ``AAuthSigner.make_hook`` awaits ``ensure_fresh_jwt`` on each call.
"""

from __future__ import annotations

from typing import Callable

import httpx

from . import get_signer

McpHttpFactory = Callable[..., httpx.AsyncClient]


def wrap_mcp_httpx_factory(base: McpHttpFactory) -> McpHttpFactory:
    """Return a factory that adds AAuth signing to clients produced by ``base``.

    The wrapped factory preserves ``base``'s defaults (follow_redirects,
    timeouts, etc.). It only appends a request event hook — never
    overrides existing hooks the caller may have configured.

    Resolution of the signer is deferred to call time, not wrap time, so
    the wrapper composes cleanly with ``init_signer()`` ordering at
    startup.
    """

    def factory(
        headers: dict[str, str] | None = None,
        timeout: httpx.Timeout | None = None,
        auth: httpx.Auth | None = None,
    ) -> httpx.AsyncClient:
        client = base(headers=headers, timeout=timeout, auth=auth)
        signer = get_signer()
        if signer is None:
            return client
        hooks = client.event_hooks
        request_hooks = list(hooks.get("request", []))
        request_hooks.append(signer.make_hook())
        hooks["request"] = request_hooks
        client.event_hooks = hooks
        return client

    return factory
