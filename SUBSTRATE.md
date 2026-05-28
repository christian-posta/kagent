# SUBSTRATE.md — kagent on agent-substrate (PoC plan)

> Status: planning document. Authored 2026-05-22; **Phases 0–5 + follow-ups A2 and §20 implemented and validated 2026-05-22/23 (see §15–§20)**. §20 is the in-cluster helm-installed CRD-driven path with a built-in-style k8s-agent — closest to "production shape" today.
> Target: working demo on a `kind` cluster that already has `agent-substrate` installed.

---

## 1. Goal

Run kagent in a mode where Agent CRs **do not produce per-agent Deployments**. Instead, an `Agent` (or `SandboxAgent`) results in an `ate.dev/v1alpha1/ActorTemplate` registered against a shared `WorkerPool` of pre-warmed gVisor sandbox pods. The agent process runs inside a worker pod only when traffic arrives; gVisor checkpoint/restore handles memory + filesystem state across suspensions. Demo shows N agents multiplexed onto M < N worker pods, with cold suspend at rest and sub-second resume on demand.

This is a PoC. The plan trades polish for "the demo end-state works on the user's existing cluster." Production hardening is explicitly out of scope.

---

## 2. Demo end state (what we want to show)

> **Planning-time names.** The example shell output in this section uses `kagent-agents` as the worker namespace and shows pod counts like 2/2; the *shipped* demo uses `kagent-substrate-poc` with 1/1 controller pods. The shape of the demo (no per-agent pods, snapshot-at-rest, on-demand resume) is what's load-bearing, not the literal names.

### What runs where

- **Kagent platform** (controller, UI, Postgres) — normal K8s Deployments in the `kagent` namespace. **Not** sandboxed; substrate has nothing to do with these.
- **Substrate platform** (`ate-api-server`, `atelet` DaemonSet, `atenet-router`, `atenet-dns`, `valkey`, `rustfs`) — already installed in the `ate-system` namespace. **Not** changed by this work.
- **Worker pool** — a small fixed set of pre-warmed gVisor sandbox pods (e.g., 3) in `kagent-agents`. Created by a `WorkerPool` CR. These are the *only* agent-related pods at rest.
- **Agent workloads** — the actual Python ADK process for each kagent Agent. **Lives inside a substrate worker pod** when active, lives only as a snapshot in rustfs when idle. No per-agent Deployment, no per-agent Pod.

### What the first demo agent will be

The first demo deliberately uses a **simple, custom sample agent** — model + system prompt only, no skills, no code execution. Something like a `demo-assistant` that just chats.

> **2026-05-23 update.** §20 has since validated the in-cluster helm path with a `SandboxAgent` that mirrors the helm-shipped `k8s-agent` (`type: Declarative`, `RemoteMCPServer` tools). That agent doesn't set `spec.skills`, so the skills-init path never engages — it works on substrate as-is. The only built-in-style agents that still hit a real substrate constraint are ones that *do* set `spec.skills` (none of the helm-shipped agents do today). See §13 for the framing correction.

### Visible state of the cluster

```
$ kubectl get pods -n kagent
NAME                                READY   STATUS    AGE
kagent-controller-...               2/2     Running   2h
kagent-ui-...                       1/1     Running   2h
postgresql-0                        1/1     Running   2h

$ kubectl get pods -n kagent-agents
NAME                                READY   STATUS    AGE
worker-pool-kagent-0                2/2     Running   30m   # warm, no actor on it
worker-pool-kagent-1                2/2     Running   30m   # warm, no actor on it
worker-pool-kagent-2                2/2     Running   30m   # warm, no actor on it

$ kubectl get sandboxagents -n kagent-agents
NAME              READY   AGE
demo-assistant    True    5m   # status reflected from actor

$ kubectl ate get actors -n kagent-agents
ACTOR ID                       STATUS         WORKER
demo-assistant                 SUSPENDED      (none)
```

After a request:

```
$ curl -X POST -H "Host: demo-assistant.actors.resources.substrate.ate.dev" \
    http://localhost:8000/a2a/agents/kagent-agents/demo-assistant/message \
    -d '{"message":"hello"}'
(streaming A2A response)

$ kubectl ate get actors -n kagent-agents
ACTOR ID                       STATUS         WORKER
demo-assistant                 RUNNING        worker-pool-kagent-1
```

After idle timeout (or manual suspend):

```
$ kubectl ate suspend actor demo-assistant
$ kubectl ate get actors -n kagent-agents
ACTOR ID                       STATUS         WORKER
demo-assistant                 SUSPENDED      (none)
# next request resumes it from snapshot, possibly onto a different worker
```

**Bonus shot:** create 10 SandboxAgents, drive light traffic to all of them, show only 3 worker pods running. That's the multiplex.

---

## 3. How this differs from kagent today

> **Naming note.** Planning-time drafts called this `WorkloadMode=deployment` vs `WorkloadMode=sandbox`. What actually shipped is **CRD kind** — `kind: Agent` produces a Deployment via the default backend; `kind: SandboxAgent` routes through the configured sandbox backend (substrate, when enabled). `WorkloadMode` exists internally as a derived enum but isn't a user-set field.

| Concern | Today (`kind: Agent`) | After PoC (`kind: SandboxAgent` + substrate backend) |
|---|---|---|
| Per-agent K8s objects | `Deployment` + `Service` + `Secret` + `ServiceAccount` | `ActorTemplate` (Substrate CR) + `Secret` for config; no Deployment, no Service |
| Pods at rest | 1 pod per Agent, always running (~384Mi/100m baseline) | 0 pods per Agent; only N shared worker pods |
| Cold start | Pod scheduling + Python boot (~5–10s) | Snapshot restore (target 100ms; in practice TBD) |
| Request entry | A2A mux proxies to `<agent>.<ns>:8080` | A2A mux proxies through `atenet-router.ate-system.svc:80` with `Host: <actor-id>.actors.resources.substrate.ate.dev` |
| State on restart | DB-backed conversation events replay | Same DB-backed replay **plus** in-memory state preserved via gVisor checkpoint |
| Lifecycle | Pod runs forever | Actor suspends after idle; resumes on request |

---

## 4. What I've verified in both codebases

These are anchor points the plan depends on. If any turn out to be wrong, the plan needs revisiting.

### Kagent side

- The `Backend` interface in `go/core/pkg/sandboxbackend/backend.go` is the integration seam. Core methods: `BuildSandbox`, `GetOwnedResourceTypes`, `ComputeReady` (plus the optional `DeletingBackend.OnDelete` added in B1; see §18).
- Two existing backends ship today: `agentsxk8s` (emits `agents.x-k8s.io/v1alpha1/Sandbox`) at `go/core/pkg/sandboxbackend/agentsxk8s/agentsxk8s.go`, and `openshell` (gRPC AsyncBackend) at `go/core/pkg/sandboxbackend/openshell/`. The substrate backend lives at `go/core/pkg/sandboxbackend/substrate/`.
- Default backend is wired in `go/core/cmd/controller/main.go`'s `selectSandboxBackend` as `agentsxk8s.New()`; the substrate backend is selected when `--substrate-worker-pool-name` is set.
- `Agent` and `SandboxAgent` are parallel CRDs (`go/api/v1alpha2/agent_types.go`, `go/api/v1alpha2/sandboxagent_types.go`) with identical `AgentSpec`. The derived `WorkloadMode` enum lives in `go/api/v1alpha2/agentobject.go`. Same reconciler dispatches based on CRD kind.
- The `BuildInput.PodTemplate` arriving at `BuildSandbox` is fully constructed: image, env (incl. KAGENT_NAMESPACE/NAME/URL, OTEL_*, model creds via `SecretKeyRef`), volumes, mounts, init containers, readiness probe, security context. Construction lives in `go/core/internal/controller/translator/agent/manifest_builder.go`.
- Agent process required env: `KAGENT_NAME`, `KAGENT_NAMESPACE`, `KAGENT_URL` (default `http://kagent-controller.kagent:8083`). Without these the python `_config.py` raises on startup.
- A2A request flow today: external client → kagent controller `:8083` → `handlerMux.ServeHTTP` in `go/core/internal/a2a/a2a_handler_mux.go:88-116` → per-agent `A2AClient` → HTTP to `<agent>.<ns>:8080`.

### Substrate side (on the kind cluster)

- Control plane gRPC endpoint: `api.ate-system.svc.cluster.local:443` (TLS with pod certificates; kubectl-ate uses `InsecureSkipVerify` for the port-forward path).
- Routing entry point: `atenet-router.ate-system.svc:80`. ExtProc extracts actor ID from the `Host` header suffix `actors.resources.substrate.ate.dev` (`internal/resources/actor.go:25-27`), calls `Control.ResumeActor`, rewrites `:authority` to the worker pod IP, forwards.
- Snapshot backend on kind: rustfs (S3-compatible) at `rustfs.ate-system.svc:9000`, creds `rustfsadmin/rustfsadmin`. URI scheme `s3://...`.
- CRDs: `ate.dev/v1alpha1/WorkerPool` (fields: `replicas`, `ateomImage`) and `ate.dev/v1alpha1/ActorTemplate` (fields below in §6). Both namespaced.
- `ActorTemplate.spec.containers` is a **restricted** container shape: `name`, `image`, `command`, `ports`, `env`. **No volumeMounts, no volumes, no initContainers, no securityContext, no probes, no imagePullSecrets.** This is the biggest constraint of the integration. (See §7 for the workaround.)
- Worker selection: random shuffle of free workers in the referenced pool (`cmd/servers/ateapi/controlapi/workflow_resume.go:142-157`). No node affinity.
- Actors are **not** auto-created from an ActorTemplate. The user (or a controller) calls `Control.CreateActor` explicitly with the template ref + actor ID. The actor starts in `STATUS_SUSPENDED`; first HTTP request via atenet triggers resume.
- `Control.SuspendActor` is the call to evict back to snapshot. **Substrate does not auto-suspend on idle today**; that policy lives outside substrate.

---

## 5. Architecture after integration

```
External A2A client
        │
        ▼
┌────────────────────────┐
│  kagent-controller     │  (unchanged port 8083)
│  ┌──────────────────┐  │
│  │ a2a_handler_mux  │──┼──► if Agent.workloadMode == sandbox:
│  │  (modified)      │  │       proxy to atenet-router with Host header
│  └──────────────────┘  │
│  ┌──────────────────┐  │     else: proxy to <agent>.<ns>:8080 (today's path)
│  │ SubstrateBackend │──┼──► BuildSandbox → ActorTemplate CR
│  │  (NEW)           │  │     ComputeReady → status from Control.GetActor
│  └──────────────────┘  │
│  ┌──────────────────┐  │
│  │ actor lifecycle  │──┼──► gRPC: api.ate-system.svc:443
│  │  controller (NEW)│  │     CreateActor / SuspendActor (idle policy)
│  └──────────────────┘  │
└────────────────────────┘
        │ HTTP (with Host: <actor-id>.actors.resources.substrate.ate.dev)
        ▼
┌────────────────────────┐
│  atenet-router         │  (substrate, unchanged)
│  Envoy + ExtProc       │  parses actor-id, calls ResumeActor,
└────────────────────────┘  rewrites :authority to worker IP
        │ HTTP
        ▼
┌────────────────────────┐
│  worker pod (gVisor)   │  shared, pre-warmed; runs the agent process
│  ├─ ateom container    │  manages the sandbox + checkpoint/restore
│  └─ kagent-adk inside  │  the actual agent process, port 80 (or 8080)
│     gVisor sandbox     │
└────────────────────────┘
        │ outbound:
        ├──► PostgreSQL (kagent.svc:5432)         ⚠ verify in Phase 0
        ├──► kagent-controller :8083 (sessions)   ⚠ verify in Phase 0
        └──► LLM provider (api.openai.com, etc.)  ⚠ verify in Phase 0
```

The three ⚠s mark the connectivity that must be proved in Phase 0 before anything else makes sense. Substrate's counter demo has external network calls commented out — outbound from a gVisor sandbox to in-cluster Services or the internet is the single biggest unknown.

---

## 6. Reference: the CRs we'll be producing

> **Plan vs reality.** The CR shapes below describe the *original plan* — including a substrate-flavored kagent image (`app-substrate:dev`) and `KAGENT_CONFIG_JSON` env-var plumbing that was **not built**. What actually ships uses a Phase-0 FastAPI stand-in and no env-var config dance; see §17 and `demo/substrate-poc/01-substrate.yaml` + `03-agents.yaml` for the live YAML.

### WorkerPool (one shared pool for the demo)

```yaml
apiVersion: ate.dev/v1alpha1
kind: WorkerPool
metadata:
  name: kagent-pool
  namespace: kagent-agents
spec:
  replicas: 3
  ateomImage: <whatever the user's substrate install uses; reuse existing>
```

### ActorTemplate (one per kagent Agent)

```yaml
apiVersion: ate.dev/v1alpha1
kind: ActorTemplate
metadata:
  name: demo-assistant            # = Agent name
  namespace: kagent-agents        # = Agent namespace
  ownerReferences:
    - apiVersion: kagent.dev/v1alpha2
      kind: SandboxAgent
      name: demo-assistant
      controller: true
spec:
  pauseImage: registry.k8s.io/pause:3.10.2
  runsc:                          # reuse from counter demo
    amd64: { url: "...", sha256Hash: "..." }
    arm64: { url: "...", sha256Hash: "..." }
  workerPoolRef:
    namespace: kagent-agents
    name: kagent-pool
  snapshotsConfig:
    location: s3://kagent-snapshots/demo-assistant/
  containers:
  - name: kagent
    image: cr.kagent.dev/kagent-dev/kagent/app-substrate:dev   # NEW image (§7)
    command: ["/kagent-substrate-entrypoint.sh"]
    ports:
    - containerPort: 8080
      name: http
    env:
    - name: KAGENT_NAME
      value: demo-assistant
    - name: KAGENT_NAMESPACE
      value: kagent-agents
    - name: KAGENT_URL
      value: http://kagent-controller.kagent:8083
    - name: KAGENT_CONFIG_JSON           # NEW — config-via-env workaround
      value: |
        {"model": {...}, "instruction": "...", "stream": false}
    - name: KAGENT_AGENT_CARD_JSON       # NEW — agent card via env
      value: |
        {"name":"demo_assistant", ...}
    - name: OPENAI_API_KEY
      valueFrom:
        secretKeyRef:
          name: openai-creds
          key: api-key
```

Two things stand out and drive the bulk of the work:

1. **No `volumeMounts`** means today's `/config` Secret volume can't be carried across as-is. Config has to ride in env vars and be materialized to disk by a shim entrypoint (see §7).
2. **No `initContainers`** means skills-init can't run. For the PoC we deliberately restrict to agents with **no skills** and **no code execution** (`spec.skills` empty, `spec.declarative.executeCodeBlocks` false). Out of scope; tracked in §13.

---

## 7. The image shim — config via env vars

> **Plan, not shipped.** This shim image (`app-substrate:dev`) was the planned answer to "how do we ship the real kagent ADK image without volume mounts." Phase 0 went with a stand-in FastAPI agent instead (path B), so the shim image was **never built**. §13 lists three paths for closing this gap when integrating the real ADK; option 2 (fetch skills + config from the controller at startup) is essentially this shim plus one new controller endpoint.

Since ActorTemplate containers can't mount Secrets at `/config`, we build a small variant of the kagent ADK image whose entrypoint materializes config files from env vars before exec'ing kagent-adk.

**New file:** `python/Dockerfile.substrate` (or add to existing Dockerfile via a build arg):

```dockerfile
FROM cr.kagent.dev/kagent-dev/kagent/app:dev
COPY scripts/substrate-entrypoint.sh /kagent-substrate-entrypoint.sh
RUN chmod +x /kagent-substrate-entrypoint.sh
ENTRYPOINT ["/kagent-substrate-entrypoint.sh"]
```

**New file:** `python/scripts/substrate-entrypoint.sh`:

