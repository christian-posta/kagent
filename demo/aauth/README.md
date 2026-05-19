# AAuth Demo — End-to-End Testing Guide

This guide walks through testing the kagent AAuth integration end to end:

```
kagent agent (Kind pod)
  └─ httpx event hook signs every outbound request (RFC 9421)
  │
  ▼
agentgateway (host: localhost:3030)
  └─ ext_authz CheckRequest
  │
  ▼
extauth-aauth-resource (host: localhost:7070)
  └─ verifies HTTP message signature
  │   - Phase 1 (hwk):  inline bare public key → level=pseudonymous
  │   - Phase 2 (jwt):  aa-agent+jwt with cnf.jwk → level=identified
  │                     JWT fetched at startup from controller's
  │                     /aauth/agent-jwt endpoint (gated by a
  │                     TokenReview of the agent pod's projected
  │                     SA token), auto-refreshed before exp, and
  │                     verified against the controller's
  │                     /.well-known/jwks.json (key persisted in
  │                     a Secret across restarts)
  ▼
upstream (e.g. OpenAI api.openai.com)
```

**Two phases, same wire test:**

| Phase | Scheme | Identity | Controller-side work | Agent signing key | Controller issuer key |
|------:|:-------|:---------|:---------------------|:------------------|:----------------------|
| 1     | `hwk`  | pseudonymous (bare key) | none | per pod restart | n/a |
| 2     | `jwt`  | identified via `aa-agent+jwt` | issuer endpoint + JWKS | per pod restart | persisted in Secret `kagent-aauth-issuer` |

Phase 2 builds on Phase 1: the same Python signer hook is wired into every outbound httpx call site (controller client, A2A subagent calls, OpenAI / Anthropic / Ollama LLM clients). The difference is only in **what credential** rides in the `Signature-Key` header.

The controller's issuer key is generated on first start and stored in a Secret in the controller's namespace, so JWKS stays stable across controller restarts — agent JWTs minted before the restart remain verifiable afterward. The agent re-mints its JWT automatically as `exp` approaches (default refresh window: 5 min before expiry), so a long-running agent pod does not need a restart at the 24 h boundary.

`POST /aauth/agent-jwt` is gated by **Kubernetes TokenReview** of the agent pod's projected ServiceAccount token: the agent sends its SA token as a Bearer header on the mint request, the controller validates it against the kube-apiserver, and derives the canonical `sub` from the resulting `system:serviceaccount:<ns>:<name>` username. The body's `sub` is only a sanity check — it must match the derived value or the request is rejected. This means an agent identity cannot be impersonated by any other pod in the cluster; only the pod whose SA token corresponds to that identity can mint a JWT for it.

---

## What you need

