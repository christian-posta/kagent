# AAuth Demo — End-to-End Testing Guide

This guide walks through testing the kagent AAuth integration end to end:

```
kagent agent (Kind pod)
  └─ httpx event hook signs every outbound request (RFC 9421)
  │   Signature-Key carries an aa-agent+jwt minted by the controller
  │   and bound to the agent's ephemeral Ed25519 key via cnf.jwk
  │
  ▼
agentgateway (host: localhost:3030)
  └─ ext_authz CheckRequest
  │
  ▼
extauth-aauth-resource (host: localhost:7070)
  └─ verifies HTTP message signature
  │     The aa-agent+jwt is verified against the controller's
  │     /.well-known/jwks.json (issuer key persisted in a Secret
  │     across restarts). On success, identity level=identified.
  │
  │   In-cluster signed traffic (agent → controller, agent → agent)
  │   is observed by additional middleware doors on the controller's
  │   HTTP server AND on each agent's A2A endpoint. Log-only by
  │   default; flip AAUTH_VERIFY_MODE=enforce to reject.
  ▼
upstream (e.g. OpenAI api.openai.com)
```

The agent generates an Ed25519 keypair at pod start, then fetches an
`aa-agent+jwt` from the controller's `POST /aauth/agent-jwt` endpoint. The
JWT binds the agent's signing public key (via `cnf.jwk`) to its kagent
identity (`sub=aauth:<name>@<ns>.kagent.local`), signed by the controller's
issuer key. The signer uses `sig_scheme="jwt"` and embeds the JWT in the
`Signature-Key` header on every outbound request. The same Python signer
hook is wired into every outbound httpx call site (controller client, A2A
subagent calls, OpenAI / Anthropic / Ollama LLM clients).

The controller's issuer key is generated on first start and stored in a
Secret in the controller's namespace, so JWKS stays stable across
controller restarts — agent JWTs minted before the restart remain
verifiable afterward. The agent re-mints its JWT automatically as `exp`
approaches (default refresh window: 5 min before expiry), so a long-running
agent pod does not need a restart at the 24 h boundary.

`POST /aauth/agent-jwt` is gated by **Kubernetes TokenReview** of the agent
pod's projected ServiceAccount token: the agent sends its SA token as a
Bearer header on the mint request, the controller validates it against the
kube-apiserver, and derives the canonical `sub` from the resulting
`system:serviceaccount:<ns>:<name>` username. The body's `sub` is only a
sanity check — it must match the derived value or the request is rejected.
This means an agent identity cannot be impersonated by any other pod in the
cluster; only the pod whose SA token corresponds to that identity can mint
a JWT for it.

---

## Where each verifier lives and what it covers

