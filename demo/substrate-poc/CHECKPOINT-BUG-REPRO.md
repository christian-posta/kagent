# `runsc checkpoint pause: exit 128` on long-running workloads — substrate repro

Standalone bug repro for the substrate team. Self-contained: this doc plus the
two `spike-actortemplate-{a,b}.yaml` files in this directory are everything you
need to reproduce the failure on your own substrate cluster.

> **STATUS (2026-05-28): FIX VERIFIED ON OUR FORK.** A 5-line patch to
> `cmd/ateom-gvisor/main.go::CheckpointWorkload` makes spike-B (and the
> kagent ADK Declarative-agent path) succeed. The patch tolerates
> post-checkpoint `cmdState`/`cmdDelete` failures (logs a warning,
> continues) instead of failing the RPC. This is intended as an
> upstream PR; the substrate team should review the patch shape (see
> "Proposed fix" below) and decide whether they prefer this approach,
> `runsc checkpoint -resume=true`, or something else.

> **TL;DR (revised after instrumenting the sentry with `-debug-log`, 2026-05-27 evening).**
> The bug is **NOT** in gVisor's checkpoint code. The bug is in substrate's
> `ateom-gvisor/main.go::CheckpointWorkload` — specifically the post-checkpoint
> cleanup sequence. Sequence today:
>
> 1. `runsc checkpoint pause` — **succeeds**; gVisor's normal behavior is to
>    serialize state, SIGKILL all tasks, and exit the sentry. We verified the
>    sentry log shows "Checkpoint completed successfully" and exit status 0.
> 2. `runsc state pause/openclaw` — succeeds (just reads on-disk state).
> 3. `runsc delete -force openclaw` — **FAILS with exit 128**. The sentry
>    that the application container shared is already gone (it exited as
>    part of step 1), but `runsc delete` still tries to connect to the
>    sentry's control server.
>
> The fatal line from the sentry-side debug log is:
> ```
> FATAL ERROR: destroying container "openclaw": connecting to control server at PID 42: connection refused
> ```
> The `runsc checkpoint pause: exit status 128` log line that surfaces to
> the ate-controller is **misleading**: it comes from the *retry* attempts.
> The first cycle fails at the delete step (not the checkpoint step); each
> subsequent retry fails at the checkpoint step because the sandbox is gone
> from the first cycle's already-successful checkpoint.
>
> Earlier sections of this document (kept below for the audit trail) blamed
> the 20s timer, then process-specific gVisor checkpoint incompatibility.
> Both were wrong. The actual cause is much simpler: a sequencing issue in
> substrate's cleanup-after-checkpoint code.

## Why spike-A works and spike-B doesn't (best guess pending substrate team review)

We have not fully pinned down the difference, but observation: spike-A
(`tail -f /dev/null`) reaches Ready while spike-B (openclaw gateway) does
not, on identical CheckpointWorkload logic. Possible factors:

- **Race condition between sentry exit and runsc-state file updates.** With
  a minimal workload (`tail -f`), the sentry exits faster and the
  runsc-state directory is cleaned to a "stopped" state before
  `runsc delete -force` runs; that path short-circuits and returns
  success. With a richer workload (openclaw, kagent ADK), there are more
  tasks the sentry has to SIGKILL, so the sentry exits ~30 ms later;
  by the time substrate's `runsc delete -force` fires, runsc-state still
  indicates "running" and runsc tries to connect to the now-dead sentry
  control plane.
- This explains the apparent process-specificity we documented in earlier
  drafts: it's not the process's syscall mix that matters, it's the
  *latency* of the sentry's cleanup pass, which scales with the number of
  tasks/threads/file descriptors the sentry has to tear down.

