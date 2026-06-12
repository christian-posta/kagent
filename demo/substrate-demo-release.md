# Demo Guide — declarative agent on Substrate (kagent v0.9.7)

A short walkthrough that goes from "no cluster" to "chatting with a declarative agent running inside a gVisor sandbox" using the **released** kagent (v0.9.7, the first release with PR #1981) and the **released** substrate chart (v0.0.6). **No source builds, no repo clones** — everything pulls from published OCI artifacts.

The companion guide [`substrate-demo.md`](substrate-demo.md) covers the from-source / from-main flow and includes both the harness and the gotchas; this one is the stripped-down release version.

## What this gets you

- A kind cluster running [Agent Substrate](https://github.com/kagent-dev/substrate) in `ate-system`
- kagent v0.9.7 installed via its published Helm chart
- A `SandboxAgent` running on substrate, reachable from the kagent UI

What it deliberately doesn't cover: the `AgentHarness` (openclaw) path, AAuth, idle-suspension. See `substrate-demo.md` for those.

---

## Prerequisites

- `kind`, `kubectl`, `helm` on `PATH`
- Docker Desktop (or equivalent) running
- `OPENAI_API_KEY` exported in your shell

```bash
export OPENAI_API_KEY="sk-..."
```

---

## Step 1 — Create a kind cluster

```bash
kind create cluster --name kagent-substrate
```

The substrate v0.0.6 chart defaults to **JWT auth mode** (Kubernetes ServiceAccount tokens), so a vanilla kind cluster works — no `PodCertificate` feature gate, no custom kind config. If you've used `substrate-demo.md` before, this is much simpler than what was previously required.

---

## Step 2 — Install substrate from its published Helm chart

```bash
helm upgrade --install substrate-crds \
  oci://ghcr.io/kagent-dev/substrate/helm/substrate-crds \
  --version 0.0.6 \
  --namespace ate-system --create-namespace --wait

helm upgrade --install substrate \
  oci://ghcr.io/kagent-dev/substrate/helm/substrate \
  --version 0.0.6 \
  --namespace ate-system --wait --timeout 10m
```

Verify everything is running:

```bash
kubectl get pods -n ate-system
```

You want `ate-api-server`, `ate-controller`, `atelet-*`, `atenet-router`, `valkey-cluster-{0..5}`, `rustfs` all Running (plus a few Completed init Jobs).

---

## Step 3 — Install kagent v0.9.7 with substrate enabled

> **Before you run helm**, verify the OpenAI key really is in the shell. If you forget the `export` from the prerequisites, the install runs silently with an empty key, no `kagent-openai` secret gets created, and the default agent pods land in `CreateContainerConfigError`.
>
> ```bash
> [[ -n "${OPENAI_API_KEY:-}" ]] && echo "key is set (len=${#OPENAI_API_KEY})" || { echo "OPENAI_API_KEY is empty — export it first"; }
> ```
>
> ⚠️ **Don't combine the export with the helm command on one line** — `OPENAI_API_KEY="$(cat ...)" helm ... --set providers.openAI.apiKey="${OPENAI_API_KEY}"` evaluates `${OPENAI_API_KEY}` at parse time (i.e. before the inline assignment runs) and passes an empty string. Either `export` on its own line first, or splice the key directly: `--set providers.openAI.apiKey="$(cat ~/path/to/key)"`.

```bash
helm upgrade --install kagent-crds \
  oci://ghcr.io/kagent-dev/kagent/helm/kagent-crds \
  --version 0.9.7 \
  --namespace kagent --create-namespace --wait

helm upgrade --install kagent \
  oci://ghcr.io/kagent-dev/kagent/helm/kagent \
  --version 0.9.7 \
  --namespace kagent --timeout 10m --wait \
  --set providers.openAI.apiKey="${OPENAI_API_KEY}" \
  --set providers.default=openAI \
  --set controller.substrate.enabled=true \
  --set controller.substrate.ateApiEndpoint=dns:///api.ate-system.svc:443 \
  --set controller.substrate.ateApiInsecure=true \
  --set substrateWorkerPool.create=true \
  --set substrateWorkerPool.replicas=1 \
  --set substrateWorkerPool.ateomImage=ghcr.io/kagent-dev/substrate/ateom-gvisor:v0.0.6
```

The `controller.substrate.*` / `substrateWorkerPool.*` lines are the ones that turn on the substrate integration. Everything else is standard kagent install.

### Tuning the WorkerPool size

`substrateWorkerPool.replicas=1` is the chart default, set explicitly above so the knob is visible. One worker is enough for a declarative-only demo because session actors release their slot the instant they snapshot back to object storage — a single worker can serve many concurrent declarative sessions sequentially. Bump it when:

- You add a long-lived `AgentHarness` (e.g. openclaw). The `ahr-<...>` actor pins a slot for the lifetime of the CR, so you need at least `1 + (number of harnesses)`.
- You want simultaneous, overlapping declarative sessions during the demo (rare at talk-pace; usually fine on 1).

Three ways to change it, depending on permanence:

```bash
# 1) Quick, ephemeral — scale the live CR. Reverts on the next helm upgrade.
kubectl scale workerpool kagent-default -n kagent --replicas=3

# 2) Stick it into the helm release — survives upgrades. Use --reuse-values
#    so you don't have to repeat every --set flag.
helm upgrade kagent oci://ghcr.io/kagent-dev/kagent/helm/kagent \
  --version 0.9.7 --namespace kagent --reuse-values \
  --set substrateWorkerPool.replicas=3

# 3) Fresh install — just change the value on the Step 3 install command above.
```

If helm hits its `--timeout 10m` while waiting on the cold-start pod startup race (controller restarts a couple of times waiting on postgres), wait for the controller manually and continue:

```bash
kubectl wait deploy/kagent-controller -n kagent --for=condition=Available --timeout=10m
```

**Sanity check** — confirm the key landed correctly and the default agents are healthy:

```bash
kubectl get secret kagent-openai -n kagent         # should exist with 1 data entry
kubectl get pods -n kagent | grep -v Running       # only header + Completed jobs expected
```

If you see `CreateContainerConfigError` on the default agent pods, the secret didn't get created — re-run only the second helm command with `--reuse-values --set providers.openAI.apiKey="$(cat ~/path/to/key)"` to patch it in. The deployments will roll to new pods automatically.

---

## Step 4 — Open the kagent UI

```bash
kubectl port-forward -n kagent svc/kagent-ui 8001:8080
```

Open <http://localhost:8001>. Skip the first-run wizard if it appears.

---

## Step 5 — Create the declarative agent on substrate

Pick one of these two paths.

### Option A — via the UI

1. **Create** → **Agent** → choose **Declarative** as the type.
2. Set the basics:
   - **Name**: `hello-substrate`
   - **Namespace**: `kagent`
   - **Model config**: `default-model-config`
   - **Runtime**: `Go` *(required — Python ADK isn't supported on substrate today)*
   - **System message**:
     ```
     You are a friendly assistant living inside an Agent Substrate sandbox.
     When asked who you are, say "I am hello-substrate, a Go ADK declarative
     agent running inside a gVisor actor."
     ```
3. In the **Sandbox** / **Platform** section (label depends on UI version), set **Platform** to **`substrate`** and select the worker pool `kagent-default`.
4. Save.

### Option B — via `kubectl`

Reference manifest at [`demo/agents/hello-substrate.yaml`](agents/hello-substrate.yaml):

```bash
kubectl apply -f demo/agents/hello-substrate.yaml
```

If you don't have this repo checked out, the same manifest inline:

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

### Wait for Ready

```bash
kubectl wait sandboxagent/hello-substrate -n kagent --for=condition=Ready --timeout=5m
```

First-time golden snapshot takes ~60–90s.

---

## Step 6 — Chat with it

In the UI (<http://localhost:8001>), pick **`kagent/hello-substrate`** from the Agents list. Send:

> *What are you, and where are you running? Answer in one sentence.*

Reply:

> *I am hello-substrate, a Go ADK declarative agent running inside a gVisor actor.*

Behind the scenes: a per-session gVisor actor was restored from the golden snapshot, ran the LLM call, and snapshotted itself back to object storage. Open **View → Substrate** to see the actor in the inventory — between requests it'll sit `Suspended`.

---

## Cleanup

```bash
kind delete cluster --name kagent-substrate
```

---

## Differences from the from-source guide

If you've used [`substrate-demo.md`](substrate-demo.md), here's what this release flow skips:

- **No kagent source build** — `oci://ghcr.io/kagent-dev/kagent/helm/kagent:0.9.7`.
- **No substrate source clone** — `oci://ghcr.io/kagent-dev/substrate/helm/substrate:0.0.6`. The substrate chart's default JWT auth mode means a stock kind cluster works, no `PodCertificate` feature gate, no custom kind config.
- **No `ateom-gvisor` build** — the released `ghcr.io/kagent-dev/substrate/ateom-gvisor:v0.0.6` is used directly.
- **No OpenClaw image mirror** — only required for the `AgentHarness` demo, which this guide skips.
- **`WorkerPool` replicas = 1** is fine because there's no long-lived harness; session actors release the slot on every snapshot-back.

If something goes wrong, the troubleshooting section in `substrate-demo.md` ("Gotchas worth knowing before you present") still applies — particularly the helm-install-timeout-while-postgres-starts pattern.

## Credits

The substrate-as-helm-chart approach in this guide was adapted from [AdminTurnedDevOps/agentic-demo-repo](https://github.com/AdminTurnedDevOps/agentic-demo-repo/blob/main/substrate/kagent-substrate-install.md).
