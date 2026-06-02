# AAuth on substrate-hosted declarative agents

How kagent's RFC 9421 / AAuth signing flow works when the agent is hosted as
an **agent-substrate actor** instead of a regular Kubernetes pod, and how to
demo it end-to-end.

For the non-substrate version of the demo (declarative agent in a regular
Deployment) see `demo/aauth/README.md`. This document only covers what's
different on substrate.

---

## Why this exists

The base AAuth flow (`demo/aauth/`) anchors agent identity in K8s TokenReview
of the agent pod's projected ServiceAccount token. That doesn't work for
substrate-hosted agents:

- **No per-agent ServiceAccount.** Substrate multiplexes many actors onto a
  small pool of worker pods. The worker pod has one SA; the actor processes
  inside it have nothing of their own to present.
- **No volume mounts inside the actor.** Substrate's `ActorTemplate.spec.
  containers[]` schema doesn't include `volumes` or `volumeMounts`. There is
  no way to mount the audience-scoped projected SA token used in deployment
  mode into the actor's gVisor sandbox.

So substrate-mode mint has to attest identity through a different mechanism
that's available inside the sandbox: the network identity of the worker pod
hosting the actor, cross-checked against substrate's own placement record.

---

## How it works

The mint endpoint (`POST /aauth/agent-jwt`) autodetects the request shape:

- If the body contains `substrate_actor_id`, the **substrate-aware
  authenticator** runs (described below).
- Otherwise, the existing **K8s TokenReview authenticator** runs (unchanged
  — that's the path `demo/aauth/` exercises).

### Trust chain (substrate path)

A single attested lookup, no bearer token required:

```
POST /aauth/agent-jwt
  body: { "substrate_actor_id": "<id>", "public_key_jwk": {...} }
  (Authorization header NOT required for this path)
  ▼
controller
  callerIP   = host part of r.RemoteAddr
  actor      = substrate.Control.GetActor(actor_id)

  assert: actor != nil
  assert: actor.Status == STATUS_RUNNING
  assert: actor.AteomPodIp == callerIP        ← placement attestation
  assert: actor.ActorTemplateName / Namespace non-empty

  sub = "aauth:" + actor.ActorTemplateName + "@" + actor.ActorTemplateNamespace + ".kagent.local"
  if body.sub != "" and body.sub != sub → reject

  mint aa-agent+jwt(sub, body.public_key_jwk)
  ▼