Three separate verifiers sit on different traffic boundaries in this setup. They are **not redundant** — each covers a boundary the others don't. Get this wrong and you'll either think extauth is unnecessary (it isn't), or think the in-process verifiers double up on extauth's job (they don't).

```
                              ┌─────────────────────────────────────────┐
                              │     KIND CLUSTER                        │
                              │                                         │
   ┌─────────┐                │  ┌──────────────┐                       │
   │ client  │────────────────┼──▶│  controller │ ← door 2 (Go middleware)
   │ (curl,  │ (via Ingress / │  │   :8083      │   verifies inbound to
   │  UI,    │  port-forward  │  │              │   /api/sessions, tasks,
   │  agent) │  or proxy)     │  │  /api/a2a/.. │   mcp, a2a proxy
   └─────────┘                │  │  proxies on  │
                              │  │   to agent…  │
                              │  └──────┬───────┘
                              │         │
   ┌─────────┐                │         ▼
   │ client  │────────────────┼──▶ ┌──────────┐ ← door 3 (Python ASGI
   │ direct  │ (port-forward, │    │  agent   │   middleware) verifies
   │ to pod  │  service DNS)  │    │  :8080   │   every inbound to the
   └─────────┘                │    │          │   agent's A2A endpoint
                              │    └─────┬────┘
                              │          │ signed outbound
                              │          │ (LLM calls)
                              │          ▼
                              │    agentgateway :3030 ─┐
                              │                        │ ext_authz
                              │    extauth-aauth-resource :7070
                              │     ← door 1 (sidecar) verifies signed
                              │       OUTBOUND traffic before it leaves
                              │       the AAuth-aware boundary
                              │                        │
                              └────────────────────────┼──┐
                                                       ▼
                                                 OpenAI / Anthropic / …
                                                 (do not speak AAuth)
```


| Door | Implementation                                                  | Verifies                                                                                                              | Why this verifier vs. another                                                                                                                                               |
| ---- | --------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1    | extauth-aauth-resource sidecar of agentgateway (outside kagent) | Outbound traffic from agents to external APIs that don't speak AAuth                                                  | The upstream (OpenAI etc.) can't verify signatures itself — the proxy has to. extauth is one implementation; Envoy/Istio/etc. with a different ext_authz module also works. |
| 2    | Go middleware in the controller's HTTP server                   | Every request to `:8083/api/*`, including the `/api/a2a/<ns>/<name>` proxy that other agents and external clients use | The controller is AAuth-aware in-process; no external gateway needed. Replaces extauth at this boundary.                                                                    |
| 3    | Python ASGI middleware in front of each agent's A2A handler     | Every request that lands on the agent pod's `:8080`                                                                   | The agent pod is AAuth-aware in-process; no external gateway needed. Catches direct pod-to-pod traffic that bypasses the controller proxy.                                  |


### Which verifier sees which traffic


| Scenario                                                                     | Verifier(s) that see it                                                                                                                     |
| ---------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| Agent → OpenAI / Anthropic / Ollama / other external LLM                     | **Door 1** (agentgateway + extauth). This is what §C2 demonstrates.                                                                         |
| Agent → controller (sessions, tasks, MCP)                                    | **Door 2** (controller's Go middleware). Tested in §E1.                                                                                     |
| External client (curl, UI, browser) → controller `/api/`*                    | **Door 2**.                                                                                                                                 |
| Agent → another agent **via** the controller's `/api/a2a/<ns>/<name>/` proxy | **Door 2** at the controller, then **door 3** at the receiving agent. Two verifiers in series.                                              |
| Agent → another agent **directly** (pod-to-pod via service DNS)              | **Door 3** at the receiving agent.                                                                                                          |
| External client → agent pod via port-forward or service DNS                  | **Door 3**. Tested in §E2 (unsigned curl) and §E3 (helper script signed).                                                                   |
| External client → agent via an Ingress + your own gateway                    | The gateway can delegate to extauth-style verification (effectively a "door 1 for inbound"), and/or **door 3** still catches it at the pod. |


### When you still need extauth (or an equivalent gateway-side verifier)

- Anywhere kagent's outbound traffic crosses into a system that doesn't speak AAuth: external LLMs, MCP servers hosted elsewhere, third-party APIs.
- Anywhere you want defense-in-depth at the **network boundary** rather than the receiving service: a hardened cluster Ingress with an AAuth verifier in front of it lets you reject unsigned traffic before it ever reaches an agent pod.

### When you don't

- For traffic that stays inside kagent (agent ↔ controller, agent ↔ agent), doors 2 and 3 are in-process and cover the same ground a sidecar verifier would. You don't need extauth in front of the controller or in front of each agent pod.

### Defaults today

- **Door 1 (extauth):** enforces — unsigned traffic is rejected with the AAuth challenge (§C1).
- **Doors 2 and 3 (in-process verifiers):** log-only by default. They run on every request, write a `aauth: verified caller=…` or `aauth: unverified reason=…` log line, and never reject. Flip with `AAUTH_VERIFY_MODE=enforce` to start gating. The intent is to watch the logs for a while and find unsigned traffic before you turn enforcement on.

---

## What you need

- A Kind cluster with kagent installed.
- Local docker registry at `localhost:5000` (the kagent `make create-kind-cluster` target sets this up).
- `[agentgateway](https://github.com/agentgateway/agentgateway)` binary on `$PATH`.
- `[extauth-aauth-resource](https://github.com/christian-posta/extauth-aauth-resource)` cloned somewhere — referred to below as `$EXTAUTH_REPO`.
- A real `OPENAI_API_KEY` is NOT required — the demo uses a placeholder. We're testing the AAuth path, not OpenAI. The OpenAI response will be 401 with "Incorrect API key"; that's expected.
- Free local ports: `3030` (agentgateway), `7070` (extauth gRPC), `8080` (extauth HTTP), `8083` (controller port-forward), `18080` (agent port-forward).

If you've cloned `extauth-aauth-resource` elsewhere, substitute that path everywhere `$EXTAUTH_REPO` appears below. The demo was developed against `~/go/src/github.com/christian-posta/extauth-aauth-resource`.

---

## Part A — One-time setup

### A1. Build & deploy kagent with the AAuth changes

From the kagent repo root:

```bash
# Build the controller (has the AAuth issuer endpoints) and the agent runtime
# (has the Python signer).
DOCKER_REGISTRY=localhost:5000 make build-controller build-app

# Apply the new CRD shape (adds spec.declarative.aauth).
helm upgrade kagent-crds helm/kagent-crds \
  --namespace kagent --kube-context kind-kagent --wait

# Roll out the new controller + agent images, and turn ON the controller's
# AAuth issuer by setting AAUTH_ISSUER_URL. http://localhost:8083 is the
# canonical issuer URL — extauth reaches it via the port-forward in Part B.
helm upgrade kagent helm/kagent \
  --namespace kagent --kube-context kind-kagent \
  --reuse-values \
  --set tag=$(git describe --tags --always) \
  --set registry=localhost:5000 \
  --set imagePullPolicy=Always \
  --set controller.aauth.issuerUrl=http://localhost:8083 \
  --wait --timeout 5m
```

**Note on `--timeout`:** if the UI image isn't built in your tree, the upgrade will report `context deadline exceeded` once the UI pod times out — that's harmless. The controller and agent deployments roll out first; check them directly:

```bash
kubectl --context kind-kagent get deploy -n kagent kagent-controller aauth-test-agent -o wide
```

### A2. Build the extauth service

```bash
cd $EXTAUTH_REPO
go build -o aauth-service ./cmd/server
```

### A3. Generate the extauth resource signing key

This key is **not** used for verifying the agent's request signature (that uses the agent's `cnf.jwk` plus the controller's issuer JWKS). extauth still needs the file to exist so it can sign resource-token challenges when it issues a 401.

```bash
# From the kagent repo root:
openssl genpkey -algorithm ed25519 -out demo/aauth/resource_key.pem
```

The file is in `.gitignore` and never leaves your machine.

### A4. Apply the demo Agent and ModelConfig

The demo Agent ships in `examples/aauth-test-agent.yaml`. It uses a `ModelConfig` whose `baseUrl` points at agentgateway:

```yaml
# applied automatically by kubectl apply -f below; shown here for reference
apiVersion: kagent.dev/v1alpha2
kind: ModelConfig
metadata:
  name: aauth-model-config
  namespace: kagent
spec:
  apiKeySecret: kagent-openai
  apiKeySecretKey: OPENAI_API_KEY
  model: gpt-4.1-mini
  provider: OpenAI
  openAI:
    baseUrl: http://host.docker.internal:3030/openai/v1
```

The `host.docker.internal:3030` URL is how a pod inside the Kind cluster reaches agentgateway running on your host. Verified working on macOS Docker Desktop.

```bash
# Apply both:
cat <<'EOF' | kubectl --context kind-kagent apply -f -
apiVersion: kagent.dev/v1alpha2
kind: ModelConfig
metadata:
  name: aauth-model-config
  namespace: kagent
spec:
  apiKeySecret: kagent-openai
  apiKeySecretKey: OPENAI_API_KEY
  model: gpt-4.1-mini
  provider: OpenAI
  openAI:
    baseUrl: http://host.docker.internal:3030/openai/v1
EOF
kubectl --context kind-kagent apply -f examples/aauth-test-agent.yaml
```

Verify the agent pod is ready and got the AAuth env vars:

```bash
kubectl --context kind-kagent wait --for=condition=ready pod \
  -l app.kubernetes.io/name=aauth-test-agent -n kagent --timeout=2m

kubectl --context kind-kagent exec -n kagent deploy/aauth-test-agent -- env | grep AAUTH
```

You should see:

```
AAUTH_ENABLED=true
AAUTH_AGENT_ID=aauth:aauth-test-agent@kagent.kagent.local
AAUTH_CONTROLLER_URL=http://kagent-controller.kagent:8083
AAUTH_SA_TOKEN_PATH=/var/run/secrets/kagent.dev/aauth/token
```

The `AAUTH_CONTROLLER_URL` tells the signer where to mint its `aa-agent+jwt`. If it's missing, the signer can't get a credential and outbound signing will be disabled.

The `AAUTH_SA_TOKEN_PATH` points at an **audience-scoped** projected ServiceAccount token (audience `kagent-controller`, separate from the default SA token). The controller's `TokenReview` enforces that audience, so a token leaked from this pod can't be replayed against any other service that calls TokenReview without an audience check.

```bash
# Confirm the token file is mounted:
kubectl --context kind-kagent exec -n kagent deploy/aauth-test-agent -- \
  ls -l /var/run/secrets/kagent.dev/aauth/token
```

---

## Part B — Per-session: start the local services

Open **four terminals**. Each runs one foreground process so you can watch the logs.

### B1. (Terminal 1) Port-forward the controller

```bash
kubectl --context kind-kagent port-forward -n kagent svc/kagent-controller 8083:8083
```

Verify the AAuth endpoints are reachable from the host:

```bash
curl -s http://localhost:8083/.well-known/aauth-agent.json | jq .
curl -s http://localhost:8083/.well-known/jwks.json | jq .
```

Expected JWKS body:

```json
{
  "keys": [
    {
      "alg": "EdDSA",
      "crv": "Ed25519",
      "kid": "kagent-issuer-1",
      "kty": "OKP",
      "use": "sig",
      "x": "..."
    }
  ]
}
```

### B2. (Terminal 2) Start extauth

```bash
cd $EXTAUTH_REPO
AAUTH_CONFIG=/absolute/path/to/kagent/demo/aauth/aauth-config.yaml ./aauth-service
```

You should see:

```
Loaded configuration from .../aauth-config.yaml
Configured resource id="kagent-agents" issuer="http://localhost:8080" hosts=[host.docker.internal host.docker.internal:3030 localhost localhost:3030]
Policy Engine starting on :7070
Starting HTTP API on :8080
```

The `issuer="http://localhost:8080"` line here is the **resource** issuer (extauth's own HTTP server, where it signs resource-token challenges when it issues a 401). It is *not* the controller's issuer — that one lives at `http://localhost:8083` and is configured under `agent_servers` in `aauth-config.yaml`. Don't confuse the two if you're chasing a verification failure.

### B3. (Terminal 3) Start agentgateway

```bash
OPENAI_API_KEY=sk-placeholder \
  agentgateway -f /absolute/path/to/kagent/demo/aauth/agw-config.yaml
```

A real key would work too; the placeholder is fine because the AAuth verification happens **before** the upstream call, and that's what we're testing.

You should see `started bind bind="bind/3030"`.

### B4. (Terminal 4) Port-forward the agent's A2A endpoint

This lets you trigger an LLM call from the host:

```bash
kubectl --context kind-kagent port-forward -n kagent svc/aauth-test-agent 18080:8080
```

---

## Part C — Trigger a request and observe the verification

### C1. Sanity check: unsigned curl gets the AAuth challenge

```bash
curl -si -X POST http://localhost:3030/openai/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' \
  | head -20
```

Expected response: `HTTP/1.1 401 Unauthorized` with:

```
aauth-requirement: requirement=auth-token
www-authenticate: AAuth
signature-error: error=invalid_signature
```

The body is `{"error":"missing_signature"}`. This proves agentgateway is delegating to extauth and the gate is closed.

### C2. Send a real A2A message to the agent

```bash
curl -s -X POST http://localhost:18080/ \
  -H "Content-Type: application/json" \
  -d '{
        "jsonrpc": "2.0",
        "id":      "req-1",
        "method":  "message/send",
        "params": {
          "message": {
            "kind":       "message",
            "messageId":  "msg-1",
            "role":       "user",
            "parts":      [{"kind": "text", "text": "hello from the aauth demo"}]
          }
        }
      }' | jq '.result.history[-1]'
```

The response depends on whether the `OPENAI_API_KEY` you exported when starting agentgateway is real or a placeholder:

- **Placeholder key** (`OPENAI_API_KEY=sk-placeholder`): the AAuth path completes successfully and the request reaches OpenAI, which rejects it with a 401 "Incorrect API key". You'll see an error string in the agent's history. **This is still a success for the AAuth flow** — the request had to be signed and verified to reach OpenAI at all.
- **Real key** (`OPENAI_API_KEY=sk-proj-...` or similar): you get an actual completion back. Example response:
  ```json
  {
    "kind": "message",
    "messageId": "...",
    "role": "agent",
    "parts": [{"kind": "text", "text": "Hello! How can I help you today?"}],
    "metadata": {
      "kagent_usage_metadata": {
        "promptTokenCount":     195,
        "candidatesTokenCount": 17,
        "totalTokenCount":      212
      }
    }
  }
  ```
  Either outcome proves the AAuth chain end-to-end. Use `jq '.result.history[-1].parts'` to see just the text and `jq '.result.history[-1].metadata'` for token counts.

### C3. Confirm in extauth's log

In terminal 2 (extauth), the most recent log line will look like:

```json
{
  "time":         "2026-05-12T02:52:30Z",
  "resource_id":  "kagent-agents",
  "level":        "identified",
  "agent_server": "http://localhost:8083",
  "delegate":     "aauth:aauth-test-agent@kagent.kagent.local",
  "result":       "allowed",
  "latency_ms":   9
}
```

What this means:

- `level=identified` — the controller's issuer signed an `aa-agent+jwt` that attests "this is `aauth:aauth-test-agent@kagent.kagent.local`". extauth fetched `/.well-known/jwks.json` from the controller, verified the JWT, then verified the request signature with the `cnf.jwk` public key embedded in the JWT.
- `delegate` — the agent's `sub`, derived server-side from its SA token via TokenReview. No other pod can produce a JWT with this `sub`.

---

## Troubleshooting

### Agent didn't mint a JWT at startup

The signer needs `AAUTH_CONTROLLER_URL` to be set on the agent pod. The
translator injects it only when the controller has `AAUTH_ISSUER_URL` set.
Check both:

```bash
# Controller side (distroless image — no env/shell in the container):
kubectl --context kind-kagent get deploy -n kagent kagent-controller \
  -o jsonpath='{range .spec.template.spec.containers[0].env[*]}{.name}{"="}{.value}{"\n"}{end}' \
  | grep '^AAUTH'
# Expect: AAUTH_ISSUER_URL=http://localhost:8083 (when helm sets controller.aauth.issuerUrl)

kubectl --context kind-kagent logs -n kagent deploy/kagent-controller \
  | grep -E 'AAuth (issuer|verifier) enabled'
# Expect issuerURL=http://localhost:8083 and mode=log (empty AAUTH_VERIFY_MODE → log)

# Agent side (Python image has env):
kubectl --context kind-kagent exec -n kagent deploy/aauth-test-agent -- env | grep AAUTH
# Expect AAUTH_ENABLED, AAUTH_AGENT_ID, AND AAUTH_CONTROLLER_URL all set
```

If the agent has `AAUTH_ENABLED` but no `AAUTH_CONTROLLER_URL`, the
translator didn't pick it up — most likely because the agent pod predates
the controller upgrade. Restart the agent and confirm the mint succeeds:

```bash
kubectl --context kind-kagent rollout restart deploy/aauth-test-agent -n kagent
kubectl --context kind-kagent logs -n kagent deploy/aauth-test-agent \
  | grep -E "AAuth: minted|failed to fetch aa-agent\\+jwt"
# Expect: "AAuth: minted aa-agent+jwt — expires_in=86400"
```

If you see `failed to fetch aa-agent+jwt`, the mint endpoint rejected the
request — see the next section.

### Agent log shows `failed to fetch aa-agent+jwt`

The controller's `POST /aauth/agent-jwt` endpoint requires a Kubernetes
ServiceAccount Bearer token (validated via `TokenReview`). If the agent pod
is missing its projected SA token, or the controller can't reach the
kube-apiserver, the mint fails and the signer cannot operate.

Check, in order:

```bash
# 1) The agent pod has its SA token mounted (default in standard pod specs).
kubectl --context kind-kagent exec -n kagent deploy/aauth-test-agent -- \
  ls -l /var/run/secrets/kubernetes.io/serviceaccount/token
# Expect: a file present, non-zero size.

# 2) The controller is allowed to call TokenReview.
kubectl --context kind-kagent get clusterrolebinding kagent-auth-delegator
# Expect: a ClusterRoleBinding to system:auth-delegator. If missing, helm
# upgrade with the current chart will install it.

# 3) The agent's request reached the controller and was rejected.
kubectl --context kind-kagent logs -n kagent deploy/aauth-test-agent | grep -i aauth
# Look for the failed fetch line — the response body will say why.
```

If the binding is missing, install the chart at the current revision. If
the SA token is missing, your pod spec is non-standard — make sure
`automountServiceAccountToken: false` is **not** set on the agent's pod
template or ServiceAccount.

### Agent fetched the JWT but extauth still says `missing_signature`

The wire was empty. Either:

- The signer didn't install the httpx hook (look in pod logs for `AAuth signing enabled`).
- The model client doesn't go through agentgateway (check that the ModelConfig has `openAI.baseUrl: http://host.docker.internal:3030/openai/v1`).

### extauth says `iss must be https, or http for localhost`

`allow_insecure_jwt_issuer` is not enabled in `aauth-config.yaml`. The aauth library only accepts `http://` issuers on `localhost`, `127.0.0.1`, `::1`, or `*.localhost`. For the demo the controller's canonical URL must be `http://localhost:8083`, AND `allow_insecure_jwt_issuer: true` must be set on the resource. In production, the controller should be reachable over HTTPS and this flag stays false.

### extauth fails JWT verification with `signature verification failed`

The controller's issuer key is now persisted in `kagent-aauth-issuer` (in the controller's namespace), so a normal controller restart should NOT cause this. If you see it anyway:

```bash
# Did the Secret survive? It should exist and contain both keys:
kubectl --context kind-kagent get secret -n kagent kagent-aauth-issuer -o json | jq '.data | keys'
# Expect: ["ed25519-private.pem", "kid"]
```

If the Secret was deleted (manually, or by reinstalling the chart with `--force-recreate`), the controller generated a fresh keypair on its next start. JWTs minted against the old key are now unverifiable. Restart the agent so it fetches a new JWT:

```bash
kubectl --context kind-kagent rollout restart deploy/aauth-test-agent -n kagent
```

### extauth says `expired_jwt`

The agent's JWT is past its `exp`. The signer normally re-mints automatically within the 5-minute pre-expiry window, so this should only show up if:

- the agent pod was paused/throttled long enough to miss its refresh window, or
- the controller's `/aauth/agent-jwt` endpoint was unreachable across the entire refresh window, or
- you've intentionally lowered `_REFRESH_WINDOW_SECONDS` for testing.

Fix: restart the agent pod so it mints a fresh JWT.

```bash
kubectl --context kind-kagent rollout restart deploy/aauth-test-agent -n kagent
```

The next request will log `level=identified` again.

### `authority` mismatch

extauth reconstructs the signed `@authority` from `GetHost()` plus the preserved `Host` header. agentgateway strips the port from `GetHost()` but keeps the original `Host` header (with port) in the headers map, and `AuthorityForSignature`'s merge prefers the value with a non-standard port. So the demo doesn't need `authority_override` — but if you ever do (e.g. another proxy in front of agentgateway that fully strips the Host header), uncomment it:

```yaml
authority_override: "host.docker.internal:3030"
```

### Port 3030 already in use

The default demo port was changed from 3000 → 3030 to avoid conflicting with `kubectl port-forward` on 3000. If 3030 is also taken, do a global `s/3030/<your-port>/g` across:

```
demo/aauth/agw-config.yaml
demo/aauth/aauth-config.yaml
demo/aauth/README.md  (this file)
```

…and update the ModelConfig `baseUrl` you applied in Step A4 to match.

### Helm upgrade times out on the UI

You probably haven't built the UI image. The controller and agent images roll first, so the AAuth path works even though helm reports timeout. Ignore the timeout or `--set ui.image.tag=<existing-tag>` to keep the old UI image.

---

## Cleaning up

```bash
# Stop the four local processes (Ctrl-C in each terminal).
# Then optionally delete the demo resources:
kubectl --context kind-kagent delete -f examples/aauth-test-agent.yaml
kubectl --context kind-kagent delete modelconfig -n kagent aauth-model-config

# To remove the resource_key.pem (it's git-ignored, but explicit cleanup):
rm demo/aauth/resource_key.pem
```

---

## Files in this directory


| File                                      | What it is                                                                                                                                                                                                                                                                                                                     |
| ----------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `agw-config.yaml`                         | agentgateway config — binds :3030, proxies `/openai/*` to api.openai.com, delegates to extauth on :7070, sets `aauth_resource_id: kagent-agents`.                                                                                                                                                                              |
| `aauth-config.yaml`                       | extauth resource config — gRPC :7070, HTTP :8080, accepts `jwt` scheme, points at the controller's JWKS at `http://localhost:8083`.                                                                                                                                                                                            |
| `resource_key.pem`                        | extauth's signing key for resource-token challenges. Generated locally, git-ignored.                                                                                                                                                                                                                                           |
| `README.md`                               | This file.                                                                                                                                                                                                                                                                                                                     |
| `FOLLOWUPS.md`                            | Spec-conformance gaps and demo cleanup items. Persistent issuer key, SA-TokenReview gate, audience-scoped SA token, JWT auto-refresh, and log-only in-cluster verification are all DONE. Remaining: enforce-mode rollout plan, canonicalization quirks, key rotation, optional `jti`, and SDK coverage for Gemini/Bedrock/MCP. |
| (repo root) `scripts/aauth-call-agent.py` | Debug helper for §E3. Mints a JWT against the controller using the calling pod's SA token, signs an A2A `message/send` request, posts it. Not shipped in the agent image.                                                                                                                                                      |


## What this demo proves

When you see `level=identified` in extauth's log, the following has all happened in one request:

1. The kagent controller started up and either loaded the existing Ed25519 issuer keypair from the `kagent-aauth-issuer` Secret, or generated and persisted a new one on first start. It publishes the public half at `/.well-known/jwks.json` and `/.well-known/aauth-agent.json`.
2. The agent pod started, generated its own Ed25519 signing keypair, read its projected ServiceAccount token from `/var/run/secrets/kubernetes.io/serviceaccount/token`, and called the controller's `POST /aauth/agent-jwt` with the SA token as a Bearer header and `{public_key_jwk}` in the body. The controller validated the SA token via `TokenReview` against the kube-apiserver, derived the canonical `sub` from the resulting `system:serviceaccount:<ns>:<name>` username, and minted an `aa-agent+jwt` signed by its issuer key with the agent's signing public key bound via `cnf.jwk`. The agent will re-mint that JWT automatically as it nears expiry.
3. The agent's adk runtime made an outbound LLM call. An httpx event hook intercepted the request and added RFC 9421 `Signature`, `Signature-Input`, and `Signature-Key: sig=jwt;jwt="..."` headers, signing over `@method @authority @path signature-key created`.
4. agentgateway received the request, sent an `ext_authz.Check` CheckRequest to extauth on `:7070`.
5. extauth identified the resource via the `aauth_resource_id: kagent-agents` context extension, then:
  - Parsed the JWT in the `Signature-Key` header.
  - Checked `typ=aa-agent+jwt`, `dwk=aauth-agent.json`.
  - Fetched the issuer's JWKS from `http://localhost:8083/.well-known/jwks.json`.
  - Verified the JWT signature with that key.
  - Extracted `cnf.jwk` (the agent's public key).
  - Used `cnf.jwk` to verify the HTTP message signature against the reconstructed signature base.
6. extauth told agentgateway to allow the request. agentgateway forwarded it to OpenAI. OpenAI rejected the placeholder API key (HTTP 401 from OpenAI itself — distinct from the AAuth 401), and the rejection was relayed back to the agent.

If any of those steps had failed, you'd see `result=error` in the extauth log with a `reason=...` explaining which stage broke, and `LogAuthorityResolutionOnFailure` would dump every header it saw for forensics.

## Part E — In-cluster verification (log-only)

This part exercises **doors 2 and 3** from the [topology section](#where-each-verifier-lives-and-what-it-covers) — the in-cluster verifiers that observe signed traffic between kagent components without needing extauth or any external gateway.


| Receiver                                         | Location                                       | Logs to                                            |
| ------------------------------------------------ | ---------------------------------------------- | -------------------------------------------------- |
| Controller `:8083` (sessions/tasks/MCP) — door 2 | Go middleware in the HTTP server               | controller pod log, logger `aauth`                 |
| Each agent's A2A endpoint `:8080` — door 3       | Starlette middleware in the Python ADK runtime | agent pod log, logger `kagent.adk.aauth._verifier` |


Default mode is `log` when the controller has `AAUTH_ISSUER_URL` set —
nothing is rejected; every protected request gets a single log line
(`aauth: verified caller=...` or `aauth: unverified reason=...`). Flip to
`enforce` with `AAUTH_VERIFY_MODE=enforce` to start gating.

**You do not need agentgateway or extauth running for any of E1–E3.** Those
prove the door-1 (outbound) chain in §C2/§C3; this part proves the door-2 /
door-3 (in-cluster) chains. The only services you need up are the
controller and agent pods (already running in the cluster) plus, for E2,
an agent port-forward to send the unsigned curl.

The three observable behaviors you can prove:

### E1. Controller verifier — positive

Every signed call the agent makes to the controller (sessions, tasks) is
verified. Trigger §C2 as usual, then:

```bash
kubectl --context kind-kagent logs -n kagent deploy/kagent-controller \
  | grep '"aauth: verified"' | tail -5
```

Expect entries like:

```json
{"logger":"aauth","msg":"aauth: verified",
 "caller":"aauth:aauth-test-agent@kagent.kagent.local",
 "level":"identified","method":"POST","path":"/api/sessions"}
```

A handful of `"aauth: unverified"` entries for paths with long query strings
or event-stream POSTs are expected too (RFC 9421 canonicalization
quirks) — they're benign in log mode.

### E2. Agent inbound verifier — negative

The §C2 curl arrives at the agent unsigned (the host doesn't sign):

```bash
kubectl --context kind-kagent logs -n kagent deploy/aauth-test-agent \
  | grep "aauth: unverified" | tail -1
```

Expect:

```
WARNING - aauth: unverified reason='Missing signature headers' method=POST path=/
```

That's the verifier doing its job — `log` mode doesn't reject, but the
warning is visible.

### E3. Agent inbound verifier — positive

To exercise the positive path, use the helper script in
`scripts/aauth-call-agent.py`. It mints a JWT against the controller (using
the calling pod's SA token, which gets TokenReview'd), generates an
Ed25519 keypair, signs an A2A `message/send` request, and POSTs it.

```bash
# Copy the script into a pod that has the SA token (the agent pod itself
# works — its SA already has a kagent-controller-audience token mounted).
AGENT=$(kubectl --context kind-kagent get pods -n kagent \
  -l app.kubernetes.io/name=aauth-test-agent -o jsonpath='{.items[0].metadata.name}')

kubectl --context kind-kagent cp scripts/aauth-call-agent.py \
  kagent/$AGENT:/tmp/aauth-call-agent.py

kubectl --context kind-kagent exec -n kagent $AGENT -- \
  python /tmp/aauth-call-agent.py \
    --controller http://kagent-controller.kagent:8083 \
    --agent     http://aauth-test-agent.kagent:8080 \
    --message   "signed inbound test"

# Then check the agent log:
kubectl --context kind-kagent logs -n kagent $AGENT \
  | grep "aauth: verified" | tail -1
```

Expect:

```
INFO - aauth: verified caller=aauth:aauth-test-agent@kagent.kagent.local method=POST path=/
```

The `caller` here is the calling pod's identity, derived server-side from
the SA token used in step 1 — so the verifier is asserting more than "this
request is signed"; it's asserting which workload signed it.

### Config knobs


| Env var                       | Default                                                                | Purpose                                                               |
| ----------------------------- | ---------------------------------------------------------------------- | --------------------------------------------------------------------- |
| `AAUTH_VERIFY_MODE`           | `log` when `AAUTH_ISSUER_URL` set, else `off`                          | `off` / `log` / `enforce`                                             |
| `AAUTH_VERIFY_AUTHORITIES`    | derived from `KAGENT_NAME` / `KAGENT_NAMESPACE` plus `localhost:8080`  | extra Host values the agent verifier accepts (port-forwards, proxies) |
| `AAUTH_VERIFY_ISSUER_REWRITE` | injected by the translator when controller issuer URL ≠ in-cluster URL | maps the JWT's canonical `iss` to a reachable URL for JWKS fetch      |


The Go controller's verifier sits *after* the existing `AuthnMiddleware` in
the chain, so the standard bearer-token authn still runs first. On
successful verification the middleware attaches `aauth.VerifiedIdentity` to
the request context (Go side) or `request.state.aauth_identity` (Python
side) — future enforcement / policy will read these.

## Optional: prove the persistent issuer key

With the demo running (Part B services up), the controller's JWKS should stay byte-identical across restarts. Quick proof:

```bash
# Capture the current public key (terminal 1's port-forward must be up).
BEFORE=$(curl -s http://localhost:8083/.well-known/jwks.json | jq -r '.keys[0].x')

# Bounce the controller.
kubectl --context kind-kagent rollout restart deploy/kagent-controller -n kagent
kubectl --context kind-kagent rollout status  deploy/kagent-controller -n kagent

# Re-establish the port-forward (the old one closed when the pod terminated),
# then re-read the JWKS.
# (Re-run the command from §B1 in another terminal first.)
AFTER=$(curl -s http://localhost:8083/.well-known/jwks.json | jq -r '.keys[0].x')

[ "$BEFORE" = "$AFTER" ] && echo "JWKS stable ✓" || echo "JWKS rotated ✗"
```

If `BEFORE = AFTER`, the controller correctly loaded its key from `kagent-aauth-issuer` instead of generating a fresh one. Re-running §C2 immediately after the restart should still return `level=identified` *without* restarting the agent pod — agent JWTs minted before the restart continue to verify.

Sanity-check the Secret itself:

```bash
kubectl --context kind-kagent get secret -n kagent kagent-aauth-issuer \
  -o json | jq '{type, labels: .metadata.labels, keys: (.data | keys)}'
# Expect: type=Opaque, app.kubernetes.io/component=aauth-issuer,
# keys=["ed25519-private.pem","kid"]
```

If the Secret is missing (e.g. the namespace was wiped), the next controller start will recreate it and any in-flight agent JWTs will become invalid — restart the agent pod once.

## Optional: prove the TokenReview gate

The mint endpoint refuses anything that isn't a valid in-cluster ServiceAccount token. Probe it from a throwaway pod:

```bash
kubectl --context kind-kagent run aauth-probe --rm -it --restart=Never \
  --image=curlimages/curl:latest --command -- sh

# Inside the pod:
URL=http://kagent-controller.kagent:8083/aauth/agent-jwt

# 1) No Authorization header → 401.
curl -sS -o /dev/null -w "HTTP %{http_code}\n" \
  -X POST $URL -H "Content-Type: application/json" \
  -d '{"public_key_jwk":{}}'

# 2) A bogus bearer token → 403.
curl -sS -o /dev/null -w "HTTP %{http_code}\n" \
  -X POST $URL -H "Content-Type: application/json" \
  -H "Authorization: Bearer not.a.real.token" \
  -d '{"public_key_jwk":{"kty":"OKP"}}'

# 3) This pod's own SA token, but with a sub claim that doesn't match
#    the SA → 403 ("body sub ... does not match ServiceAccount-derived sub ...").
TOKEN=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)
curl -sS -o /dev/null -w "HTTP %{http_code}\n" \
  -X POST $URL -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"sub":"aauth:not-me@kagent.kagent.local","public_key_jwk":{"kty":"OKP"}}'

# 4) This pod's SA token with no sub claim → 200 (the controller mints a JWT
#    bound to this probe pod's identity, e.g. aauth:default@default.kagent.local).
curl -sS -X POST $URL -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"public_key_jwk":{"kty":"OKP","crv":"Ed25519","x":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}' \
  | head -c 200; echo
```

Calls 1–3 prove no other pod can impersonate the demo agent — the only credential the controller accepts for `sub=aauth:aauth-test-agent@kagent.kagent.local` is the token mounted in the `aauth-test-agent` pod itself.

The relevant chart binding is in `helm/kagent/templates/rbac/auth-delegator-clusterrolebinding.yaml` (binds the controller's SA to the built-in `system:auth-delegator` ClusterRole for `tokenreviews.create`):

```bash
kubectl --context kind-kagent get clusterrolebinding \
  -l app.kubernetes.io/instance=kagent | grep auth-delegator
# Expect: kagent-auth-delegator   ClusterRole/system:auth-delegator
```

## Part F — Signed agent → controller MCP path (no extauth)

This part demonstrates a fully self-contained signing loop that *does not
need* extauth or agentgateway. The agent uses kagent's built-in MCP server
(hosted at `/mcp` on the controller) as a tool, and every MCP HTTP call
is signed by the agent and verified by door 2 on the controller.

Why it works without extauth: the receiver in this flow is the kagent
controller itself, which already runs the Go AAuth verifier on every
non-skipped HTTP route — including `/mcp`. There's no external API in
the path, so there's nothing to put a proxy in front of.

### F1. Apply the demo agent (it already includes the built-in MCP toolset)

`examples/aauth-test-agent.yaml` now declares a `RemoteMCPServer` pointing
at `http://kagent-controller.kagent:8083/mcp` and wires it into the agent's
`tools` list. If you applied the example before this change, re-apply it:

```bash
kubectl --context kind-kagent apply -f examples/aauth-test-agent.yaml
kubectl --context kind-kagent rollout status -n kagent deploy/aauth-test-agent
```

Sanity-check the tool wiring:

```bash
kubectl --context kind-kagent get agent -n kagent aauth-test-agent \
  -o jsonpath='{.spec.declarative.tools[*].mcpServer.name}{"\n"}'
# Expect: kagent-builtin-mcp
```

### F2. Trigger an MCP tool call and watch door 2 verify it

Port-forward the agent if you haven't already:

```bash
kubectl --context kind-kagent port-forward -n kagent svc/aauth-test-agent 18080:8080 &
```

Now run the verification recipe. It does three things in one shot:

1. Records a timestamp before the curl so the log filter sees only this
  request's entries (the controller logs `/mcp` traffic from other agents
   and clients constantly — without `--since-time` you can't tell yours
   from theirs).
2. Sends a prompt strong enough to force the LLM to actually call
  `list_agents` rather than answer from memory.
3. Filters the controller log to only your `path=/mcp` lines and pretty-prints them.

> **Note.** The proof here is the **controller's verifier log**, not the
> agent's textual reply. The agent invokes the MCP tool *before* it composes
> its final reply, so the `/mcp` calls happen — and door 2 verifies them —
> even if the agent's final reply fails (e.g. if extauth isn't running and
> the post-tool LLM call returns "Connection error"). We discard the reply
> body for this reason.

```bash
T0=$(date -u +%Y-%m-%dT%H:%M:%SZ)

# Fire the curl. Discard the response body — we only care about the
# controller's verifier log for this demo.
curl -sS -X POST http://localhost:18080/ \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc":"2.0","id":"f-1","method":"message/send",
    "params":{"message":{"kind":"message","messageId":"f-msg-1","role":"user",
      "parts":[{"kind":"text","text":"You MUST call the list_agents tool right now. Do not answer from memory. Just call list_agents and tell me the count."}]}}
  }' >/dev/null

# Show only this curl's /mcp verifier lines.
kubectl --context kind-kagent logs -n kagent deploy/kagent-controller --since-time=$T0 \
  | grep '"aauth' | grep '"path":"/mcp"' \
  | jq -c '{ts, msg, caller, method, path}'
```

Expected output: **5 lines**, all `aauth: verified`, all with the agent's
identity as `caller`:

```
{"ts":"…","msg":"aauth: verified","caller":"aauth:aauth-test-agent@kagent.kagent.local","method":"POST","path":"/mcp"}
{"ts":"…","msg":"aauth: verified","caller":"aauth:aauth-test-agent@kagent.kagent.local","method":"GET","path":"/mcp"}
{"ts":"…","msg":"aauth: verified","caller":"aauth:aauth-test-agent@kagent.kagent.local","method":"POST","path":"/mcp"}
{"ts":"…","msg":"aauth: verified","caller":"aauth:aauth-test-agent@kagent.kagent.local","method":"POST","path":"/mcp"}
{"ts":"…","msg":"aauth: verified","caller":"aauth:aauth-test-agent@kagent.kagent.local","method":"DELETE","path":"/mcp"}
```

That's one MCP session over streamable-HTTP: initialize → SSE stream open
(GET) → list_tools → call `list_agents` → session close (DELETE). The
agent pod authenticated itself to the controller on every leg.

A few things that can make the output look different:

- **MCP sessions are reused.** A second curl against the same agent pod
may produce only 1–2 lines (just the tool call itself) because the
session opened by the first curl is still alive. Restart the agent pod
to force a clean session:
  ```bash
  kubectl --context kind-kagent rollout restart -n kagent deploy/aauth-test-agent
  kubectl --context kind-kagent rollout status  -n kagent deploy/aauth-test-agent
  # Port-forward dies with the old pod; restart it:
  lsof -ti :18080 | xargs -r kill -9
  kubectl --context kind-kagent port-forward -n kagent svc/aauth-test-agent 18080:8080 &
  ```
- **You see only `unverified … missing_signature` entries from `remote_addr=10.244.0.1`.** That's not your agent — `10.244.0.1` is the kind node bridge gateway (external traffic NAT'd into the cluster), typically the kagent UI or a port-forwarded curl hitting the controller's `/mcp` directly. The agent pod itself shows up with its pod IP (e.g. `10.244.0.71`). The recipe's `caller` field filter ignores these. If you've lost your port-forward or it's pointing at a dead pod sandbox, the agent's MCP calls never happen and you'll only see this background noise — fix the port-forward and retry.
- **Zero lines.** The LLM answered from memory without calling the tool. Strengthen the prompt ("the only way to answer is to call the list_agents tool — if you skip the tool call, your answer is wrong") and retry, or rollout-restart the agent to clear any session/response cache.
- `**unverified reason="invalid_signature"` on `/mcp` from your agent.** The signer is running but the verifier can't fetch JWKS — usually the authority-rewrite issue. Re-check `AAUTH_VERIFY_ISSUER_REWRITE` (§B2).

### F3. Inspect the JWT the controller issued to the agent

The signer logs the minted `aa-agent+jwt` once at startup (and again on each
refresh). This is the **same JWT** that rides in the `Signature-Key` header
on every `/mcp` request from this agent, so what you see here is exactly
what door 2 verifies.

```bash
kubectl --context kind-kagent logs -n kagent deploy/aauth-test-agent \
  | sed -n '/AAuth: minted/,/AAuth signing enabled/p'
```

Expected output (the `token=…` line is one long base64 string, abbreviated
here):

```
2026-05-20 16:45:09,801 - kagent.adk.aauth._signer - INFO - AAuth: minted aa-agent+jwt — expires_in=86400
  token=eyJhbGc…J3a-9QpkchMCBA
  header={
    "alg": "EdDSA",
    "kid": "kagent-issuer-1",
    "typ": "aa-agent+jwt"
  }
  claims={
    "cnf": {
      "jwk": {
        "crv": "Ed25519",
        "kty": "OKP",
        "x": "cYRI7zv66dSbNDXQo8RCbzhrLJS_Yjj-tENakG_vrus"
      }
    },
    "dwk": "aauth-agent.json",
    "exp": 1779381909,
    "iat": 1779295509,
    "iss": "http://localhost:8083",
    "sub": "aauth:aauth-test-agent@kagent.kagent.local"
  }
  timing=iat=2026-05-20T16:45:09Z exp=2026-05-21T16:45:09Z ttl_seconds=86400
2026-05-20 16:45:09,801 - kagent.adk.aauth._signer - INFO - AAuth signing enabled — agent_id=aauth:aauth-test-agent@kagent.kagent.local scheme=jwt
```

What every field means, mapped to the AAuth draft:


| Field         | Spec source           | What it means here                                                                                                                                               |
| ------------- | --------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `typ`         | §535                  | `aa-agent+jwt` — agent token type. Distinguishes from PS tokens, etc.                                                                                            |
| `alg` + `kid` | JWS header            | The controller signs with `EdDSA` using key id `kagent-issuer-1` (matches the kid in its JWKS).                                                                  |
| `iss`         | §535 (required claim) | The controller's canonical URL. Verifiers fetch `<iss>/.well-known/<dwk>` to discover the JWKS.                                                                  |
| `sub`         | §535                  | The agent's identity. Derived **server-side** from the SA token via TokenReview — the body's `sub` is only a sanity-check.                                       |
| `cnf.jwk`     | §535, RFC 7800        | Proof-of-possession key — the Ed25519 public key the agent generated in-process. The HTTP signature is made with the matching private key, never leaves the pod. |
| `dwk`         | §548                  | `aauth-agent.json` — tells verifiers which well-known file to fetch from `iss` to find the JWKS.                                                                 |
| `iat`, `exp`  | RFC 7519              | Mint and expiry (24h TTL by default). The signer auto-refreshes within 5 min of `exp`.                                                                           |


> The token's signature (3rd dotted segment) is **the controller's
> signature over header+claims**, not the agent's. The agent's identity is
> attested by the controller; the agent then proves possession of `cnf.jwk`
> by signing each outbound HTTP request with the matching private key.
> A verifier checks both: the JWT (with the controller's JWKS) and the
> request signature (with the JWT's embedded `cnf.jwk`).

A standalone decode if you only have the raw token (e.g. captured off the
wire) — the JWT uses base64url encoding *without* padding, so plain
`base64 -d` will choke on roughly half of tokens. Use python:

```bash
TOKEN=$(kubectl --context kind-kagent logs -n kagent deploy/aauth-test-agent \
          | grep -oE 'token=[^[:space:]]+' | head -1 | cut -d= -f2-)

decode_jwt_segment() {
  python3 -c "import sys,base64,json; \
seg = sys.argv[1].split('.')[int(sys.argv[2])]; \
pad = '=' * (-len(seg) % 4); \
print(json.dumps(json.loads(base64.urlsafe_b64decode(seg + pad)), indent=2))" \
    "$TOKEN" "$1"
}

decode_jwt_segment 0   # header
decode_jwt_segment 1   # claims
```

Or paste the token into [https://jwt.io](https://jwt.io).

### F4. Negative test: turn signing off and re-run

Edit the agent so AAuth is disabled, redeploy, and repeat §F2:

```bash
kubectl --context kind-kagent patch agent -n kagent aauth-test-agent \
  --type merge -p '{"spec":{"declarative":{"aauth":{"enabled":false}}}}'
kubectl --context kind-kagent rollout restart -n kagent deploy/aauth-test-agent
kubectl --context kind-kagent rollout status  -n kagent deploy/aauth-test-agent
```

The port-forward will break when the pod restarts — kill and restart it:

```bash
lsof -ti :18080 | xargs -r kill -9
kubectl --context kind-kagent port-forward -n kagent svc/aauth-test-agent 18080:8080 &
```

Now repeat the §F2 recipe (capture `T0`, run the curl, filter the log).
With signing off, the `caller`-field filter (the one in §F2) will print
**zero** lines. Switch the filter to look for `unverified` entries from
the agent pod's IP:

```bash
AGENT_IP=$(kubectl --context kind-kagent get pods -n kagent \
  -l app.kubernetes.io/name=aauth-test-agent -o jsonpath='{.items[0].status.podIP}')

kubectl --context kind-kagent logs -n kagent deploy/kagent-controller --since-time=$T0 \
  | grep '"aauth' | grep '"path":"/mcp"' \
  | grep "$AGENT_IP" \
  | jq -c '{ts, msg, reason, method, path}'
```

Expected: every line is `"msg":"aauth: unverified","reason":"missing_signature"`.
That confirms the wrapper is gated on the signer — turn AAuth off, signing
disappears.

Re-enable AAuth when you're done:

```bash
kubectl --context kind-kagent patch agent -n kagent aauth-test-agent \
  --type merge -p '{"spec":{"declarative":{"aauth":{"enabled":true}}}}'
kubectl --context kind-kagent rollout restart -n kagent deploy/aauth-test-agent
```

### F5. What's actually happening

```
prompt
  ↓
LLM decides to call list_agents
  ↓
Python MCP toolset (KAgentMcpToolset) ──────────────────────┐
  uses httpx_client_factory (wrapped by                     │
  kagent.adk.aauth.wrap_mcp_httpx_factory)                  │
  ↓                                                         │
  httpx.AsyncClient with the signer's request hook attached │
  ↓                                                         │
POST http://kagent-controller.kagent:8083/mcp               │ signed
  Signature, Signature-Input, Signature-Key headers added   │ outbound
  Authorization: aa-agent <jwt>                             │
  ↓                                                         │
controller HTTP router                                      │
  → AuthnMiddleware (bearer-token authn, unchanged)         │
  → AAuthVerifier.Middleware (door 2) ←────────────────────-┘
  → /mcp handler → list_agents tool
```

No extauth, no agentgateway, no Envoy in the path. The only AAuth-aware
component besides the controller is the agent pod itself, and the only
shared trust root is the controller's JWKS.

Limitations (tracked in `FOLLOWUPS.md`):

- The `invoke_agent` MCP tool dispatches an A2A call from the controller
to the target agent. That leg is currently unsigned — the controller
has the issuer key but not a workload identity it signs *as*. A
separate piece of work.
- `RemoteMCPServer` resources pointing at external (non-kagent) MCP servers
do not get verifier coverage on the receiving side. Same shape of
problem as external LLMs; same fix (sidecar verifier or native AAuth in
the upstream).

