# AAuth Demo — Known Gaps & Follow-ups

This file tracks the deltas between the demo implementation and full
conformance with [draft-hardt-oauth-aauth-protocol][spec]. The demo is
intentionally minimal — it proves the wire flow (agent signs, gateway
delegates to extauth, extauth verifies via controller JWKS) and produces
`level=identified` in extauth's log. Anything beyond that is captured below.

[spec]: https://datatracker.ietf.org/doc/draft-hardt-oauth-aauth-protocol/

## Spec conformance gaps

### 1. Missing `jti` claim in `aa-agent+jwt`  — **must-fix before any rollout**

Spec §535 lists `jti` as a REQUIRED claim ("Unique token identifier for replay
detection, audit, and revocation"). Our minted tokens omit it.

- File: `go/core/internal/aauth/issuer.go` — `MintAgentJWT`
- Fix: add `"jti": uuid.NewString()` (or any unique value) to the claims map.
- Why this matters: without `jti`, a verifier cannot deduplicate replays or
  revoke a specific token. extauth doesn't enforce it today, but any
  spec-conformant verifier will reject our tokens.

### 2. Agent identity proof at `POST /aauth/agent-jwt`  — **DONE** (Kubernetes TokenReview)

The controller now requires a Bearer SA token on the mint request, validates
it via `TokenReview` against the kube-apiserver, and derives the canonical
`sub` from the SA's `system:serviceaccount:<ns>:<name>` username. The body
`sub` is optional and must match (or is rejected with 403). See
`go/core/internal/aauth/tokenreview.go` and the binding in
`helm/kagent/templates/rbac/auth-delegator-clusterrolebinding.yaml`.

Audience-scoped SA tokens (project the agent pod's token with
`audience: kagent-controller` and configure `K8sSubjectAuthenticator.Audiences`
to require it) remain a hardening follow-up. Without that, a token stolen
from the agent pod could be replayed against any other service that calls
TokenReview without an audience check.

### 3. Ephemeral controller issuer key  — **DONE** (persisted in Secret)

The controller's Ed25519 issuer keypair is now loaded from (or generated
into, on first start) the Secret `kagent-aauth-issuer` in the controller's
namespace. JWKS stays stable across restarts; outstanding agent JWTs remain
verifiable. See `go/core/internal/aauth/keystore.go`.

Companion change: the Python signer auto-refreshes its JWT within a
5-minute window before expiry, so long-running agent pods no longer need a
restart at the 24 h boundary. See `python/.../aauth/_signer.py`
(`ensure_fresh_jwt`).

### 4. Agent token `kid` rotation

We use a fixed `kid = "kagent-issuer-1"` carried in the Secret. The kid
field is in place; rotation logic (writing a second key under a new kid,
serving both in the JWKS during a grace period, then retiring the old one)
has not been built yet.

### 5. Agent Provider metadata is minimal

We publish only `issuer` + `jwks_uri` at `/.well-known/aauth-agent.json`.
Spec §2186 lists several OPTIONAL fields. None are required, but adding
`client_name` (e.g. "kagent") gives verifiers and audit logs something
human-readable.

- File: `go/core/internal/aauth/issuer.go` — `AgentMetadata()`

### 6. No `ps` claim in agent tokens

Spec §554 makes `ps` (person server URL) optional. Adding it would unlock
PS-asserted and federated access modes (§265-365 / adoption matrix §2311-12).
Out of scope for now — we have no PS.

### 7. HTTP issuer URL with port — **intentional, demo-only**

Spec §2129-2131: server identifiers MUST be HTTPS with no port. We use
`http://localhost:8083`. This is by design for the local demo and is
documented in the README. The `aauth-go-library` accepts it only when
`allow_insecure_jwt_issuer: true` is set on the resource, and only for
`localhost` / `127.0.0.1` / `::1` / `*.localhost`.

Production should move the controller behind HTTPS at a canonical host with
no port (`https://kagent.example`). No code change in kagent — just a
deployment-side change (cert termination, Ingress) plus flipping
`allow_insecure_jwt_issuer: false` in the resource's extauth config.

## Demo hygiene

### 8. README §A1 typo

`kubectl get deploy ... kagent-controller kagent-controller -o wide` lists the
controller name twice. Probably intended `kagent-controller aauth-test-agent`.

### 9. README §B2 expected-log clarifier

The line `issuer="http://localhost:8080"` in extauth's startup log is the
*resource* issuer (extauth's own HTTP port for resource-token challenges),
not the controller's issuer. A first-time reader can mistake it for a
mis-configured controller URL. Add a one-line note in §B2.

### 10. Body content-digest not signed

Phase 1/2 deliberately skip RFC 9530 `content-digest` to sidestep streaming
LLM responses. Spec §2098 lets resources require additional components via
the `additional_signature_components` metadata field. If the resource ever
opts in, the Python signer needs to capture the body — which means draining
the httpx request stream first. Track separately from this demo.

## Out of scope for kagent (upstream work)

These call sites can't be signed today because the underlying SDK doesn't
expose an httpx injection point:

- **Gemini (google-genai)** — no httpx hook surface
- **Bedrock (boto3)** — uses botocore.httpsession, not httpx
- **MCP** — `mcp` SDK hardcodes `httpx.AsyncClient()` internally

Each needs an upstream PR before kagent can sign their traffic.