JWT returned. Signer flips scheme to "jwt" and signs every outbound httpx
request with RFC 9421 headers from then on.
```

Two independent things are attested:

| Check | What attests it | What it proves |
|---|---|---|
| `callerIP == actor.AteomPodIp` | K8s pod networking (preserves source IP cluster-internal) + substrate's placement record | The request really came from the worker pod that substrate says is hosting this actor right now. |
| `sub` from `actor.ActorTemplate{Name,Namespace}` | substrate's registry (which mirrors what kagent's translator wrote when it provisioned the ActorTemplate) | The agent identity this token will carry matches what kagent provisioned. |

The kagent translator sets `ActorTemplate.Name = Agent.Name` and
`ActorTemplate.Namespace = Agent.Namespace` 1:1 at translate time, so
substrate's view IS the kagent identity. No separate label lookup or
attestation is needed beyond what `Control.GetActor` already returns.

### Why no bearer token

The actor's gVisor sandbox inherits nothing from the worker pod's projected
SA volume mounts. Substrate's container schema can't carry per-actor
projected tokens either. So the only attestable thing reaching the
controller from inside an actor is the network 4-tuple — i.e. the source IP.
Source IP, plus substrate's view of placement, plus the fact that kagent
named the template after the agent, is enough to bind the mint request to
the correct kagent identity.

### Lazy mint and the golden-snapshot timing race

Substrate takes a "golden snapshot" of an ActorTemplate's container before
any individual actor instance is created. If the Python signer mints
eagerly at process start, that mint runs *during* the golden snapshot —
before substrate's Control plane has any actor record for this instance.
`Control.GetActor("kagent--<name>")` returns NotFound, the mint fails, and
the signer's failure state gets snapshotted. Every resume then comes up
broken because the snapshotted state is "no JWT, fall back to hwk".

Fix: in substrate mode, the signer skips the eager startup mint and lets
`ensure_fresh_jwt()` mint lazily on the first outbound httpx call. By that
point the actor exists in substrate (it has to — the call wouldn't have
been routed to it otherwise). Substrate-mode is detected by the presence of
`KAGENT_SUBSTRATE_ACTOR_ID` in the agent's environment, which the kagent
translator injects only for sandbox-mode agents.

### Residual gap (worth being explicit about)

The trust unit is the **worker pod**, not the individual actor instance.
If two actors are multiplexed on the same worker, an attacker who escaped
one actor's gVisor sandbox could present `substrate_actor_id` of a sibling
on the same worker and pass both checks (substrate confirms the sibling is
on this same pod IP). gVisor's sandbox boundary is the actual isolation
barrier here — AAuth doesn't strengthen or weaken it. Per-actor isolation
would require substrate to surface actor-process identity to its control
plane, which is upstream work.

---

## What the kagent code does differently in substrate mode

### Controller / translator

1. `go/core/pkg/sandboxbackend/substrate/substrate.go` — `applyAgentImageOverride()`
   appends two env vars when `aauthEnabled(agent)` on a substrate-mode
   container:
   - `KAGENT_SUBSTRATE_ACTOR_ID` = `ActorIDFor(namespace, name)` (the
     substrate actor id this template will be instantiated as)
   - `AAUTH_SA_TOKEN_PATH` is force-rewritten to `/var/run/secrets/kubernetes.io/serviceaccount/token`,
     which is harmless: the substrate-mode signer never reads it.
2. `go/core/internal/aauth/substrate_authenticator.go` — implements the
   `Control.GetActor`-based mint flow above. ~120 lines incl. comments.
3. `go/core/internal/aauth/handlers.go` — `HandleIssueAgentJWT` branches on
   `request.SubstrateActorID`: substrate path skips the
   `Authorization: Bearer` check entirely, deployment path is unchanged.
4. `go/core/pkg/app/app.go` — constructs the `SubstrateSubjectAuthenticator`
   only when the active sandbox backend is `substrate.Backend` *and* it has
   a live Control client. Substrate mint is unavailable otherwise.

### Python signer

5. `python/packages/kagent-adk/src/kagent/adk/aauth/_signer.py`:
   - `_build_jwt_request()` includes `substrate_actor_id` in the body when
     `KAGENT_SUBSTRATE_ACTOR_ID` env var is set.
   - `__init__` skips the synchronous eager mint when substrate mode is
     detected (avoids the golden-snapshot timing race; relies on lazy
     refresh instead).
   - `ensure_fresh_jwt()` no longer short-circuits on `scheme != "jwt"` —
     it attempts the first mint regardless, so an actor that starts in hwk
     mode can upgrade to jwt on the first outbound call.

### What does NOT change

- The mint endpoint URL (`POST /aauth/agent-jwt`) is the same. One endpoint,
  one handler, autodetected.
- The minted aa-agent+jwt is byte-identical in shape: same `typ`, `iss`,
  `dwk`, `cnf.jwk`, signed with the same issuer key.
- All three verifier doors (door 1 = extauth+agentgateway, door 2 = Go
  middleware on controller, door 3 = Python ASGI middleware on each agent)
  verify substrate-minted tokens with no changes.

### RBAC

No new RBAC needed. The substrate authenticator reads only via the
substrate Control gRPC API (which the controller already dials). No
`ActorTemplate.Get` is performed — that's the simplification over the
original plan; substrate's `GetActor` already returns the template
name/namespace.

---

## Demo: AAuth signing from a substrate-hosted declarative agent

This walks the in-cluster MCP path (Part F equivalent of the deployment-mode
demo) end-to-end. It is fully self-contained — no host-side `agentgateway`
or `extauth-aauth-resource` needed. For an external-LLM path (Part C
equivalent) wire the same `aauth-model-config` from `demo/aauth/` and the
ModelConfig will route the actor's outbound through agentgateway as usual.

### Prerequisites

- A Kind cluster with substrate installed and a kagent install whose chart
  is built from this branch. Easiest: use the existing
  `demo/substrate-poc/DEMO.md` flow up through the controller-image build
  step, then come back here. The demo was validated on a cluster set up by
  that flow, with `defaultWorkloadMode: sandbox`.
- A working `OPENAI_API_KEY` Secret named `kagent-openai` (the test agent
  uses the cluster's `default-model-config` which references it). A
  placeholder key works too — the LLM call will 401, but the AAuth/MCP
  loop completes before that point.
- `kubectl-ate` plugin on PATH (used only for debugging actor state).

### One-time build + roll out

Substrate's restricted `ActorTemplate` schema means we can't use the
standard `make build-controller`: `go/go.mod` has a local `replace`
directive pointing at `agent-substrate/substrate` on the host filesystem,
which doesn't exist inside the Docker build context. Build on the host,
ship via a thin distroless wrapper (same trick as the rest of
`demo/substrate-poc/DEMO.md`).

```bash
# Build the controller binary on the host (where the local replace resolves).
cd /path/to/kagent
CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOARCH) \
    go -C go build -o /tmp/kagent-controller-substrate ./core/cmd/controller