```bash
#!/bin/sh
set -e
mkdir -p /config
[ -n "$KAGENT_CONFIG_JSON" ]      && printf '%s' "$KAGENT_CONFIG_JSON" > /config/config.json
[ -n "$KAGENT_AGENT_CARD_JSON" ]  && printf '%s' "$KAGENT_AGENT_CARD_JSON" > /config/agent-card.json
[ -n "$KAGENT_SRT_SETTINGS_JSON" ] && printf '%s' "$KAGENT_SRT_SETTINGS_JSON" > /config/srt-settings.json
exec kagent-adk static --host 0.0.0.0 --port 8080 --filepath /config
```

Notes:
- We use the `static` subcommand. The existing image entrypoint uses `run`, but `run` does not accept `--filepath` (despite the PodTemplate passing it today). One of those is wrong; investigate in Phase 0 before relying on either.
- The image must be loadable into the kind cluster. `kind load docker-image cr.kagent.dev/kagent-dev/kagent/app-substrate:dev` after a local `docker build`.

**Configurable image registry:** the SubstrateBackend will accept a controller flag `--substrate-agent-image` so we don't hardcode.

---

## 8. The phased plan

Each phase ends in something demonstrable. If a phase doesn't work, we stop and reassess — we don't roll forward broken assumptions.

### Phase 0 — Hand-run the whole loop with zero kagent code changes

**Goal:** Prove the runtime pieces work before we write any backend code. Discover the unknowns now.

Steps:

1. **Build the substrate-flavored kagent image** (§7) and `kind load` it. Verify it can run with `docker run -e KAGENT_NAME=foo -e KAGENT_NAMESPACE=bar -e KAGENT_URL=http://...:8083 ...`.
2. **Verify the `static` vs `run` entrypoint question.** Open `python/packages/kagent-adk/src/kagent/adk/cli.py` and confirm which subcommand accepts `--filepath /config`. Update the shim accordingly. (If neither does what we expect, this is the first signal that the existing PodTemplate args drift from the code.)
3. **Hand-write a `ConfigMap` containing a minimal valid `config.json` + `agent-card.json`** for one agent, copy the contents into env vars, and **hand-write the ActorTemplate** (§6 shape). Apply it.
4. **Apply a `WorkerPool` with `replicas: 2`** in a fresh `kagent-agents` namespace. Wait for the golden snapshot to be taken (`kubectl wait --for=condition=Ready actortemplate/demo-assistant`). This step alone validates: image pull on worker, entrypoint runs, port 8080 listens, gVisor checkpoint succeeds. **If golden snapshot fails, stop here and dig in — every downstream phase depends on this working.**
5. **Manually `kubectl ate create actor demo-assistant --template kagent-agents/demo-assistant`.**
6. **Send a request:** `kubectl port-forward -n ate-system svc/atenet-router 8000:80`, then `curl -H "Host: demo-assistant.actors.resources.substrate.ate.dev" http://localhost:8000/.well-known/agent-card.json`. Should return the agent card from the resumed actor.
7. **THE CONNECTIVITY TEST.** Now drive a real A2A request that forces the agent to (a) call the kagent controller for session creation, (b) call the LLM provider. If any of those outbound calls fail from inside gVisor, we have a real problem — Phase 0 doesn't complete until we resolve it.
   - Likely failures and where to look:
     - DNS resolution inside the sandbox: gVisor uses host DNS by default. Check the kind CoreDNS reachability from inside the sandbox.
     - In-cluster Service IPs: requires the sandbox to be on the kind pod network. Confirm by `kubectl exec` into the ateom container, then attempting to reach kagent-controller and Postgres directly. If atenet's network setup precludes this, this is the blocker for the whole integration and we need to talk to the substrate team.
     - Egress to the internet (LLM APIs): a kind cluster usually has internet egress; if it doesn't, point the agent at a local mock LLM for the demo.
8. **Suspend, observe snapshot in rustfs, resume, verify the agent's in-memory state survives.** A simple test: ask the agent to remember a value, suspend, resume, ask it back. If conversation events are DB-backed (they are), this works trivially; the more interesting test is whether mid-stream in-memory state (e.g., a partially built tool-call) survives, but that's an enhancement not a gate.

**Exit criteria for Phase 0:** end-to-end request works, suspend/resume works, the connectivity matrix from §5 is fully proved. Document any workarounds (e.g., specific gVisor flags, network policy additions) — those go into the backend's emitted YAML in Phase 2.

**Estimated effort:** 2–4 days. Most of the time is in step 7 (networking debugging).

---

### Phase 1 — SubstrateBackend skeleton

**Goal:** A new `Backend` implementation that compiles, is wireable behind a flag, and produces a placeholder `ActorTemplate` object. Doesn't have to work end-to-end yet.

Files to add:

```
go/core/pkg/sandboxbackend/substrate/
├── substrate.go            # Backend implementation (BuildSandbox, ComputeReady, GetOwnedResourceTypes)
├── substrate_test.go       # Unit tests modeled on agentsxk8s_test.go
├── translate.go            # PodTemplate → ActorTemplate.Containers translation
├── translate_test.go
└── config.go               # Backend config struct (image name, worker pool ref, snapshots location, gRPC endpoint)
```

External dependency to add to `go.mod`:

```
require github.com/agent-substrate/substrate v0.0.0-<commit>
```

We import:
- `github.com/agent-substrate/substrate/api/v1alpha1` for `ActorTemplate`, `WorkerPool`, `Container`, `SnapshotsConfig`, `RunscConfig`.
- `github.com/agent-substrate/substrate/proto/ateapipb` for the `Control` gRPC client (Phase 4).

Code sketch — `substrate.go`:

```go
package substrate

import (
    "context"
    "fmt"

    substratev1 "github.com/agent-substrate/substrate/api/v1alpha1"
    "github.com/kagent-dev/kagent/go/api/v1alpha2"
    "github.com/kagent-dev/kagent/go/core/pkg/sandboxbackend"
    corev1 "k8s.io/api/core/v1"
    apierrors "k8s.io/apimachinery/pkg/api/errors"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/types"
    "sigs.k8s.io/controller-runtime/pkg/client"
)

type Config struct {
    AgentImage         string // e.g. cr.kagent.dev/kagent-dev/kagent/app-substrate:dev
    WorkerPoolName     string // shared default; can be per-agent overridden later
    WorkerPoolNS       string
    SnapshotsLocation  string // e.g. s3://kagent-snapshots/
    PauseImage         string // gke or k8s pause
    RunscAmd64URL      string
    RunscAmd64SHA      string
    RunscArm64URL      string
    RunscArm64SHA      string
}

type Backend struct {
    cfg Config
}

func New(cfg Config) *Backend { return &Backend{cfg: cfg} }

func (b *Backend) GetOwnedResourceTypes() []client.Object {
    return []client.Object{&substratev1.ActorTemplate{}}
}

func (b *Backend) BuildSandbox(ctx context.Context, in sandboxbackend.BuildInput) ([]client.Object, error) {
    if in.Agent == nil {
        return nil, fmt.Errorf("agent is required")
    }
    container, err := translateContainer(in.PodTemplate, b.cfg.AgentImage)
    if err != nil {
        return nil, fmt.Errorf("translate container: %w", err)
    }
    at := &substratev1.ActorTemplate{
        TypeMeta: metav1.TypeMeta{APIVersion: substratev1.GroupVersion.String(), Kind: "ActorTemplate"},
        ObjectMeta: metav1.ObjectMeta{
            Name:      pickName(in),
            Namespace: in.Agent.GetNamespace(),
            Labels:    mergeLabels(in),
        },
        Spec: substratev1.ActorTemplateSpec{
            PauseImage:      b.cfg.PauseImage,
            Containers:      []substratev1.Container{container},
            WorkerPoolRef:   corev1.ObjectReference{Namespace: b.cfg.WorkerPoolNS, Name: b.cfg.WorkerPoolName},
            SnapshotsConfig: substratev1.SnapshotsConfig{Location: b.cfg.SnapshotsLocation + in.Agent.GetName() + "/"},
            Runsc:           b.runscConfig(),
        },
    }
    return []client.Object{at}, nil
}

func (b *Backend) ComputeReady(ctx context.Context, cl client.Client, nn types.NamespacedName) (metav1.ConditionStatus, string, string) {
    at := &substratev1.ActorTemplate{}
    if err := cl.Get(ctx, nn, at); err != nil {
        if apierrors.IsNotFound(err) {
            return metav1.ConditionUnknown, "ActorTemplateNotFound", err.Error()
        }
        return metav1.ConditionUnknown, "ActorTemplateGetFailed", err.Error()
    }
    // Substrate exposes Phase + a Ready Condition. Phase==Ready means golden snapshot taken.
    if at.Status.Phase == substratev1.PhaseReady {
        return metav1.ConditionTrue, "ActorTemplateReady", "golden snapshot ready"
    }
    return metav1.ConditionFalse, "ActorTemplate"+string(at.Status.Phase), "template not yet ready"
}
```

Code sketch — `translate.go` (the heart of the translation):

```go
func translateContainer(pt corev1.PodTemplateSpec, agentImage string) (substratev1.Container, error) {
    if len(pt.Spec.Containers) == 0 {
        return substratev1.Container{}, fmt.Errorf("no containers in pod template")
    }
    src := pt.Spec.Containers[0] // the "kagent" container
    // Lift relevant env. Anything that referenced a Secret/Field works as-is — ActorTemplate accepts corev1.EnvVar.
    env := append([]corev1.EnvVar{}, src.Env...)
    // Inject config-via-env: read the project-built config bytes from somewhere passed in
    // through the BuildInput (we may need to extend BuildInput, or read them off labels/annotations).
    // For Phase 1, leave a TODO and hardcode placeholder.

    return substratev1.Container{
        Name:    "kagent",
        Image:   agentImage,
        Command: []string{"/kagent-substrate-entrypoint.sh"},
        Ports:   src.Ports,
        Env:     env,
    }, nil
}
```

Wiring — modify `go/core/cmd/controller/main.go`:

```go
import "github.com/kagent-dev/kagent/go/core/pkg/sandboxbackend/substrate"

// add flags:
//   --substrate-enabled
//   --substrate-agent-image
//   --substrate-worker-pool (NAMESPACE/NAME)
//   --substrate-snapshots (s3://...)
//   --substrate-control-endpoint (api.ate-system.svc.cluster.local:443)
//   --substrate-pause-image
//   --substrate-runsc-{amd64,arm64}-{url,sha}

// then:
var sb sandboxbackend.Backend = agentsxk8s.New()
if cfg.Substrate.Enabled {
    sb = substrate.New(substrate.Config{ ... })
}
return &app.ExtensionConfig{SandboxBackend: sb, ...}, nil
```

The biggest open question this phase exposes: **how do we get the rendered `config.json` and `agent-card.json` bytes into `BuildInput`?** Today the translator builds them and puts them into a `Secret` mounted at `/config`. For the substrate path we need those bytes inlined into env vars on the ActorTemplate container.

Options, in increasing order of invasiveness:

- **A. Read them back from the Secret object the translator already built.** The reconciler builds the Secret before `BuildSandbox`. The backend gets a `client.Client` only in `ComputeReady`, not in `BuildSandbox`, so this requires plumbing.
- **B. Extend `BuildInput` with the raw config bytes.** Add `ConfigJSON`, `AgentCardJSON`, `SRTSettingsJSON` fields. Update both existing backends to ignore them. Cleanest; needs touching the interface.
- **C. Pre-build the env vars inside the translator** so they're already in `PodTemplate.Spec.Containers[0].Env` by the time `BuildSandbox` sees them, conditional on `WorkloadMode==sandbox` and the backend being substrate. Bleeds backend-specific logic into the translator. Cheap; ugly.

Recommendation: **option B**. The interface change is one line; both existing backends just drop the new fields. The substrate backend uses them.

**Exit criteria for Phase 1:** `make build` passes, unit tests pass, controller starts with `--substrate-enabled=true` and a default config. Creating a `SandboxAgent` produces an `ActorTemplate` object visible via `kubectl get actortemplate -A`. No request routing yet.

**Estimated effort:** 2–3 days.

---

### Phase 2 — Faithful translation + golden snapshot working

**Goal:** The ActorTemplate produced by the backend actually achieves `Phase=Ready` (golden snapshot is taken), and `SandboxAgent.status` reflects it.

This is where Phase 0's lessons get encoded. Specific work:

1. **Wire `BuildInput.ConfigJSON` / `AgentCardJSON`** (the option B change from Phase 1) through to env vars on the ActorTemplate container.
2. **Strip env vars that don't make sense in sandbox mode**, e.g. anything that referenced a volume mount we can't satisfy (kagent skills, projected SA token at `/var/run/secrets/tokens`). For the PoC we declare these unsupported and emit a warning event on the SandboxAgent.
3. **Translate secret-backed env vars (`valueFrom.secretKeyRef`)** as-is. The substrate sandbox needs to be able to read them — this is the second connectivity question (along with §5 outbound). Validate by setting OPENAI_API_KEY from a Secret and confirming it reaches the agent process.
4. **Annotate the ActorTemplate with ownership** of the SandboxAgent so deletion cascades cleanly. Substrate's controller honors standard ownerReferences.
5. **Hook `ComputeReady`** to read both ActorTemplate.Status.Phase AND optionally Control.GetActor (if we have a gRPC client in this phase — see Phase 4). For now, Phase is enough.

**Exit criteria for Phase 2:** Create a `SandboxAgent` CR → `ActorTemplate` is created → golden snapshot is taken → `SandboxAgent.status.ready=True`. No actor running yet, no traffic yet.

**Estimated effort:** 3–5 days. Most goes into reconciling the env/secret/config dance.

---

### Phase 3 — A2A request routing through atenet

**Goal:** An A2A request to `kagent-controller:8083/a2a/agents/<ns>/<name>/...` for a sandboxed agent gets routed through `atenet-router`, triggers `ResumeActor`, and reaches the resumed agent in a worker pod.

Files to modify:

- `go/core/internal/a2a/a2a_handler_mux.go` — the routing decision
- Wherever the per-agent `A2AClient` URL is constructed (search for `:8080` in the a2a package)

The routing decision needs to know whether the target agent is sandboxed. Two implementations:

**Implementation 3a — the agent card knows the URL.** Today the controller builds an `AgentCard.URL` like `http://<agent>.<ns>:8080`. For sandboxed agents, build it as `http://atenet-router.ate-system.svc:80` with the actor-id used as the `Host` header during proxying. The mux's `SetAgentHandler` already takes an `A2AClient`; we construct a client whose underlying HTTP transport sets `Host: <actor-id>.actors.resources.substrate.ate.dev` on every outbound.

**Implementation 3b — a routing wrapper.** Keep the per-agent client URL as a synthetic name; wrap it in a custom RoundTripper that rewrites Host and Dial address. Less invasive on the trpc-a2a-go library but harder to reason about.

Recommendation: **3a**. Cleaner, fewer surprises.

Actor ID convention: `actor-id` must match the substrate regex `[a-z0-9]([-a-z0-9]*[a-z0-9])?` and be ≤63 chars. Use `<agent-name>` if it fits, else `<truncated-name>-<short-hash>`. The actor ID needs to be deterministic from the agent name+namespace.

DNS: requests from the kagent controller to `atenet-router.ate-system.svc:80` resolve via in-cluster DNS. The Host header carries the actor identity. No need to use the `actors.resources.substrate.ate.dev` mesh DNS (which atenet-dns serves to clients) — the Host header alone is enough because atenet's ExtProc reads from the header, not from any DNS lookup the kagent controller does.

**Exit criteria for Phase 3:** `curl` against the kagent controller's A2A endpoint for a sandboxed agent returns a response from the actually-running agent process inside the sandbox. This is also where we verify Phase 0's connectivity findings hold under real traffic (LLM call goes out, session is persisted to DB, response comes back).

**Estimated effort:** 3–5 days.

---

### Phase 4 — Actor lifecycle automation

