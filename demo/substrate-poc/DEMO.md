# Demo — kagent on agent-substrate (three working paths)

In-cluster, helm-installed, CRD-driven walkthrough demonstrating **three
distinct kagent-on-substrate runtimes**, all running on a single kind
cluster against a substrate fork carrying six small patches:

1. **BYO Agent** — a hand-rolled FastAPI image (`demo-alpha`) packaged as
  `kind: Agent, spec.type: BYO, spec.workloadMode: sandbox`. Boots in
   ~2s, proves the substrate runtime + A2A + UI chat panel end-to-end.
2. **Declarative Agent** — the helm-shipped `k8s-agent` shape
  (`kind: Agent, spec.type: Declarative, runtime: python`) running the
   real kagent ADK with `gpt-4o-mini` + `kagent-tool-server` MCP tools,
   inside a gVisor sandbox. Answers real `kubectl get` questions.
3. **AgentHarness on substrate** — `kind: AgentHarness, spec.runtime:
  substrate, spec.backend: openclaw`. Auto-provisions a per-harness`  WorkerPool`+`ActorTemplate`, runs an openclaw VM inside a  substrate gVisor sandbox, and serves the OpenClaw Control UI through  a kagent-controller proxy at`  /api/agentharnesses///gateway/`.

See [SUBSTRATE.md](../../SUBSTRATE.md) for the full design + history of
substrate-side fork patches.

> **What the demo proves:** kagent agents defined entirely by CRD run
> inside substrate gVisor sandboxes with no per-agent Deployment. The
> Declarative path calls `gpt-4o-mini` over the public internet and uses
> MCP to call back into `kagent-tool-server` for real `kubectl get pods`
> data. Substrate's checkpoint/restore preserves the Python process
> memory across suspend cycles. The AgentHarness path lets kagent
> orchestrate the openclaw VM stack on top of substrate via the
> `runtime: substrate, backend: openclaw` knob, replicating the
> `examples/substrate-openclaw` workflow from pj-kagent.

## Substrate fork patches required for this demo

The upstream `agent-substrate/substrate` repo on `main`
(SHA `a436ea2…` as of 2026-05-27) needs six small patches before any of
the three demos work end-to-end on `kind-on-macOS`. They live as three
logical commits on a `**ceposta-kagent`** branch in your local substrate
checkout (intended as upstream PRs after substrate-team review).

```bash
cd ~/go/src/github.com/agent-substrate/substrate
git checkout ceposta-kagent
git log --oneline master..HEAD
```

You should see three commits ahead of `master`:

```
4125b45  substrate timeouts: bump restore-path budgets for cold-cache workloads
6ac9710  ateom-gvisor: enable sentry debug logs + tolerate post-checkpoint cleanup race
b4f4930  atelet: skip device/FIFO tar entries during image unpack
```

What's in each commit and why:


| Commit                 | Files                                                                                                                                | Effect                                                                                                                                                                                                                                                                                                              |
| ---------------------- | ------------------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `b4f4930` atelet       | `cmd/atelet/oci.go`                                                                                                                  | Skip `tar.TypeChar/TypeBlock/TypeFifo` entries during image unpack. Lets atelet unpack any Debian/Wolfi/Alpine base image (every workload image with a `/dev/null` char-device in its base layer).                                                                                                                  |
| `6ac9710` ateom-gvisor | `cmd/ateom-gvisor/runsc.go` + `cmd/ateom-gvisor/main.go`                                                                             | Enable sentry debug logs (diagnostic; toggles were pre-staged as commented-out lines upstream) + demote post-checkpoint `cmdState`/`cmdDelete` failures from fatal RPC errors to warnings. Without the second half, openclaw + kagent ADK both fail with `runsc checkpoint pause: exit status 128` retried forever. |
| `4125b45` timeouts     | `cmd/atenet/internal/app/router/resumer.go` + `cmd/atenet/internal/app/router/xds.go` + `cmd/ateapi/internal/controlapi/workflow.go` | Three coordinated timeout increases: atenet's `bgCtx` 15s→60s, atenet's ext_proc `Timeout` + `MessageTimeout` 5s→60s, and ateapi's `ResumeActor`/`SuspendActor` lock TTL 30s→120s. All three are needed because the kagent ADK image's `runsc restore` consistently lands at 14–18s on kind/macOS.                  |


Each commit has rationale in its message; each patch has a `kagent fork:`
or `kagent local fork:` comment on the modified lines so future-`git blame` makes the change obvious.

**Heads-up:** if your local substrate checkout doesn't have a
`ceposta-kagent` branch (or upstream gets ahead of `a436ea2…`), you'll
need to recreate the branch — either cherry-pick the three commits onto
the new `master` or apply them manually using the same line-level edits.
The commit messages contain enough context to recreate the patches by
hand if needed.

> **What the demo proves:** a kagent agent defined entirely by CRD
> (`Agent` with `workloadMode: sandbox` + `ModelConfig` + `RemoteMCPServer`) runs inside
> substrate's gVisor sandbox with no per-agent Deployment, calls
> `gpt-4o-mini` over the public internet, and uses MCP to call back into
> `kagent-tool-server` for real `kubectl get pods` data. The agent process
> is checkpointed to object storage when idle; the next request resumes
> the same Python process from snapshot.

## Prerequisites

- `docker`, `kubectl`, `helm`, `go`, `kind`, `kubectl-ate` plugin on PATH.
- A checkout of `agent-substrate` at `~/go/src/github.com/agent-substrate/substrate`
(its `hack/install-ate-kind.sh` script installs substrate into the kind
cluster).
- An OpenAI API key at `~/bin/openai-key` (one line, no trailing newline).
Any provider supported by kagent works; replace `providers.openAI.`* in
the values file accordingly.