# Package the binary into a distroless image and push to the kind registry.
cp /tmp/kagent-controller-substrate /tmp/binctx
cp go/Dockerfile.substrate-demo /tmp/
docker buildx build --push --platform linux/$(go env GOARCH) \
    --build-arg BINARY=binctx \
    -t localhost:5001/kagent-dev/kagent/controller:aauth-substrate \
    -f /tmp/Dockerfile.substrate-demo /tmp

# Build the substrate-app image (the kagent-adk runtime with the
# substrate-mode signer changes). This rebuilds kagent-adk → app →
# app-substrate transitively.
make build-substrate-app

# Retag with a stable substrate-aauth tag so helm rollout pulls a fresh
# digest. (build-substrate-app pushes to :dev, but pods with IfNotPresent
# won't re-pull a tag they already have cached, so we need a new name.)
docker rmi localhost:5001/kagent-dev/kagent/app-substrate:dev
docker pull localhost:5001/kagent-dev/kagent/app-substrate:dev
docker tag  localhost:5001/kagent-dev/kagent/app-substrate:dev \
            localhost:5001/kagent-dev/kagent/app-substrate:aauth-substrate
docker push localhost:5001/kagent-dev/kagent/app-substrate:aauth-substrate
```

Upgrade CRDs (this branch's `Agent` CRD has the `spec.declarative.aauth`
field), then roll out the controller and switch the substrate agent image:

```bash
helm upgrade kagent-crds helm/kagent-crds \
  --namespace kagent --wait

helm upgrade kagent helm/kagent \
  --namespace kagent --reuse-values \
  --set controller.image.tag=aauth-substrate \
  --set substrate.agentImage=localhost:5001/kagent-dev/kagent/app-substrate:aauth-substrate \
  --set controller.aauth.issuerUrl=http://localhost:8083 \
  --wait --timeout 5m
