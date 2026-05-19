# AAuth Phase 0 + Phase 1 Plan (Option A)

## Context

kagent today handles agent identity via Kubernetes ServiceAccount + projected SA token + a few request headers (`X-Agent-Name`, `X-User-Id`). There is no cryptographic agent identity, no signature on outbound requests, and the receiving side has no way to verify *which* agent originated a call beyond trusting headers (which any client can set).

This plan adds **opt-in AAuth** ([draft-hardt-oauth-aauth-protocol](https://datatracker.ietf.org/doc/draft-hardt-oauth-aauth-protocol/)) signing to **declarative agents** (the Python google-adk runtime). Goal: when enabled, every outbound HTTP request an agent makes carries RFC 9421 HTTP Message Signature headers, with the agent's identity asserted via an `aa-agent+jwt` JWT bound to the signing key.

The work is **strictly additive**:
- Disabled by default. When disabled, runtime is byte-identical to today.
- When enabled, AAuth headers (`Signature`, `Signature-Input`, `Signature-Key`) are added *alongside* the existing `Authorization: Bearer <projected-sa-token>` and `X-Agent-Name` headers — nothing existing is removed or rewired.
- Receiving-side verification is deferred to a later phase (Phase 3). Phase 1 produces signatures; nothing yet enforces them.

The end-state vision is that AAuth replaces the projected SA token as the in-cluster identity mechanism, but that is *not* this phase.

## Scope

### In scope
- **Phase 0**: CRD field `spec.aauth.enabled`, env-var plumbing in the Go translator, no behavior change at runtime when disabled.
- **Phase 1**: Outbound signing wired into 5 call sites in the Python runtime:
  1. Agent → controller (tasks, sessions) — httpx event hooks
  2. Agent → other agent (A2A) — `_SubagentInterceptor`
  3. Agent → OpenAI — httpx event hooks on the OpenAI SDK's http_client
  4. Agent → Anthropic — httpx event hooks on the Anthropic SDK's http_client
  5. Agent → Ollama — httpx event hooks on the Ollama SDK's http_client

### Out of scope (explicitly deferred)
- **Gemini (google-genai)** signing — SDK has no httpx injection point. Defer pending upstream PR.
- **Bedrock (boto3)** signing — uses botocore.httpsession, not httpx. Defer pending upstream PR or botocore session patch.
- **MCP** signing — `mcp` SDK hardcodes `httpx.AsyncClient()` internally; needs upstream change. Defer.
- **Phase 2** — controller-side `aa-agent+jwt` issuer endpoint.
- **Phase 3** — verification middleware on the receiving side (controller HTTP server, agent A2A receivers).
- **BYO (non-declarative) agents.**

## Design decisions

These are decisions I'm making with rationale. Each can be overridden cheaply later because the surface area is small.

1. **Keypair lifecycle: ephemeral, generated per pod start.** No K8s Secret, no controller dependency, key never leaves the pod. Acceptable because Phase 1 has no verifier — agent identity is observable but not enforced. Phase 2 swaps in persistent issuer-bound keys without touching the Python signer interface.

2. **JWT in Phase 1: self-issued dev JWT.** Agent generates Ed25519 keypair AND self-signs an `aa-agent+jwt` with that key, using a synthesized `iss` derived from its name+namespace. **Not** spec-conformant for production (no real issuer JWKS endpoint), but produces full spec-shaped headers we can inspect on the wire. Phase 2 swaps in a controller-issued JWT — only the source of the JWT bytes changes; signer interface is identical.

3. **Signed components: minimal RFC 9421 covered fields.** Only `@method`, `@authority`, `@path`, `signature-key`, `created`. **No body content-digest** in Phase 1. Rationale: matches the spec example exactly, sidesteps streaming/body-capture complications (A2A streaming, LLM streaming responses). Content-digest can be enabled later behind an additional flag.

4. **Signature algorithm: EdDSA (Ed25519).** Spec recommendation.

5. **Configuration via env vars on the agent pod.** Set by the Go translator when `spec.aauth.enabled=true`:
   - `AAUTH_ENABLED` (bool, default false)
   - `AAUTH_AGENT_ID` (full URI: `aauth:<name>@<namespace>.kagent.local`)
   - `AAUTH_ISSUER_URL` (Phase 1: derived from name/namespace; Phase 2: controller URL)

6. **Library packaging.** `aauth` Python library added as a dep on `kagent-adk` via `pyproject.toml`, pinned to a specific version (or Git ref if not yet published).

7. **Module layout.** New `kagent.adk.aauth` submodule, internal-only (not exported from `kagent.adk.__init__`).

8. **Signer access pattern.** Module-level singleton (`kagent.adk.aauth.get_signer()`) set once in `cli.py` startup. Avoids plumbing the signer through every constructor on the way to the LLM providers. Each call site does `signer = get_signer(); if signer: ...`.

## Phase 0 — Go side

### Files to modify

**`go/api/v1alpha2/agent_types.go`** — new `AAuthConfig`, optional field on `DeclarativeAgentSpec` (near line 198):
```go
// AAuthConfig configures opt-in AAuth (HTTP Message Signatures) for outbound agent requests.
type AAuthConfig struct {
    // Enabled turns on AAuth signing for this agent's outbound requests.
    // +kubebuilder:default=false
    Enabled bool `json:"enabled,omitempty"`
}

// In DeclarativeAgentSpec:
// +optional
AAuth *AAuthConfig `json:"aauth,omitempty"`
```
No `issuerRef` yet — Phase 2 introduces it. Keep API surface minimal.

**`go/core/pkg/env/kagent.go`** — register env-var constants:
```go
AAUthEnabled   = RegisterStringVar("AAUTH_ENABLED",    "false", "Enable AAuth signing", ComponentAgentRuntime)
AAUthAgentID   = RegisterStringVar("AAUTH_AGENT_ID",   "",      "AAuth agent identifier URI", ComponentAgentRuntime)
AAUthIssuerURL = RegisterStringVar("AAUTH_ISSUER_URL", "",      "AAuth issuer URL", ComponentAgentRuntime)
```

**`go/core/internal/controller/translator/agent/compiler.go`** — `translateInlineAgent()` reads `spec.Declarative.AAuth` and propagates into `AgentManifestInputs` (new field `AAuthEnabled bool`).

**`go/core/internal/controller/translator/agent/manifest_builder.go`** — `collectSharedEnv()` conditionally appends three env vars when AAuth is enabled. Mirror the existing conditional patterns (see how `KAGENT_PROPAGATE_TOKEN` is set today).

**`go/Makefile`** — no change. After CRD edits, run `make -C go manifests` to regenerate `helm/kagent-crds/templates/*.yaml` and deepcopy code.

### Tests
- `agent_types_test.go`: deepcopy roundtrip for `AAuthConfig`.
- `manifest_builder_test.go`: table test — with `aauth.enabled=true`, env vars present; with `enabled=false`, env vars absent; with `aauth=nil`, env vars absent.

## Phase 1 — Python side

### New module: `python/packages/kagent-adk/src/kagent/adk/aauth/`

```
aauth/
  __init__.py     # get_signer(), set_signer(), AAuthSigner re-export
  config.py       # AAuthConfig dataclass + from_env()
  bootstrap.py    # generate_keypair() + sign_dev_jwt()
  signer.py       # AAuthSigner class
  hooks.py        # make_request_hook(signer) → async httpx event hook
```

**`config.py`** — env-var parsing into a typed struct:
```python
@dataclass
class AAuthConfig:
    enabled: bool
    agent_id: str
    issuer_url: str

    @classmethod
    def from_env(cls) -> "AAuthConfig | None":
        if os.getenv("AAUTH_ENABLED", "false").lower() != "true":
            return None
        return cls(
            enabled=True,
            agent_id=os.environ["AAUTH_AGENT_ID"],
            issuer_url=os.environ["AAUTH_ISSUER_URL"],
        )
```

**`bootstrap.py`** — generate Ed25519 keypair + self-sign dev JWT:
```python
def bootstrap_dev_identity(cfg: AAuthConfig) -> tuple[Ed25519PrivateKey, str]:
    """Phase 1: generate keypair + self-signed aa-agent+jwt."""
    priv, pub = aauth.generate_ed25519_keypair()
    jwk = aauth.public_key_to_jwk(pub, kid="dev-1")
    jwt = _craft_aa_agent_jwt(
        iss=cfg.issuer_url, sub=cfg.agent_id, cnf_jwk=jwk, signing_key=priv,
    )
    return priv, jwt
```

**`signer.py`** — wraps the `aauth` library; this is the only place the lib API is touched:
```python
class AAuthSigner:
    def __init__(self, private_key, agent_id: str, agent_token: str):
        self._key = private_key
        self._agent_id = agent_id
        self._token = agent_token

    def sign(self, method: str, url: str, headers: dict[str, str], body: bytes | None = None) -> dict[str, str]:
        """Returns headers to add. Body is unused in Phase 1 (no content-digest)."""
        return aauth.sign_request(
            method=method, target_uri=url, headers=headers, body=None,
            private_key=self._key, sig_scheme="jwt", jwt=self._token,
        )

    @classmethod
    def from_env(cls) -> "AAuthSigner | None":
        cfg = AAuthConfig.from_env()
        if cfg is None:
            return None
        priv, jwt = bootstrap_dev_identity(cfg)
        return cls(priv, cfg.agent_id, jwt)
```

**`hooks.py`** — httpx event hook factory:
```python
def make_request_hook(signer: AAuthSigner):
    async def hook(request: httpx.Request):
        sig_headers = signer.sign(
            method=request.method,
            url=str(request.url),
            headers=dict(request.headers),
        )
        request.headers.update(sig_headers)
    return hook
```

### Modifications to existing files

**`cli.py`** — instantiate signer once at startup:
```python
# In run() / static() entrypoint, before KAgentApp.build():
from kagent.adk.aauth import AAuthSigner, set_signer
set_signer(AAuthSigner.from_env())  # None if disabled
```

**`_a2a.py` (lines 96-100)** — extend httpx event_hooks for the controller client:
```python
from kagent.adk.aauth import get_signer, make_request_hook

hooks = token_service.event_hooks()
signer = get_signer()
if signer:
    hooks.setdefault("request", []).append(make_request_hook(signer))
http_client = httpx.AsyncClient(base_url=..., event_hooks=hooks)
```

**`_remote_a2a_tool.py` (lines 70-77)** — extend `_SubagentInterceptor.intercept()`:
```python
async def intercept(self, method_name, request_payload, http_kwargs, agent_card, context):
    headers = dict(http_kwargs.get("headers", {}))
    headers[_SOURCE_HEADER] = _SOURCE_SUBAGENT
    if context and _USER_ID_CONTEXT_KEY in context.state:
        headers["x-user-id"] = context.state[_USER_ID_CONTEXT_KEY]
    signer = get_signer()
    if signer:
        sig_headers = signer.sign(method="POST", url=agent_card.url, headers=headers)
        headers.update(sig_headers)
    http_kwargs["headers"] = headers
    return request_payload, http_kwargs
```
Assumption: A2A is JSON-RPC over POST and the URL is the bare `agent_card.url`. Worth a smoke test that this matches the actual wire request. See Risk #2.

**`models/_openai.py` (lines ~401-424)** — attach event hook to the SDK's httpx client:
```python
from kagent.adk.aauth import get_signer, make_request_hook

def _create_http_client(self) -> httpx.AsyncClient:
    event_hooks: dict[str, list] = {}
    signer = get_signer()
    if signer:
        event_hooks["request"] = [make_request_hook(signer)]
    return httpx.AsyncClient(..., event_hooks=event_hooks)

# Then AsyncOpenAI(..., http_client=self._create_http_client())
```

**`models/_anthropic.py` (lines ~36-58)** — same pattern. `_create_http_client()` already exists; add the event_hooks.

**`models/_ollama.py` (lines ~156-165)** — same pattern. Construct an `httpx.AsyncClient` with the hook and pass it via the kwargs that Ollama's `AsyncClient(**kwargs)` forwards to httpx. May require switching from `**self._tls_httpx_kwargs()` style to an explicit `httpx.AsyncClient` instance (Ollama SDK supports both).

### New dependency

`python/packages/kagent-adk/pyproject.toml`:
```toml
dependencies = [
    ...
    "aauth @ git+https://github.com/christian-posta/aauth-python-library@<commit-sha>",
    ...
]
```
Pin to a specific commit SHA until the library has a PyPI release.

### Tests

`python/packages/kagent-adk/tests/aauth/` — pytest, async where needed:
- `test_config.py` — `AAuthConfig.from_env()` returns None when disabled, populated struct when enabled.
- `test_bootstrap.py` — `bootstrap_dev_identity()` produces a parseable `aa-agent+jwt` with correct claims (`typ`, `iss`, `sub`, `cnf.jwk`, `iat`, `exp`).
- `test_signer.py` — given a fixed key + JWT, signing produces deterministic-shaped headers; verify with the library's `verify_signature()` round-trip.
- `test_hooks_controller.py` — using `respx`, intercept an outbound httpx call from a client built with `make_request_hook(signer)`; assert `Signature`, `Signature-Input`, `Signature-Key` headers on the captured request.
- `test_hooks_a2a.py` — instantiate `_SubagentInterceptor` with a signer; call `intercept()`; assert signature headers added to `http_kwargs["headers"]`.
- `test_hooks_openai.py` — patch the OpenAI endpoint with `respx`; instantiate the model; assert outbound request has signature headers.
- Negative tests for each: with `AAUTH_ENABLED=false`, no headers added.

## Verification (end-to-end)

1. **Unit tests:**
   - Python: `cd python && uv run pytest packages/kagent-adk/tests/aauth/ -v`
   - Go: `make -C go test`

2. **Lint:** `make -C go lint`

3. **CRD codegen:** `make -C go manifests` — diff should show new `aauth` field on `Agent` in `helm/kagent-crds/templates/kagent.dev_agents.yaml`.

4. **E2E (local Kind cluster):**
   - `make create-kind-cluster && make helm-install`
   - Deploy two declarative agents in a test namespace, one with `spec.aauth.enabled: true`. Have it call the other as a subagent.
   - On the *receiving* agent, temporarily enable httpx wire logging (`HTTPX_LOG_LEVEL=DEBUG`) or add a debug logger middleware to capture inbound headers.
   - Grep logs for `Signature`, `Signature-Input`, `Signature-Key`. All three must be present, well-formed (RFC 9421 structured field syntax).
   - Repeat for an OpenAI call: log the outbound httpx request headers from the agent pod. Verify signature headers present.
   - **Negative test:** redeploy with `spec.aauth.enabled: false`. Confirm no AAuth headers in any logged request, and that behavior is otherwise identical.

5. **Manual verification of signature integrity:**
   - From a captured request, take method, URL, headers, the `Signature` / `Signature-Input` / `Signature-Key` triple, and run them through the library's `verify_signature()` standalone. Should return `valid=True`.

## Risk register

1. **Self-issued JWT is not spec-conformant.** Phase 1 only — acceptable because nothing verifies. Hard block: do not roll out a verifier (Phase 3) before the controller-issued JWT (Phase 2) replaces self-issuance. Track in plan as a Phase-2 prerequisite.

2. **A2A signing approximates the wire request.** We assume method=POST and url=`agent_card.url`. If the A2A SDK appends path segments or rewrites the URL, the signature won't cover the actual sent request. Mitigation: integration test that captures the *actual* sent bytes (via `respx`) and verifies the signature against them. If the assumption breaks, fall back to wrapping the A2A client's httpx transport.

3. **Per-pod ephemeral keys** = new identity on every restart. Phase 2 fixes this with persistent keys. Acceptable for Phase 1.

4. **Library is "exploratory."** `christian-posta/aauth-python-library` is marked exploratory; API may shift. Pin to a commit SHA. The `signer.py` wrapper is the only file that touches the library's API, so churn is bounded to one file.

5. **httpx event-hook ordering** is undefined relative to other hooks. The bearer-token hook adds `Authorization` and `X-Agent-Name` — we want those signed *if* they end up in the covered set. Phase 1 covered set is only `@method @authority @path signature-key created`, so header ordering doesn't matter. If we add header coverage later (Phase 1.5), enforce hook ordering: bearer-token first, signer last.

6. **Body content-digest is deliberately not signed in Phase 1.** This means a man-in-the-middle could swap the body without breaking the signature. Acceptable for Phase 1 (no verifier anyway). Phase 3 (verifier) must coincide with enabling content-digest.

## Rollout

- **Phase 0 lands first**, no behavior change. Mergeable independently.
- **Phase 1 lands behind `spec.aauth.enabled: false` default.** Internal dogfood on one test agent. Observe headers, sanity-check signatures with the library's verifier in a standalone script.
- **Do not promote to "stable"** until Phase 2 (issuer) and Phase 3 (verifier) are at least scoped and tracked.

## Critical files reference

Go:
- `go/api/v1alpha2/agent_types.go` (around line 198: `DeclarativeAgentSpec`)
- `go/core/pkg/env/kagent.go`
- `go/core/internal/controller/translator/agent/compiler.go` (`translateInlineAgent()`)
- `go/core/internal/controller/translator/agent/manifest_builder.go` (`collectSharedEnv()`, around line 315-335)

Python (new):
- `python/packages/kagent-adk/src/kagent/adk/aauth/{__init__,config,bootstrap,signer,hooks}.py`
- `python/packages/kagent-adk/tests/aauth/*`

Python (modified):
- `python/packages/kagent-adk/src/kagent/adk/cli.py` (startup)
- `python/packages/kagent-adk/src/kagent/adk/_a2a.py` (line 96-100)
- `python/packages/kagent-adk/src/kagent/adk/_remote_a2a_tool.py` (line 70-77)
- `python/packages/kagent-adk/src/kagent/adk/models/_openai.py` (line 401-424)
- `python/packages/kagent-adk/src/kagent/adk/models/_anthropic.py` (line 36-58)
- `python/packages/kagent-adk/src/kagent/adk/models/_ollama.py` (line 156-165)
- `python/packages/kagent-adk/pyproject.toml` (add `aauth` dep)