If correct, the fix is a substrate-side change in `CheckpointWorkload`'s
cleanup logic — either:
- skip `runsc delete -force <app-container>` entirely when the checkpoint
  already succeeded (the sandbox is gone, there's nothing to delete), or
- retry the delete a few times with backoff to absorb the race, or
- check runsc-state file directly and treat "stopped" sandbox as success.

The substrate team will know which of those is the right shape; we don't
want to land a fix without their review.

---

## Original problem statement (kept for context)

> **Earlier TL;DR (now superseded by the breakthrough finding above).**
> Substrate's golden-snapshot phase (`SuspendActor` at `t=20s`)
> fails reliably with `runsc checkpoint pause: exit 128` for workloads that
> run more than trivial code (e.g., `openclaw gateway run`, kagent's ADK, ...).
> The same `ActorTemplate` shape with `tail -f /dev/null` as the workload
> reaches `Ready`. Failure is **NOT** boot timing — we tested with a 120s
> snapshot deadline and saw the same failure, and we observed it with a
> workload that's fully booted by t=6s.

## Environment

| Component | Value |
|---|---|
| Substrate SHA | `a436ea216ae3ed368e135d58ce765518b0fa1bfb` (main, as of 2026-05-27) |
| Substrate local patch | `cmd/atelet/oci.go`: skip `tar.TypeChar/TypeBlock/TypeFifo` during image unpack — needed for Debian/Wolfi base images. NOT believed to be related to this bug (spike-A succeeds with the same patch on the same image). |
| runsc URL | `gs://gvisor/releases/nightly/2026-05-19/x86_64/runsc` |
| runsc sha256 | `a397be1abc2420d26bce6c70e6e2ff96c73aaaab929756c56f5e2089ea842b63` |
| gVisor version (from runsc startup log) | `release-20260511.0-42-ga7924c4ef10d-dirty, go1.25.5, amd64` |
| Platform | `systrap` |
| Host | kind 1.35.0 cluster (`kindest/node:v1.35.0`) on Docker Desktop / macOS (LinuxKit Linux VM under Apple `Hypervisor.framework`) |
| Pause image | `registry.k8s.io/pause:3.10.2@sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4` |
| Workload image (both spikes) | `ghcr.io/kagent-dev/nemoclaw/sandbox-base:2026.5.4` (public, Debian 12 base, has `openclaw` + `bash`/`curl`/`base64`) |

Substrate installed via `hack/install-ate-kind.sh --deploy-ate-system`.
`WorkerPool` `poc-pool` has 2 replicas backing the `kagent-substrate-poc` namespace.

## Reproduction steps

Both spike `ActorTemplate`s target the same `poc-pool` WorkerPool, use the same
image, the same pause image, the same runsc. The **only** difference is what
the workload container runs.

```bash
# Pre-req: substrate up, WorkerPool 'poc-pool' Ready (2 replicas), namespace
# 'kagent-substrate-poc' exists. (Adjust namespace in YAMLs if needed.)

# Spike A — workload does nothing interesting:
#   command: ["/bin/sh", "-c", "echo 'spike-a bare boot' && tail -f /dev/null"]
kubectl apply -f spike-actortemplate-a.yaml

# Spike B — workload runs the openclaw gateway:
#   command runs `openclaw gateway run --port 80 --allow-unconfigured` in the
#   background, curl-probes it ready, then tails the log.
kubectl apply -f spike-actortemplate-b.yaml

# Watch phase progression:
kubectl get actortemplate -n kagent-substrate-poc -w
```

## Expected vs observed

| ActorTemplate | Workload | Expected | Observed |
|---|---|---|---|
| `spike-a-bare` | `tail -f /dev/null` | Reaches `Ready` after the 20s golden-snapshot timer | ✅ `Ready` in ~25s |
| `spike-b-openclaw` | `openclaw gateway run --port 80 --allow-unconfigured` | Reaches `Ready` after the 20s timer | ❌ Stuck in `WaitGoldenActor`; `runsc checkpoint pause: exit status 128` retried forever |

So: same image, same pause image, same runsc, same WorkerPool, same `snapshotsConfig`,
same `pauseImage`, same RBAC, same kernel — only the workload command changes,
and only the openclaw one fails.

## Failure timeline (spike-B, verbatim from ateom-gvisor container logs)

The full ateom container log dump from one failing run is at the end of this
document. The relevant timeline:

```
t≈0s    INFO  Actor starting  spike-b-openclaw/7ea58451-…
t≈0s    runsc create -bundle=…/pause -pid-file=…/pause.pid pause
t≈0.2s  runsc start pause
t≈0.4s  runsc create -bundle=…/openclaw -pid-file=…/openclaw.pid openclaw
t≈0.4s  runsc start openclaw   (gVisor sandbox running both containers)
t≈0.5s  INFO  Actor started   (RunWorkload RPC returns OK to substrate)
t≈13s   openclaw [gateway] binding to non-loopback address    ← openclaw is up
t≈13s   openclaw [health-monitor] started (interval 300s)
t≈13s   openclaw [canvas] host mounted at http://0.0.0.0:80/__openclaw__/canvas/
t≈18s   ateom log: "gateway up"                                ← curl probe in startup script succeeded
                                                              ← workload provably operational
[no further runsc output — gVisor sandbox dies silently here]
t≈20s   substrate: runsc checkpoint -image-path=… pause       ← golden snapshot attempt
t≈20s   maincli.go: Sandbox isn't running anymore, marking container pause as stopped
t≈20s   util.go: FATAL ERROR: checkpoint failed: cannot checkpoint container "pause" in state stopped
t≈20s   exit status 128
t≈20s+  substrate retries CheckpointWorkload; every retry sees `state stopped` and exits 128
```

**Key observation: there is no error from gVisor between `gateway up` (t≈18s)
and `runsc checkpoint pause` (t≈20s).** The gVisor sandbox transitions from
"running" to "not running" silently from runsc's perspective. runsc was
invoked with `-log-format json --alsologtostderr` so any output from the
sentry would appear in the ateom container's stdout — there is none.

## What we ruled out

1. **The image.** Both spikes use the identical workload image
   (`ghcr.io/kagent-dev/nemoclaw/sandbox-base:2026.5.4`). Spike A reaches
   `Ready` cleanly; only the *process running inside* the image differs.

2. **OCI unpack.** Spike A succeeds, demonstrating the image unpacks correctly
   (with our local atelet patch for `tar.TypeChar/Block/Fifo` entries). Our
   patch is identical for both spikes.

3. **Boot timing (the original 20s-timer hypothesis).** We initially blamed
   substrate's hardcoded `TakeGoldenSnapshotAt = now + 20s` for capturing the
   process mid-init. Two independent experiments disprove this:

   a. We patched `internal/controllers/actortemplate_controller.go` to set the
      snapshot deadline to `now + 120s` instead of `now + 20s`. The kagent ADK
      had a full two minutes to finish initializing — well past any cold-start
      budget. **`runsc checkpoint pause: exit 128` still failed** the same way.
      Patch reverted (it added maintenance burden without fixing anything).

   b. Spike B's workload (openclaw gateway) emits its "gateway up" line at
      **t=18s** — provably fully booted, listening on port 80, well before
      substrate's 20s snapshot deadline. The checkpoint at t=20s still fails.

4. **OpenAI/network dependency.** Earlier we hypothesized the failure was
   specific to processes holding long-lived TLS sockets (kagent ADK → OpenAI).
   Spike B (openclaw `--allow-unconfigured`, no LLM credentials, no outbound
   network) reproduces the same failure with no outbound TLS in the picture.

5. **A particular pod-network state.** Recycling the worker pod between runs
   (to get a known-good `eth0` per §11) does not change the outcome — fresh
   worker pods reproduce the same failure on spike B and the same success on
   spike A.

## What we're asking the substrate team

After the breakthrough finding above, the questions are simpler:

1. **Is the post-checkpoint `runsc delete -force <app-container>` in
   `CheckpointWorkload` necessary?** The sentry is already gone (the
   successful `runsc checkpoint pause` killed it as part of normal
   operation). Could the call be removed, or wrapped in a "tolerate
   sandbox-already-gone" check that swallows the
   `connection refused` error?

2. **Should `CheckpointWorkload` differentiate "checkpoint succeeded but
   cleanup failed" from "checkpoint failed"?** Today both surface as the
   same error to the ate-controller, which retries `CheckpointWorkload`
   forever even though the first call already produced a valid checkpoint
   on disk. The controller could instead transition to `Ready` once the
   checkpoint is in object storage and treat downstream cleanup as
   best-effort.

3. **Why does spike-A (`tail -f /dev/null`) succeed while spike-B
   (openclaw) fails through the same code path?** Our best guess is a
   race between sentry exit and runsc-state file updates — workloads
   with more tasks/threads/fds take longer for the sentry to tear down,
   widening the race window. A few specific ideas:
   - Is it really a race, or is there a deterministic difference in
     `runsc delete -force`'s codepath that we're missing?
   - If race, would a short retry loop in `cmdDelete` (or in
     `CheckpointWorkload` after `cmdCheckpoint`) close it?
   - Does gVisor have a flag (`-resume=false` is the default; maybe a
     `-leave-state-clean` option?) that ensures runsc-state is settled
     before `runsc checkpoint` returns?

4. **Is there a `runsc checkpoint -resume` use case we should adopt
   instead?** `runsc checkpoint` supports `-resume` which serializes
   state without killing the tasks. If substrate used `-resume=true`
   for the golden snapshot, the sandbox would stay alive after the
   snapshot, the follow-up `runsc delete -force` would target a still-
   running sandbox (which it can shut down cleanly), and the whole
   sequence would be ordering-safe. We didn't test this — wasn't sure
   if it's the intended pattern for golden snapshots.

5. **Separate but related: GKE install path.** Substrate's installer
   currently hangs on GKE clusters because `install-ate.sh
   --deploy-ate-system` waits for `certificates.k8s.io/v1beta1.ClusterTrustBundle`
   which GKE does not serve in stable, rapid, or alpha-flag clusters. Is
   there a documented GKE-supported install path, or an upstream issue
   tracking this? We tried `--enable-kubernetes-alpha` (didn't unlock
   v1beta1) and the next-newer rapid-channel version (didn't help).

## Evidence — what the sentry-side debug logs actually show

Captured 2026-05-27 evening after instrumenting substrate's
`cmd/ateom-gvisor/runsc.go` to add `-debug -debug-log=<actor-dir>/runsc-debug-logs/<container>/ -debug-log-format=json -panic-log=<...>/panic.log`
to all runsc invocations (the toggles were already pre-staged as
commented-out lines in the substrate source — see step 1 of the
"Diagnostic plan" section below). The sentry now writes per-runsc-command
files to `/run/ateom-gvisor/actors/<actor>/runsc-debug-logs/<container>/`
on the worker pod's host (it's a hostPath mount; reach the kind node via
`docker exec`).

### The first checkpoint's sentry log proves the checkpoint succeeded

`runsc-debug-logs/pause/runsc.log.<ts>.boot.txt` (the sentry process,
not the runsc CLI):

```
state.go:159] Save CPU usage: 603.892559ms
state.go:161] Save succeeded: exiting...
watchdog.go:202] Starting watchdog, period: 45s, timeout: 3m0s, action: logWarning
state.go:116] Tasks resumed after save.
kernel_restore.go:113] Checkpoint completed successfully.
urpc.go:372] urpc: RPC call for method containerManager.Checkpoint succeeded.
task_signals.go:200] [   5(   4):  10(   9)] Signal 9, PID: 5, TID: 10, fault addr: 0x0: terminating thread group
...  (sentry SIGKILLs all task groups — pause PID 1, openclaw PID 2, app PIDs 5, 44, ...)
loader.go:1423] Gofer socket disconnected, killing container "pause"
loader.go:1423] Gofer socket disconnected, killing container "openclaw"
boot.go:674] application exiting with exit status 0
watchdog.go:218] Stopping watchdog
cli.go:310] Exiting with status: 0
```

And the first `runsc checkpoint` CLI log
(`runsc-debug-logs/pause/runsc.log.<ts>.checkpoint.txt`):

```
container.go:827] Checkpoint container, cid: pause
sandbox.go:1622] Checkpoint sandbox "pause", imagePath "/run/.../checkpoint", opts {...Resume:false...}
sandbox.go:836] Connecting to sandbox "pause"
urpc.go:592] urpc: successfully marshalled 272 bytes.
urpc.go:635] urpc: unmarshal success.
cli.go:310] Exiting with status: 0
```

Both exit 0. The checkpoint succeeded. The sandbox then exits cleanly,
as designed (`Resume:false` is gVisor's default for `runsc checkpoint`).

### The follow-up delete fails with `connection refused`

`runsc-debug-logs/openclaw/runsc.log.<ts>.delete.txt`:

```
cli.go:280] runsc process spawned at 22:46:33.425489
cli.go:283] **************** gVisor ****************
container.go:906] Destroy container, cid: openclaw
container.go:1255] Destroying container, cid: openclaw
sandbox.go:2129] Destroying container, cid: openclaw, sandbox: pause
sandbox.go:836] Connecting to sandbox "pause"
container.go:930] stopping container: destroying container "openclaw": destroying container "openclaw": connecting to control server at PID 42: connection refused
util.go:107] FATAL ERROR: destroying container: stopping container: destroying container "openclaw": destroying container "openclaw": connecting to control server at PID 42: connection refused
```

PID 42 is the sentry process that **just successfully exited** as part
of the prior `runsc checkpoint pause`. `runsc delete -force` tries to
connect to that sentry's control server, gets `connection refused`,
and exits 128. This is the actual cause of the failure that surfaces
to the ate-controller as `exit status 128`.

### Substrate's `CheckpointWorkload` flow that produces this

`cmd/ateom-gvisor/main.go:296-369`:

```go
func (s *AteomService) CheckpointWorkload(ctx context.Context, req ...) (..., error) {
    // ...
    // Checkpoint pause container (root of the sandbox)
    if err := rcmd.cmdCheckpoint(ctx, "pause", checkpointPath); err != nil {
        return nil, fmt.Errorf("while checkpointing pause: %w", err)
    }
    // After this point the sentry has SIGKILLed all tasks and exited.
    // The runsc-state files may not yet reflect the dead sandbox.

    // cmdState calls — succeed (read on-disk state)
    if err := rcmd.cmdState(ctx, "pause"); err != nil { ... }
    for _, ctr := range req.GetSpec().GetContainers() {
        if err := rcmd.cmdState(ctx, ctr.GetName()); err != nil { ... }
    }

    // cmdDelete calls — FAIL with "connecting to control server: connection refused"
    for _, ctr := range req.GetSpec().GetContainers() {
        if err := rcmd.cmdDelete(ctx, ctr.GetName()); err != nil {
            return nil, fmt.Errorf("while deleting %q application container: %w", ctr.GetName(), err)
        }
    }
    // never reached:
    if err := rcmd.cmdDelete(ctx, "pause"); err != nil { ... }
    // ...
}
```

The error wrapper at this layer says `while deleting "openclaw"
application container: while running \`runsc delete\`: exit status 128`,
which is what bubbles up to ate-controller. The real cause is hidden
two layers deeper: `connecting to control server: connection refused`.

### Why this loops forever

The ActorTemplate controller treats the `CheckpointWorkload` RPC failure
as transient and retries (`error: while suspending golden actor: rpc
error: code = Internal/Unknown ...`). Each retry calls
`CheckpointWorkload` again. On retry:

- `cmdCheckpoint(pause)` now fails immediately because the sandbox is
  already gone from the first cycle's successful checkpoint — runsc
  prints `Sandbox isn't running anymore, marking container pause as
  stopped` and then `FATAL ERROR: checkpoint failed: cannot checkpoint
  container "pause" in state stopped`.
- This produces the `exit status 128` line that ate-controller logs
  forever after.

So the visible-to-operators error (`runsc checkpoint pause: exit 128 /
state stopped`) is **the retry symptom, not the root cause**. The root
cause is the failed `runsc delete -force <app-container>` on the very
first cycle.

## Proposed fix (verified on our fork 2026-05-28)

The minimal patch is to `cmd/ateom-gvisor/main.go::CheckpointWorkload`:
demote `cmdState`/`cmdDelete` failures after a successful `cmdCheckpoint`
from fatal errors to warnings. The post-checkpoint runsc-state files
haven't yet been updated to "stopped" when the next runsc call fires
(race with sentry exit cleanup), so runsc tries to connect to the now-
dead sentry's control server and exits 128. Since the checkpoint already
wrote a valid image to object storage, and atelet handles actor-
directory teardown (per the contract comment at line 304-305), these
cleanup errors are post-success hiccups, not actual checkpoint failures.

### The patch

```go
// cmd/ateom-gvisor/main.go (around line 324)

// Check state of all containers to mimic containerd.
//
// Without this, `runsc delete` occasionally throws an error.
//
// Post-checkpoint, the sentry has exited (Resume:false is the default for
// runsc checkpoint), so cmdState and cmdDelete will hit a race between the
// sentry exit and the runsc-state files updating to "stopped". When the
// race lands wrong, runsc tries to connect to the dead sentry's control
// server and fails with "connection refused" → exit 128. We tolerate that
// here: the checkpoint already wrote a valid image to object storage, and
// atelet is responsible for tearing down the OCI bundles and resetting the
// actor directory (see contract above). cmdState/cmdDelete failures here
// are post-success cleanup hiccups, not actual checkpoint failures.
if err := rcmd.cmdState(ctx, "pause"); err != nil {
    slog.WarnContext(ctx, "post-checkpoint runsc state failed (sandbox already gone); continuing",
        slog.String("container", "pause"), slog.String("error", err.Error()))
}
for _, ctr := range req.GetSpec().GetContainers() {
    if err := rcmd.cmdState(ctx, ctr.GetName()); err != nil {
        slog.WarnContext(ctx, "post-checkpoint runsc state failed (sandbox already gone); continuing",
            slog.String("container", ctr.GetName()), slog.String("error", err.Error()))
    }
}

// Delete all application containers. Same race as above — tolerate the
// "sandbox already gone" failure.
for _, ctr := range req.GetSpec().GetContainers() {
    if err := rcmd.cmdDelete(ctx, ctr.GetName()); err != nil {
        slog.WarnContext(ctx, "post-checkpoint runsc delete failed (sandbox already gone); continuing",
            slog.String("container", ctr.GetName()), slog.String("error", err.Error()))
    }
}

// Delete pause container. Same race.
if err := rcmd.cmdDelete(ctx, "pause"); err != nil {
    slog.WarnContext(ctx, "post-checkpoint runsc delete failed (sandbox already gone); continuing",
        slog.String("container", "pause"), slog.String("error", err.Error()))
}
```

### Verification

Applied the patch, rebuilt `ateom-gvisor` via `ko build`, pushed to
`localhost:5001/ateom-gvisor:latest`, recycled worker pods. Then
re-applied spike-B (`openclaw gateway run`, which had been failing
reliably for days):

```
12:35:25 phase=ResumeGoldenActor
12:35:30 phase=ResumeGoldenActor
12:35:36 phase=ResumeGoldenActor
12:35:41 phase=ResumeGoldenActor
12:35:46 phase=WaitGoldenActor
12:35:51 phase=WaitGoldenActor
12:35:56 phase=WaitGoldenActor
12:36:02 phase=WaitGoldenActor
12:36:07 phase=Ready    🎉
```

ActorTemplate reaches Ready in ~42s. Golden snapshot ID:
`040c6cc0-4ef4-4ec9-8e7b-fe1842f18e08`.

The warning fires as expected, confirming the race triggered and the
patch caught it:

```
2026-05-28T12:36:01.007Z INFO  Actor checkpointing
2026-05-28T12:36:01.605Z WARN  post-checkpoint runsc delete failed (sandbox already gone); continuing
                               container=openclaw error=while running `runsc delete`: exit status 128
2026-05-28T12:36:01.667Z INFO  Actor checkpointed
```

So:
- `runsc delete openclaw` still fails with `exit 128` (confirming the race
  is real and reproducible on this run too).
- Our patch demotes it to a warning, lets `CheckpointWorkload` complete,
  and the ActorTemplate reaches `Ready`.
- Total elapsed time from `Actor checkpointing` to `Actor checkpointed`:
  660 ms.

`demo-alpha` (the FastAPI BYO agent) continues to work end-to-end through
the unified A2A path on the patched ateom-gvisor — `request #4` echoed
back correctly from the same Python process across multiple suspend/
resume cycles. No regression.

### What the patch does NOT fix

The patch addresses the **golden-snapshot orchestration** failure inside
`CheckpointWorkload`. Two failures we observed in this session are NOT
covered by it:

1. **The kagent ADK's post-resume runtime.** With the patch,
   `k8s-agent-substrate` reaches `Ready` and `RestoreWorkload`
   returns success on each request, but the in-sandbox ADK process
   returns HTTP 500 on every A2A `message/send`. The restored process
   appears to have invalid file-descriptor state (open httpx TLS pools
   pointing at network state from pre-suspend, asyncio loop bookkeeping,
   etc.). This is what SUBSTRATE.md §17 was working on with the
   `_substrate_checkpoint_friendly.install()` hook — it's a separate
   gVisor / app-state compatibility issue that exists at the
   application level, not in substrate's checkpoint orchestration.

2. **Stuck `STATUS_SUSPENDING` actors.** Even with the patch, when an
   ActorTemplate is deleted, its actor sometimes wedges in
   `STATUS_SUSPENDING` until the assigned worker pod is force-killed.
   Documented in SUBSTRATE.md §11. Unchanged by our patch since it's a
   separate flow.

The fix is specifically: **the golden-snapshot phase reliably succeeds
for any workload that gVisor can `runsc checkpoint` cleanly**, instead
of being held hostage to a post-checkpoint cleanup race.

### Open questions for the substrate team (what to confirm before PR)

1. **Are `cmdState`/`cmdDelete` here ever load-bearing on the success
   path?** The contract comment at main.go:304-305 says atelet handles
   actor-directory teardown after `CheckpointWorkload` returns. If
   that's literally the case, the `cmdDelete` calls in
   `CheckpointWorkload` are belt-and-suspenders and can be made
   best-effort safely.

2. **Race window vs deterministic difference?** Spike-A succeeds and
   spike-B fails on the same code path. Our best guess is a race
   window that scales with how many tasks/threads/fds the sentry has to
   tear down on exit. If true, the warning will fire occasionally even
   for small workloads on slow hosts. Confirm whether this is the right
   mental model, or if there's a deterministic state-file lifecycle
   difference we're missing.

3. **`runsc checkpoint -resume=true` as an alternative?** A different
   shape would be to keep the sandbox alive after the snapshot
   (`Resume:true` flag on the checkpoint RPC). The follow-up
   `cmdDelete` would then talk to a still-live sentry and succeed
   cleanly. The trade-off: holds the worker pod busy longer per
   snapshot. We didn't try this — substrate team can say whether
   that's the preferred direction.

4. **Should the comment at line 326 (`// Without this, runsc delete
   occasionally throws an error.`) get updated?** That comment implies
   `cmdState` is itself a workaround for a race. The current patch
   doesn't remove the workaround — it just makes the workaround's
   failure non-fatal.

## Verbatim ateom log fingerprint to grep for

When the bug fires you will see this sequence in the ateom-gvisor container's
log (one block per retry — substrate retries `CheckpointWorkload` aggressively):

```
INFO  About to run runsc checkpoint  container=pause
runsc args: [... -log-format json --alsologtostderr -allow-connected-on-save
            -root /run/ateom-gvisor/actors/<actor>/runsc-state checkpoint
            -image-path /run/ateom-gvisor/actors/<actor>/checkpoint pause]
W     Sandbox isn't running anymore, marking container pause as stopped:
W     FATAL ERROR: checkpoint failed: cannot checkpoint container "pause" in state stopped
INFO  Handle RPC method=/ateom.Ateom/CheckpointWorkload err="while checkpointing pause: while running `runsc checkpoint`: exit status 128"
```

The first checkpoint attempt in each cycle is slightly different — substrate
first tries to delete the `openclaw` application container and that fails too:

```
INFO  Handle RPC method=/ateom.Ateom/CheckpointWorkload err="while deleting \"openclaw\" application container: while running `runsc delete`: exit status 128"
```

(After that initial attempt, substrate gives up on the application container
and retries `runsc checkpoint pause` directly, getting `state stopped` each
time.)

## How to clean up the wedged state

Spike B leaves the assigned worker in a stuck `ASSIGNED` state because the
actor sits in `STATUS_SUSPENDING` forever. The workaround documented in our
SUBSTRATE.md §11 is to delete the worker pod manually; the WorkerPool will
respawn a clean replacement:

```bash
kubectl delete -f spike-actortemplate-b.yaml
ASSIGNED=$(kubectl ate get workers 2>/dev/null | awk 'NR>1 && $4=="ASSIGNED" {print $3}' | head -1)
[ -n "$ASSIGNED" ] && kubectl delete pod -n kagent-substrate-poc "$ASSIGNED"
```

## Diagnostic plan — getting more information out of runsc

Source-grounded notes from reading the gVisor tree at
`~/go/src/github.com/google/gvisor` (release-20260511.0-42-ga7924c4ef10d).

### What `exit 128` actually means

The exit code is unremarkable. The line

```
FATAL ERROR: checkpoint failed: cannot checkpoint container "pause" in state stopped
```

is emitted at `runsc/cmd/checkpoint.go:128` via `util.Fatalf`, which always
calls `os.Exit(128)` (see `runsc/cmd/util/util.go:130`). The "state stopped"
half comes from `container.CheckStopped` at `runsc/container/container.go:2276`
— runsc tries to query the sentry, the RPC fails, runsc concludes the sandbox
is dead and marks the container `Stopped`, and the checkpoint then fails as a
downstream consequence.

So `exit 128` tells us nothing beyond "runsc CLI hit a fatal path." The
question we need to instrument is **why the sandbox dies silently between
t≈18s (gateway up) and t≈20s (SuspendActor begins)**.

### Why we currently see no sentry output

The runsc args substrate constructs include `--alsologtostderr`, but that
flag only mirrors the **runsc CLI process's** log entries to stderr. The
**sentry** runs as a separate `runsc boot` process; unless `-debug-log` is
set, it has no log destination at all and its warnings/panics go nowhere.
That is the root reason this failure is silent.

### Changes to try, in priority order

1. **Add `-debug -debug-log -panic-log` to the runsc flags atelet builds.**

   Locate where atelet constructs the runsc argv (currently passes
   `-log-format json --alsologtostderr -allow-connected-on-save -root …`)
   and add:

   ```
   -debug
   -debug-log=/var/log/runsc/%TIMESTAMP%-%COMMAND%.log
   -panic-log=/var/log/runsc/panic.log
   -debug-log-format=json
   ```

   The `%TIMESTAMP%` and `%COMMAND%` substitutions are documented at
   `runsc/config/flags.go:72`. A writable mount is required in the
   ateom-gvisor container (hostPath or emptyDir on `/var/log/runsc`)
   because the image is distroless. After the next failed run, the
   sentry's own log stream will be in that directory — that is the
   single highest-leverage data point the substrate team will want.

2. **Capture a live stack dump just before the checkpoint deadline.**

   While the sandbox is still alive at ~t=19s (after the curl probe
   confirms `gateway up`), run from inside the ateom-gvisor container's
   namespaces:

   ```
   runsc -root /run/ateom-gvisor/actors/<actor>/runsc-state \
       debug --stacks <container-id>
   runsc -root /run/ateom-gvisor/actors/<actor>/runsc-state \
       debug --ps <container-id>
   ```

   `runsc/cmd/debug.go` exposes `--stacks`, `--ps`, `--profile-heap`,
   `--strace=all`, `--signal=…`. Stacks at t=19s plus a `kill -0
   $sentry_pid` polling loop between t=18s and t=20s will distinguish
   "sentry exits" from "sentry wedges/deadlocks."

3. **Add `-strace` to the runsc create flags for spike-B only.**

   This catches whether openclaw issues a syscall the sentry rejects
   right before sandbox death. Verbose, but spike-B's workload is small
   so the volume is tractable.

4. **Check host kernel logs.**

   Inside the kind node (`docker exec -it <kind-container> dmesg -T |
   tail -200`) after a failed run. If the sentry was OOM-killed by the
   host kernel it shows up there. systrap-platform sentries can be
   memory-hungry, and the LinuxKit VM under Apple Hypervisor.framework
   has a fixed RAM budget.

5. **Get a shell into the distroless ateom-gvisor container.**

   ```
   kubectl debug -n kagent-substrate-poc <worker-pod> \
       --image=busybox --target=ateom-gvisor -it -- sh
   ```

   The ephemeral container shares ateom-gvisor's mount/PID namespaces,
   so `/run/ateom-gvisor/actors/<actor>/` is browsable and any debug-log
   files we configure in step 1 are readable post-mortem.

6. **Differential: try without `-allow-connected-on-save`, and try
   `-platform=ptrace`.**

   `-allow-connected-on-save` is the flag that lets checkpoint proceed
   with established sockets; if openclaw's gateway leaves a listening
   socket bound to a non-loopback address (we observed this in the log
   at t≈13s: `binding to non-loopback address`), and that save path is
   recent/lightly-tested, removing the flag should produce a loud,
   immediate refusal instead of silent death — itself useful signal.
   Switching from `-platform=systrap` to `-platform=ptrace` rules out
   a systrap-specific issue on the nested-virt host.

### Suggested order of operations for the next debugging session

1. Patch atelet to add `-debug -debug-log -panic-log` and mount a
   writable `/var/log/runsc` (~30 min change).
2. Re-run spike-B; copy the resulting debug-log + panic-log out via
   `kubectl debug`.
3. If the debug-log shows the sentry's own perspective on death (panic,
   OOM, syscall denial, etc.), we have our answer and ship it to
   substrate as a follow-up to this report.
4. If the debug-log is also silent, escalate to `runsc debug --stacks`
   timing experiments and host-side `dmesg`.

## Pointers into our docs

- `SUBSTRATE.md §22` — full root-cause-revision narrative for the kagent side
  (covers both the 120s timer patch experiment and the openclaw spike).
- `SUBSTRATE.md §11` — the original risk table entry, now updated to reflect
  the corrected understanding.
- `SUBSTRATE.md §17` — `_substrate_checkpoint_friendly.install()` hook: closes
  httpx TLS pools 3s after each request. Makes the *first* suspend cycle work
  for the kagent ADK. The *second* suspend (after resume) still fails, which
  is consistent with the "sandbox dies during the wait window" interpretation
  rather than a "process state can't be serialized" interpretation.
- `HARNESS-IN-SUBSTRATE-PLAN.md` (top of file) — the spike result that drove
  the harness-port-on-substrate decision.

---

## Appendix: full ateom log capture from one failing spike-B run

A 638-line capture (the ateom container's stdout for the duration of one
spike-B reconcile cycle) was saved to `/tmp/spike-worker-ateom.log` during
this debugging session. Key lines:

| Line | Time | Event |
|---|---|---|
| 211 | 19:02:45.381 | `Actor starting` (substrate RunWorkload begins) |
| 245 | 19:02:45.465 | `runsc create … pause` |
| 271 | 19:02:45.659 | `runsc start pause` |
| 301 | 19:02:45.758 | `runsc create … openclaw` |
| 313 | 19:02:45.790 | `runsc start openclaw` |
| 326 | 19:02:45.891 | `Actor started` (RunWorkload returns OK) |
| 328 | 19:03:03.832 | `gateway up` (ateom-side curl probe succeeded) |
| 330 | 19:03:03.846 | openclaw `[gateway] binding to non-loopback address` |
| 344 | 19:03:05.008 | `About to run runsc checkpoint` (substrate's SuspendActor begins, 20s after RunWorkload) |
| 417 | 19:03:05.674 | `while deleting "openclaw" application container … exit status 128` |
| 419 | 19:03:05.713 | Next attempt: `About to run runsc checkpoint` (pause-only) |
| 421 | 19:03:05.736 | `Sandbox isn't running anymore, marking container pause as stopped` |
| 432 | 19:03:05.737 | `FATAL ERROR: checkpoint failed: cannot checkpoint container "pause" in state stopped` |
| 434 | 19:03:05.740 | `Handle RPC … err="while checkpointing pause: while running \`runsc checkpoint\`: exit status 128"` |
| 436+ | 19:03:05.757+ | Substrate keeps retrying `CheckpointWorkload`, same error every time |

Can ship the full log on request — it's not committed to the repo (debug
artifact, redacted via `git` history caution).
