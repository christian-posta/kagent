# Implementation Plan: AgentHarness running in substrate

Lifting pj-kagent's harness-in-substrate work into this repo, on top of the
Agent+SandboxAgent unification we just landed (SUBSTRATE.md §21).

---

## Stage 6 SPIKE RESULT — 2026-05-25: openclaw fails golden-snapshot the same way the kagent ADK does

**Verdict: do not start stages 1–5.** Substrate cannot golden-snapshot an
openclaw workload today; porting pj-kagent's harness code would yield the
same broken runtime we've been hitting all session.

### What ran

Applied two minimal `ActorTemplate` CRs directly to the running cluster
(`kind-kagent-substrate`, ns `kagent-substrate-poc`, against the working
`poc-pool` WorkerPool that already serves demo-alpha):

| Spike | Image | Command | Result |
|---|---|---|---|
| A — bare | `localhost:5001/nemoclaw-sandbox-base:2026.5.4` | `echo … && tail -f /dev/null` | **Ready** ✅ — golden snapshot took ~25s |
| B — openclaw | same image | `openclaw gateway run --port 80 --allow-unconfigured` + curl loop + tail | **Stuck WaitGoldenActor** ❌ — `runsc checkpoint: exit status 128` retrying forever |

### What this proves

1. **The nemoclaw Debian-12 image unpacks and boots in gVisor.** Image is not the blocker; our local atelet patch (skip device/FIFO tar entries) handles the unpack correctly.
2. **The openclaw gateway process itself triggers `runsc checkpoint: exit 128`.** Same failure shape as the kagent ADK in §11.
3. **pj-kagent pins the identical gVisor build** (`nightly/2026-05-19/x86_64`, sha256 `a397be1…`). They have no secret runsc that dodges this.
4. **Image-vs-gVisor and process-vs-gVisor are separable failure modes.** We now know how to isolate them in any future debug.

### Worker log fingerprint (spike B)

```
14:26:44 [gateway] security warning: dangerous config flags enabled: …controlUi.dangerouslyDisableDeviceAuth=true.
14:26:50 Actor checkpointing
14:26:50 err: "while deleting \"openclaw\" application container: while running `runsc delete`: exit status 128"
14:26:50 (retries) err: "while checkpointing pause: while running `runsc checkpoint`: exit status 128"  (loops indefinitely)
```

Openclaw boots cleanly (gateway emits the security warning ~6s after start).
The failure is at the 20s `SuspendActor` mark when ateom asks runsc to
checkpoint the pause container — same point where the kagent ADK fails.

### Diff vs. demo-alpha (which DOES work)

`demo-alpha` runs a minimal FastAPI/uvicorn process inside the same gVisor
sandbox and golden-snapshots cleanly. So the blocker isn't "all real
workloads fail" — it's specific to whatever syscalls the kagent ADK + openclaw
processes have made by t=20s that gVisor systrap can't checkpoint. Both
workloads share that property; the FastAPI stand-in doesn't.

### Local substrate changes worth noting

Our `/Users/ceposta/go/src/github.com/agent-substrate/substrate` checkout
has one unpushed patch in `cmd/servers/atelet/oci.go` — skips
`tar.TypeChar/TypeBlock/TypeFifo` entries during image unpack (without it,
Wolfi/Alpine/Chainguard bases die immediately). This is load-bearing for
spike A and the demo. Not blocking for the openclaw failure but useful to
know exists.

Substrate is up-to-date with origin/main (`e1ff0ad Handle Kubernetes 1.36 pod
certificate requests (#8)`) — 8 commits total in the repo, very young. No
upstream fixes to pull. `git fetch` blocked on SSH creds for the private
repo; if newer commits exist, can't see them from here.

### Dependency check (open question #2 in the original plan)

- pj-kagent's `go/go.mod`: `github.com/agent-substrate/substrate v0.0.0` — same module path we use.
- No `replace` directive in any of pj-kagent's go.mod files. **They consume the same upstream substrate we do.**
- pj-kagent pins the same runsc URL/SHA we do.
- Conclusion: there's no hidden substrate fork or runsc patch giving pj-kagent a working path.

### What needs to happen before harness-in-substrate is worth the port

Pick one:

1. **Substrate-side fix**: identify which syscalls trip the checkpoint and either patch gVisor or harden ateom's CheckpointWorkload. Deep substrate work; not a kagent-side fix.
2. **Bypass golden-snapshot for harness mode**: change substrate so harness `ActorTemplate`s skip the resume→suspend→Ready flow and start workloads on demand. Substrate API change; not in kagent's scope.
3. **Different openclaw build / startup sequence**: maybe openclaw can be built or configured so its initial syscalls don't trip checkpoint. Speculative — would need openclaw-side investigation.

