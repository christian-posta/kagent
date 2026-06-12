# Demo Guide — kagent on Agent Substrate (post #1981)

A 10-minute walkthrough of running both **AgentHarnesses** (openclaw-style developer surfaces) and **declarative agents** as snapshotted gVisor actors on Agent Substrate, using `kagent-dev/kagent` **main** (PR #1981 is merged as commit `32e72210`).

The story you're telling: *"Instead of one always-on Deployment per agent, every agent (or harness) is a gVisor sandbox that lives as a snapshot in object storage and only consumes a worker slot for the duration of an active request."*

---

## TL;DR — automated setup

```bash
# Platform only (no demo agents — create them yourself via the UI or kubectl):
bash demo/up.sh

# Or platform + both demo agents pre-applied:
bash demo/up.sh --full

# Either way:
kubectl port-forward -n kagent svc/kagent-ui 8001:8080 --context kind-kagent-substrate
open http://localhost:8001
# When done:
bash demo/down.sh
```

`up.sh` (default) refreshes both repos, creates the kind cluster, installs substrate, builds `ateom-gvisor`, mirrors the OpenClaw image to the local registry (works around the ghcr.io pull failure), and helm-installs kagent. It **does not** apply the demo agents — that's so you can show the audience creating them live, or so you can experiment with custom variants. The two reference manifests are checked in at `demo/agents/`:

```bash
# After up.sh, deploy whichever you want to show:
kubectl --context kind-kagent-substrate apply -f demo/agents/hello-substrate.yaml
kubectl --context kind-kagent-substrate apply -f demo/agents/peterj-claw.yaml
```

`bash demo/up.sh --full` applies both manifests and waits for them to be Ready as the last setup step — useful when you want a clean Ready-state to start a recording from.

Useful env-var overrides (all optional): `KIND_CLUSTER_NAME`, `KAGENT_DIR`, `SUBSTRATE_DIR`, `OPENAI_KEY_FILE`, `WORKER_REPLICAS`, `PULL_REPOS=0` (skip git fetch/reset).

The rest of this document walks through the manual steps and the talking points for the live demo — read it before presenting, even if you used `up.sh` to set everything up.

---

## What you'll show

| Scene | Visual | Substrate concept |
|---|---|---|
| 1 | kagent UI Agents list with mixed Deployment + substrate agents | `SandboxAgent` + `AgentHarness` are first-class alongside `Agent` |
| 2 | `/substrate` panel — KPI tiles, ActorTemplates, Actors, Workers | The new operator visibility surface in PR #1981 |
| 3 | Chat with `hello-substrate` from the UI | A2A → contextId → per-session gVisor actor → OpenAI |
| 4 | Refresh `/substrate` — actor count goes 4 → 5, new actor is Suspended | Session actors are snapshot-restored on demand, suspended between requests |
| 5 | Open `peterj-claw` AgentHarness → OpenClaw Control UI | Full developer harness running inside a substrate-hosted gVisor sandbox, surfaced through kagent's proxy |
| 6 | OpenClaw Overview "UPTIME 11h" while pod is 30min old | gVisor preserved the in-process clock from the golden snapshot |

---

## Prerequisites

- Docker Desktop running (macOS / Linux)
- `kind`, `kubectl`, `helm`, `go`, `ko` installed
- `OPENAI_API_KEY` (or use whichever provider your `default-model-config` is wired to)
- Two repos cloned side-by-side at standard Go paths:
  - `$GOPATH/src/github.com/kagent-dev/kagent` on the PR branch or main with #1981 merged
  - `$GOPATH/src/github.com/kagent-dev/substrate` (the kagent fork)

> **PR #1981 is merged to `kagent-dev/kagent` main as of `32e72210`.** The original `peterj/substrate-declar` branch was deleted upstream. The substrate fork at `kagent-dev/substrate` is also under active development — pull latest on both before each demo session to avoid schema drift.

---

## Suspend behavior — what's automatic, what isn't

Worth understanding *before* you start the demo, because it determines how you size the WorkerPool and which "wow" moments are real vs. aspirational.

| Resource | Running-actor model | Suspends automatically? |
|---|---|---|
| `SandboxAgent` (declarative on substrate) | One golden snapshot + one session actor per A2A `contextId` | **Yes — per request.** Session actors are created Suspended, resumed from the golden for the duration of the LLM call, then snapshotted back to Suspended. |
| `AgentHarness` (openclaw/nemoclaw/hermes on substrate) | One dedicated long-lived `ahr-<...>` actor per harness | **No.** The harness actor stays `Running` until you delete the `AgentHarness` CR. PR #1981 has no idle-suspender. |
| Golden actors (both kinds) | The snapshot template itself | Always `Suspended` — they only exist to be cloned. |

**Practical consequence**: a single-replica WorkerPool fills up quickly if you create any AgentHarnesses — each harness permanently camps one worker slot. Declarative agents are much lighter, because their session actors release the slot the moment the response is sent.

**If you want auto-suspend for harnesses**, it lives on the `ceposta-substrate` branch (`go/core/pkg/sandboxbackend/substrate/idle_suspender.go`) but is not in main. Without it, the only way to free a harness's worker slot is `kubectl delete agentharness <name>`.

---

## One-time setup

These steps build local images, spin up substrate, then helm-install kagent against it. Budget ~15 minutes the first time, ~3 minutes on subsequent re-deploys.

### 1. Refresh both repos

```bash
cd $GOPATH/src/github.com/kagent-dev/kagent
git fetch upstream main
git checkout main && git reset --hard upstream/main

cd $GOPATH/src/github.com/kagent-dev/substrate
git fetch origin main
git checkout main && git reset --hard origin/main       # force-updated branch
```

> Other in-flight kagent branches you might want to demo *next* (not used by this guide): `eitanya/sandbox-crd` (new `Sandbox` CRD with openshell backend), `eitanya/sandbox-hosts` (SRT support in Go ADK + network config), `peterj/sandboxascheckbox` (UI: sandbox as a checkbox on BYO/declarative agents).

### 2. Create a kind cluster + install substrate

```bash
cd $GOPATH/src/github.com/kagent-dev/substrate

# Spin up a new cluster (won't touch your other ones)
KIND_CLUSTER_NAME=kagent-substrate ./hack/create-kind-cluster.sh

# Install the substrate control plane (ate-api, ate-controller, atelet, atenet, valkey, rustfs)
# The install script's own wait will time out after a few minutes — that's fine,
# the pods continue converging in the background.
KIND_CLUSTER_NAME=kagent-substrate ./hack/install-ate-kind.sh --deploy-ate-system

# Build and push ateom-gvisor (the per-worker gVisor runtime image)
export KO_DOCKER_REPO=localhost:5001
export KO_DEFAULTPLATFORMS=linux/$(go env GOARCH)
./hack/run-tool.sh ko build -B ./cmd/ateom-gvisor
```

Wait for `ate-api-server` to be Ready (~3–5 min from cold; image extract is the slow part):

```bash
kubectl --context kind-kagent-substrate wait deploy/ate-api-server-deployment -n ate-system --for=condition=Available --timeout=300s
kubectl --context kind-kagent-substrate get pods -n ate-system
```

You should see `ate-api-server-deployment`, `ate-controller`, `atelet-*`, `atenet-router`, `valkey-cluster-{0..5}`, `rustfs` all Running.

### 3. Pre-pull the OpenClaw harness image to local registry (required for Demo 2)

The `ghcr.io/kagent-dev/nemoclaw/sandbox-base` image **cannot be pulled by atelet** today — its HTTP/2 transport gets `PROTOCOL_ERROR` from ghcr.io on every attempt (each fails after ~9 minutes). Mirror it to the local registry the kind cluster already trusts, and pin the AgentHarness to that ref. **Skip this step only if you're not running Demo 2.**

```bash
# Pull through Docker (which works fine), then re-tag and push to localhost:5001.
docker pull ghcr.io/kagent-dev/nemoclaw/sandbox-base@sha256:d52bee415dc4c0dba7164f9eabe727574c056d4f211781f20af249707883a3b4
docker tag  ghcr.io/kagent-dev/nemoclaw/sandbox-base@sha256:d52bee415dc4c0dba7164f9eabe727574c056d4f211781f20af249707883a3b4 \
            localhost:5001/nemoclaw/sandbox-base:demo
docker push localhost:5001/nemoclaw/sandbox-base:demo
```

**Note the new digest printed by `docker push`** — Docker re-encodes on push so the localhost reference will not be `sha256:d52b...`. You need the new digest for the `workloadImage` field in Demo 2. Capture it with:

```bash
DIGEST=$(docker inspect --format='{{index .RepoDigests 0}}' localhost:5001/nemoclaw/sandbox-base:demo)
echo "$DIGEST"   # localhost:5001/nemoclaw/sandbox-base@sha256:...
```

### 4. Build + install kagent

```bash
cd $GOPATH/src/github.com/kagent-dev/kagent

OPENAI_API_KEY="$(cat ~/path/to/openai-key)" \
KAGENT_DEFAULT_MODEL_PROVIDER=openAI \
KIND_CLUSTER_NAME=kagent-substrate \
make helm-install KAGENT_HELM_EXTRA_ARGS="\
  --set controller.substrate.enabled=true \
  --set controller.substrate.ateApiEndpoint=dns:///api.ate-system.svc:443 \
  --set controller.substrate.ateApiInsecure=true \
  --set substrateWorkerPool.create=true \
  --set substrateWorkerPool.replicas=2 \
  --set substrateWorkerPool.ateomImage=localhost:5001/ateom-gvisor:latest"
```

> **Why `replicas=2`?** See [Suspend behavior](#suspend-behavior--whats-automatic-what-isnt) above. The AgentHarness's `ahr-` actor never suspends in PR #1981, so it permanently camps one worker slot. Two replicas leaves room for declarative session actors to schedule. If you only demo declaratives (no harness), `replicas=1` is fine — session actors release their slot on every snapshot-back.

`make helm-install` will likely time out at 5 minutes — that's normal, see the gotchas section. The pods will be Running by the time you've finished reading the next section.

Wait for the kagent controller to be fully Ready (the controller races postgres on cold install and may restart 2–3 times before stabilizing):

```bash
kubectl --context kind-kagent-substrate wait deploy/kagent-controller -n kagent --for=condition=Available --timeout=300s
```

### 5. Port-forward the UI

```bash
kubectl port-forward -n kagent svc/kagent-ui 8001:8080 --context kind-kagent-substrate
```

Open <http://localhost:8001>. Skip the first-run wizard.

---

## Demo 1 — Declarative agent on Substrate (Go ADK)

**The pitch**: A `SandboxAgent` with `spec.platform: substrate` runs as a snapshotted gVisor actor. Each A2A request resumes a per-session actor from the golden snapshot, runs the LLM call, and suspends back to object storage when done. No long-lived process per agent.

### Apply the agent

The reference manifest is at [`demo/agents/hello-substrate.yaml`](agents/hello-substrate.yaml):

```bash
kubectl --context kind-kagent-substrate apply -f demo/agents/hello-substrate.yaml
```

Or paste it into the kagent UI's "Create Agent" form — the field names map 1:1 with the YAML.

The full manifest, for reference and adaptation:

```yaml
apiVersion: kagent.dev/v1alpha2
kind: SandboxAgent
metadata:
  name: hello-substrate
  namespace: kagent
spec:
  type: Declarative
  description: Tiny declarative agent running inside a substrate actor
  declarative:
    runtime: go
    modelConfig: default-model-config
    systemMessage: |
      You are a friendly assistant living inside an Agent Substrate sandbox.
      When asked who you are, say "I am hello-substrate, a Go ADK declarative
      agent running inside a gVisor actor."
  platform: substrate
  substrate:
    workerPoolRef:
      name: kagent-default
```

> **Schema note**: `spec.platform` and `spec.substrate` are top-level fields on `SandboxAgentSpec`. An earlier rebase had them nested under `spec.sandbox.platform` / `spec.sandbox.substrate` — that's gone. If you see `unknown field "spec.sandbox.platform"` errors, you're working from an older copy of this guide or an older API.

> **`runtime: go` is required.** Substrate-backed declaratives are forced onto the Go ADK runtime — the Python runtime is rejected by spec validation because Python+httpx state doesn't survive gVisor checkpoint reliably.

### Wait for Ready

```bash
kubectl wait sandboxagent/hello-substrate -n kagent --for=condition=Ready --timeout=120s
```

Takes ~60–90s to take the golden snapshot the first time.

### Show the UI

1. Open `http://localhost:8001/`, click the **hello-substrate** card from the agents grid.
2. Send a message: *"What are you, and where are you running? Answer in one sentence."*
3. Response: *"I am hello-substrate, a Go ADK declarative agent running inside a gVisor actor."*

### Show the substrate side

Open `http://localhost:8001/substrate`. Highlight:

- **`hello-substrate` ActorTemplate** is `Ready` with a stable `golden:` UUID.
- **Actors list** has 2 entries for hello-substrate:
  - `75d4bbdf-...` — the golden actor, Suspended (lives only as a snapshot)
  - `asr-kagent-hello-substrate-ctx-...` — the session actor created by your UI chat, also Suspended (already snapshotted back after responding)
- **Workers**: still 1/2 busy — declarative session actors don't pin worker slots between requests.

Send another message in the same chat → refresh `/substrate` → the same `asr-...` session actor's `version` increments. **That's the snapshot/restore cycle running invisibly under each chat turn.**

### Why this matters

- Hundreds of declarative agents → hundreds of golden snapshots in object storage, but capacity sized to *concurrent* sessions not *total* agents.
- Session isolation: each `contextId` gets its own gVisor sandbox restored from the shared golden, so two users hitting the same agent never share process state.
- Per-session memory cost is the snapshot size on rustfs/GCS, not RAM.

---

## Demo 2 — AgentHarness with OpenClaw on Substrate

**The pitch**: An `AgentHarness` is a *developer surface* — a long-lived agent runtime (OpenClaw / NemoClaw / Hermes) with its own UI, chat history, agent-building tools. Running it as a substrate actor means it inherits the same golden-snapshot deployment story, and kagent reverse-proxies its UI through the kagent dashboard so operators see it as just another resource.

### Apply the harness

The reference manifest is at [`demo/agents/peterj-claw.yaml`](agents/peterj-claw.yaml). Its `workloadImage` field is **pinned to the deterministic digest produced by `demo/up.sh`** when it mirrors the OpenClaw image — same digest every run, given the upstream pin.

```bash
kubectl --context kind-kagent-substrate apply -f demo/agents/peterj-claw.yaml
```

If `up.sh` warned that the live mirror digest didn't match the YAML (e.g. you re-pinned a new upstream version), use the live digest instead:

```bash
DIGEST=$(docker inspect localhost:5001/nemoclaw/sandbox-base:demo \
  --format '{{range .RepoDigests}}{{println .}}{{end}}' \
  | grep '^localhost:5001/' | head -1)
sed -E "s|^( *workloadImage: ).*$|\1${DIGEST}|" demo/agents/peterj-claw.yaml \
  | kubectl --context kind-kagent-substrate apply -f -
```

> **Critical**: `workloadImage` must point at the **localhost:5001** mirror that `up.sh` pushed, and it must be **set at creation time**. The kagent controller does not reconcile `workloadImage` changes after the ActorTemplate is created (the ActorTemplate is rendered once and treated as terminal). If you forget, the harness will hang forever in `ResumeGoldenActor` because atelet cannot pull `ghcr.io/kagent-dev/nemoclaw/sandbox-base` (see gotchas).

The full manifest, for reference and adaptation:

```yaml
apiVersion: kagent.dev/v1alpha2
kind: AgentHarness
metadata:
  name: peterj-claw
  namespace: kagent
spec:
  runtime: substrate
  backend: openclaw
  description: OpenClaw on Agent Substrate
  modelConfigRef: default-model-config
  substrate:
    workerPoolRef:
      name: kagent-default
    gatewayToken: test-token
    workloadImage: localhost:5001/nemoclaw/sandbox-base@sha256:d45725b8910de0a29ee03df2e72f7393bf3d1254a32b18dfbeb2d53739859407
```

### Wait for Ready

```bash
kubectl wait agentharness/peterj-claw -n kagent --for=condition=Ready --timeout=180s
```

Takes ~60–90s for the OpenClaw image to extract + golden-snapshot (pulling from `localhost:5001` over loopback is fast).

### Show the UI

1. From the kagent agents list (`http://localhost:8001/agents`), click the **peterj-claw** card.
2. The kagent UI proxies you straight to the OpenClaw Control connect screen at `http://localhost:8001/api/agentharnesses/kagent/peterj-claw/gateway/`.
3. The Gateway URL field is pre-filled correctly. Set:
   - **Gateway URL**: `http://localhost:8001/api/agentharnesses/kagent/peterj-claw/gateway/` (replace any `ws://` prefix)
   - **Gateway Token**: `test-token`
4. Click **Connect** → OpenClaw Control opens with full nav (Chat, Channels, Sessions, Agents, Skills, Dreaming, etc.)

### Talking points on the Overview screen

- **WebSocket URL** shows the kagent-proxied path — the harness sees itself behind kagent's proxy, no direct access to the substrate-managed pod required.
- **UPTIME** likely shows a value way longer than the cluster has existed. **That's gVisor preserving the in-process clock from when the golden snapshot was taken.** Restart the harness → still that uptime, plus elapsed wall-clock. Great visual for the "snapshot/restore is *real* state preservation" point.
- **Status: OK** + **Event Log** at bottom shows live RPCs flowing through the proxy chain (kagent UI → kagent controller → atenet-router → gVisor actor → openclaw).

### Why this matters

Harnesses are heavyweight, stateful processes — exactly the workloads where "spin one up per developer" gets expensive on traditional Deployments. With substrate:

- Idle harnesses *could* sit as snapshots, not running pods (once the idle-suspender lands).
- Bring-up time of a *new* developer's harness is ~snapshot-restore time (seconds), not container cold-start.
- Identity / RBAC is per-`AgentHarness` CR; substrate hides the placement details.
- `spec.substrate.workloadImage` lets operators pin a specific harness image at any registry that the cluster's worker pods can reach — useful for air-gapped environments and for the ghcr.io workaround above.

---

## Cleanup

```bash
# Tear down the demo objects but keep substrate + kagent running for the next demo
kubectl --context kind-kagent-substrate delete sandboxagent hello-substrate -n kagent
kubectl --context kind-kagent-substrate delete agentharness peterj-claw -n kagent

# Full teardown
kind delete cluster --name kagent-substrate
```

---

## Gotchas worth knowing before you present

These bit me during the first run-through. None are user-visible if you've already set up the cluster, but worth being aware of.

### AgentHarness hangs in `ResumeGoldenActor` — ghcr.io pulls fail with `PROTOCOL_ERROR`

Symptom: `kubectl describe agentharness peterj-claw` sits at `phase: ResumeGoldenActor` indefinitely (~10 min between retries). `kubectl logs -n ate-system ds/atelet` shows repeated `Cache miss` for `ghcr.io/kagent-dev/nemoclaw/sandbox-base@sha256:d52b...` followed by `pullCache.Fetch: while reading image: stream error: stream ID N; PROTOCOL_ERROR; received from peer` after each ~9-minute pull attempt.

Cause: atelet's HTTP/2 pull client gets RST_STREAM from ghcr.io for this specific image — possibly rate-limiting, possibly an HTTP/2 incompatibility. `docker pull` of the same digest works fine from the host, so this is not a host network problem.

Fix: pre-pull through Docker, push to `localhost:5001`, and set `spec.substrate.workloadImage` on the AgentHarness **at creation time**. This is documented in the [setup step 3](#3-pre-pull-the-openclaw-harness-image-to-local-registry-required-for-demo-2) and Demo 2 sections.

Patching `workloadImage` on an existing AgentHarness does **not** work — the kagent controller renders the ActorTemplate once at creation and never re-renders. You must delete and recreate the AgentHarness (see next gotcha for the finalizer trap).

### Stuck `AgentHarness` won't delete (finalizer hung)

Symptom: `kubectl delete agentharness peterj-claw` hangs. The AgentHarness has `metadata.finalizers: ["kagent.dev/agent-harness-backend-cleanup"]` but never makes progress.

Cause: the finalizer waits for the substrate-side actor to be cleaned up. If the actor never reached `Running` (e.g. because the image pull was failing), there's nothing to clean up but the controller's cleanup path still blocks on substrate calls that time out.

Fix:

```bash
kubectl patch agentharness peterj-claw -n kagent --type=json \
  -p='[{"op": "remove", "path": "/metadata/finalizers"}]'
# If the ActorTemplate is still hanging around, delete it too:
kubectl delete actortemplate peterj-claw -n kagent --wait=false
```

This is safe in a demo cluster. In production you'd want to investigate why the finalizer is hanging instead of bypassing it.

### "no free workers available" — declarative agent stuck in `ResumeGoldenActor`

Symptom: `kubectl describe sandboxagent` shows `ActorTemplate golden snapshot is not ready`; ate-controller logs repeat `rpc error: code = FailedPrecondition desc = no free workers available`.

Cause: WorkerPool replicas = 1 and a long-running `AgentHarness` actor is camping the only slot. PR #1981 ships without the idle-suspender that would auto-snapshot the harness when idle.

Fix: scale the pool: `kubectl scale workerpool kagent-default -n kagent --replicas=2`. Then **restart ate-controller** to reset its exponential backoff: `kubectl rollout restart deploy/ate-controller -n ate-system`. New reconcile attempts pick up the new capacity.

### `make helm-install` echoes the OpenAI key in the install log

Symptom: helm `--set providers.openAI.apiKey=sk-proj-...` is logged verbatim by Make.

Cause: Make prints commands by default. The key sits in `/private/tmp/.../*.output` on disk.

Mitigation: rotate the key after every demo, or use `helm install` directly with a values file and a Secret reference instead of `--set`.

### Substrate breaks ~24h after install (TLS certificate expired)

Symptom: After the cluster has been idle overnight, `ate-api-server` is `0/1 Running`, logs show:

```
tls: failed to verify certificate: x509: certificate has expired or is not yet valid:
current time 2026-XX-XXTHH:MM:SSZ is after 2026-XX-(X-1)TH'H':M'M':S'S'Z
```

Cause: Substrate uses Kubernetes 1.36's `PodCertificate` projected volume feature to issue short-lived (~24h) TLS certs for valkey, ate-api, atelet, etc. The `podcertificate-controller` *can* issue new ones — but the existing pods only refresh certs **at pod startup**. After 24h, the certs on disk inside long-running pods expire.

Fix: restart pods one at a time so they re-mount fresh certs. **Don't bulk-restart valkey** — see next item.

For ate-api-server and ate-controller (single replicas), a normal `kubectl rollout restart deploy/...` is fine.

### Don't `rollout restart statefulset/valkey-cluster`

Symptom: after `kubectl rollout restart statefulset/valkey-cluster -n ate-system`, every valkey pod logs `Cluster state changed: fail` with `NODE <id> () possibly failing` for all peers. ate-api-server still can't connect even with fresh certs (now i/o timeouts instead of TLS errors).

Cause: Redis Cluster's persisted topology (in the AOF) stores the node IDs of peers. When all 6 pods restart simultaneously, each pod comes up expecting to find its peers at known node IDs, but every peer has also just restarted and lost gossip state. Quorum can't re-form.

Fix (for a demo cluster): wipe and reinit.

```bash
kubectl delete pod -n ate-system -l app.kubernetes.io/name=valkey-cluster
kubectl delete pvc -n ate-system -l app.kubernetes.io/name=valkey-cluster
# Re-run the init job
kubectl get job -n ate-system valkey-cluster-init -o yaml \
  | kubectl replace --force -f -
```

Substrate will re-reconcile its state from K8s (ActorTemplate CRs, WorkerPools, the snapshots in rustfs).

**Prevention**: if you need to cycle valkey certs without nuking state, restart pods one at a time with a wait between each — `for i in 0 1 2 3 4 5; do kubectl delete pod valkey-cluster-$i -n ate-system; sleep 30; done` lets cluster gossip catch up between pods.

### CRD schema rejection (`unknown field "spec.sandbox.platform"`, etc.)

Symptom: `kubectl apply` on the SandboxAgent fails with `strict decoding error: unknown field "spec.sandbox.platform", unknown field "spec.sandbox.substrate"` (or similar for `workloadImage`, `substrate`, etc.).

Cause: The PR branch is force-rebased frequently. Field paths move. An earlier shape nested `platform` and `substrate` under `spec.sandbox`; the current shape has them at the top of `SandboxAgentSpec`. AgentHarness shape is more stable but has also gained fields (`workloadImage`).

Fix: check the current API on the branch you have:

```bash
kubectl --context kind-kagent-substrate explain sandboxagent.spec | head -40
kubectl --context kind-kagent-substrate explain agentharness.spec.substrate | head -40
```

The CRDs are the source of truth — if they don't match this guide, the guide is stale, not the cluster.

### Helm install times out at 5 minutes

Symptom: `Error: context deadline exceeded`, but `kubectl get pods -n kagent` shows everything Running.

Cause: bundled postgres + 12 default kagent agents + UI take just over 5 min on a cold cluster. The chart's `--wait` flag gives up.

Mitigation: ignore the failure, re-run `helm upgrade --install kagent helm/kagent ... --reuse-values` to mark the release deployed. Or set `--timeout 10m` in the Makefile target.

---

## Things this demo does **not** show

These are deferred-or-out-of-scope vs. the broader substrate roadmap. Mention or skip depending on audience:

- **AAuth** — per-actor JWT identity, RFC 9421 signing. Not in main; lives on `ceposta-aauth-substrate`.
- **Idle suspension for harnesses** — auto-snapshot an `AgentHarness` actor when idle for N seconds. Not in main; lives on `ceposta-substrate` (`go/core/pkg/sandboxbackend/substrate/idle_suspender.go`). See the [Suspend behavior table](#suspend-behavior--whats-automatic-what-isnt) for what *does* auto-suspend today (declarative session actors).
- **Multi-tenant identity** — substrate's gVisor sandbox is the actor-vs-actor isolation barrier. Per-actor *workload identity* (e.g. distinct K8s SA tokens) needs substrate-side work.
- **Cross-cluster placement** — single kind cluster here; substrate's design supports remote workers but kagent doesn't drive that today.