**Goal:** The kagent controller calls `Control.CreateActor` when a `SandboxAgent` is reconciled, and calls `Control.SuspendActor` when traffic stops. No more manual `kubectl ate` for the demo to work.

Two new pieces:

**4a — gRPC client to substrate Control API.**

New package: `go/core/internal/substrate/client.go`. Wraps `ateapipb.ControlClient`. Handles connection setup, retry, TLS. For the PoC, dial `api.ate-system.svc.cluster.local:443` with `InsecureSkipVerify` (matching kubectl-ate's posture). Production would want proper pod-certificate-based mTLS — explicitly out of scope.

```go
type Client struct {
    ctl ateapipb.ControlClient
    conn *grpc.ClientConn
}

func Dial(endpoint string) (*Client, error) { /* TLS skip-verify dial */ }
func (c *Client) CreateActorIfMissing(ctx context.Context, actorID, tmplNS, tmplName string) error
func (c *Client) Suspend(ctx context.Context, actorID string) error
func (c *Client) Get(ctx context.Context, actorID string) (*ateapipb.Actor, error)
```

**4b — CreateActor in the SandboxAgent reconciler.**

After the ActorTemplate is observed `Ready`, the reconciler calls `CreateActorIfMissing`. Idempotent: if the actor already exists, no-op. This makes the SandboxAgent's lifecycle declarative.

Failure modes to handle gracefully:
- Substrate API unreachable → set `Ready=False, Reason=SubstrateAPIUnreachable` and requeue.
- Actor already exists in a bad state → log and continue; surface in status.

**4c — Idle suspend policy.**

For the PoC, a simple time-based policy: a goroutine in the controller polls (or watches via an informer) recent A2A activity per actor; if no requests in N minutes (configurable, default 10m), call `Control.SuspendActor`.

Where to get "last request time" from? The kagent controller already proxies every A2A request through the mux — wrap the mux handler to update an in-memory `map[actorID]time.Time` on each request. The suspend loop sweeps that map.

The in-memory map is fine for the demo. Production would want this surfaced in the database (controller restart loses idle timers; not the end of the world — they just reset, and the next request resumes anyway).

**Exit criteria for Phase 4:** Create a SandboxAgent → actor is auto-created in substrate (visible via `kubectl ate get actor`) → traffic flows → no traffic for 10m → actor is auto-suspended → next request auto-resumes. The "no agent pods at rest" property holds because the worker pool is shared and empty in `SUSPENDED` state.

**Estimated effort:** 3–5 days.

---

### Phase 5 — Demo polish

**Goal:** Make the demo reproducible and impressive.

- A `Makefile` target `demo-substrate` that:
  1. Verifies substrate is installed in the kind cluster.
  2. Loads the substrate-flavored agent image.
  3. Applies a `WorkerPool` with N replicas.
  4. Applies M `SandboxAgent` CRs (M > N) with simple LLM configs (or a mock LLM if internet egress is unreliable).
  5. Drives a synthetic traffic pattern that exercises resume + suspend cycles.
- A short demo script (talking points + commands) so the user can re-run it on a recording.
- Metrics: count of `SandboxAgent` CRs vs running worker pods vs `RUNNING` actors. A simple `kubectl ate list actors` snapshot is enough.
- A "scale knob" demo: bump `WorkerPool.replicas` from 3 → 1, observe that everything still works (modulo concurrency).

**Estimated effort:** 1–2 days.

---

## 9. Total estimate and ordering

| Phase | Effort | Demoable at the end? |
|---|---|---|
| 0. Hand-run | 2–4 days | Yes — manual but full loop works |
| 1. Backend skeleton | 2–3 days | No (compiles, but no end-to-end) |
| 2. Translation + golden snapshot | 3–5 days | Partial — agent template ready, no traffic |
| 3. A2A routing | 3–5 days | Yes — full end-to-end via kagent's API |
| 4. Lifecycle automation | 3–5 days | Yes — fully declarative |
| 5. Demo polish | 1–2 days | The user-facing demo |
| **Total** | **~3–4 weeks** | for one focused engineer |

Phase 0 is the single most important phase. If outbound networking from a gVisor sandbox to in-cluster Services doesn't work, the entire plan needs reshaping (e.g., proxy outbound through atenet, or accept that sandboxed agents lose their controller callback path). **Do not start Phase 1 until Phase 0's connectivity test passes.**

---

## 10. File-level change inventory

> **Plan, not shipped exactly.** This is the *anticipated* file list. The actually-shipped files differ: substrate code lives in `go/core/pkg/sandboxbackend/substrate/` (not `go/core/internal/substrate/`); the Python shim Dockerfile (`python/Dockerfile.substrate`) wasn't built; the demo lives in `demo/substrate-poc/` (not `manifests/substrate-demo/`). §16 and §17 list the actual shipped files.

New files:

```
SUBSTRATE.md                                            (this doc)
go/core/pkg/sandboxbackend/substrate/
  ├── substrate.go
  ├── substrate_test.go
  ├── translate.go
  ├── translate_test.go
  └── config.go
go/core/internal/substrate/
  ├── client.go                  # gRPC client to ateapi.Control
  └── lifecycle.go               # CreateActor + idle-suspend policy
python/Dockerfile.substrate      # variant image with shim entrypoint
python/scripts/substrate-entrypoint.sh
manifests/substrate-demo/
  ├── workerpool.yaml
  ├── sample-sandbox-agent.yaml
  └── kustomization.yaml
Makefile                         # add demo-substrate target
```

Modified files:

```
go/core/pkg/sandboxbackend/backend.go        # extend BuildInput with ConfigJSON/AgentCardJSON/SRTSettingsJSON
go/core/pkg/sandboxbackend/agentsxk8s/...    # ignore new BuildInput fields (no-op change)
go/core/pkg/sandboxbackend/openshell/...     # ignore new BuildInput fields
go/core/internal/controller/translator/agent/manifest_builder.go
                                             # populate new BuildInput fields when calling BuildSandbox
go/core/internal/a2a/a2a_handler_mux.go      # detect sandbox-mode agent → route via atenet
go/core/cmd/controller/main.go               # new flags + backend selection
go/core/pkg/app/app.go                       # surface flags in Config
go.mod / go.sum                              # add github.com/agent-substrate/substrate
```

---

## 11. Open questions & risks

Updated after Phase 0 validation (2026-05-22).

| Risk | Severity | Status |
|---|---|---|
| Outbound network from gVisor sandbox to in-cluster Services + external internet | ~~Highest~~ → **RESOLVED** | Proven in Phase 0. DNS (in-cluster + external) and HTTP/HTTPS (in-cluster + external) all work from inside the sandbox. The biggest unknown of the plan is gone. |
| `kagent-adk run` vs `static` mismatch | ~~Medium~~ → **RESOLVED** | Two image layers: `kagent-adk` base uses `run`, `kagent/app` overrides ENTRYPOINT to `static`. No real contradiction. Shim should exec `kagent-adk static --filepath /config`. |
| Substrate API stability (pre-alpha) | High | Pin to a commit in go.mod; accept churn; consider vendoring the proto if breakage is frequent. |
| ActorTemplate restricted container schema (no volumes, no init, no securityContext, no probes) | Medium | Phase 1 onward: feature-flag the sandbox path to "no skills, no code exec." File issue with substrate to grow the schema. |
| **Substrate ignores image `WORKDIR` (sets `Cwd:/`) and ignores image `CMD`/`ENTRYPOINT`** | Medium | **NEW (Phase 0).** `ActorTemplate.spec.containers[].command` is mandatory. Translator must emit absolute paths or `--app-dir` flags. Kagent image must be shimmed so command starts at the right cwd. |
| **`runsc checkpoint pause: exit 128` on certain workloads — gVisor checkpoint incompatibility, NOT a timer issue** | High | **NEW (Phase 0); ROOT CAUSE REVISED 2026-05-27 (§22).** Originally documented as a timer problem (substrate's hardcoded `TakeGoldenSnapshotAt = now + 20s`, `internal/controllers/actortemplate_controller.go:112`). That theory was tested and disproved: a 120s timer patch produced the same failure, and a totally different process (openclaw, provably booted by t=6s) hit the identical `runsc checkpoint pause: exit 128` failure at the snapshot point. The 20s timer is real and inflexible, but it is NOT the cause of the kagent-adk failure. **Actual cause**: gVisor systrap can't checkpoint certain process state (open TLS sockets, asyncio loop state, particular fd patterns). The kagent ADK + OpenAI SDK + OTEL combination hits this; openclaw hits it for different reasons; minimal FastAPI/uvicorn (demo-alpha) does not. **Workarounds**: (a) use a fast-booting *and* checkpoint-friendly BYO agent (FastAPI Phase-0 stand-in works — see DEMO.md), (b) for the ADK path, the `_substrate_checkpoint_friendly.install()` hook (§17) closes httpx pools and makes the first suspend cycle work, but post-resume state isn't stably checkpointable. Real fix requires gVisor/substrate-side work. |
| **Substrate's Redis client doesn't follow MOVED redirects in the "iterate all masters" path** | Medium | **NEW (Phase 0).** When the Valkey cluster topology shifts (e.g., after pod restart), `ListActors`/`ListWorkers` and any workflow step depending on them (`AssignWorker`) fail with `MOVED <slot> <ip>`. File upstream. Workaround for PoC: keep substrate uptime stable. |
| **Worker pod `eth0` becomes unrecoverable after failed sandbox setup** | Medium | **NEW (Phase 0).** When `runsc create`/`restore` fails partway, the pod's `eth0` is moved into a netns that gets cleaned up, but not restored to the pod. Subsequent attempts on that pod fail with `eth0: Link not found`. Workaround: delete the worker pod, WorkerPool respawns clean. Worth filing upstream. |
| **uvloop may not survive checkpoint/restore** | Low | **NEW (Phase 0).** Used `--loop asyncio` defensively. Not conclusively proved problematic. Kagent's ADK should run with the same flag under substrate until confirmed. |
| **GCS runsc download has tight timeout** | Low (setup-only) | **NEW (Phase 0).** atelet's per-attempt timeout is ~28s. On slow networks (saw ~800KB/s to GCS = ~100s for the 84MB binary), pre-stage at `/run/ateom-gvisor/static-files/runsc-<sha>` on each node. Demo setup script should do this. |
| Snapshot storage (rustfs) credentials are hardcoded | Low (PoC only) | Acceptable for demo; production needs proper secrets. |
| Activation latency: 100ms target is for runtime restore, not Python ADK request-readiness | Medium | Phase 0 saw ~3–5s end-to-end resume on Python FastAPI. The "100ms" is gVisor's own restore step; image pull, sandbox setup, and routing dominate. Measure with real ADK in Phase 2. |
| mTLS to Substrate Control API uses pod certs; our client uses InsecureSkipVerify | Low (PoC only) | Production needs `podcert.ate.dev` integration; out of scope for PoC. |
| Actor ID collisions / DNS-label constraints | Low | Actor IDs must match `[a-z0-9]([-a-z0-9]*[a-z0-9])?` and be ≤63 chars. Use `<ns>-<name>` and truncate. Document. |
| Substrate controller plane is single-replica | Low (PoC only) | State lives in Valkey; restart is fine. Still single point of failure for the demo. |

---

## 12. Testing strategy

- **Unit tests** for `translate.go`: feed a representative `corev1.PodTemplateSpec` (with env, secret refs, etc.), assert the produced `ActorTemplate.Container` has the right shape. Model on `agentsxk8s_test.go`.
- **Unit tests** for `BuildSandbox`: feed a `BuildInput`, assert the emitted ActorTemplate has correct ownership, name, snapshots path.
- **Unit tests** for `ComputeReady`: fake client returns ActorTemplate in various Phase states, assert the right ConditionStatus + reason.
- **Integration test** (kind, optional): a `make e2e-substrate` target that requires substrate to be installed. Apply a SandboxAgent, wait for Ready, send a request via the controller's mux, assert response. Skipped by default; runnable when substrate is available.
- **Manual demo run-through** documented in §5 of the demo script (Phase 5).

We deliberately do *not* try to write tests that mock substrate's Control API — the tests would just be testing our mock. Better to skip-by-default integration tests against the real thing.

---

## 13. Out of scope for the PoC

These are real and known, but the first demo doesn't need them:

- **Agents with `spec.skills` set.** When that field is present (it lives at the top of `AgentSpec`, not nested under `Declarative`) the translator emits a `skills-init` initContainer that populates an `emptyDir` volume at `/skills`. `ActorTemplate.spec.containers` does not expose `initContainers` or `volumes`, so that path can't run inside substrate as-is. Three credible follow-ups, in increasing order of how much they change:
  1. **Bake skills into a per-agent image.** Build-time skills are part of the image; substrate runs it unchanged. Loses the ability to update skills without a rebuild.
  2. **Fetch skills from the kagent controller at startup.** The substrate-entrypoint shim (§7) grows a step: before exec'ing `kagent-adk`, `curl` the controller for the skills tarball and unpack to `/skills`. The agent already talks to the controller for sessions, so the network path is proven. Cleanest; needs a small new HTTP endpoint on the controller and a small entrypoint change.
  3. **Get substrate to add `initContainers` + `volumes` to `ActorTemplate.spec.containers`.** Right approach long-term; depends on upstream substrate work.

  > **Important framing correction (2026-05-23).** Earlier drafts of this doc claimed the helm-shipped built-in agents (k8s-agent, istio-agent, prometheus-agent, helm-agent, etc.) all hit this gap. That was wrong. Those agents are `type: Declarative` and load their tools via a `RemoteMCPServer` reference to `kagent-tool-server`, not via `spec.skills`. They have no initContainer in the translated PodTemplate (verified: `buildSkillsRuntime` returns early when `spec.Skills == nil`). **§20 demonstrates the built-in k8s-agent running on substrate today** with no skills-delivery infrastructure changes. Only agents that *also* declare `spec.skills` (a user choice in the CR; none of the helm-shipped built-ins do) trip the gap above.
- **Agents with `executeCodeBlocks: true`.** Requires `privileged: true` on the container; `ActorTemplate` exposes no `securityContext`. Same shape of fix.
- **Per-agent worker pool isolation.** One shared pool for the PoC. Substrate has worker classes on its roadmap; revisit then.
- **Production-grade mTLS** from kagent → ate-api-server. PoC uses InsecureSkipVerify.
- **HA of the substrate control plane.** Substrate itself is single-replica today; we don't try to fix that.
- **Snapshot garbage collection.** Snapshots accumulate. Manual cleanup or wait for substrate roadmap.
- **Migrating existing `Agent` CRs to `SandboxAgent`.** PoC creates fresh SandboxAgents; conversion is a separate, easy task once the seam is proven.
- **Replacing the kagent-controller HTTP callback path** with direct substrate SessionIdentity-minted credentials. Interesting future work; PoC keeps the controller in the agent's request loop.

---

## 14. Definition of done for the PoC

Original DoD (planning-time) was written assuming the real kagent ADK image and an LLM-driven agent. What actually shipped uses a Phase-0 FastAPI stand-in so the LLM-and-DB criteria don't apply. Below is the *adjusted* DoD that matches what's been built and validated on the kind cluster — each item is reproducible via the `make demo-substrate-*` targets.

1. `make demo-substrate-up` succeeds end-to-end from a clean cluster (substrate pre-installed in `ate-system`, nothing in `kagent-substrate-poc`).
2. After `up`, a 2-replica WorkerPool and 4 BYO SandboxAgent CRs result in:
   - 4 SandboxAgents with `status.conditions[Ready]=True`.
   - 2 worker pods running; **0 agent pods** (`kubectl get pods -n kagent-substrate-poc` shows only `poc-pool-*`).
   - 4 `ActorTemplate`s in `Phase=Ready` (one per SandboxAgent, emitted by the substrate backend).
   - 4 substrate actors in `STATUS_SUSPENDED`.
3. `make demo-substrate-traffic` (POST through `kagent-controller:18093/api/a2a-sandboxes/<ns>/<name>/`) returns 200 for each agent, and the actor goes `SUSPENDED → RUNNING` on a worker pod. The `auto-suspend` cycle below proves the request actually reached the resumed agent process.
4. After `--substrate-idle-timeout` (30s in the demo) of no traffic, the IdleSuspender's sweep loop emits `"auto-suspended idle actor"` for each touched actor and they return to `STATUS_SUSPENDED`. Worker pods go FREE.
5. A second request to a suspended actor resumes the **same** Python process: `GET /info` via atenet shows the original `startup_uuid` unchanged and `request_count` continuing to increment across the suspend cycle.
6. `make demo-substrate-down` removes the controller, port-forwards, Postgres docker, and the demo namespace (cascading to SandboxAgents, ActorTemplates, and their snapshots). Substrate + kagent CRDs are left untouched; a follow-up `make demo-substrate-up` from this state succeeds.

What the demo does **not** cover (called out so a viewer doesn't expect it):

- A real LLM call from the agent process. The stand-in doesn't talk to OpenAI/etc. Phase 0 §15 proved external HTTPS egress from the gVisor sandbox works, so a real LLM is mechanical wiring.
- Session/memory records in the kagent database. The stand-in doesn't speak A2A; the kagent controller's session machinery isn't exercised. Wiring is in place (`postgres-database-url` flag, migrations run on startup) but unused by the dummy.
- Running the actual built-in kagent agents (k8s-agent, istio-agent). *Superseded by §20* — these are `type: Declarative` with `RemoteMCPServer` tools and no `spec.skills`, so substrate's restricted schema is not a blocker. §20 runs the built-in k8s-agent end-to-end via the helm path.

---

## 15. Phase 0 outcomes (validated 2026-05-22)

The exit criteria for Phase 0 were met using a **stand-in FastAPI agent** (`demo/substrate-poc/`) rather than the real kagent ADK image. The real ADK image was subsequently built and validated end-to-end — see §19 (A2) and §20.

**Pass/fail against the gating questions:**

| Question | Result | Evidence |
|---|---|---|
| Substrate can host a Python web server in a gVisor sandbox | ✅ | uvicorn+FastAPI started inside sandbox; `Application startup complete` + `Uvicorn running on http://0.0.0.0:80` in ateom logs |
| Golden snapshot captures Python in-memory state | ✅ | 11 MB `pages.img.zstd` (vs. 80 KB for an empty snapshot) |
| Request routing through atenet works | ✅ | `curl -H "Host: demo.actors.resources.substrate.ate.dev" http://atenet-router/...` returned 200 from resumed actor |
| In-memory state survives suspend→resume | ✅ | Same `startup_uuid` and continuing `request_count`/`uptime` across multiple suspend/resume cycles |
| Outbound from sandbox: in-cluster DNS | ✅ | `kubernetes.default.svc.cluster.local` → `10.96.0.1` |
| Outbound from sandbox: external DNS | ✅ | `example.com` resolved |
| Outbound from sandbox: in-cluster HTTP | ✅ | `atenet-router.ate-system.svc` reachable (returned 404 on synthetic Host — reachability proven) |
| Outbound from sandbox: external HTTPS | ✅ | `https://example.com` returned 200 |

**Substrate-side findings that surfaced during Phase 0** (all listed in §11): mandatory `containers[].command` with absolute paths because of `Cwd:/`; hardcoded 20s golden-snapshot timer with no readiness signal; Valkey MOVED-redirect bug breaks operations after topology shifts; worker `eth0` corrupted by failed sandbox setup; `--loop asyncio` recommended over uvloop; pre-stage runsc on slow networks.

**Net:** the biggest "this might be a dead end" risk (in-sandbox networking) is cleared. Plan §8's effort estimate (~3–4 weeks) holds. Phase 1 work proceeds.

---

## 16. Phases 1–4 outcomes (validated 2026-05-22)

After Phase 0 cleared the runtime unknowns, Phases 1–4 wired the substrate backend into the kagent controller, exercised the full SandboxAgent → ActorTemplate → actor lifecycle, and added the idle-suspend policy.

### Phase 1 — `SubstrateBackend` skeleton

New package `go/core/pkg/sandboxbackend/substrate/`:
- `substrate.go`: `Backend` implementing `BuildSandbox`, `ComputeReady`, `GetOwnedResourceTypes`.
- `translate.go`: `corev1.Container` → `substratev1.Container`, including the Phase 2 fix to concatenate `Command + Args` into a single `Command` (substrate's restricted schema has no `Args`).
- `config.go`: `Config` carrying worker pool ref, snapshots location, pause image, runsc URLs+SHAs, and (added in Phase 3) `Control` client + `RouterURL`.

Wired in `go/core/cmd/controller/main.go` behind `--substrate-worker-pool-name` (opt-in; no flag = default `agentsxk8s` backend). Eight `--substrate-*` flags surface in `--help` with defaults matched to Phase 0's verified working values. Unit tests cover field translation, `BuildSandbox` shape, `ComputeReady` phase mapping, and substrate scheme registration.

### Phase 2 — Faithful translation + golden snapshot

- Hermetic translator test (`go/core/internal/controller/translator/agent/substrate_backend_test.go`) proves the kagent translator + substrate backend emits a correctly-shaped ActorTemplate for a BYO SandboxAgent.
- Live cluster test: SandboxAgent CR → ActorTemplate created with ownerRef cascade, snapshots path, runsc URLs, command — reaches `Phase=Ready` and propagates to `SandboxAgent.status.conditions[Ready]=True, reason=WorkloadReady, message="golden snapshot ready"`.
- `AnnotationUnsupportedFields` (`kagent.dev/substrate-unsupported`) on the emitted ActorTemplate + structured log line on each reconcile, listing fields substrate can't carry (`volumes,volume-mounts,probes,resources` show up live because kagent's translator still emits them).
- Refactored `EnsureSandboxBackendAPIsRegistered(ctx, client, backend)` to probe `backend.GetOwnedResourceTypes()` instead of hardcoding `agents.x-k8s.io/v1alpha1/Sandbox`, so SandboxAgent reconcile no longer fails when the agent-sandbox CRD is absent.

### Phase 3 — A2A routing + actor lifecycle wiring

- `routing.go`: `ActorIDFor(ns, name)` (DNS-1123 with hash-truncate overflow path), `HostHeaderFor`, `HostRewritingTransport`, `ValidateActorID`. Unit-tested for boundary cases.
- `control_client.go`: thin wrapper over `ateapipb.ControlClient` — `CreateActorIfMissing` (idempotent: `AlreadyExists` → success), `GetActor`, `SuspendActor` (idempotent: `FailedPrecondition`/`NotFound` → success). Dial posture matches kubectl-ate (TLS with `InsecureSkipVerify`; production hardening explicitly deferred).
- `a2a_routing.go` + new generic `sandboxbackend.SandboxRoutingFunc` seam: when the active backend is substrate, the A2A registrar's per-agent client dials `--substrate-router-url` and uses a custom `http.Client` that rewrites the outgoing `Host` header to `<actor-id>.actors.resources.substrate.ate.dev`.
- `Backend.ComputeReady` now calls `ensureActor()` on `Phase=Ready` — automatic, idempotent `CreateActor` driven from the standard reconcile loop. The SandboxAgent's `Ready` message surfaces the actor identity and status (e.g. `"golden snapshot ready; actor \"…\" is STATUS_SUSPENDED"`).
- Live cluster test: A2A request via `kagent-controller:8093/api/a2a-sandboxes/<ns>/<name>/` → mux's per-agent `A2AClient` → atenet-router with rewritten Host → ResumeActor → resumed agent. Counter on the Python process incremented across multiple suspend/resume cycles, proving the same in-memory process was reached via the kagent mux.

### Phase 4 — Idle suspend policy

- `idle_suspender.go`: in-memory `map[actorID]time.Time`, periodic sweep, `Touch` callback wired into `HostRewritingTransport` so every outbound A2A request bumps the timer. Implements `manager.Runnable` so the sweep loop joins the controller's lifecycle. `NeedLeaderElection: true` to avoid double-suspend in HA. `IdleSuspenderConfig` validates inputs and returns `nil` when wiring is incomplete (graceful degradation).
- Two new flags: `--substrate-idle-timeout` (0 = disabled, > 0 = enabled) and `--substrate-idle-sweep-interval` (default 30s).
- `ExtensionConfig.ManagerRunnables []manager.Runnable` slot added in `app.go` so backend-specific background loops join the manager Add path.
- Unit tests cover `Touch`+`expired` semantics, sweep → SuspendActor → forget, clean context-cancel exit, retry/backoff on RPC failure, nil-receiver safety.
- Live cluster test (with `--substrate-idle-timeout=15s --substrate-idle-sweep-interval=5s`): SandboxAgent in `STATUS_SUSPENDED` → A2A request via mux → `STATUS_RUNNING` (resume + Touch) → 14s of silence → IdleSuspender sweep log line `"auto-suspended idle actor"` → `STATUS_SUSPENDED` again. A second request resumes the same Python process (UUID stable, counter increments from 3 → 4, uptime monotonic).

### What Phases 1–4 prove together

The demo end-state described in §2 is now operationally true:
- Worker pool of N pre-warmed sandbox pods.
- Zero per-agent Deployments / Services.
- SandboxAgent creation → ActorTemplate → substrate golden snapshot → automatic CreateActor → actor sits SUSPENDED at rest.
- A2A request via kagent controller → atenet routing → on-demand resume → request reaches the in-process Python agent inside gVisor.
- After `--substrate-idle-timeout` of no traffic → automatic SuspendActor → worker released → next request resumes from snapshot, in-memory state preserved.

Risks updated in §11: outbound networking, `kagent-adk` entrypoint mismatch, and ActorTemplate restricted schema are all resolved or worked around. The pre-alpha API stability risk remains and is the main reason the integration is pinned to a local `replace` directive.

---

## 17. Phase 5 outcomes — packaged demo (validated 2026-05-22)

Phase 5 wraps the live-proven Phase 4 plumbing into a one-command reproducible demo. **The full §2 demo end-state now happens via `make demo-substrate-up`.**

### What shipped

| File | Purpose |
|---|---|
| `Makefile` (new `##@ Substrate Demo` section, ~14 targets) | `demo-substrate-{preflight,runsc,image,crds,pg,pf,pool,controller,agents,up,status,traffic,logs,down}`. Idempotent; each step has a clear failure mode with a tail of the relevant log when it fails. |
| `demo/substrate-poc/03-agents.yaml` | Four BYO SandboxAgents (`demo-alpha`/`bravo`/`charlie`/`delta`) sharing a 2-replica WorkerPool — visibly exercises multiplexing (4-on-2). |
| `demo/substrate-poc/status.sh` | The "dashboard view": SandboxAgents + Ready, WorkerPool replicas, worker pods Running, substrate actor assignments, snapshot disk usage. Read-only, recording-friendly. |
| `demo/substrate-poc/DEMO.md` | Step-by-step walkthrough: each numbered beat has the command + expected output. Calls out "what you're about to see" upfront, prereqs, scope-out notes. |
| `demo/substrate-poc/01-substrate.yaml` (trimmed) | Now just the demo Namespace. The WorkerPool is auto-provisioned by the controller's `WorkerPoolEnsurer` from the `--substrate-worker-pool-{name,namespace,replicas,ateom-image}` flags; the per-agent ActorTemplate is emitted by `BuildSandbox`. Operators no longer need to apply any substrate CRs by hand. |

### What the demo proves on a fresh run

After `make demo-substrate-up`:
- 4 SandboxAgents with `status.conditions[Ready]=True, message="golden snapshot ready; actor … is STATUS_SUSPENDED"`.
- 2 worker pods, both FREE.
- 4 actors in substrate, **all STATUS_SUSPENDED at rest** — zero per-agent pods.
- ~50 MB per snapshot in rustfs (Python uvicorn + FastAPI captured memory).

After `make demo-substrate-traffic`:
- All four agents resume on demand; substrate juggles them onto the 2 workers; HTTP 200 returned through the kagent A2A mux.

After ~30 seconds idle:
- IdleSuspender sweep fires; actors all return to STATUS_SUSPENDED; workers FREE. Captured in the controller log as `auto-suspended idle actor` (one per sweep cycle).

After `make demo-substrate-down`:
- Clean tear-down: controller / port-forwards / Postgres docker stopped; demo namespace deleted (cascades agents → ActorTemplates → snapshots). Substrate + kagent CRDs left in place. Re-running `up` from this state succeeds.

### Fixes Phase 5's dry-run surfaced

1. **Port collisions on default ports.** Initial attempts used `5432` (Postgres), `8092` (probe), `8093` (HTTP) — these clash with other dev work and earlier-phase controllers that may still be running. Makefile now uses uncommon ports (`15432`, `18092`, `18093`) and the `demo-substrate-controller` target detects startup errors (`address already in use`, `FATAL`, `unable to (create manager|connect)`) instead of waiting for the success marker forever.
2. **`pg_isready` infinite wait.** Original target had `until docker exec … pg_isready; do sleep 1; done` with no timeout. If `docker run` silently failed (port busy, etc.), the wait spun forever. Now bounded at 30 iterations with explicit failure + last 10 lines of pg logs.
3. **Stale Phase-0 ActorTemplate in `01-substrate.yaml`.** It wasn't referenced by anything in Phase 5 but was confusing. Removed; Phase 0's snapshot of the file is in git history.

### Operational gotchas worth remembering

- In Phase 5 the controller runs **out-of-cluster** (binary on host) via `make demo-substrate-*` — fast for iteration. The in-cluster helm path is now wired and validated in §20; the `SandboxRoutingFunc` then dials `http://atenet-router.ate-system.svc` directly, no port-forward needed.
- On slow networks, `demo-substrate-runsc` pre-stages the gVisor binary (~84 MB) on the kind node. This is idempotent: a second `up` re-uses it.
- `make help` lists every target with its short description. `make demo-substrate-up` is the only command needed once the cluster + substrate are in place.

---

## 18. Code reading guide

If you're picking this up cold and want to read the implementation rather than the design doc, here's the recommended order:

| Step | File | What you'll see |
|---|---|---|
| 1 | `go/core/pkg/sandboxbackend/backend.go` | The base `Backend` interface every backend implements, plus the optional `DeletingBackend` interface for backends with external cleanup (substrate uses this for actor delete). |
| 2 | `go/core/pkg/sandboxbackend/sandbox_routing.go` | The `SandboxRoutingFunc` type — generic seam for backends that need custom A2A dialing. |
| 3 | `go/core/pkg/sandboxbackend/substrate/substrate.go` | `Backend` impl: `BuildSandbox`, `ComputeReady` (which now also calls `ensureActor` as a side effect), `GetOwnedResourceTypes`, and `OnDelete` (driven by the reconciler's finalizer; calls `DeleteActorSequenced`). |
| 4 | `go/core/pkg/sandboxbackend/substrate/translate.go` | `corev1.Container → substratev1.Container`, including the `Command+Args` concat (Phase 2 finding) and the unsupported-fields detector. |
| 5 | `go/core/pkg/sandboxbackend/substrate/routing.go` | `ActorIDFor` (DNS-1123 with hash-truncate overflow), `HostRewritingTransport` (the per-request `Touch` hook lives here). |
| 6 | `go/core/pkg/sandboxbackend/substrate/control_client.go` | gRPC wrapper over `ateapipb.ControlClient` with idempotent `CreateActorIfMissing`/`SuspendActor`/`DeleteActor`. |
| 7 | `go/core/pkg/sandboxbackend/substrate/delete_actor.go` | `DeleteActorSequenced`: suspend-then-delete state machine driven from the SandboxAgent finalizer. Treats `NotFound` as success at every step; bounded by `actorDeleteTimeout`. |
| 8 | `go/core/pkg/sandboxbackend/substrate/idle_suspender.go` | In-memory `actorID→lastTouch` map, periodic sweep, `manager.Runnable` lifecycle. |
| 9 | `go/core/pkg/sandboxbackend/substrate/workerpool_ensurer.go` | `manager.Runnable` that get-or-creates the shared `WorkerPool` at controller startup from the `--substrate-worker-pool-*` flags. Patches `ateomImage` on drift but never touches `replicas` after create (operators own scaling). |
| 10 | `go/core/pkg/sandboxbackend/substrate/a2a_routing.go` | `NewSandboxRoutingFunc` — glues the host-rewriting transport + IdleSuspender's `Touch` callback together. |

Integration points (where the substrate-specific code meets the rest of kagent):

| Where kagent calls into the backend | File:line |
|---|---|
| Translator dispatches `BuildSandbox` vs deployment | `go/core/internal/controller/translator/agent/manifest_builder.go` (search for `a.sandboxBackend.BuildSandbox`) |
| Controller-reference cascade onto backend-returned objects | `go/core/internal/controller/translator/agent/manifest_builder.go` (`setManifestOwnerReferences`) |
| Reconciler's per-backend API-availability probe | `go/core/pkg/sandboxbackend/apis_available.go:EnsureSandboxBackendAPIsRegistered` (called from `go/core/internal/controller/reconciler/reconciler.go` *and* `go/core/internal/httpserver/handlers/agents.go:validateAgentObject`; both must use the backend-aware probe, not the deprecated `EnsureAgentSandboxAPIsRegistered`) |
| A2A registrar's per-agent client | `go/core/internal/a2a/a2a_registrar.go:upsertAgentHandler` + `resolveDialing` |
| Backend + IdleSuspender + WorkerPoolEnsurer wiring at startup | `go/core/cmd/controller/main.go:selectSandboxBackend` + `buildSubstrateIdleSuspender` + `buildSubstrateWorkerPoolEnsurer` |
| Finalizer-driven external cleanup (`DeletingBackend.OnDelete`) | `go/core/internal/controller/reconciler/reconciler.go:ReconcileKagentSandboxAgent` + `finalizeSandboxAgent` (const `sandboxAgentFinalizer = "kagent.dev/substrate-actor"`) |
| Flags + scheme registration + manager runnables hook | `go/core/pkg/app/app.go` (`Substrate` struct in `Config`, `SetFlags`, `ManagerRunnables` slot) |

Tests worth reading as executable spec:

- `go/core/pkg/sandboxbackend/substrate/substrate_test.go` — `BuildSandbox` shape, ownerRef, snapshots path, unsupported-fields annotation.
- `go/core/pkg/sandboxbackend/substrate/routing_test.go` — actor-ID overflow path, host-rewriting transport.
- `go/core/pkg/sandboxbackend/substrate/idle_suspender_test.go` — touch semantics, sweep + forget, ctx cancel, RPC-failure backoff.
- `go/core/pkg/sandboxbackend/substrate/delete_actor_test.go` — suspend-then-delete state machine: already-suspended / running / suspending / NotFound / empty-ID / nil-receiver / ctx-cancel, plus `OnDelete` on the backend.
- `go/core/pkg/sandboxbackend/substrate/workerpool_ensurer_test.go` — get-or-create-or-patch semantics: nil-on-empty-inputs, default replicas, no-op when matching, patches `ateomImage` while preserving operator-set `replicas`.
- `go/core/internal/controller/translator/agent/substrate_backend_test.go` — hermetic kagent-translator + substrate-backend integration test, modeled on the existing `agentsxk8s` equivalent.

---

## 19. Follow-up A2 — real kagent ADK inside substrate (validated 2026-05-23)

A2 (§13's "skills + config over HTTP" / config-via-env shim follow-up) was the headline gap from the original PoC: Phases 0–5 used a Phase-0 FastAPI stand-in instead of a real kagent ADK image. A2 swapped in the real ADK image driven by an actual OpenAI ModelConfig and validated it end-to-end on the kind cluster.

### What shipped (in addition to Phases 1–5)

| Layer | Change | File(s) |
|---|---|---|
| kagent-adk CLI | Added `--local` flag to `static` subcommand (in-memory SessionService — agent doesn't need a route back to the controller). Added `--loop` and `--http` flags so the shim can force `asyncio`/`h11` to dodge gVisor checkpoint issues with uvloop/httptools. | `python/packages/kagent-adk/src/kagent/adk/cli.py` |
| kagent ADK image | Built from current source (modern `kagent-adk static` entrypoint) via existing `make build-kagent-adk` + `make build-app`. Local registry: `localhost:5001/kagent-dev/kagent/app:v0.3.4-…`. | `python/Dockerfile`, `python/Dockerfile.app` (unchanged) |
| Substrate-shim variant | New `python/Dockerfile.substrate` layering a config-from-env shim on top of the `app` image. Shim materializes `/tmp/config/config.json`, `agent-card.json` (and optional `srt-settings.json`) from `KAGENT_CONFIG_JSON` / `KAGENT_AGENT_CARD_JSON` / `KAGENT_SRT_SETTINGS_JSON` env vars, fixes the substrate-stripped PATH so `kagent-adk` resolves, then exec's `kagent-adk static --local --loop asyncio --http h11 --filepath /tmp/config`. | `python/Dockerfile.substrate`, `python/substrate-entrypoint.sh` |
| BuildInput plumbing | New `ConfigJSON` / `AgentCardJSON` / `SRTSettingsJSON` fields on `sandboxbackend.BuildInput`. Translator populates them from the bytes that otherwise go into the `/config` Secret. Other backends ignore. | `go/core/pkg/sandboxbackend/backend.go`, `go/core/internal/controller/translator/agent/manifest_builder.go` |
| Substrate backend image swap | `Config.AgentImage` (wired via `--substrate-agent-image` flag). When set, `BuildSandbox` overrides the translator's image with the shim image, rewrites the command to the shim entrypoint + listener args, and injects the three `KAGENT_*_JSON` env vars. | `go/core/pkg/sandboxbackend/substrate/config.go`, `substrate.go` |
| Env-var resolution | New `resolveEnvForSandbox` on the translator: walks the primary container's env in sandbox mode and replaces `ValueFrom.FieldRef` (`metadata.namespace`, `metadata.name`) + `ValueFrom.SecretKeyRef` with plain `Value` strings. Required because substrate's OCI bundle generator doesn't do kubelet-style env resolution — drops `ValueFrom` silently. | `go/core/internal/controller/translator/agent/manifest_builder.go:resolveEnvForSandbox` |
| Substrate upstream patch | `cmd/servers/atelet/oci.go:untar` now skips `tar.TypeChar`/`Block`/`Fifo` entries (`/dev/null`, `/dev/zero`, etc.) instead of erroring out. Required to unpack any image built `FROM cgr.dev/chainguard/wolfi-base` (or alpine) which carry char-device entries in the base layer. | `~/go/src/github.com/agent-substrate/substrate/cmd/servers/atelet/oci.go` |

### What works on the live cluster

| Step | Evidence |
|---|---|
| Substrate accepts the wolfi-based kagent ADK image | atelet `Run` RPC returns `err:null` after the untar patch |
| Shim materializes config from env | `substrate-entrypoint: materialized config in /tmp/config (config.json=234B, agent-card.json=374B)` |
| All `ValueFrom` env resolved before reaching the sandbox | ActorTemplate spec shows every env var has plain `value` — including `OPENAI_API_KEY` from the `kagent-openai` Secret and `KAGENT_NAMESPACE` from `metadata.namespace` |
| Real kagent ADK boots inside the gVisor sandbox | `INFO: Application startup complete. INFO: Uvicorn running on http://0.0.0.0:80` |
| Golden snapshot of the running kagent ADK | `Actor checkpointed` (after switching to `--loop asyncio --http h11`; uvloop caused `runsc checkpoint: exit 128`) |
| SandboxAgent Ready=True with real ADK | `golden snapshot ready; actor "kagent-substrate-poc--real-assistant" is STATUS_SUSPENDED` |
| A2A request via kagent mux → resume → real OpenAI call → response | `Q: "What is 2+2? Answer with just the number." → A: "4"` with `promptTokenCount: 200, candidatesTokenCount: 2` |
| IdleSuspender fires `SuspendActor` on schedule | Controller log: `SuspendActor failed; will retry next sweep` — the dispatch is correct |

### What's blocked → A2.7 partial fix

**A2.7 attempt (2026-05-23): `_substrate_checkpoint_friendly.install()` hook.**

Hypothesis from above was that gVisor can't serialize live keep-alive TLS sockets (the httpx.AsyncClient pool the OpenAI SDK holds to `api.openai.com`). Test: add a background asyncio task that walks `gc.get_objects()` 3 seconds after each request and calls `aclose()` on every live `httpx.AsyncClient` instance. Wired into the `kagent-adk static --local` path (i.e., always-on in sandbox mode). See `python/packages/kagent-adk/src/kagent/adk/_substrate_checkpoint_friendly.py`.

| Issue | Status after A2.7 |
|---|---|
| **First auto-suspend after a real LLM call.** Was failing with `runsc checkpoint: exit status 128`. | ✅ **FIXED.** First full eviction-policy cycle now works end-to-end on real kagent ADK + OpenAI: golden snapshot → A2A request → real `gpt-4o-mini` answer → 30s idle → IdleSuspender fires → `auto-suspended idle actor` log line → STATUS_SUSPENDED, workers FREE. Reproduced live. |
| **Resume from post-request snapshot, then second auto-suspend.** Second request after auto-suspend returns a real LLM answer ("substrate" with 3 candidate tokens) — so resume works fine. But the auto-suspend that *follows* that second request fails again with the same `exit status 128` chain. | ⚠️ **Still flaky.** The hook closes httpx clients, but something about the restored process state is no longer cleanly checkpointable. Possible causes: (a) the background asyncio task didn't survive gVisor restore intact and isn't firing post-resume; (b) the OpenAI SDK or its dependencies opened new sockets/state that the hook isn't covering; (c) restored asyncio event-loop state has fds gVisor can't re-serialize. We didn't isolate which. |

### Net for the PoC after A2.7

What the user originally asked for — *"kagent agents running in the substrate workers and being resumed suspended based on an eviction policy"* — is **achieved for the headline cycle on real kagent ADK + real OpenAI:**

1. SandboxAgent CR → translator + substrate backend emit ActorTemplate with the shim image + resolved env.
2. Substrate workers materialize the real kagent ADK process inside gVisor and golden-snapshot it.
3. Actor sits at rest as a snapshot; first A2A request resumes it and gets a real `gpt-4o-mini` response back through the kagent mux.
4. IdleSuspender fires the eviction-policy call at 30s — gVisor checkpoint succeeds (thanks to A2.7's httpx pool close) — actor returns to STATUS_SUSPENDED.
5. Next request resumes from the new snapshot and answers with another real LLM call.

The *post-resume-then-suspend-again* cycle isn't reliably checkpointable. For a PoC focused on demonstrating eviction policy, this is the asymptotic ceiling: one full evict-and-resume cycle works; sustained ping-pong over hours doesn't. Three honest paths from here:

1. **Make the close hook restore-resilient.** Add diagnostic logging to the close loop, verify the asyncio task survives gVisor restore, and if not, re-spawn it on the first post-restore request. The current code doesn't have shutdown-cancellation, so the task *should* persist; live debugging would confirm.
2. **Disable httpx keep-alive entirely.** Configure the OpenAI SDK / litellm to use `httpx.Limits(max_keepalive_connections=0)` so no TCP connection survives past a single LLM call. Eliminates the most likely culprit. Requires reaching into google-adk's OpenAI provider config.
3. **Wait for gVisor.** Substrate's own `claude-code-multiplex` demo apparently survives sustained suspend/resume of a real LLM-driven agent. Worth diffing their setup against this one — they likely either use a non-httpx LLM client (anthropic-sdk-go is Go-based, not Python) or have a specific configuration we're missing. The Anthropic agent runs in a separate process via subprocess so its httpx state never lives inside the sandboxed process.

### Files added/changed beyond §17

```
python/Dockerfile.substrate                                          NEW — shim image on top of kagent/app
python/substrate-entrypoint.sh                                       NEW — config-from-env shim
python/packages/kagent-adk/src/kagent/adk/cli.py                     MOD — added --local, --loop, --http to `static`
python/packages/kagent-adk/src/kagent/adk/_substrate_checkpoint_friendly.py
                                                                     NEW (A2.7) — httpx pool close on idle
go/core/pkg/sandboxbackend/backend.go                                MOD — BuildInput.ConfigJSON/AgentCardJSON/SRTSettingsJSON
go/core/pkg/sandboxbackend/substrate/config.go                       MOD — Config.AgentImage
go/core/pkg/sandboxbackend/substrate/substrate.go                    MOD — applyAgentImageOverride
go/core/internal/controller/translator/agent/manifest_builder.go     MOD — pass config bytes + resolveEnvForSandbox
go/core/pkg/app/app.go                                               MOD — --substrate-agent-image flag
go/core/cmd/controller/main.go                                       MOD — wire AgentImage through Config
demo/substrate-poc/04-real-agent.yaml                                NEW — ModelConfig + Declarative SandboxAgent

(external; outside kagent repo)
~/go/src/github.com/agent-substrate/substrate/cmd/servers/atelet/oci.go
                                                                     MOD — skip Char/Block/FIFO tar entries (file upstream)
```

### How to reproduce A2 on the cluster

```
# 1. Build images (kagent-adk → app → substrate variant)
make build-app   # builds kagent-adk + app, pushes to localhost:5001
cd python && docker buildx build --push --platform linux/amd64 \
    --build-arg KAGENT_ADK_VERSION=v0.3.4-662-g401a2461 \
    --build-arg DOCKER_REGISTRY=localhost:5001 \
    --build-arg DOCKER_REPO=kagent-dev/kagent \
    -t localhost:5001/kagent-dev/kagent/app-substrate:dev \
    -f Dockerfile.substrate .

# 2. Patch + redeploy substrate's atelet (the char-device tar fix)
# (Already done; re-applies are idempotent — see oci.go diff in §19.)
cd ~/go/src/github.com/agent-substrate/substrate && hack/install-ate-kind.sh --deploy-atelet

# 3. Provide the OpenAI API key (Secret in the demo namespace)
kubectl create secret generic kagent-openai -n kagent-substrate-poc \
    --from-literal=OPENAI_API_KEY="$(tr -d '\n' < ~/bin/openai-key)"

# 4. Start the controller with --substrate-agent-image set
make demo-substrate-up SUBSTRATE_DEMO_AGENT_IMAGE=localhost:5001/kagent-dev/kagent/app-substrate:dev
# (or run the controller manually with --substrate-agent-image=…)

# 5. Apply the real-ADK SandboxAgent
kubectl apply -f demo/substrate-poc/04-real-agent.yaml
kubectl wait sandboxagent real-assistant -n kagent-substrate-poc --for=condition=Ready --timeout=5m

# 6. Send an A2A request — real OpenAI response from inside the gVisor sandbox
curl -sS -X POST -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":"1","method":"message/send","params":{"message":{"role":"user","parts":[{"kind":"text","text":"What is 2+2?"}]}}}' \
    http://localhost:18093/api/a2a-sandboxes/kagent-substrate-poc/real-assistant/ | jq
```

**Important post-PoC step:** the `OPENAI_API_KEY` Value appears in substrate's atelet gRPC request logs because substrate logs the full container spec (including resolved env values). Rotate the key when you're done with the PoC.

---

## 20. In-cluster helm-installed demo of the built-in k8s-agent (validated 2026-05-23)

The previous milestones ran the controller as a **host binary** against
the kind cluster — fast for iteration, but not what an operator would do.
Today's run installs kagent through its helm chart inside the cluster
and proves the full CRD-driven path with one of the helm-shipped
built-in agents (k8s-agent), running inside substrate's gVisor sandbox.

### What shipped (in addition to Phases 1–5 + A2)

| File | Purpose |
|---|---|
| `helm/kagent/values.yaml` | New top-level `substrate:` block; values turn on the substrate backend, set the auto-provisioned WorkerPool's ateom image, control endpoint, idle timer. Default `enabled: false` — substrate flags are only emitted when explicitly turned on. |
| `helm/kagent/templates/controller-configmap.yaml` | Conditional block emits `SUBSTRATE_*` env vars when `substrate.enabled: true`. `app.LoadFromEnv` auto-maps each onto its `--substrate-*` flag, so the controller binary needs no in-cluster-specific wiring. |
| `helm/kagent/templates/rbac/writer-role.yaml`, `getter-role.yaml` | Conditional `ate.dev` rules grant the controller create/update/patch/delete + get/list/watch on `workerpools` and `actortemplates`, gated on `substrate.enabled`. |
| `go/Dockerfile.substrate-demo` | Demo-only thin distroless wrapper around a host-built controller binary. Required because `go/go.mod` carries a local `replace` directive pointing at the agent-substrate source — that path doesn't exist inside Docker build contexts, so the default `go/Dockerfile` can't `go mod download`. |
| `demo/substrate-poc/05-builtin-k8s-agent.yaml` | `SandboxAgent` clone of the helm-shipped `k8s-agent` (`type: Declarative`, references `default-model-config`, `kagent-tool-server` `RemoteMCPServer`, `kagent-builtin-prompts` ConfigMap). |
| `demo/substrate-poc/kagent-substrate-values.yaml` | Helm values for the in-cluster install. Disables every built-in `*-agent` chart, leaves `kagent-tools` on. |
| `demo/substrate-poc/DEMO.md` | Rewritten end-to-end manual walkthrough. The host-binary `make demo-substrate-*` flow is downgraded to "iteration mode"; the helm path is now primary. |

### Auth-gap finding: the projected SA token isn't actually needed

`SUBSTRATE.md §13` previously claimed the helm-shipped k8s-agent and
friends couldn't run on substrate because they need the projected
ServiceAccount token at `/var/run/secrets/tokens/kagent-token` — and
substrate's `ActorTemplate.spec.containers` has no volumes. Two findings
that disprove that:

1. **`kagent-tool-server` has no MCP-layer auth.** The kagent-tools chart
   exposes `tools.k8s.tokenPassthrough` (default `false`), which only
   controls whether an incoming bearer token is *passed through to kubectl*
   for user impersonation. The MCP endpoint itself accepts unauthenticated
   requests; tool calls run under the tool-server's own ServiceAccount and
   its `ClusterRole`.
2. **`kagent-adk` skips the token path under `--local`.** The substrate
   shim entrypoint (`python/substrate-entrypoint.sh`) forces `kagent-adk
   static --local`, which never instantiates `KAgentTokenService`. So the
   missing `/var/run/secrets/tokens/kagent-token` file is never read; no
   `Authorization` header is attempted on outbound calls.

Net: built-in Declarative agents that consume tools via `RemoteMCPServer`
(k8s-agent, istio-agent, helm-agent, prometheus-agent, etc.) work on
substrate today with no controller / shim / chart changes. The PoC path
unauthenticated to tool-server is acceptable because tool-server's own
RBAC enforces what the agent can do. A production deployment would want
to close this loop — see §13.

### What this run proved end-to-end

`SandboxAgent k8s-agent-substrate` in `kagent` namespace (matching the
helm-shipped k8s-agent's spec field-for-field, except `kind: SandboxAgent`
instead of `kind: Agent`):

1. **In-cluster controller** (helm Deployment, `controller:substrate-demo`
   image) read its `SUBSTRATE_*` env vars from the configmap, selected the
   substrate backend, and auto-provisioned the shared `poc-pool` WorkerPool
   in `kagent-substrate-poc` (B2's `WorkerPoolEnsurer`).
2. **No agent Deployment** ever appeared. The controller's substrate
   backend emitted an `ActorTemplate` in the `kagent` namespace; substrate
   captured the golden snapshot; `status.conditions[Ready]=True, message="golden snapshot ready; actor … is STATUS_SUSPENDED"`.
3. **A2A request through the kagent mux** (`kubectl port-forward
   svc/kagent-controller 18093:8083`):
   `POST /api/a2a-sandboxes/kagent/k8s-agent-substrate/` with a real
   prompt ("How many namespaces exist? Just give me the count and list
   them.") → host-rewriting transport dialed atenet → substrate
   `ResumeActor` → `runsc restore` → in-sandbox kagent ADK received the
   message.
4. **Real LLM call**: kagent ADK called `gpt-4o-mini` over the public
   internet from inside the gVisor sandbox. Prompt token count
   `~1900`, completion `28` tokens (which were the JSON tool-call args).
5. **MCP tool call back into the cluster**: ADK dialed
   `http://kagent-tools.kagent:8084/mcp` (substrate-proven in-cluster
   HTTP egress from Phase 0). No `Authorization` header (per the auth
   finding above). `kagent-tool-server` ran `k8s_get_resources` under
   its own SA RBAC and returned the real `kubectl get ns -o wide` output.
6. **Final answer composition**: tool result fed back to the LLM
   (prompt `2365`, completion `148` tokens); rendered as a markdown table
   of the actual 10 namespaces (`ate-system`, `default`, `kagent`,
   `kagent-substrate-poc`, `kube-node-lease`, …).
7. **Auto-suspend**: between the two requests, the controller's
   IdleSuspender (`--substrate-idle-timeout=30s`) fired
   `"auto-suspended idle actor" actor=kagent--k8s-agent-substrate`; the
   actor went back to `STATUS_SUSPENDED` and both workers became `FREE`.
   The next request resumed from snapshot and answered correctly.

### How to reproduce on a fresh kind cluster

See `demo/substrate-poc/DEMO.md`. The full sequence from zero, with
substrate's `install-ate-kind.sh` doing the substrate-side install:

```bash
# 1. New kind cluster (uses substrate's required ClusterTrustBundle / podcert feature gates)
KIND_CLUSTER_NAME=kagent-substrate \
    ~/go/src/github.com/agent-substrate/substrate/hack/create-kind-cluster.sh

# 2. Install substrate (~5 min)
~/go/src/github.com/agent-substrate/substrate/hack/install-ate-kind.sh --deploy-ate-system

# 3. Build + push the controller image (host build because of the go.mod replace)
CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOARCH) \
    go -C go build -o /tmp/binctx ./core/cmd/controller
cp go/Dockerfile.substrate-demo /tmp/
docker buildx build --push --platform linux/$(go env GOARCH) \
    --build-arg BINARY=binctx \
    -t localhost:5001/kagent-dev/kagent/controller:substrate-demo \
    -f /tmp/Dockerfile.substrate-demo /tmp

# 4. Build + push the substrate-shim kagent app image
VERSION=substrate-demo make build-substrate-app

# 5. Install kagent via helm
kubectl create ns kagent-substrate-poc --dry-run=client -o yaml | kubectl apply -f -
helm install kagent-crds helm/kagent-crds -n kagent --create-namespace
helm install kagent helm/kagent -n kagent \
    -f demo/substrate-poc/kagent-substrate-values.yaml \
    --set providers.openAI.apiKey="$(tr -d '\n' < ~/bin/openai-key)"

# 6. Apply the built-in-style SandboxAgent
kubectl apply -f demo/substrate-poc/05-builtin-k8s-agent.yaml
kubectl wait sandboxagent k8s-agent-substrate -n kagent \
    --for=condition=Ready --timeout=300s

# 7. Drive a real prompt through the kagent A2A mux
kubectl port-forward -n kagent svc/kagent-controller 18093:8083 &
curl -sS -X POST -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":"1","method":"message/send","params":{"message":{"role":"user","messageId":"m1","parts":[{"kind":"text","text":"How many namespaces exist? Just the count and list."}]}}}' \
    http://localhost:18093/api/a2a-sandboxes/kagent/k8s-agent-substrate/
```

### Net for the PoC after §20

Every concrete demand from the original "kagent on substrate" goal has
been demonstrated against the kind cluster:

- **Built-in agent CRD (k8s-agent), not BYO.** ✓ Verified end-to-end with
  a SandboxAgent that field-for-field mirrors the helm-shipped
  `k8s-agent`.
- **No per-agent Deployments.** ✓ Only `poc-pool` workers (shared) exist
  at rest; the actor is a snapshot in rustfs.
- **Real LLM call.** ✓ gpt-4o-mini, prompt+completion accounted for in
  metadata.
- **Real Kubernetes work.** ✓ MCP tool call → `kubectl get ns -o wide` →
  data back to LLM → final markdown answer.
- **In-cluster controller via helm.** ✓ Deployment, not host binary.
  Substrate flags flow through helm values → configmap → env vars →
  `LoadFromEnv` → controller config.
- **Suspend / resume preserves the Python process.** ✓ IdleSuspender fired
  between requests; next request resumed from the same snapshot and
  answered correctly.

Remaining gaps documented in §13 (no skills-using agents, no
`executeCodeBlocks: true`, no production tool-server auth, no
controller-backed sessions across suspend) are *enhancements* on top of a
demonstrably working baseline.

### UI-driven create flow (validated 2026-05-23)

The kagent UI uses the same controller HTTP API the `curl` walkthrough
exercises. Verified end-to-end by enabling the UI in helm
(`ui.replicas: 1`, image `localhost:5001/kagent-dev/kagent/ui:substrate-demo`)
and POST'ing a UI-shaped payload directly to `/api/sandboxagents`:

```json
{
  "apiVersion": "kagent.dev/v1alpha2",
  "kind": "SandboxAgent",
  "metadata": {"name": "ui-created-agent", "namespace": "kagent"},
  "spec": {
    "type": "Declarative",
    "declarative": {
      "runtime": "python",
      "systemMessage": "…",
      "modelConfig": "default-model-config",
      "stream": true,
      "tools": [{
        "type": "McpServer",
        "mcpServer": {
          "name": "kagent-tool-server", "namespace": "kagent",
          "kind": "RemoteMCPServer", "apiGroup": "kagent.dev",
          "toolNames": ["k8s_get_resources", "k8s_describe_resource", "k8s_get_events"]
        }
      }]
    }
  }
}
```

(That's what `ui/src/app/actions/agents.ts:fromAgentFormDataToSandboxAgent`
produces when the user picks the **"Sandbox workload"** type in the form
and leaves the BYO image field empty.)

Result: SandboxAgent reached `Ready`, ActorTemplate reached `Phase=Ready`,
A2A request via `kagent-controller:8083/api/a2a-sandboxes/kagent/ui-created-agent/`
answered correctly with a real `gpt-4o-mini` call + MCP tool call returning
the actual list of deployments in the `kagent` namespace.

### What also got fixed during UI verification

**Bug: HTTP-server preflight hardcoded to agent-sandbox CRD.**
`go/core/internal/httpserver/handlers/agents.go:validateAgentObject` was
still calling the deprecated `EnsureAgentSandboxAPIsRegistered`, which
unconditionally requires `agents.x-k8s.io/v1alpha1/Sandbox`. On a
substrate-only cluster (where the agent-sandbox CRD is absent) every
SandboxAgent create through the HTTP API failed with the misleading error
`"install the agent-sandbox CRDs and controller before using SandboxAgent"`.

The reconciler's equivalent preflight was migrated to the backend-aware
`EnsureSandboxBackendAPIsRegistered(ctx, client, backend)` back in Phase 2
(§16); the HTTP server's wasn't. One-line fix: swap the call to the same
backend-aware function. The UI's create flow would have been completely
broken end-to-end without this fix — it doesn't show up in the curl
walkthrough because that path applies SandboxAgents via `kubectl apply`,
which bypasses the HTTP server's validation.

**Lesson:** any "is the sandbox backend ready?" preflight in a new code
path needs to use `EnsureSandboxBackendAPIsRegistered`, not the legacy
`EnsureAgentSandboxAPIsRegistered`. The latter is kept only as a
backwards-compat wrapper and should be considered deprecated for all
internal callers (annotation on the function in
`apis_available.go:Deprecated`).

### Fresh-cluster rerun (2026-05-23)

Walked through DEMO.md verbatim against a deleted-and-recreated kind
cluster to validate the documented steps:

- Substrate install: ✅ clean (~3 min)
- Controller + app-substrate + UI image builds: ✅ all three succeeded on
  first try
- helm install: ✅ controller restarted 3 times during postgres-wait
  (documented), then stable
- WorkerPool auto-provisioned + 2 worker pods Running: ✅
- `kubectl apply 05-builtin-k8s-agent.yaml` → first attempt **stuck in
  `WaitGoldenActor`** because substrate's `runsc checkpoint pause` failed
  with `cannot checkpoint container "pause" in state stopped` — that's
  the §11 "worker setup flake" risk. After deleting the worker pods and
  re-applying the SandboxAgent, the second attempt reached Ready in
  ~1 minute. DEMO.md now carries an explicit recovery recipe inline.
- Real A2A prompt → real `gpt-4o-mini` + MCP tool call → real
  `kubectl get pods -n kube-system` table returned: ✅

**Honest takeaway:** the documented path works, but you may need to
recycle the worker pods once. The first-try success rate of substrate's
`runsc checkpoint` on a fresh worker is not 100% — it's a real pre-alpha
substrate flake, not a kagent bug. The recovery is mechanical and bounded.

---

## 21. Agent + SandboxAgent unification (2026-05-24)

The two parallel CRDs (`Agent` and `SandboxAgent`) collapsed into one.
The runtime choice — per-agent Deployment vs substrate-routed
sandbox — is now a **field on `AgentSpec`** instead of a separate kind.

### Why

Both CRDs already used the same `AgentSpec`. The only material
difference was which backend processed them: `Agent` → deployment
backend (`agentsxk8s`); `SandboxAgent` → configured sandbox backend
(substrate, when enabled). The duplication leaked into the UI (which
hadn't been updated to query both endpoints, so SandboxAgents were
invisible in the agent list), the HTTP API (parallel `/api/agents` and
`/api/sandboxagents` routes + `/api/a2a` and `/api/a2a-sandboxes` mux
paths), the reconciler (two dispatch methods), and the helm chart (two
RBAC rule sets + two CRD manifests). All for a single boolean choice.

The unification trades a hard breaking change for a simpler ongoing
shape: one kind, one endpoint per concern, one reconciler dispatch
method, one informer.

### What changed

- **New field `AgentSpec.workloadMode`** (`*WorkloadMode`, optional).
  When unset, falls back to the controller's `--default-workload-mode`
  flag (chart value `controller.defaultWorkloadMode`, defaults to
  `deployment`). Values: `deployment`, `sandbox`.
- **`SandboxAgent` CRD deleted.** `go/api/v1alpha2/sandboxagent_types.go`
  removed; deepcopy entries removed; the `helm/kagent-crds/templates/
  kagent.dev_sandboxagents.yaml` chart template removed.
- **Reconciler collapsed.** `ReconcileKagentSandboxAgent` removed;
  `ReconcileKagentAgent` is the unified entry point and branches
  internally on `agent.GetWorkloadMode()`. The substrate finalizer
  (`kagent.dev/substrate-actor`) is applied conditionally — only when
  `workloadMode == sandbox` AND the active backend implements
  `DeletingBackend`. `sandboxagent_controller.go` deleted.
- **HTTP API collapsed.** `/api/sandboxagents/*` routes + handlers
  removed; everything served via `/api/agents/*`. The `/api/a2a` mux
  path is the sole A2A endpoint for all agents — `/api/a2a-sandboxes`
  was retired alongside the `sandboxPathPrefix` parameter on
  `NewA2AHttpMux` and the `sandboxA2AURL` field on `A2ARegistrar`. The
  per-agent transport (host-rewriting for sandbox-mode) is still
  selected by `agent.GetWorkloadMode()` in `resolveDialing` — no path
  prefix needed.
- **DB schema unchanged.** The `WorkloadType` discriminator column on
  the Agent table already existed and is reused. We also fixed a stray
  pre-existing bug where sandbox-mode rows had their `Type` column
  overwritten to `"SandboxAgent"` (mixing agent spec type with workload
  runtime); now `Type` stays `Declarative` / `BYO` as the column name
  implies, and `WorkloadType` carries the sandbox/deployment
  distinction.
- **UI collapsed.** `ui/src/types/index.ts` drops the `SandboxAgent`
  interface; `AgentSpec.workloadMode` added. The "Sandbox workload"
  form option in `agents/new/page.tsx` now produces a `kind: Agent`
  payload with `spec.workloadMode: sandbox` and POSTs to `/api/agents`.
  `a2a-sandboxes` proxy route removed; `lib/a2aClient.ts` always uses
  `/a2a`. The list view bug — SandboxAgents invisible because
  `getAgents()` only called `/api/agents` — disappears as a side
  effect; everything is `kind=Agent` now.
- **Helm + RBAC.** New chart value `controller.defaultWorkloadMode`
  surfaces the controller's default. The substrate demo values flip it
  to `sandbox`. `sandboxagents` rules removed from
  `helm/kagent/templates/rbac/{writer,getter}-role.yaml`.
- **CLI.** `go/core/cli/internal/cli/agent/invoke.go` and `internal/
  tui/workspace.go` drop their `WorkloadMode == sandbox → a2a-sandboxes`
  branch; both always use `/api/a2a`.
- **Demos.** All four `kind: SandboxAgent` YAMLs in `demo/substrate-poc/`
  rewritten to `kind: Agent` (the demo values file sets
  `defaultWorkloadMode: sandbox`, so the field doesn't need to be
  explicit per-agent).

### Migration story

Hard cut (per the §20 decision). No conversion webhook, no
deprecation period. Existing `SandboxAgent` CRs in a cluster that
upgrades will be orphaned — the CRD is removed; the controller has no
reconciler for them; helm leaves them for the operator to clean up
manually. Acceptable for alpha. Documented in DEMO.md.

To migrate by hand: `kubectl get sandboxagent -A -o yaml`, change
`kind: SandboxAgent` → `kind: Agent`, add `spec.workloadMode: sandbox`,
apply, then delete the old CRs (after stripping the finalizer if
substrate is unreachable).

### Live-cluster verification (2026-05-24)

Built `localhost:5001/kagent-dev/kagent/controller:unified-agent` from
the post-unification source. `helm upgrade kagent ...` with the
substrate values file (which sets `defaultWorkloadMode: sandbox`). The
verifiable kagent-side path works end-to-end:

| Step | Result |
|---|---|
| `kubectl apply -f 05-builtin-k8s-agent.yaml` (`kind: Agent`, no `spec.workloadMode`) | ✅ created |
| Controller log: `"controllerKind":"Agent"` + `"substrate backend dropped unsupported PodTemplate fields"` | ✅ unified reconciler dispatched to substrate |
| `kubectl get actortemplate -n kagent k8s-agent-substrate` → `phase=WaitGoldenActor` | ✅ substrate backend emitted ActorTemplate |
| `kubectl get agent k8s-agent-substrate -n kagent -o jsonpath='{.status.conditions[?(@.type==\"Accepted\")].message}'` → `Agent configuration accepted` | ✅ unified status writer ran |
| `HEAD /api/a2a/kagent/k8s-agent-substrate/` → `400` (route matched, just wrong method) | ✅ unified A2A endpoint registered |

Initial verification attempt was blocked by the `runsc checkpoint
pause: exit 128` flake documented in §11. Recycling workers +
restarting `ate-api-server` didn't help — the cluster had ~9h of
accumulated substrate-side state across many test cycles, including 6
ghost actor records in Valkey.

After a full substrate reinstall (`kubectl delete ns ate-system` then
re-running `hack/install-ate-kind.sh --deploy-ate-system`), the path
unblocked:

| Step | Result |
|---|---|
| Reinstalled substrate (~3 min) | ✅ fresh `ate-system`, clean Valkey + rustfs |
| Re-applied `05-builtin-k8s-agent.yaml` (`kind: Agent`, empty `workloadMode`) | ✅ created |
| `ActorTemplate` golden snapshot | ✅ `phase=Ready` in ~30s |
| `Agent` `Ready=True` | ✅ `golden snapshot ready; actor "kagent--k8s-agent-substrate" is STATUS_SUSPENDED` |
| First A2A request via the UI chat panel | ❌ first try hit §11 `eth0: Link not found` on resume — worker recycled |
| Second A2A request via the UI chat panel | ✅ `POST /api/a2a/kagent/k8s-agent-substrate/` `status=200` `duration=3.06s` from the UI pod's IP — real LLM answer rendered |

Unification path verified end-to-end through both `curl` and a
browser-driven UI session. The §11 flake is real and per-request:
expect to recycle a worker after the first attempt on a fresh
substrate. After one successful sandbox boot, subsequent
resume/suspend cycles on that same worker are stable.

### Stale-image gotcha worth flagging

When verifying live, I rolled the controller image but forgot the UI
image. The pre-unification UI (built before the path collapse) still
POSTed to `/a2a-sandboxes/<ns>/<name>/`, and the new controller only
serves `/a2a/...` — the result was a 404 visible in the browser. Both
image tags need to be rebuilt together. The
`demo/substrate-poc/kagent-substrate-values.yaml` file now pins
`controller.image.tag` and `ui.image.tag` to the same
`unified-agent` value so this gotcha is harder to repeat.

### Real bug found during the fresh-cluster rerun: BYO image override

The 2026-05-24 fresh-cluster rerun surfaced a substrate-backend bug
that had been latent since the A2 (real-ADK) work: `applyAgentImageOverride`
in `go/core/pkg/sandboxbackend/substrate/substrate.go` ran
**unconditionally** whenever `--substrate-agent-image` was set,
including for BYO agents. The override replaces the user's image with
the kagent-shim image and appends `KAGENT_CONFIG_JSON` env (empty for
BYO agents, since the kagent translator only builds config bytes for
Declarative agents). The shim's first line refuses to start without
`KAGENT_CONFIG_JSON`:

```sh
if [ -z "${KAGENT_CONFIG_JSON:-}" ]; then
  echo "substrate-entrypoint: FATAL: KAGENT_CONFIG_JSON is not set" >&2
  exit 64
fi
```

So BYO agents using a substrate install configured with `agentImage`
would never start. Their pause container would exit instantly (because
the app container exited at startup), and every `runsc checkpoint
pause` subsequently failed with `exit 128`. We were mis-attributing
this to substrate's pre-alpha gVisor when the kagent-side translation
was the actual cause.

**Fix**: skip the override for `spec.type == BYO`. After applying:

| Phase-0 BYO Agent (`substrate-poc-agent:p1`) | Result |
|---|---|
| Apply → `ActorTemplate` golden snapshot | ✅ `phase=Ready` in 40s (FastAPI boots in 2s, well under substrate's 20s timer) |
| Agent `Ready=True` | ✅ |
| First A2A request via UI chat panel / `curl` | ❌ first try hit `eth0: Link not found` on resume — recycled the assigned worker |
| Subsequent A2A requests | ✅ `POST /api/a2a/kagent-substrate-poc/demo-alpha/ status=200` — the Phase-0 stand-in's minimal A2A `message/send` handler returns an echo task. Full unified path proven end-to-end. |

The Declarative `k8s-agent-substrate` (real kagent ADK + gpt-4o-mini)
*still* hits `runsc checkpoint pause: exit 128` on most cold starts.
At the time of this verification we attributed that to the 20s
golden-snapshot timer firing before the ADK finished booting. We have
since disproved that hypothesis — see §22 for the experiments (120s
timer patch + openclaw spike) that show the failure is
process-specific gVisor checkpoint incompatibility, not boot timing.
DEMO.md leads with the BYO Phase-0 path (reliable) and treats the
Declarative path as "optional, may need retries."

### Phase-0 stand-in now implements A2A `message/send`

The `demo/substrate-poc/agent.py` FastAPI stand-in used to only serve
plain HTTP routes (`/healthz`, `/info`, `/connectivity`, `/echo`,
`/.well-known/agent-card.json`). The kagent A2A mux + UI chat panel
both speak A2A JSON-RPC and POST to `/`, which the stand-in didn't
have — so even with substrate working, sending a chat message returned
a FastAPI 404. Added a minimal handler that:

- Accepts `{"jsonrpc":"2.0","method":"message/send","params":{...}}`
- Echoes the prompt text back as the task artifact
- Returns the response wrapped in A2A's `task` shape

That's enough for the UI chat panel to render a reply, demonstrating
the full unified path through a browser-driven session. The image is
tagged `localhost:5001/substrate-poc-agent:p1` (was `:p0` for the
pre-A2A-handler version).

### Files touched

```
go/api/v1alpha2/agent_types.go                  MOD — WorkloadMode field on AgentSpec
go/api/v1alpha2/agentobject.go                  MOD — GetWorkloadMode reads field + package default
go/api/v1alpha2/sandboxagent_types.go           DELETED
go/api/v1alpha2/zz_generated.deepcopy.go        MOD — WorkloadMode deepcopy + SandboxAgent entries removed
go/api/config/crd/bases/kagent.dev_sandboxagents.yaml   DELETED (regenerated)
go/api/httpapi/types.go                         MOD — drop SandboxAgent kind branch
go/core/internal/controller/sandboxagent_controller.go  DELETED
go/core/internal/controller/agentobject_helpers.go      MOD — collectSandboxAgentRefs removed
go/core/internal/controller/reconciler/reconciler.go    MOD — collapsed dispatch, conditional finalizer
go/core/internal/controller/reconciler/utils/reconciler_utils.go   MOD — drop "SandboxAgent" from owner-kind filter
go/core/internal/httpserver/server.go           MOD — drop APIPathSandboxAgents + APIPathA2ASandboxes + routes
go/core/internal/httpserver/handlers/agents.go  MOD — 5 SandboxAgent handlers + normalizer + agentObjects helper removed
go/core/internal/a2a/a2a_handler_mux.go         MOD — sandboxPathPrefix removed; single routeKey
go/core/internal/a2a/a2a_registrar.go           MOD — sandboxA2AURL removed; single informer (Agent)
go/core/cli/internal/cli/agent/invoke.go        MOD — drop a2a-sandboxes branch
go/core/cli/internal/tui/workspace.go           MOD — same
go/core/pkg/app/app.go                          MOD — DefaultWorkloadMode field + flag + SetDefaultWorkloadMode call; single controller registration; updated NewA2A*HttpMux signatures
helm/kagent/values.yaml                         MOD — controller.defaultWorkloadMode
helm/kagent/templates/controller-configmap.yaml MOD — DEFAULT_WORKLOAD_MODE env
helm/kagent/templates/rbac/writer-role.yaml     MOD — sandboxagents rules removed
helm/kagent/templates/rbac/getter-role.yaml     MOD — same
helm/kagent-crds/templates/kagent.dev_sandboxagents.yaml   DELETED
helm/kagent-crds/templates/kagent.dev_agents.yaml         MOD — regenerated with workloadMode
ui/src/types/index.ts                           MOD — SandboxAgent interface removed; AgentSpec.workloadMode + WorkloadMode type added
ui/src/app/actions/agents.ts                    MOD — fromAgentFormDataToSandboxAgent → fromAgentFormDataToSandboxModeAgent (kind=Agent + workloadMode); createAgent collapses to /agents endpoint; waitForSandboxAgentReady → waitForAgentReady
ui/src/app/a2a-sandboxes/                       DELETED (Next.js proxy route)
ui/src/lib/a2aClient.ts                         MOD — runInSandbox param ignored; single path
ui/src/components/chat/ChatInterface.tsx        MOD — waitForAgentReady rename
demo/substrate-poc/02-sandbox-agent.yaml        MOD — kind: SandboxAgent → kind: Agent
demo/substrate-poc/03-agents.yaml               MOD — same (4 instances)
demo/substrate-poc/04-real-agent.yaml           MOD — same
demo/substrate-poc/05-builtin-k8s-agent.yaml    MOD — same
demo/substrate-poc/kagent-substrate-values.yaml MOD — controller.defaultWorkloadMode: sandbox
```

Test changes:
```
go/core/internal/controller/{mcp_server_tool,service}_controller_test.go   MOD — fakeReconciler.ReconcileKagentSandboxAgent stub removed
go/core/internal/controller/translator/agent/{adk_api_translator,substrate_backend}_test.go   MOD — SandboxAgent CRs swapped for Agent + workloadMode field
go/core/internal/httpserver/handlers/{agents,test_helpers}_test.go         MOD — SandboxAgent fixtures + scheme registrations + handler tests collapsed
go/core/pkg/sandboxbackend/{filter_translator_owned,substrate/substrate}_test.go   MOD — same
go/core/test/e2e/invoke_api_test.go             MOD — generateSandboxAgent/setupSandboxAgentWithOptions/setupSandboxA2AClient renamed + return *v1alpha2.Agent; kubectl-wait target is agents.kagent.dev now
```

### Net for the PoC after §21

Same end state the post-A2 / §20 work proved (kagent ADK in substrate
sandbox driven by a Declarative CRD), just under a single CRD kind.
The UI now correctly shows everything in one list. The HTTP API is
half the surface it was. The reconciler is one method instead of two.
The doc surface is smaller too (every "Agent vs SandboxAgent"
explanation collapses to "workloadMode").

§13's remaining out-of-scope items (skills-using agents,
`executeCodeBlocks: true`, production tool-server auth, controller-
backed session persistence across suspend) are unchanged — they're
orthogonal to the CRD shape.

## 22. Root cause revision — `runsc checkpoint: exit 128` is NOT a gVisor checkpoint bug at all; it's a substrate cleanup-ordering bug (2026-05-27); **FIX VERIFIED ON OUR FORK (2026-05-28)**

### Status

A 5-line patch to substrate's `cmd/ateom-gvisor/main.go::CheckpointWorkload`
makes openclaw (and by extension the kagent ADK Declarative-agent path)
golden-snapshot reliably. The patch demotes `cmdState`/`cmdDelete`
failures after a successful `cmdCheckpoint` from fatal errors to
warnings. The fix is ready for upstream PR pending substrate-team
review of the approach. **Now superseded by §23, which documents the
final state (all three demo paths working) and the full list of six
substrate patches required to get there.** This section is kept for the
debugging-narrative trail; §23 is the load-bearing summary.

**Verification (2026-05-28 12:36 UTC):** spike-B (openclaw gateway, which
had failed reliably for days) reaches `Ready` in ~42s with golden snapshot
ID `040c6cc0-4ef4-4ec9-8e7b-fe1842f18e08`. The race condition still
triggers (`runsc delete openclaw: exit status 128` happens), but the
patch logs a warning and lets `CheckpointWorkload` return success
instead of failing the RPC. Demo-alpha (FastAPI BYO) continues to work
end-to-end on the patched ateom-gvisor — no regression.

### Final answer (after instrumenting the sentry)

After patching substrate to add `-debug -debug-log -panic-log` to all
runsc invocations (the toggles were already pre-staged as commented-out
lines in `cmd/ateom-gvisor/runsc.go` — just needed to uncomment + add
`os.MkdirAll` for the log directory), we captured the actual cause:

1. **`runsc checkpoint pause` succeeds.** The sentry debug log shows
   `kernel_restore.go:113] Checkpoint completed successfully.` and the
   runsc CLI exits with status 0. The sandbox then SIGKILLs all tasks
   and exits — gVisor's normal behavior for a `Resume:false` checkpoint.

2. **`runsc delete -force openclaw` (the follow-up cleanup step) fails
   with `connection refused`.** The sentry that the openclaw container
   shared is already gone (it exited cleanly as part of step 1), but
   `runsc delete -force` still tries to connect to the sentry's control
   server at the sentry's old PID. Fatal error → exit 128.

3. **Substrate's `CheckpointWorkload` treats this as a complete failure**
   and returns an error to the ate-controller. The controller retries
   `CheckpointWorkload` forever; each retry now fails at the
   `runsc checkpoint` step because the sandbox is already gone from the
   first cycle's already-successful checkpoint. The visible-to-operators
   `runsc checkpoint pause: exit 128 / state stopped` error is the retry
   symptom, not the root cause.

**This is a substrate-side ordering bug in `CheckpointWorkload`. gVisor
is doing the right thing. The 20s timer is irrelevant. The "process-
specific gVisor checkpoint incompatibility" theory was wrong.**

The full evidence chain (sentry boot.txt, checkpoint.txt, delete.txt)
was captured during this investigation. The standalone bug-report doc
(`CHECKPOINT-BUG-REPRO.md` + spike YAML files) lived in
`demo/substrate-poc/` for ~two days then was removed once §23 superseded
it — kept in git history if the substrate team wants the verbatim logs.

### How spike-A still succeeds (best guess pending substrate-team review)

`spike-actortemplate-a.yaml` runs `tail -f /dev/null` and reaches `Ready`
on the same `CheckpointWorkload` code path. Best guess: race condition.

The sentry's exit takes time proportional to how many tasks/threads/fds
it has to SIGKILL during cleanup. With `tail -f` there's almost nothing
to tear down, so the sentry exits faster, and the runsc-state files are
updated to "stopped" before substrate's `runsc delete -force` fires —
runsc then short-circuits and returns success. With openclaw or the
kagent ADK there's much more state, so the sentry takes ~30 ms longer,
the runsc-state files haven't updated yet, and runsc tries to connect to
the sentry control plane — getting connection refused.

If this is correct, the fix is one of:
- skip `runsc delete -force <app-container>` entirely when the checkpoint
  already succeeded (the sandbox is gone, there's nothing to delete);
- retry `cmdDelete` a few times with short backoff to absorb the race;
- treat `connecting to control server: connection refused` as a benign
  signal that the sandbox is gone and the cleanup is already done;
- switch to `runsc checkpoint -resume=true` so the sandbox stays alive
  after the snapshot and downstream cleanup is ordering-safe.

### Earlier wrong hypotheses (preserved for the audit trail)

The hypotheses below were the analysis we had *before* enabling the
sentry debug logs. They were both wrong about the root cause, but were
reasonable conclusions from the surface-level evidence available.



§11 originally listed the kagent-adk Declarative-agent failure under
"No readiness signal — hardcoded 20s timer before golden snapshot."
The narrative was: the ADK needs more than 20s to finish booting, so
substrate captures a mid-init snapshot, the app container exits, and
the pause container ends up in state `stopped` — at which point
`runsc checkpoint pause` returns `exit 128`. That story was wrong.
The 20s timer is real and inflexible, but it is **not** the cause of
the failure. Two independent experiments disprove it.

### Experiment 1: 120s timer patch (≈2026-05-23)

We patched
`agent-substrate/substrate/internal/controllers/actortemplate_controller.go:112`
to set `TakeGoldenSnapshotAt = now + 120s` instead of `now + 20s`.
The kagent ADK had a full two minutes to finish model init, MCP
discovery, configmap reads, and become fully request-ready — well
past any reasonable cold-start budget. The patch was reverted because:

- The ADK reached request-ready inside the 120s window (verified by
  hitting `/.well-known/agent-card.json` from inside the sandbox
  before the snapshot deadline).
- **`runsc checkpoint pause: exit 128` still failed**, in the same way
  and at the same step.
- If the cause were "container wasn't listening when snapshot fired,"
  this experiment would have succeeded. It didn't.

Patch reverted at user request (it was adding maintenance burden
without fixing anything).

### Experiment 2: openclaw spike on kind (2026-05-25)

We ran a Stage-6 spike for the harness-in-substrate port plan, using a
**completely different workload**: `nemoclaw/sandbox-base:2026.5.4`
running `openclaw gateway run --port 80 --allow-unconfigured`.

| Variant | Workload | Result |
|---|---|---|
| Spike A | `/bin/sh -c 'tail -f /dev/null'` in nemoclaw base image | **Ready** ✅ (~25s, clean golden snapshot) |
| Spike B | `openclaw gateway run` in same image | **Stuck WaitGoldenActor**, `runsc checkpoint pause: exit 128` ❌ |

Spike B's openclaw emitted its `[gateway] security warning` log line
at **t=6s** — fully booted, gateway listening on port 80, well within
substrate's 20s timer window. Yet the snapshot at t=20s still failed
with the identical `exit 128`. **Different process, identical
failure mode, with the process provably ready.**

Spike A proves the nemoclaw image itself unpacks and runs in gVisor
just fine (with our local `atelet/oci.go` device-tar patch). So the
blocker isn't the image, the OCI unpack, or readiness — it's
**whatever the openclaw process does between t=0 and t=20s that
gVisor systrap can't checkpoint.**

### Net conclusion

`runsc checkpoint pause: exit 128` is **process-specific gVisor
checkpoint incompatibility**, not boot timing. The pattern:

| Workload | Behavior | Why |
|---|---|---|
| `tail -f /dev/null` | Checkpoints fine | No interesting fd state |
| FastAPI + uvicorn (demo-alpha) | Checkpoints fine | Single-threaded asyncio, no external TLS, minimal fds |
| Openclaw gateway | Fails | Multi-goroutine Go binary with listener state gVisor can't serialize |
| kagent ADK + OpenAI SDK + OTEL | Fails | Long-lived httpx TLS pools to api.openai.com, asyncio loop state, OTEL spans |

§17's `_substrate_checkpoint_friendly.install()` hook (closes httpx
pools 3s after each request) is consistent with this: it makes the
*first* suspend cycle work by clearing the offending TLS state. The
*post-resume* state has new uncheckpointable artifacts, which is why
the second suspend cycle still fails. The hook partially mitigates;
it doesn't fix the underlying gVisor systrap limitation.

### Why this matters

- **Don't extend the 20s timer to "fix" this.** It won't, and even if
  the ADK had more time the post-init state still wouldn't be
  checkpointable.
- **DEMO.md and §11 used to recommend "(b) file substrate upstream
  for a probe-based wait."** A probe-based wait would still help for
  cases where the process genuinely isn't listening by t=20s, but it
  is **not** the path to making the kagent-adk Declarative-agent demo
  reliable. That requires either:
  1. gVisor / substrate-side fix to handle whatever syscall state
     trips checkpoint (deep substrate work, not in kagent's scope).
  2. A different agent runtime that avoids the offending state pattern
     (BYO agent — what demo-alpha demonstrates).
  3. Substrate exposing a "no golden snapshot, boot fresh on every
     resume" mode for incompatible workloads (substrate API change).
- **(Superseded 2026-05-28)** The harness-in-substrate port has been
  completed and verified working — see §23 for the final state.
  This section's pessimism predated patches #3–#6.

### Documentation updates as part of this revision

- §11 risk entry rewritten to lead with the correct cause.
- `demo/substrate-poc/DEMO.md` rewritten to reflect all three demo paths
  working (see §23 below for the full final state).

## 23. Final state — three working demo paths + AgentHarness substrate-backend port (2026-05-28)

The cumulative work in §15–§22 plus the AgentHarness port now produces
**three kagent-on-substrate paths that all work end-to-end on a single
kind cluster** (kind 1.35 on Docker Desktop / macOS):

| Path | CRD | Status |
|---|---|---|
| BYO Agent | `Agent` + `spec.type: BYO, workloadMode: sandbox` | Works. Single-prompt latency ~1s warm, ~3s cold-resume. |
| Declarative Agent | `Agent` + `spec.type: Declarative, runtime: python` | Works. First prompt ~15s (cold restore); subsequent ~1s. Real `gpt-4o-mini` + `kagent-tool-server` MCP calls. |
| AgentHarness (openclaw) | `AgentHarness` + `spec.runtime: substrate, backend: openclaw` | Works. WorkerPool + ActorTemplate auto-provisioned; openclaw VM reaches RUNNING in ~5min on a fresh substrate; Control UI accessible through kagent's `/api/agentharnesses/<ns>/<name>/gateway/` proxy. |

### Six substrate-side patches required

All in `agent-substrate/substrate` SHA `a436ea2…`. Carried as
local-uncommitted changes; intended as upstream PRs after substrate-team
review.

| # | File | Patch summary |
|---|---|---|
| 1 | `cmd/atelet/oci.go` | Skip `tar.TypeChar/Block/Fifo` entries during image unpack (lets atelet unpack Debian/Wolfi/Alpine bases). |
| 2 | `cmd/ateom-gvisor/runsc.go` | `-debug -debug-log -panic-log -debug-log-format=json` on all runsc invocations. Diagnostic. |
| 3 | `cmd/ateom-gvisor/main.go::CheckpointWorkload` | Demote post-checkpoint `cmdState`/`cmdDelete` failures from RPC errors to warnings. Fixes the original `runsc checkpoint pause: exit 128` retry loop on any non-trivial workload. |
| 4 | `cmd/atenet/internal/app/router/resumer.go` | bgCtx 15s → 60s. The kagent ADK image's `runsc restore` takes 14–18s on kind/macOS. |
| 5 | `cmd/atenet/internal/app/router/xds.go` | ext_proc `Timeout` + `MessageTimeout` 5s → 60s. Coordinated with #4. |
| 6 | `cmd/ateapi/internal/controlapi/workflow.go::ResumeActor/SuspendActor` | Lock TTL 30s → 120s (workflow timeout = `ttl - 2s padding`). The AgentHarness Resume path hits ate-api's own 28s workflow timeout. |

### Kagent-side additions for the AgentHarness substrate-backend port

Lifted from pj-kagent (with structural adaptations to fit our existing
package layout); ~2500 LOC of new code.

| Area | Files |
|---|---|
| New top-level openclaw helpers | `go/core/pkg/sandboxbackend/openclaw/` (bootstrap_shared/substrate, constants, credentials, defaults, modelconfig, provider, secrets, types). Channels (Telegram/Slack) stubbed out for this fork — restore from pj-kagent when needed. |
| Substrate harness sub-package | `go/core/pkg/sandboxbackend/substrate/harness/` (client, config, delete_actor, delete_provision, gateway_token, openclaw, provision_actortemplate, provision_openclaw, provision_shared, provision_workerpool, provision, templates/openclaw_startup.sh.tmpl). |
| CRD additions | `AgentHarnessSpec.Runtime` (enum: openshell, substrate), `AgentHarnessSpec.Substrate`, `AgentHarnessStatus.Substrate`. Optional; default `runtime: openshell` preserves existing behavior. |
| Controller dispatch | `agentharness_controller.go` lifted wholesale from pj-kagent with imports adjusted for our `substrate/harness/` sub-package. Adds `OpenshellBackends`, `SubstrateBackends`, `SubstrateProvisioner` fields. |
| Substrate-side event sources | `agentharness_substrate_watches.go` watches `WorkerPool`, `ActorTemplate`, `Deployment` for reconcile triggers. |
| HTTP proxy handler | `httpserver/handlers/agentharness_gateway.go` proxies `/api/agentharnesses/<ns>/<name>/gateway/` (and subpaths, including WebSockets) to the openclaw gateway via the actor's pod IP. Lifted from pj-kagent. |
| AsyncBackend interface | `sandboxbackend/async.go` merged `OnAgentHarnessReady` into `AsyncBackend` (matches pj-kagent); both openshell and harness backends implement it. |
| app.go wiring | New `substrateHarnessEnabled` gate (set when `cfg.Substrate.ControlEndpoint != "" && cfg.Substrate.WorkerPoolAteomImage != ""`); builds `harness.Client`, `SubstrateBackends`, `harness.Provisioner` at startup, and an `AgentHarnessGatewayConfig` for the HTTP proxy. |

### Things this state does NOT address

- **Worker-recycle stuck actors.** If a worker pod is recycled while an actor is `STATUS_RESUMING`, the actor wedges pointing at the dead pod. Workaround: `kubectl ate suspend actor <id>` → controller's next reconcile resumes cleanly on a live worker. Worth documenting upstream as an ate-controller responsibility.
- **GKE install.** Substrate's `install-ate.sh --deploy-ate-system` hangs on every GKE channel we tried (stable/regular/rapid/alpha-cluster) because `certificates.k8s.io/v1beta1.ClusterTrustBundle` is not served. Open question for the substrate team.
- **Channels (Telegram/Slack) in the openclaw harness.** We stubbed those out when lifting the openclaw package (`bootstrap_substrate.go` now passes an empty channelEnv); restore from pj-kagent's `channels_substrate.go` + `channels_shared.go` when needed.

### Pointers

- `demo/substrate-poc/DEMO.md` — current walkthrough for all three paths.
- `demo/substrate-poc/05-builtin-k8s-agent.yaml` — Declarative path.
- `demo/substrate-poc/06-openclaw-harness.yaml` — AgentHarness path.

## 24. Substrate observability endpoints + UI page (2026-05-28)

The kagent controller now exposes two read-only endpoints that proxy
substrate's `Control.ListWorkers` / `Control.ListActors` RPCs, plus a
Next.js page that consumes them. Goal: surface the live state of the
worker pool and the actors it hosts directly in the kagent UI, without
needing `kubectl ate`.

### Backend

`go/core/internal/httpserver/handlers/substrate.go` defines
`SubstrateHandler` with two methods:

| Endpoint | Method | Payload |
|---|---|---|
| `/api/substrate/workers` | `GET` | `[]{workerNamespace, workerPool, workerPod, actorNamespace?, actorTemplate?, actorId?, ip, version}` |
| `/api/substrate/actors`  | `GET` | `[]{actorId, version, actorTemplateNamespace?, actorTemplateName?, status, ateomPodNamespace?, ateomPodName?, ateomPodIp?, lastSnapshot?, inProgressSnapshot?}` |

Both wrap responses in the standard `{error, data, message}` envelope
(`api.NewResponse(...)`). Sort order is stable across polls — workers
sort by namespace → pool → pod, actors by Running-first → template
namespace/name → id. Without that the UI tables shuffle every refresh
because substrate's RPCs return no defined order.

The handlers return HTTP 501 (`NewNotImplementedError`) when the
controller wasn't started with the substrate backend (`harness.Client`
nil). Same gate as the AgentHarness gateway proxy — both share the
`Client` instance hoisted to function scope in `app.go`.

`harness.Client` gained `ListWorkers(ctx) ([]*Worker, error)` and
`ListActors(ctx) ([]*Actor, error)` on top of the existing
GetActor/CreateActor/ResumeActor/SuspendActor/DeleteActor methods.
Both empty-request RPCs against `ateapi.Control`.

### Frontend

| File | Purpose |
|---|---|
| `ui/src/app/actions/substrate.ts` | Server actions calling the two endpoints, returning `BaseResponse<SubstrateWorker[]>` / `BaseResponse<SubstrateActor[]>`. |
| `ui/src/app/substrate/page.tsx` | Client component polling both endpoints every 2 s via `setInterval`. Workers card first, Actors card second — small-display ordering preference: the "most relevant" actor info sits last so scroll-stop lands on it. |
| `ui/src/components/Header.tsx` | New "Substrate" entry in the View dropdown (desktop + mobile), Boxes icon from lucide-react. |

The page renders with `LoadingState` while the first fetch is in flight,
then never blanks on subsequent errors — keeps the last good payload and
shows the error inline next to the timestamp. That way a transient
controller restart doesn't wipe the screen.

### Wiring

`ServerConfig` gained `SubstrateHarnessClient *harness.Client`; `app.go`
declares `substrateHarnessClient` at function scope (was previously
block-scoped inside the `substrateHarnessEnabled` branch) so the HTTP
server can reuse the same dialed `harness.Client` the AgentHarness
gateway already uses.

### Trade-offs

- **Polling, not push.** A websocket / SSE feed would scale better, but
  the substrate Control API is unary RPC only. Polling every 2 s
  against a local controller is cheap; the typical 6-actor / 3-worker
  payload is < 2 KB.
- **No filtering.** The endpoints return everything. If a tenancy
  story lands, add `?namespace=` and have the handler intersect with
  the caller's authz scope.
- **Stale snapshot URIs.** The `lastSnapshot` field on a `Suspended`
  actor can point at an unfinished snapshot in valkey if a prior
  Suspend was canceled mid-write — see DEMO.md's troubleshooting
  section on "actor wedged in Resuming". The UI just renders whatever
  ate-api returns; it can't tell a valid snapshot from a broken one.