Whichever path: the port (Stages 1–5 below) becomes ~3 days of mechanical work that's only useful once one of (1)/(2)/(3) lands. **Don't start the port until then.**

---

## Context

kagent's `AgentHarness` CRD today supports three backends — `openshell`,
`openclaw`, `nemoclaw` — all hosted by the **OpenShell gateway** (a
gRPC-based remote control plane that provisions actual VMs). The
`sandboxbackend.AsyncBackend` interface (`go/core/pkg/sandboxbackend/async.go`)
is the abstraction: `EnsureAgentHarness` / `GetStatus` /
`DeleteAgentHarness`. Today only openshell-backed implementations exist.

pj-kagent added a second dimension to AgentHarness: `spec.runtime` (enum:
`openshell` | `substrate`). When `runtime: substrate`, the harness's VM
(e.g., an openclaw box) runs inside a substrate gVisor sandbox instead of
being provisioned by the openshell gateway. The user-visible benefits:

- Reuses kagent's existing substrate install — no openshell gateway needed
- gVisor-level isolation, snapshot-based suspend/resume
- Cheaper than a full openclaw VM stack

The pj-kagent design is **orthogonal to our Agent+SandboxAgent unification**:
they kept Agent and SandboxAgent separate; we unified them. Their addition
was a new harness-in-substrate runtime; we don't have that. The two pieces
of work can be composed.

## Code inventory: what to lift

Sizes from pj-kagent (`~/go/src/github.com/kagent-dev/pj-kagent/`):

| pj-kagent file | LOC | Action in our repo |
|---|---|---|
| `api/v1alpha2/agentharness_types.go` (substrate additions) | ~80 | **Adapt** — add `AgentHarnessRuntime`, `AgentHarnessSubstrateSpec`, `AgentHarnessSubstrateStatus`, regen CRD + deepcopy |
| `core/pkg/sandboxbackend/substrate/provision.go` | 301 | **Lift mostly as-is** — `Provisioner.Ensure(ah)` returns `EnsureResult{WorkerPoolRef, ActorTemplateRef, ActorTemplateReady, ManagedWorkerPool, ManagedActorTemplate}`. Auto-provisions a WorkerPool when `spec.substrate.workerPool` is set; adopts an existing one when `workerPoolRef` is set. Same for ActorTemplate. |
| `core/pkg/sandboxbackend/substrate/provision_openclaw.go` | 88 | **Lift** — builds the openclaw container spec for the ActorTemplate (image, command, env, ports). Uses the harness's `gatewayPort` + `gatewayToken`. |
| `core/pkg/sandboxbackend/substrate/openclaw.go` | 231 | **Lift** — the `AsyncBackend` impl for `backend: openclaw, runtime: substrate`. Implements EnsureAgentHarness/GetStatus/DeleteAgentHarness against the substrate Control API (CreateActor/GetActor/DeleteActor). |
| `core/pkg/sandboxbackend/substrate/delete_provision.go` | 109 | **Lift** — cleanup: deletes the ActorTemplate, deletes the WorkerPool if it was auto-provisioned (`ManagedWorkerPool=true`), strips the annotations. |
| `core/pkg/sandboxbackend/substrate/client.go` | 114 | **Compare + merge** with our existing `control_client.go` (which is tailored for Agent CRD lifecycle — CreateActorIfMissing, SuspendActor, DeleteActorSequenced). pj-kagent's client probably has similar primitives but focused on the AsyncBackend pattern. |
| Tests (`provision_test.go`, `provision_openclaw_test.go`, `delete_provision_test.go`, `openclaw_test.go`, `delete_actor_test.go`) | ~270 | **Lift** — adapt imports/paths |
| `core/internal/controller/agentharness_controller.go` (substrate dispatch) | ~50 | **Adapt** — add `runtime` switch + `SubstrateBackends` + `SubstrateProvisioner` fields. Pattern at pj-kagent's lines 56-70 + 126-130. |
| `core/pkg/sandboxbackend/openshell/openclaw/bootstrap_substrate_test.go` | ~80 | **Lift** — proves openclaw's bootstrap script works inside a substrate sandbox |
| `core/pkg/app/app.go` substrate-harness wiring | ~30 | **Add** — build `SubstrateBackends` + `Provisioner` at startup, pass to AgentHarnessController |