```

Confirm the controller picked up both the issuer and the substrate
authenticator:

```bash
kubectl logs -n kagent deploy/kagent-controller | grep -E "AAuth (issuer|substrate|verifier)"
```

Expected (3 lines):

```
"msg":"AAuth issuer enabled","issuerURL":"http://localhost:8083","kid":"kagent-issuer-1","secret":"kagent/kagent-aauth-issuer"
"msg":"AAuth substrate authenticator enabled"
"msg":"AAuth verifier enabled","mode":"log"
```

If `AAuth substrate authenticator enabled` is missing, the controller is
either not configured with the substrate sandbox backend
(`--substrate-worker-pool-name` empty) or substrate's Control endpoint
didn't dial. Both are also failures for the substrate poc itself; fix
those first.

### Apply the test agent

The demo agent ships in `examples/aauth-test-agent.yaml`. Apply it, then
patch two fields:

- `spec.workloadMode = sandbox` — opt this agent into substrate (overrides
  the cluster default if you didn't set it globally).
- `spec.declarative.modelConfig = default-model-config` — use direct
  OpenAI for the LLM call. The default `aauth-model-config` in the file
  routes through host-side agentgateway which we don't need for the
  in-cluster MCP demo.

```bash
kubectl apply -f examples/aauth-test-agent.yaml

kubectl patch agent -n kagent aauth-test-agent --type merge -p \
  '{"spec":{"workloadMode":"sandbox","declarative":{"modelConfig":"default-model-config"}}}'
```

Wait for the agent to be ready (substrate is materializing the golden
snapshot, which takes 30-60 seconds on first apply):

```bash
until kubectl get agent -n kagent aauth-test-agent \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}' \
        | grep -q WorkloadReady; do sleep 6; done
```

Verify substrate has the actor record (this confirms the kagent
controller's `Control.CreateActor` succeeded, which is required for the
substrate authenticator to attest the mint):

```bash
kubectl ate get actor kagent--aauth-test-agent
# Expect STATUS_SUSPENDED at rest (lazy activation) — that's fine; the
# first A2A request below will resume it to STATUS_RUNNING.
```

### Port-forward and trigger

```bash
# In one terminal:
kubectl port-forward -n kagent svc/kagent-controller 8083:8083

# Sanity check from the host:
curl -s http://localhost:8083/.well-known/jwks.json | jq -c '.keys[0] | {kid,alg,kty,crv}'
# {"kid":"kagent-issuer-1","alg":"EdDSA","kty":"OKP","crv":"Ed25519"}
```

Send an A2A message through the controller's proxy (this is the path that
routes to a substrate actor via atenet's Host-header routing — the actor's
`:8080` is not directly exposed):

```bash
T0=$(date -u +%Y-%m-%dT%H:%M:%SZ)

curl -sS -X POST http://localhost:8083/api/a2a/kagent/aauth-test-agent/ \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc":"2.0","id":"sb-1","method":"message/send",
    "params":{"message":{"kind":"message","messageId":"sb-msg-1","role":"user",
      "parts":[{"kind":"text","text":"You MUST call the list_agents tool right now. Do not answer from memory. Just call list_agents and tell me the count."}]}}
  }' >/dev/null
```

### Verify

The expected proof is in the controller's logs: one substrate mint and five
verified MCP calls.

```bash
# Mint allowed via substrate authenticator (one line):
kubectl logs -n kagent deploy/kagent-controller --since-time=$T0 \
  | grep "aauth: substrate mint allowed"
```

Expected:

```
"msg":"aauth: substrate mint allowed"
"remoteAddr":"10.244.0.36:..."                                  ← worker pod IP
"substrate_actor_id":"kagent--aauth-test-agent"
"sub":"aauth:aauth-test-agent@kagent.kagent.local"               ← from actor.ActorTemplate{Name,Namespace}
```

```bash
# Verifier (door 2) on every /mcp call from the actor (five lines):
kubectl logs -n kagent deploy/kagent-controller --since-time=$T0 \
  | grep '"aauth"' | grep '"path":"/mcp"' \
  | jq -c '{ts,msg,caller,method,path}'