## Cluster + substrate (one-time, ~5 min)

```bash
# 1. Switch to the ceposta-kagent branch in your local substrate checkout.
#    install-ate-kind.sh below builds substrate's images via `ko` from
#    whatever's checked out, so the patches need to be in place BEFORE
#    running it. See the "Substrate fork patches" section above for what
#    these three commits do.
cd ~/go/src/github.com/agent-substrate/substrate
git checkout ceposta-kagent
git log --oneline master..HEAD   # should show 3 commits

# 2. Create the demo cluster. Substrate's kind config enables
#    ClusterTrustBundle/PodCertificateRequest feature gates (required by
#    substrate's mTLS chain) and pre-wires the localhost:5001 kind-registry.
KIND_CLUSTER_NAME=kagent-substrate ./hack/create-kind-cluster.sh

# 3. Install substrate (~3-5 min: builds + pushes patched images via ko,
#    applies CRDs, waits for ate-system rollout). Picks up our patches
#    automatically since they're committed in the local checkout.
#
#    IMPORTANT: install-ate-kind.sh pushes atelet/ate-api/atenet/etc. but
#    NOT ateom-gvisor — that image is consumed by kagent (the WorkerPool
#    container), not by substrate itself. We have to build + push it
#    manually as a separate step, below.
./hack/install-ate-kind.sh --deploy-ate-system

# 4. Build + push the patched ateom-gvisor image. Use --base-import-paths
#    so the pushed tag is the stable `ateom-gvisor:latest` (vs ko's default
#    content-hashed name which changes between builds and breaks the values
#    file's reference). kagent-substrate-values.yaml's
#    `substrate.workerPoolAteomImage` is pinned to this bare tag.
NO_DEV_ENV=true KO_DOCKER_REPO=localhost:5001 KO_DEFAULTPLATFORMS=linux/amd64 \
  ./hack/run-tool.sh ko build --base-import-paths --push ./cmd/ateom-gvisor

# 5. Sanity-check substrate is healthy.
kubectl get pods -n ate-system
# Expect ate-api-server, ate-controller, atelet, atenet-router, rustfs, and
# valkey-cluster-{0..5} all Running. If ate-api-server is crash-looping with
# "Failed to connect to Redis/Valkey" → that's the known stale-pod-IPs bug
# (SUBSTRATE.md §11). Restart it: `kubectl rollout restart deploy/ate-api-server-deployment -n ate-system`
```

## Build + push the kagent controller image (~2 min)

The controller image must include the substrate backend code. Today
`go/go.mod` carries a local `replace` directive pointing at the
`agent-substrate` source tree, which doesn't exist inside Docker build
contexts — so we build the binary on the host and ship it via a thin
distroless wrapper:

```bash
cd ~/go/src/github.com/kagent-dev/kagent
CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOARCH) \
    go -C go build -o /tmp/kagent-controller-substrate ./core/cmd/controller

cp /tmp/kagent-controller-substrate /tmp/binctx
cp go/Dockerfile.substrate-demo /tmp/
docker buildx build --push --platform linux/$(go env GOARCH) \
    --build-arg BINARY=binctx \
    -t localhost:5001/kagent-dev/kagent/controller:unified-agent \
    -f /tmp/Dockerfile.substrate-demo /tmp
```

Also build the substrate-shim variant of the kagent app image. This image
contains the `substrate-entrypoint.sh` shim that materializes
`config.json` + `agent-card.json` from env vars (since substrate's
`ActorTemplate.spec.containers` allows no volume mounts):

```bash
VERSION=unified-agent make build-substrate-app
# Pushes localhost:5001/kagent-dev/kagent/app-substrate:dev
```