**Total: ~1000 LOC of Go + ~50 LOC of CRD schema + helm chart updates.**

## What we know about the differences with our repo

1. **Our `sandboxbackend/substrate/` already exists** for the Agent CRD path. The package has `substrate.go` (Backend impl), `control_client.go`, `routing.go`, `idle_suspender.go`, `workerpool_ensurer.go`, `delete_actor.go`. Adding the harness path means more files in the same package OR a sub-package (`substrate/harness/`).
2. **Our `WorkerPoolEnsurer`** auto-provisions a single shared WorkerPool at controller startup. pj-kagent's `Provisioner.Ensure` auto-provisions a WorkerPool *per AgentHarness* (in the harness's namespace) when one isn't referenced. **Design decision**: should harness-in-substrate share the existing WorkerPool, or get its own? Suggest: support both via `spec.substrate.workerPoolRef` (adopt) vs `spec.substrate.workerPool` (create). That's what pj-kagent does.
3. **Our `Backend` interface** (used by Agent CRD) and **`AsyncBackend` interface** (used by AgentHarness) are different. The harness path uses AsyncBackend exclusively. No cross-pollination needed.

## Stages

### Stage 1 — CRD + types (~half day)
**Files:**
- `go/api/v1alpha2/agentharness_types.go` (additions)
- `go/api/v1alpha2/zz_generated.deepcopy.go` (regen)
- `go/api/config/crd/bases/kagent.dev_agentharnesses.yaml` (regen)
- `helm/kagent-crds/templates/kagent.dev_agentharnesses.yaml` (sync from regen)

**Diff:**
- Add `AgentHarnessRuntime` type (enum: `openshell` | `substrate`).
- Add `AgentHarnessSubstrateSpec` (workerPoolRef OR workerPool, snapshotsConfig, workloadImage override, actorTemplateRef, gatewayPort, gatewayTokenSecretRef).
- Add `AgentHarnessSubstrateStatus` (workerPoolRef, actorTemplateRef, actorTemplateReady).
- Add `spec.runtime` and `spec.substrate` to `AgentHarnessSpec`.

**Verify:** `go build ./...` clean, `helm template` renders unchanged, `make -C go manifests` produces the expected schema diff.