```

Expected (one MCP session = initialize → SSE GET → list_tools → call
list_agents → DELETE close, all five legs `aauth: verified`):

```
{"ts":"…","msg":"aauth: verified","caller":"aauth:aauth-test-agent@kagent.kagent.local","method":"POST","path":"/mcp"}
{"ts":"…","msg":"aauth: verified","caller":"aauth:aauth-test-agent@kagent.kagent.local","method":"GET", "path":"/mcp"}
{"ts":"…","msg":"aauth: verified","caller":"aauth:aauth-test-agent@kagent.kagent.local","method":"POST","path":"/mcp"}
{"ts":"…","msg":"aauth: verified","caller":"aauth:aauth-test-agent@kagent.kagent.local","method":"POST","path":"/mcp"}
{"ts":"…","msg":"aauth: verified","caller":"aauth:aauth-test-agent@kagent.kagent.local","method":"DELETE","path":"/mcp"}
```

If you see `aauth: unverified ... scheme=hwk` lines instead of `verified`,
the actor is running off a stale golden snapshot from a previous (broken)
build. Delete and recreate the agent to force a fresh cold boot:

```bash
kubectl delete agent -n kagent aauth-test-agent
kubectl apply -f examples/aauth-test-agent.yaml
kubectl patch agent -n kagent aauth-test-agent --type merge -p \
  '{"spec":{"workloadMode":"sandbox","declarative":{"modelConfig":"default-model-config"}}}'
```

This is unfortunately required after any change to the controller or
app-substrate image — substrate's golden snapshot is taken once and
re-used, so a fresh image only takes effect on a fresh actor.

### Negative tests (optional)

The unit test suite covers tamper cases without needing the live cluster.
Sanity check it after any code change:

```bash
go -C go test ./core/internal/aauth/... -v -run TestSubstrateSubjectAuthenticator
```

Cases exercised (all should PASS, errors matched on substrings of the
returned `403` body):

| Case | Expected rejection reason |
|---|---|
| Empty `substrate_actor_id` | `empty substrate_actor_id` |
| Empty source address | `empty address` |
| `Control.GetActor` returns nil | `substrate has no actor` |
| Actor is `STATUS_SUSPENDED` | `not RUNNING` |
| Actor has no `ateom_pod_ip` | `no recorded ateom_pod_ip` |
| Actor placement on a different pod IP | `IP mismatch` |
| Body `sub` doesn't match `actor.ActorTemplate{Name,Namespace}` | `does not match canonical` |
| Substrate returned blank template name/namespace | `no ActorTemplate name/namespace` |

To run an in-cluster tamper test, the easiest is forging the
`substrate_actor_id` from outside the cluster. The mint endpoint will
reject because the request's source IP (port-forward's localhost address)
won't match the actor's worker pod IP:

```bash
curl -sS -X POST http://localhost:8083/aauth/agent-jwt \
  -H 'Content-Type: application/json' \
  -d '{
    "substrate_actor_id":"kagent--aauth-test-agent",
    "public_key_jwk":{"kty":"OKP","crv":"Ed25519","x":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
  }'
# {"error":"substrate attestation failed: IP mismatch: request came from 127.0.0.1 but actor \"kagent--aauth-test-agent\" is hosted on 10.244.0.36"}
```

---

## Open follow-ups

These are not blockers for the demo but are worth tracking if this path
graduates from PoC.

- **Per-actor identity.** Currently the trust unit is the worker pod; all
  actors on a worker share the same source IP and could mint as each
  other if gVisor isolation is breached. Per-actor attestation would
  require substrate to expose an actor-process credential to its control
  plane.
- **Substrate-mode Part C.** External-LLM signing through host-side
  `agentgateway` + `extauth-aauth-resource` works for deployment-mode
  agents (see `demo/aauth/`). It should work for substrate-mode too — the
  same `aauth-model-config` baseUrl override applies — but it wasn't
  validated end-to-end in this PoC because the cluster's substrate
  network routing to `host.docker.internal:3030` is non-trivial.
- **Resume safety.** Lazy mint makes the snapshotted state benign (the
  pre-mint signer is a clean "needs JWT" state, not a broken "stuck in
  hwk" state). Validated only on the active actor, not across an
  explicit suspend/resume cycle. Worth confirming before relying on this
  in long-running deployments.