And the UI image (this demo's values file enables the UI by default).
**Important: rebuild this whenever you change the controller** — the
UI calls `/api/a2a/...` paths the controller serves; a mismatch
between UI and controller versions surfaces as 404s in the browser.

```bash
VERSION=unified-agent make build-ui
# Pushes localhost:5001/kagent-dev/kagent/ui:unified-agent
```

If you don't want the UI, set `ui.replicas: 0` in
`demo/substrate-poc/kagent-substrate-values.yaml` and skip this step.

Finally, the Phase-0 stand-in agent image (`substrate-poc-agent:p5`).
This is what the four BYO agents in `03-agents.yaml` and the
single-agent reference in `02-sandbox-agent.yaml` use. It's a small
FastAPI app that proves the substrate runtime + the A2A JSON-RPC
endpoint — no LLM credentials needed:

```bash
docker build -t localhost:5001/substrate-poc-agent:p5 demo/substrate-poc \
  && docker push localhost:5001/substrate-poc-agent:p5
```

## Install kagent via helm (~1 min)

```bash
kubectl create ns kagent-substrate-poc --dry-run=client -o yaml | kubectl apply -f -

helm install kagent-crds helm/kagent-crds -n kagent --create-namespace

helm install kagent helm/kagent -n kagent \
    -f demo/substrate-poc/kagent-substrate-values.yaml \
    --set providers.openAI.apiKey="$(tr -d '\n' < ~/bin/openai-key)"
```

The values file (`demo/substrate-poc/kagent-substrate-values.yaml`):

- Points `controller.image` at the locally-built `controller:unified-agent`.
- Sets `controller.agentImage` to the substrate-shim `app-substrate:dev`.
- Pins `ui.image.tag` to `unified-agent` so UI and controller stay in sync.
- Enables `substrate.enabled: true` with workerpool config — at startup
the controller's `WorkerPoolEnsurer` auto-creates the shared `poc-pool`
WorkerPool in `kagent-substrate-poc`.
- Disables every built-in `*-agent` chart so the controller log is clean
during demo validation; only `kagent-tools` (the MCP tool server) stays
on. The `k8s-agent` is applied by hand as an `Agent` with `spec.workloadMode: sandbox` (see below).
- Enables the UI (1 replica) so you can drive the demo through both the
browser and `curl`. Disables `oauth2-proxy` (not needed for the demo).

Wait for things to settle:

```bash
# Controller restarts a couple times while postgres comes up — that's expected.
kubectl wait --for=condition=Ready pod -l app.kubernetes.io/component=controller -n kagent --timeout=180s

# WorkerPool auto-provisioned, 2 worker pods running.
kubectl get workerpool -n kagent-substrate-poc poc-pool
kubectl get pods      -n kagent-substrate-poc
```

Expect: `poc-pool` exists with `replicas=2` and `ateomImage` set; two
`poc-pool-deployment-*` pods are `1/1 Running`. **Zero agent pods** —
that's the point.

## Apply the Phase-0 BYO agent (the reliable demo path)

The simplest path that reliably demonstrates the full unification +
substrate runtime is `demo/substrate-poc/03-agents.yaml`'s `demo-alpha`
— a small FastAPI app (`substrate-poc-agent:p5`) that boots in ~2s,
gets golden-snapshotted by substrate, and serves both plain HTTP
endpoints (`/info`, `/connectivity`, etc.) and a minimal A2A JSON-RPC
endpoint at `POST /` so the kagent A2A mux and the UI chat panel can
drive it end-to-end. No LLM credentials needed.

```bash
# Apply just demo-alpha (one of the four agents in 03-agents.yaml):
cat <<'EOF' | kubectl apply -f -
apiVersion: kagent.dev/v1alpha2
kind: Agent
metadata:
  name: demo-alpha
  namespace: kagent-substrate-poc
spec:
  type: BYO
  workloadMode: sandbox
  byo:
    deployment:
      image: localhost:5001/substrate-poc-agent:p5
      cmd: /usr/local/bin/uvicorn
      args: [agent:app, --app-dir, /app, --host, 0.0.0.0, --port, "80", --log-level, info, --loop, asyncio, --http, h11]
EOF

kubectl wait agent demo-alpha -n kagent-substrate-poc --for=condition=Ready --timeout=120s

kubectl get agent demo-alpha -n kagent-substrate-poc \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}{"\n"}'
# Expect: golden snapshot ready; actor "kagent-substrate-poc--demo-alpha" is STATUS_SUSPENDED
```

> **If the very first request fails** with `eth0: Link not found` (the
> §11 worker-network corruption from a partial sandbox setup), recycle
> just that worker and retry — substrate respawns a clean replacement:
>
> ```bash
> ASSIGNED=$(kubectl ate get workers 2>/dev/null | awk '$3=="ASSIGNED" {print $2}' | head -1)
> [ -n "$ASSIGNED" ] && kubectl delete pod -n kagent-substrate-poc "$ASSIGNED"
> ```
>
> Once any cycle completes successfully on a worker, the worker's
> gVisor host state is in a known-good shape and subsequent
> resume/suspend cycles are reliable.

## Drive a real prompt — the demo moment

```bash
# Port-forward the controller's A2A HTTP endpoint.
kubectl port-forward -n kagent svc/kagent-controller 18093:8083 &

curl -sS --max-time 60 -X POST -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":"1","method":"message/send","params":{"message":{"role":"user","messageId":"msg-1","parts":[{"kind":"text","text":"hello from the unified Agent CRD!"}]}}}' \
    http://localhost:18093/api/a2a/kagent-substrate-poc/demo-alpha/ \
  | python3 -c "import json,sys; d=json.load(sys.stdin); arts=d['result']['artifacts']; [print(p['text']) for a in arts for p in a['parts'] if p['kind']=='text']"
```

You should see something like:

```
echo from substrate-poc agent (d171247c, request #1): hello from the unified Agent CRD!
```

What happened under the hood:

1. Request hit the kagent controller's A2A mux at `:8083/api/a2a/.../`
  (the unified path after the Agent+SandboxAgent merge — §21).
2. The mux's per-agent client used the **substrate routing override**
  (host-rewriting transport) — dialed `atenet-router.ate-system.svc`
   with `Host: kagent-substrate-poc--demo-alpha.actors.resources.substrate.ate.dev`.
3. atenet's ExtProc parsed the actor ID and called `ResumeActor` on
  substrate's Control API.
4. Substrate `runsc restore`d the actor from rustfs onto a FREE worker
  (~1s) and forwarded the HTTP request into the gVisor sandbox.
5. Inside the sandbox the FastAPI app's JSON-RPC handler at `POST /`
  built the A2A `task` response and returned it.
6. Response flowed back through atenet → controller → curl.

Confirm idle auto-suspend by waiting 40 seconds and watching the controller log:

```bash
kubectl logs -n kagent deploy/kagent-controller --tail=200 | grep idle-suspender
# Expect: "auto-suspended idle actor" actor=kagent-substrate-poc--demo-alpha

kubectl ate get workers   # both workers FREE again
```

Send a second request: same Python process resumes from snapshot;
substrate's `runsc restore` brings the same memory state back. Hit
`http://localhost:18000/info` (via atenet port-forward) to see the
`startup_uuid` is unchanged and `request_count` continues incrementing
across suspend/resume.

### The Declarative real-LLM path (`k8s-agent-substrate`)

`demo/substrate-poc/05-builtin-k8s-agent.yaml` is functionally
equivalent to the helm-shipped `k8s-agent` (`type: Declarative`, real
gpt-4o-mini, `kagent-tool-server` MCP tools). The chat returns real
`kubectl get` data answered by an LLM call from inside the sandbox.

**Reliability:** with the six substrate fork patches at the top of this
doc applied, this path is **deterministically Ready in ~40s** and serves
real prompts. The first request after a long idle takes ~14–18s
(`runsc restore` cold path through the OCI rootfs unpack) which is why
substrate patches #4 and #5 bump the atenet timeouts to 60s. Subsequent
requests hit the warm actor in <1s.

```bash
kubectl apply -f demo/substrate-poc/05-builtin-k8s-agent.yaml
kubectl wait agent k8s-agent-substrate -n kagent --for=condition=Ready --timeout=300s

kubectl port-forward -n kagent svc/kagent-controller 18093:8083 &

curl -sS --max-time 120 -X POST -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":"1","method":"message/send","params":{"message":{"role":"user","messageId":"k8s-1","parts":[{"kind":"text","text":"List pods in kube-system."}]}}}' \
    http://localhost:18093/api/a2a/kagent/k8s-agent-substrate/
```

Expected: a real markdown answer listing pods, generated by `gpt-4o-mini`
after the agent called `k8s_get_resources` MCP tool via `kagent-tool-server`.

### The AgentHarness openclaw path (`runtime: substrate, backend: openclaw`)

`demo/substrate-poc/06-openclaw-harness.yaml` is an `AgentHarness` that
runs an openclaw VM inside a substrate gVisor sandbox. The kagent
controller's substrate harness backend:

- auto-provisions a per-harness `WorkerPool` in the harness's namespace
- auto-provisions an `ActorTemplate` referencing the
`ghcr.io/kagent-dev/nemoclaw/sandbox-base` image
- triggers golden-snapshot of the openclaw startup (succeeds thanks to
substrate patch #3)
- creates + resumes the harness actor, watches it transition to
STATUS_RUNNING
- registers an HTTP proxy at `/api/agentharnesses/<ns>/<name>/gateway/`
that forwards browser traffic (including WebSockets) to the openclaw
Control UI inside the sandbox

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: kagent.dev/v1alpha2
kind: AgentHarness
metadata:
  name: openclaw-test
  namespace: kagent
spec:
  runtime: substrate
  backend: openclaw
  description: OpenClaw on Agent Substrate
  modelConfigRef: default-model-config
  substrate:
    snapshotsConfig:
      location: gs://ate-snapshots/kagent/openclaw-test/
    workerPool:
      replicas: 1
      # ateomImage falls back to controller.substrate.workerPoolAteomImage
      # in the helm values file (substrate's reproducible ko-built tag).
    gatewayToken: test-token
EOF

# Reaches Ready in ~5min on a fresh substrate (image pull + WorkerPool roll
# + ActorTemplate golden snapshot + actor Resume). Subsequent re-applies
# are faster because images are cached.
kubectl wait agentharness openclaw-test -n kagent --for=condition=Ready --timeout=600s

# Open the OpenClaw Control UI through the kagent proxy:
kubectl port-forward -n kagent svc/kagent-controller 18093:8083 &
open http://localhost:18093/api/agentharnesses/kagent/openclaw-test/gateway/
# Token to enter in the OpenClaw login form: test-token
```

> **Worker-recycle gotcha.** If a worker pod is recycled (e.g., by an
> ate-controller rollout) while the harness actor is `STATUS_RESUMING`,
> the actor wedges pointing at the dead pod and the harness controller
> doesn't always re-anchor. Unstick:
>
> ```bash
> kubectl ate suspend actor ahr-<ns>-<name>
> # The harness controller's next reconcile resumes it on a live worker.
> ```

## Drive the same flow through the UI

The kagent UI talks to the same controller HTTP API the `curl` step uses,
so anything that worked above will work in the browser. The values file
enables the UI by default (`ui.replicas: 1`).

```bash
kubectl port-forward -n kagent svc/kagent-ui 13000:8080 &
# open http://localhost:13000 in a browser
```

**Create a sandbox-mode Agent through the form:**

1. Go to **Agents → New Agent**.
2. **Type** dropdown → pick **"Sandbox workload"**. (After the
  SandboxAgent unification this sets `spec.workloadMode: sandbox` on a
   `kind: Agent` CR — there's no separate SandboxAgent kind anymore. See
   SUBSTRATE.md §21.)
3. Leave the **BYO image** field empty (that's what makes the Agent
  `type: Declarative` instead of `type: BYO`).
4. Fill in **Name**, **Namespace** (`kagent` works because that's where
  the helm install put the default ModelConfig and the
   `kagent-tool-server` RemoteMCPServer), **System prompt**, and
   **Model config** = `default-model-config`.
5. Under **Tools**, add an MCP tool referencing
  `kagent/kagent-tool-server` (kind `RemoteMCPServer`, apiGroup
   `kagent.dev`). Pick a subset of toolNames — `k8s_get_resources`,
   `k8s_describe_resource`, `k8s_get_events` are good starters.
6. **Submit.** The UI POSTs to `/api/agents`; the controller
  creates the CR; substrate captures the golden snapshot; the agent
   reaches `Ready` ~1 min later.

The new agent shows up in the agent list. Click it → use the chat panel
to send a real prompt. Under the hood the UI hits the same
`/api/a2a/<ns>/<name>/` endpoint the `curl` example uses, so
the substrate path is identical: atenet → resume → real LLM →
`kagent-tool-server` MCP → answer.

> **Don't add `spec.skills` in the form.** If the form has a Skills
> section, leave it empty. Skills emit a `skills-init` initContainer
> substrate's restricted `ActorTemplate.spec.containers` can't carry —
> see SUBSTRATE.md §13.

## Substrate observability in the UI

The UI ships a read-only **Substrate** page at `/substrate` (Header → View
→ Substrate) that polls the controller every 2 s and renders two tables:

- **Workers** — every worker pod backed by a gVisor sandbox, its pool, IP,
state (FREE/ASSIGNED), and which actor it currently hosts.
- **Actors** — every registered actor with status (Running, Suspended,
Resuming, Suspending), its template, current worker pod, and last
snapshot URI. Running actors sort to the top.

```bash
# Direct API access (no UI required)
kubectl port-forward -n kagent svc/kagent-controller 18093:8083 &
curl -s http://localhost:18093/api/substrate/workers | jq
curl -s http://localhost:18093/api/substrate/actors  | jq
```

The endpoints are only registered when the controller was started with
`substrate.controlEndpoint` set (i.e. substrate is enabled). Without
substrate they return HTTP 501. See SUBSTRATE.md §24 for the backend
shape.

To prove the live-update path, suspend a running actor in one terminal
and watch the row flip Running → Suspending → Suspended in the UI:

```bash
kubectl ate suspend actor <actor-id>
```

## AAuth signing for every declarative agent

`kagent-substrate-values.yaml` ships two paired AAuth settings:


| Helm value                                            | Effect                                                                                                                                                                                         |
| ----------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `controller.defaultAAuthEnabled: true`                | Translator injects `AAUTH_ENABLED=true` + the projected SA-token volume into every declarative Agent (unless `spec.declarative.aauth.enabled: false` opts out).                                |
| `controller.aauth.issuerUrl: "http://localhost:8083"` | Controller registers `/.well-known/jwks.json` + `POST /aauth/agent-jwt`. Without this, agent pods can't mint a JWT and the signer fails closed. Pair this with the `defaultAAuthEnabled` flag. |


After `helm upgrade`, every sandbox-mode declarative agent (e.g.
`k8s-agent-substrate`) gets the AAuth env vars even though its YAML
doesn't mention AAuth at all:

```bash
kubectl get actortemplate -n kagent k8s-agent-substrate \
    -o jsonpath='{range .spec.containers[*].env[*]}{.name}={.value}{"\n"}{end}' \
    | grep AAUTH
# AAUTH_ENABLED=true
# AAUTH_AGENT_ID=aauth:k8s-agent-substrate@kagent.kagent.local
# AAUTH_CONTROLLER_URL=http://kagent-controller.kagent:8083
# AAUTH_SA_TOKEN_PATH=/var/run/secrets/kubernetes.io/serviceaccount/token
```

### Stand up the external verifier (agentgateway + extauth)

**Prerequisites** (one-time):

- `agentgateway` binary on `$PATH`
([install](https://github.com/agentgateway/agentgateway)).
- `extauth-aauth-resource` cloned + built. Default location:
`~/go/src/github.com/christian-posta/extauth-aauth-resource`. Build with
`go build -o aauth-service ./cmd/server`. Override with `EXTAUTH_REPO=`.
- Resource keypair generated once:
`openssl genpkey -algorithm ed25519 -out demo/aauth/resource_key.pem`.

**Run:**

```bash
demo/substrate-poc/start-aauth-gateway.sh         # foreground-spawns 3 bg procs
# … run prompts through the UI / curl …
demo/substrate-poc/start-aauth-gateway.sh --stop  # tear down
```

The script:

1. Port-forwards `kagent-controller` to `localhost:8083` so the issuer
  JWKS + `/aauth/agent-jwt` mint endpoint are reachable from the host.
2. Launches `extauth-aauth-resource` on :7070 (gRPC for agentgateway
  ext_authz) + :8080 (HTTP for `/.well-known/aauth-resource.json` and
   `/.well-known/jwks.json`).
3. Launches `agentgateway` on :3030, delegating every request to extauth
  before forwarding upstream. Sets `OPENAI_API_KEY=sk-placeholder-not-real`
   if unset — a real key is not required for the AAuth proof; the upstream
   OpenAI 401 doesn't change what extauth verifies.

**Smoke test** — an unsigned request to agentgateway must get rejected by
extauth with `401 missing_signature`:

```bash
$ curl -s -o /dev/null -w "%{http_code}\n" \
    -X POST http://localhost:3030/openai/v1/chat/completions \
    -H "content-type: application/json" -d '{}'
401
$ curl -s -X POST http://localhost:3030/openai/v1/chat/completions \
    -H "content-type: application/json" -d '{}'
{"error":"missing_signature"}
```

`/.well-known/jwks.json` should also be reachable through the
port-forward and contain the Ed25519 issuer key:

```bash
$ curl -s http://localhost:8083/.well-known/jwks.json
```

Agent pods then point `ModelConfig.openai.base_url` at
`http://host.docker.internal:3030/openai`. See `demo/aauth/README.md` for
the verifier-boundary diagram (doors 1/2/3).

### Apply the example aauth-test-agent

`examples/aauth-test-agent.yaml` is what the verification recipes below
exercise. It declares a `RemoteMCPServer` pointing at the controller's
built-in MCP endpoint and an `Agent` that uses it as a tool. The Agent's
`spec.declarative.modelConfig` is `aauth-model-config` — a separate
ModelConfig that points `openAI.baseUrl` at agentgateway, so the agent's
LLM traffic flows through the door-1 verifier.

The example doesn't bundle that ModelConfig, so create it first:

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: kagent.dev/v1alpha2
kind: ModelConfig
metadata:
  name: aauth-model-config
  namespace: kagent
spec:
  apiKeySecret: kagent-openai
  apiKeySecretKey: OPENAI_API_KEY
  model: gpt-4o-mini
  provider: OpenAI
  openAI:
    # Routes the agent's outbound LLM traffic through agentgateway so
    # extauth (door 1) verifies the AAuth signature before it leaves
    # the host. host.docker.internal:3030 is how a pod inside the kind
    # cluster reaches agentgateway running on the host.
    baseUrl: http://host.docker.internal:3030/openai/v1
EOF

kubectl apply -f examples/aauth-test-agent.yaml
kubectl wait agent aauth-test-agent -n kagent --for=condition=Ready --timeout=180s
```

After it's Ready, the substrate actor's `KAGENT_CONFIG_JSON` carries the
agentgateway base_url and **every outbound LLM call from this agent gets
signed and ext_authz-verified**. The verification recipes below assume
this agent is up.

```bash
# Sanity check: the agent's actor template has the AAuth env vars
kubectl get actortemplate -n kagent aauth-test-agent \
    -o jsonpath='{range .spec.containers[*].env[*]}{.name}={.value}{"\n"}{end}' \
    | grep -E 'AAUTH|SUBSTRATE_ACTOR_ID'
# AAUTH_ENABLED=true
# AAUTH_AGENT_ID=aauth:aauth-test-agent@kagent.kagent.local
# AAUTH_CONTROLLER_URL=http://kagent-controller.kagent:8083
# KAGENT_SUBSTRATE_ACTOR_ID=kagent--aauth-test-agent  ← triggers substrate-mode mint path
```

> **Substrate snapshot caveat.** If you change `aauth-model-config`'s
> `baseUrl` *after* the agent's golden snapshot was taken, the running
> actor keeps its old `KAGENT_CONFIG_JSON` (env is baked into the
> snapshot). Force a fresh golden snapshot by recreating the agent —
> `kubectl delete agent aauth-test-agent -n kagent` then re-apply.

### Watching the live logs

The script writes one logfile per process under `/tmp/kagent-aauth-*.log`:


| Source                           | Path                                                              | Shows                                                                                                                                                                                                                                                          |
| -------------------------------- | ----------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **extauth** (best AAuth signal)  | `/tmp/kagent-aauth-extauth.log`                                   | `level=identified / result=allowed` JSON line per verified request with the agent's `delegate` identity. Failures dump the headers extauth received.                                                                                                           |
| **agentgateway**                 | `/tmp/kagent-aauth-agentgateway.log`                              | One summary line per request: method, host, path, status, upstream, duration — **plus the AAuth headers** (`signature-input`, `signature`, `signature-key`, `authorization`) thanks to the `frontendPolicies.accessLog` block in `demo/aauth/agw-config.yaml`. |
| **controller port-forward**      | `/tmp/kagent-aauth-controller-pf.log`                             | `kubectl port-forward` chatter only — useful to confirm the JWKS round-trips.                                                                                                                                                                                  |
| **controller verifier (door 2)** | `kubectl logs -n kagent deploy/kagent-controller | grep '"aauth'` | Per-request `aauth: verified caller=...` for in-cluster signed traffic to the controller.                                                                                                                                                                      |


```bash
# One-shot read
tail -50 /tmp/kagent-aauth-extauth.log
tail -50 /tmp/kagent-aauth-agentgateway.log

# Live "watch everything" while you click around in the UI
tail -F /tmp/kagent-aauth-*.log
```

A successful verification in extauth's log is the canonical "AAuth
worked" line:

```
{"time":"…","resource_id":"kagent-agents",
 "level":"identified","agent_server":"http://localhost:8083",
 "delegate":"aauth:aauth-test-agent@kagent.kagent.local",
 "result":"allowed","latency_ms":7}
```

A request line in the agentgateway log (tab-separated; the AAuth fields
land at the right end):

```
…  http.method=POST  http.path=/openai/v1/chat/completions  http.status=401
   aauth.signature_input="sig=(\"@method\" \"@authority\" \"@path\" \"signature-key\");created=…"
   aauth.signature="sig=:3UV8xiThC…HKZzCg:"
   aauth.signature_key="sig=jwt;jwt=\"eyJhbGciOiJFZERTQS…PlqAns89…YwpZCg\""
   aauth.authorization="Bearer sk-placeholder-not-real"
```

To pull the JWT out of the last line and decode its claims (so you can
read `sub`, `iss`, `exp`, and the agent's `cnf.jwk` public key):

```bash
JWT=$(tail -1 /tmp/kagent-aauth-agentgateway.log \
      | grep -oE 'jwt[\\]*"[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+' \
      | head -1 | sed 's/^jwt[\\]*"//')
echo "$JWT" | cut -d. -f2 \
    | python3 -c "import sys,base64,json; s=sys.stdin.read().strip(); \
                  print(json.dumps(json.loads(base64.urlsafe_b64decode(s + '=' * (-len(s) % 4))), indent=2))"
```

You'll see something like:

```json
{
  "cnf": {"jwk": {"crv": "Ed25519", "kty": "OKP", "x": "PnopTnzdHJweN4du…WkW0"}},
  "dwk": "aauth-agent.json",
  "exp": 1780424633,
  "iat": 1780338233,
  "iss": "http://localhost:8083",
  "sub": "aauth:aauth-test-agent@kagent.kagent.local"
}
```

> **Demo vs. production trade-off.** Two settings in this repo are
> deliberately tuned for local demo visibility — flip them back for
> production:
>
>
> | Setting                                                         | Demo (this repo)                                                                                                   | Production                                                                                                                                                                     |
> | --------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
> | `demo/aauth/aauth-config.yaml` → `strip_signature_headers`      | `false` (lets the AAuth headers reach the upstream so the agentgateway accessLog captures them after verification) | `true` (don't leak signing material to the upstream LLM)                                                                                                                       |
> | `demo/aauth/agw-config.yaml` → `frontendPolicies.accessLog.add` | logs raw `signature-key` (JWT) + `authorization`                                                                   | log only `signature_input` + boolean `has_`* fields, or omit entirely (the JWT in `signature-key` is a workload credential — anyone with it can sign as the agent until `exp`) |
>
>
> The local demo writes these logs to `/tmp/kagent-aauth-*.log` on your
> laptop, so the exposure is bounded to the running user. Don't enable
> the demo settings in a cluster whose logs ship to a shared aggregator
> (Loki, Splunk, Datadog, etc.).

### Verifying AAuth is actually working

`demo/aauth/README.md` has the full topology, but its verification commands
all target a deployment-mode agent pod (`kubectl exec deploy/aauth-test-agent`)
which doesn't exist in substrate mode — the agent runs inside a substrate
worker pod whose name keeps changing as actors get recycled.

For substrate-mode, three of the four verification doors documented in the
README work as-is once you adapt the commands:

**Door 1 (extauth, outbound).** Already shown above — an unsigned curl to
agentgateway returns `401 missing_signature`, proving the ext_authz
delegation is closed. **Caveat:** the agent's *real* LLM traffic only flows
through agentgateway if its `ModelConfig` points `openai.base_url` at
`http://host.docker.internal:3030/openai/v1`. The default
`default-model-config` in this install does NOT — it goes straight to
`api.openai.com`. To exercise door 1 with live LLM traffic, either:

```bash
# Patch the existing ModelConfig:
kubectl patch modelconfig -n kagent default-model-config --type merge \
    -p '{"spec":{"openAI":{"baseUrl":"http://host.docker.internal:3030/openai/v1"}}}'

# Or create a substrate-flavored ModelConfig + agent (see
# examples/aauth-test-agent.yaml's aauth-model-config for the shape).
```

**Door 2 (controller, in-cluster).** The kagent controller runs a Go
middleware verifier on every incoming HTTP request. The substrate-mode
`aauth-test-agent` calls back into the controller's built-in MCP server
(`/mcp`) for its tools (`list_agents`, `invoke_agent`), and those calls
are signed end-to-end. To prove it:

```bash
T0=$(date -u +%Y-%m-%dT%H:%M:%SZ)

# Send a prompt via the controller's A2A proxy. In substrate mode there is
# no per-agent Service to port-forward — A2A goes through the controller's
# /api/a2a/<ns>/<name>/ proxy on :8083.
curl -s -X POST http://localhost:8083/api/a2a/kagent/aauth-test-agent/ \
    -H "Content-Type: application/json" \
    -d '{
      "jsonrpc":"2.0","id":"f-1","method":"message/send",
      "params":{"message":{"kind":"message","messageId":"f-msg-1","role":"user",
        "parts":[{"kind":"text","text":"You MUST call the list_agents tool right now. Do not answer from memory. Just call list_agents and tell me the count."}]}}
    }'

# Pull the controller's verifier log for just this run, just /mcp paths.
kubectl logs -n kagent deploy/kagent-controller --since-time=$T0 \
    | grep '"aauth' | grep '"path":"/mcp"' \
    | jq -c '{ts, msg, caller, method, path}'
```

Expected: **5 lines**, every line `"msg":"aauth: verified"`, every line
with the agent's substrate-derived identity as `caller`:

```
{"ts":"…","msg":"aauth: verified","caller":"aauth:aauth-test-agent@kagent.kagent.local","method":"POST","path":"/mcp"}
{"ts":"…","msg":"aauth: verified","caller":"aauth:aauth-test-agent@kagent.kagent.local","method":"POST","path":"/mcp"}
{"ts":"…","msg":"aauth: verified","caller":"aauth:aauth-test-agent@kagent.kagent.local","method":"GET","path":"/mcp"}
{"ts":"…","msg":"aauth: verified","caller":"aauth:aauth-test-agent@kagent.kagent.local","method":"POST","path":"/mcp"}
{"ts":"…","msg":"aauth: verified","caller":"aauth:aauth-test-agent@kagent.kagent.local","method":"DELETE","path":"/mcp"}
```

That's one full MCP session over streamable-HTTP — POST initialize, GET
SSE stream, POST list_tools, POST call `list_agents`, DELETE close. The
substrate actor authenticated itself to the controller on every leg via
the substrate-mode mint path (source-IP + `Control.GetActor` attestation
instead of TokenReview — substrate sandboxes don't have an SA token to
present; see `go/core/internal/aauth/substrate_authenticator.go`).

**Door 3 (agent ASGI middleware, in-cluster).** The agent's Python ASGI
middleware verifies inbound signed traffic to its A2A endpoint. In
substrate mode the agent log is inside the substrate worker pod, which
gets recycled regularly — adapt the README's §E1/§E2 commands by first
locating the live worker:

```bash
# Which ateom pod is hosting this agent right now?
curl -s http://localhost:8083/api/substrate/actors \
    | jq '.data[] | select(.actorTemplateName=="aauth-test-agent")'
# Then `kubectl logs -n <ateomPodNamespace> <ateomPodName>` — the agent's
# stdout is multiplexed with the ateom sentry's. grep for "aauth:".
```

**Door 4 (substrate mint path, sanity).** Confirm the controller's
substrate-mode mint endpoint accepted the actor's request:

```bash
kubectl logs -n kagent deploy/kagent-controller \
    | grep "aauth: substrate mint" | tail -5
# Expect lines like:
#   "msg":"aauth: substrate mint allowed",
#   "substrate_actor_id":"kagent--aauth-test-agent",
#   "sub":"aauth:aauth-test-agent@kagent.kagent.local"
```

The deployment-mode AAuth README at `demo/aauth/README.md` has additional
verification recipes (negative tests, JWT inspection, persistent issuer
key check, TokenReview gate probes). Most of them target a `Deployment`
shape and need adaptation for substrate mode. The two substrate-friendly
ones to lift are the §C3 extauth log inspection and the §F4 negative
test (toggle `spec.declarative.aauth.enabled: false` and re-run the
door-2 recipe above — every `/mcp` line should flip to `"msg":"aauth: unverified","reason":"missing_signature"`).

## Inspection cheat sheet

```bash
# Kagent state
kubectl get agent k8s-agent-substrate -n kagent -o yaml | yq .status
kubectl get actortemplate -n kagent

# Substrate state
kubectl ate get actors
kubectl ate get workers

# Inside-the-sandbox view (bypass kagent mux)
kubectl port-forward -n ate-system svc/atenet-router 18000:80 &
curl -sS -H "Host: kagent--k8s-agent-substrate.actors.resources.substrate.ate.dev" \
    http://localhost:18000/.well-known/agent-card.json | jq

# Snapshot storage
kubectl exec -n ate-system deploy/rustfs -- du -sh /data/ate-snapshots

# Logs
kubectl logs -n kagent deploy/kagent-controller -f
kubectl logs -n kagent-substrate-poc poc-pool-deployment-XXXXX     # ateom + workload stdout
kubectl logs -n ate-system deploy/ate-api-server-deployment        # substrate Control API
kubectl logs -n ate-system ds/atelet                               # gVisor / runsc orchestration
```

## Troubleshooting: actor wedged in Resuming / "actor is resuming"

If the UI shows an AgentHarness as `Ready: False` with reason
`ActorResuming` and the actor sits in `STATUS_RESUMING` forever (visible
on the Substrate page and via `kubectl ate get actors`), one cause is
a **corrupt instance snapshot** left over from a partial suspend.

What happens: a Suspend RPC begins writing a new snapshot to rustfs, the
worker pod recycles mid-write (or the Checkpoint workflow's context is
canceled), and ate-api stores the new — incomplete — snapshot URI as the
actor's `last_snapshot`. The previous good snapshot has been overwritten.
Every Resume attempt now 404s on `pages_meta.img.zstd` because that file
was never finalized. Look for this signature in atelet logs:

```bash
kubectl logs -n ate-system ds/atelet | grep -E "Restore.*err" | tail -5
# ... NoSuchKey: The specified key does not exist
# ... while downloading pages_meta.img.zstd from GCS ...
```

The actor will never recover. The actor record in valkey holds the dead
snapshot pointer, suspend RPC rejects with `not suspended (status: STATUS_RESUMING)`, and delete RPC requires the actor be suspended.

**Recovery** — recreate the AgentHarness under a new `metadata.name`:

```bash
# Force-clear the finalizer so the stuck CR can leave.
kubectl delete agentharness -n <ns> <name> --wait=false
kubectl patch agentharness -n <ns> <name> --type=merge \
    -p '{"metadata":{"finalizers":[]}}'

# Re-apply with a different name (actor ID is derived from
# namespace+name, so a new name gets a fresh actor record in valkey
# instead of inheriting the bricked one).
sed 's/name: openclaw-test/name: openclaw-demo/' \
    demo/substrate-poc/06-openclaw-harness.yaml | kubectl apply -f -
```

The orphan record (`ahr-<ns>-<old-name>`) stays in valkey but does
nothing — no worker assignment, no traffic, no UI surface other than a
Suspending/Resuming row on the Substrate page.

Don't reuse the old name in the same install unless you've cleared
substrate state (delete the kind cluster, or wipe valkey + rustfs).

## Teardown

```bash
# Drop the Agent first so its finalizer can run while the controller
# is still alive (B1's sequenced actor delete).
kubectl delete -f demo/substrate-poc/05-builtin-k8s-agent.yaml --ignore-not-found

helm uninstall kagent      -n kagent
helm uninstall kagent-crds -n kagent
kubectl delete ns kagent kagent-substrate-poc

# Substrate + the kind cluster left in place. To remove everything:
kind delete cluster --name kagent-substrate
docker rm -f kind-registry   # if you don't need the shared registry
```

## What's deliberately out of scope today

- **The other built-in agents (istio-agent, kgateway-agent, helm-agent,
etc.).** Same shape as k8s-agent — just copy/paste the Agent
pattern, swap the system prompt, and reference the appropriate
RemoteMCPServer toolset. Their helm-shipped Agent CRs would work the
same way; we disabled them here only to keep the controller log clean
during validation.
- **Agents that use `spec.declarative.skills`.** Those still emit a
skills-init initContainer + emptyDir, which substrate's
`ActorTemplate.spec.containers` can't carry. See SUBSTRATE.md §13 for
three credible paths to close that gap.
- `**executeCodeBlocks: true`** — requires `privileged: true`; substrate
exposes no securityContext.
- **Production mTLS to `kagent-tool-server`.** Today's MCP path is
unauthenticated (tool-server uses its own SA RBAC). Real deployments
would either issue the agent a substrate `SessionIdentity` cert, plumb
a long-lived bearer via env, or pick a separate auth scheme. The PoC
punts on this.
- **Persistent sessions across suspend.** `--local` (forced by the
substrate-shim entrypoint) uses an in-memory session store. The kagent
controller-backed `KAgentSessionService` would persist, but the agent
process inside the sandbox can't currently route requests back to the
controller's Service.

## How this differs from the host-binary iteration mode

The previous `make demo-substrate-up` flow ran the controller as a host
binary against the cluster. That mode is still useful for fast inner-loop
iteration on controller code — see `Makefile`'s `demo-substrate-`* targets.
The helm path above is what to use for any "show somebody what kagent on
substrate looks like" purpose.