### Stage 2 — Provisioner + substrate-side substrate primitives (~one day)
**Files (new):**
- `go/core/pkg/sandboxbackend/substrate/harness_provision.go` (from pj-kagent's `provision.go`)
- `go/core/pkg/sandboxbackend/substrate/harness_provision_openclaw.go` (from pj-kagent's `provision_openclaw.go`)
- `go/core/pkg/sandboxbackend/substrate/harness_delete_provision.go` (from pj-kagent's `delete_provision.go`)
- `go/core/pkg/sandboxbackend/substrate/harness_provision_test.go`
- `go/core/pkg/sandboxbackend/substrate/harness_provision_openclaw_test.go`
- `go/core/pkg/sandboxbackend/substrate/harness_delete_provision_test.go`

**Key adaptation:** pj-kagent's `client.go` has an `ateActorDeleter` interface that hides the gRPC ControlClient behind a delete-actor-sequenced primitive. Our existing `control_client.go` already has `DeleteActorSequenced` (we wrote it in §16/B1). We should reuse that, not duplicate. The Provisioner takes an `ateActorDeleter` so this is a clean drop-in.

**Verify:** `go test ./core/pkg/sandboxbackend/substrate/...` passes. Run with `-count=1`.

### Stage 3 — AsyncBackend impl for openclaw-in-substrate (~half day)
**Files (new):**
- `go/core/pkg/sandboxbackend/substrate/harness_openclaw.go` (from pj-kagent's `openclaw.go`)
- `go/core/pkg/sandboxbackend/substrate/harness_openclaw_test.go`

This is the `AsyncBackend` impl. EnsureAgentHarness:
1. Validates the harness's `spec.substrate.workerPoolRef` / `actorTemplateRef` resolved by the Provisioner (Stage 2)
2. Calls substrate `CreateActor` with actor ID derived from `<ns>--<name>`
3. Returns the actor's atenet URL + actor ID as the `Handle`

GetStatus polls `Control.GetActor` and maps STATUS_SUSPENDED/RESUMING/RUNNING to a Ready condition.

DeleteAgentHarness calls `DeleteActorSequenced`.

**Verify:** unit tests pass; can construct a fake AsyncBackend chain and exercise it.

### Stage 4 — Controller wiring (~half day)
**Files (modified):**
- `go/core/internal/controller/agentharness_controller.go`

**Diff (mirrors pj-kagent's design):**
- Add fields: `SubstrateBackends map[v1alpha2.AgentHarnessBackendType]sandboxbackend.AsyncBackend`, `SubstrateProvisioner *substrate.Provisioner`.
- Add `pickBackend(ah)` helper: dispatch on `spec.runtime`:
  - `openshell` (default) → existing `r.Backends[ah.Spec.Backend]`
  - `substrate` → `r.SubstrateBackends[ah.Spec.Backend]`
- In Reconcile, when `runtime == substrate`, call `r.SubstrateProvisioner.Ensure(ctx, ah)` first to provision the WorkerPool + ActorTemplate, then the AsyncBackend's `EnsureAgentHarness` to create the actor.
- Finalizer: extends existing finalizer to also call `DeleteProvision` (Stage 2's cleanup function).

**Verify:** unit tests with fake Backends + Provisioner, controller starts cleanly.

### Stage 5 — Helm + app.go wiring (~half day)
**Files (modified):**
- `go/core/pkg/app/app.go`
- `helm/kagent/values.yaml`
- `helm/kagent/templates/controller-configmap.yaml`
- `helm/kagent/templates/rbac/{writer,getter}-role.yaml`

**Diff:**
- New helm values: `substrate.harness.{enabled, defaultWorkerPoolRef, defaultSnapshotsLocation, gatewayTokenSecret, ...}`
- Wire the openclaw substrate-backend into the AgentHarnessController via `ext.SubstrateBackends`/`SubstrateProvisioner`.
- RBAC: since the harness Provisioner creates per-harness WorkerPools (not just the shared `poc-pool`), the controller needs broader create/update on `workerpools.ate.dev`. We already grant that — verify.

**Verify:** `helm template` renders both with and without `substrate.harness.enabled=true`. `helm install` produces a healthy controller.

### Stage 6 — Image build for openclaw-in-substrate (~one day, **highest risk**)
**Files (new):**
- `go/core/pkg/sandboxbackend/openshell/openclaw/bootstrap_substrate.go` and `_test.go` (from pj-kagent)
- Possibly a new image build target in Makefile for the openclaw-substrate variant (if pj-kagent shipped one)

The openclaw VM's bootstrap script needs to run cleanly inside a substrate gVisor sandbox. pj-kagent has a `bootstrap_substrate_test.go` that asserts the bootstrap output is valid for substrate; that's the harness for any image surgery needed.

**Risks here are real:**
- We've been hitting `runsc checkpoint pause: exit 128` for the kagent ADK image during golden snapshot. **If openclaw's image triggers the same failure mode, this stage doesn't complete.**
- The openclaw container may have init steps (mount manipulation, kernel features) that substrate's restricted `ActorTemplate.spec.containers` can't carry. Substrate has no `volumes`, no `initContainers`, no `securityContext`, no `privileged`.

**De-risk plan:** before committing to stages 1-5, do a **spike (~half day):** manually craft an `ActorTemplate` with the openclaw image + bootstrap, apply it directly, see if substrate can golden-snapshot it. If yes → stages 1-5 are worth doing. If no → halt; we've found the actual blocker before investing in the integration.

**Verify:** live cluster — apply an AgentHarness with `runtime: substrate, backend: openclaw`. See it reach Ready. Open a Slack/Telegram channel against it. Run an `exec` command. Confirm the VM responds.

### Stage 7 — Docs + demo (~half day)
- New demo YAML: `demo/substrate-poc/06-harness-substrate.yaml` (AgentHarness with `runtime: substrate, backend: openclaw`)
- New section in DEMO.md walking through the harness path
- SUBSTRATE.md §22: AgentHarness-in-substrate, ported from pj-kagent

## Open questions (need answers before committing)

1. **Does openclaw boot cleanly in gVisor?** Spike in Stage 6 answers this. **If no, the whole port is moot.** Suggest running the spike first, before stages 1-5.

2. **Does pj-kagent depend on a substrate fork?** Check their `agent-substrate` checkout vs the upstream we use. If they patched substrate locally (e.g., the 20s golden-snapshot timer), we'd need to either land those patches upstream or carry the fork. **Worth confirming with pj-kagent's author** before starting.

3. **WorkerPool ownership semantics.** pj-kagent's Provisioner can create a per-harness WorkerPool. That means each AgentHarness with `runtime: substrate` could have its own gVisor sandbox pool — vs. our existing model of one shared `poc-pool` for all sandbox-mode Agents. **Design choice**: support both, default to shared pool when no `spec.substrate.workerPool` block is set. This matches pj-kagent's behavior and our existing patterns.

4. **Idle-suspend semantics for harnesses.** Agent CRDs use our `IdleSuspender` (sweep + SuspendActor). Should harness actors auto-suspend the same way? Probably yes, but openclaw's session model (interactive shell) might prefer different idle thresholds. **Decision needed**: per-harness `spec.substrate.idleTimeout` override.

5. **kagent.dev/substrate-actor finalizer overlap.** Today our Agent reconciler uses that finalizer. AgentHarness has its own (`kagent.dev/agent-harness-backend-cleanup`). They don't collide — but both end up calling `DeleteActorSequenced` against the same Control API. No conflict expected.

## Honest risks

- **Substrate runtime instability.** The same `runsc checkpoint pause: exit 128` / `runsc restore: eth0: Link not found` / "sandbox dies during the wait" issues we've been hitting for the kagent ADK will potentially hit openclaw too. **Mitigation: Stage 6 spike up front.** If we can't even get an ActorTemplate to Ready with a manually-crafted openclaw container, the port is paused.

- **Code-volume hidden in tests.** ~270 LOC of tests in pj-kagent's substrate package. Lifting tests is mechanical but they fail in subtle ways (different scheme registration, different ConditionStatus return types between codebases). Budget ~2-3 hours per test file for adaptation.

- **CRD compatibility.** Existing AgentHarness users have CRs without `spec.runtime`. The default must be `openshell` (omit-empty handling) so the upgrade is non-breaking. pj-kagent's schema already does this — port faithfully.

- **Maintenance ongoing.** ~1000 LOC of port becomes ~1000 LOC of code we now maintain. The substrate-side flakes will surface as new bug reports; we'd own triage.

## Total estimate

| Path | Time |
|---|---|
| Stage 6 spike first (de-risk) | 0.5 day |
| If spike passes: stages 1–5 in parallel where possible | 2.5–3 days |
| Stage 6 full + Stage 7 | 1 day |
| **Total focused work** | **3.5–4.5 days** |
| If spike fails: cost of having tried | 0.5 day (no rework, just stop) |

The spike is the load-bearing piece. **Strong recommendation: run Stage 6 spike first.**

## Spike: concrete first move

Before any of the above, ~half a day to answer the single question "can openclaw boot in substrate at all":

1. Get pj-kagent's openclaw substrate image (or build one from their Dockerfile).
2. Hand-write a minimal `ActorTemplate` referencing that image (no AgentHarness CRD yet).
3. Apply against the current cluster.
4. Watch substrate's golden-snapshot phase. Does it reach Ready?
5. If yes → proceed with stages 1-5.
6. If no → look at the actual gVisor failure (we've already developed the diagnostics this session).

If the spike works on the *first* try, that tells us pj-kagent's image is gVisor-compatible and the rest of the port is mechanical.

If the spike fails the same way the kagent ADK has been failing, we know upstream substrate needs a fix and the port is on hold regardless of effort.

---

**Critical files this plan would modify or create:**

```
NEW go/core/pkg/sandboxbackend/substrate/harness_provision.go
NEW go/core/pkg/sandboxbackend/substrate/harness_provision_openclaw.go
NEW go/core/pkg/sandboxbackend/substrate/harness_delete_provision.go
NEW go/core/pkg/sandboxbackend/substrate/harness_openclaw.go
NEW go/core/pkg/sandboxbackend/substrate/harness_*_test.go (5 files)
NEW go/core/pkg/sandboxbackend/openshell/openclaw/bootstrap_substrate.go (+ test)
NEW demo/substrate-poc/06-harness-substrate.yaml
NEW HARNESS-IN-SUBSTRATE-PLAN.md (this file)

MOD go/api/v1alpha2/agentharness_types.go               +80 LOC
MOD go/api/v1alpha2/zz_generated.deepcopy.go            +30 LOC (regen)
MOD go/api/config/crd/bases/kagent.dev_agentharnesses.yaml  (regen)
MOD helm/kagent-crds/templates/kagent.dev_agentharnesses.yaml  (sync)
MOD go/core/internal/controller/agentharness_controller.go +50 LOC
MOD go/core/pkg/app/app.go                              +30 LOC
MOD helm/kagent/values.yaml                             +20 LOC (substrate.harness block)
MOD helm/kagent/templates/controller-configmap.yaml     +10 LOC
MOD demo/substrate-poc/DEMO.md                          +50 LOC (new harness section)
MOD SUBSTRATE.md                                        +150 LOC (new §22)
```