- A Kind cluster with kagent installed.
- Local docker registry at `localhost:5000` (the kagent `make create-kind-cluster` target sets this up).
- [`agentgateway`](https://github.com/agentgateway/agentgateway) binary on `$PATH`.
- [`extauth-aauth-resource`](https://github.com/christian-posta/extauth-aauth-resource) cloned somewhere — referred to below as `$EXTAUTH_REPO`.
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
# Phase 2 issuer by setting AAUTH_ISSUER_URL. http://localhost:8083 is the
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
kubectl --context kind-kagent get deploy -n kagent kagent-controller kagent-controller -o wide
```

### A2. Build the extauth service

```bash
cd $EXTAUTH_REPO
go build -o aauth-service ./cmd/server
```

### A3. Generate the extauth resource signing key

The Phase 2 demo does **not** require this key for signature verification (verification uses the agent's `cnf.jwk` and the controller's issuer JWKS), but extauth still wants the file to exist so it can sign resource-token challenges when extauth issues a 401.

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
  -l kagent.dev/agent=aauth-test-agent -n kagent --timeout=2m

kubectl --context kind-kagent exec -n kagent deploy/aauth-test-agent -- env | grep AAUTH
```

You should see:

```
AAUTH_ENABLED=true
AAUTH_AGENT_ID=aauth:aauth-test-agent@kagent.kagent.local
AAUTH_CONTROLLER_URL=http://kagent-controller.kagent:8083
```

The `AAUTH_CONTROLLER_URL` is what tells the signer to use Phase 2 (jwt). If it's missing, the signer falls back to Phase 1 (hwk) — still works, just at a lower identity level.

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
            "parts":      [{"kind": "text", "text": "phase 2 hello"}]
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

**Phase 2 (jwt scheme):**
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

**Phase 1 (hwk scheme — if you re-ran with Phase 1 config):**
```json
{
  "resource_id": "kagent-agents",
  "level":       "pseudonymous",
  "result":      "allowed"
}
```

The difference is the key thing:
- `level=identified` means the controller's issuer signed an `aa-agent+jwt` that attests "this is `aauth:aauth-test-agent@kagent.kagent.local`". extauth fetched `/.well-known/jwks.json` from the controller, verified the JWT, then verified the request signature with the `cnf.jwk` public key embedded in the JWT.
- `level=pseudonymous` means extauth saw only an inline public key with no provenance — it verified the signature, but the public key has no real-world identity attached.

---

## Part D — Switching between Phase 1 and Phase 2

The same demo can run in either phase by changing **two things**:

1. `demo/aauth/aauth-config.yaml` — the `resources[0]` block.
2. Whether `AAUTH_CONTROLLER_URL` is set on the agent pod (controlled by whether the controller has `AAUTH_ISSUER_URL` set, which the translator passes through to the agent).

### To force Phase 1 (hwk only):

In `demo/aauth/aauth-config.yaml`:

```yaml
allow_pseudonymous: true
allowed_signature_key_schemes: [hwk]
# remove or comment out agent_servers and allow_insecure_jwt_issuer
```

And clear the controller's issuer URL so the translator stops injecting `AAUTH_CONTROLLER_URL`:

```bash
helm upgrade kagent helm/kagent \
  --namespace kagent --kube-context kind-kagent \
  --reuse-values \
  --set controller.aauth.issuerUrl="" \
  --wait
kubectl --context kind-kagent rollout restart deploy/aauth-test-agent -n kagent
```

Restart extauth; the next request will log `level=pseudonymous`.

### To force Phase 2 (jwt only — default in the committed config):

In `demo/aauth/aauth-config.yaml`:

```yaml
allow_pseudonymous: false
allowed_signature_key_schemes: [jwt]
allowed_jwt_types: [aa-agent+jwt]
agent_servers:
  - issuer:   "http://localhost:8083"
    jwks_uri: "http://localhost:8083/.well-known/jwks.json"
allow_insecure_jwt_issuer: true
```

Make sure `controller.aauth.issuerUrl=http://localhost:8083` is set on the controller (Step A1). After the agent pod restarts, its log should show:

```
AAuth: minted aa-agent+jwt — expires_in=86400
AAuth signing enabled — agent_id=aauth:... scheme=jwt
```

If you instead see `scheme=hwk` in the log, the controller URL didn't propagate — see "Agent stuck on hwk" below.

---

## Troubleshooting

### Agent stuck on `scheme=hwk` when Phase 2 is configured

The agent picks Phase 2 only if `AAUTH_CONTROLLER_URL` is set on the pod. The translator only injects it when the controller has `AAUTH_ISSUER_URL` set. Check both:

```bash
# Controller side:
kubectl --context kind-kagent exec -n kagent deploy/kagent-controller -- env | grep AAUTH
kubectl --context kind-kagent logs -n kagent deploy/kagent-controller | grep "AAuth issuer enabled"
# Expect: "AAuth issuer enabled" with issuerURL=http://localhost:8083

# Agent side:
kubectl --context kind-kagent exec -n kagent deploy/aauth-test-agent -- env | grep AAUTH
# Expect AAUTH_ENABLED, AAUTH_AGENT_ID, AND AAUTH_CONTROLLER_URL all set
```

If the agent has `AAUTH_ENABLED` but no `AAUTH_CONTROLLER_URL`, the translator didn't pick it up — most likely because the agent pod predates the controller upgrade. Restart the agent:

```bash
kubectl --context kind-kagent rollout restart deploy/aauth-test-agent -n kagent
```

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

### Agent log shows `falling back to hwk` after the upgrade

The controller's `POST /aauth/agent-jwt` endpoint now requires a Kubernetes ServiceAccount Bearer token (validated via `TokenReview`). If the agent pod is missing its projected SA token, or the controller can't reach the kube-apiserver, the mint fails and the signer stays on hwk for the life of the process.

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

If the binding is missing, install the chart at the current revision. If the SA token is missing, your pod spec is non-standard — make sure `automountServiceAccountToken: false` is **not** set on the agent's pod template or ServiceAccount.

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

# To turn off the controller-side issuer (back to Phase 1 only):
helm upgrade kagent helm/kagent \
  --namespace kagent --kube-context kind-kagent \
  --reuse-values --set controller.aauth.issuerUrl="" --wait

# To remove the resource_key.pem (it's git-ignored, but explicit cleanup):
rm demo/aauth/resource_key.pem
```

---

## Files in this directory

| File                       | What it is                                                                 |
|----------------------------|----------------------------------------------------------------------------|
| `agw-config.yaml`          | agentgateway config — binds :3030, proxies `/openai/*` to api.openai.com, delegates to extauth on :7070, sets `aauth_resource_id: kagent-agents`. |
| `aauth-config.yaml`        | extauth resource config — gRPC :7070, HTTP :8080, accepts `jwt` scheme, points at the controller's JWKS at `http://localhost:8083`. |
| `resource_key.pem`         | extauth's signing key for resource-token challenges. Generated locally, git-ignored. |
| `README.md`                | This file.                                                                 |
| `FOLLOWUPS.md`             | Spec-conformance gaps and demo cleanup items. The persistent issuer key and SA-TokenReview gate are DONE; remaining items are audience-scoped SA tokens, key rotation, optional `jti` for audit, and the SDK-side coverage for Gemini/Bedrock/MCP. |

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
