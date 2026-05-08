# kagent + OpenShell / OpenClaw Demo Guide

This guide walks through standing up a local kagent + OpenShell integration using a kind cluster. It documents the exact steps and workarounds discovered during initial bring-up.

---

## Repositories

| Repo | Branch | Local path |
|------|--------|------------|
| `kagent-dev/kagent` (OSS) | `main` | `~/go/src/github.com/kagent-dev/kagent` |
| `kagent-dev/OpenShell` | `feat/k8s-supervisor-sideload-fork` | `~/go/src/github.com/kagent-dev/OpenShell` |
| `solo-io/kagent-enterprise` | `main` | `~/go/src/github.com/solo-io/kagent-enterprise` |

> **Note:** `kagent-enterprise` plays no role in this demo — all work is in the OSS kagent and OpenShell repos.

---

## Prerequisites

- Docker Desktop running
- `kind`, `kubectl`, `helm` installed
- `cargo` / Rust installed (for the OpenShell CLI)
- `z3` installed: `brew install z3` (required to compile the CLI)
- `OPENAI_API_KEY` available

---

## Step 0 — Checkout the right branches

```shell
# kagent OSS — use main (AgentHarness/OpenShell integration merged via PR #1809)
cd ~/go/src/github.com/kagent-dev/kagent
git fetch upstream
git reset --hard upstream/main

# OpenShell — clone if needed, then switch to the k8s branch
# NOTE: feat/k8s-supervisor-sideload-fork is still required; not yet merged to main
git clone https://github.com/kagent-dev/OpenShell ~/go/src/github.com/kagent-dev/OpenShell
cd ~/go/src/github.com/kagent-dev/OpenShell
git checkout feat/k8s-supervisor-sideload-fork
```

---

## Step 1 — Create the kind cluster

```shell
cd ~/go/src/github.com/kagent-dev/kagent
make create-kind-cluster
```

This creates a kind cluster named `kagent` with MetalLB and a local image registry.

> **Gotcha:** The existing `kind-registry` container may already be running on port `5000` instead of the expected `5001`. Fix this by configuring containerd on the node to resolve `localhost:5000`:
>
> ```shell
> docker exec kagent-control-plane mkdir -p /etc/containerd/certs.d/localhost:5000
> docker exec -i kagent-control-plane sh -c \
>   'cat > /etc/containerd/certs.d/localhost:5000/hosts.toml' <<'EOF'
> [host."http://kind-registry:5000"]
> EOF
> ```
>
> Then use `DOCKER_REGISTRY=localhost:5000` in Step 4.

---

## Step 2 — Build and load OpenShell images

```shell
cd ~/go/src/github.com/kagent-dev/OpenShell

IMAGE_TAG=local bash tasks/scripts/docker-build-image.sh supervisor
IMAGE_TAG=local bash tasks/scripts/docker-build-image.sh gateway
```

> **Gotcha:** `kind load docker-image` fails for these images with "failed to detect containerd snapshotter". Use `docker save | ctr import` instead:
>
> ```shell
> docker save openshell/supervisor:local | \
>   docker exec -i kagent-control-plane ctr -n k8s.io images import -
>
> docker save openshell/gateway:local | \
>   docker exec -i kagent-control-plane ctr -n k8s.io images import -
> ```

---

## Step 3 — Deploy the OpenShell gateway

Create `values-local.yaml` in the OpenShell repo:

```yaml
image:
  repository: openshell/gateway
  tag: local
  pullPolicy: Never

supervisor:
  image:
    repository: openshell/supervisor
    tag: local
  sandboxImagePullPolicy: IfNotPresent

server:
  disableTls: true
  disableGatewayAuth: true

service:
  metricsPort: 0
```

Then install:

```shell
helm upgrade --install openshell deploy/helm/openshell \
  -n openshell --create-namespace \
  --kube-context kind-kagent \
  -f values-local.yaml

kubectl -n openshell create secret generic openshell-ssh-handshake \
  --from-literal=secret=$(openssl rand -hex 32)

kubectl apply -f deploy/kube/manifests/agent-sandbox.yaml

kubectl -n openshell rollout status statefulset/openshell --timeout=120s
```

---

## Step 4 — Install kagent

```shell
cd ~/go/src/github.com/kagent-dev/kagent

OPENAI_API_KEY=<your-key> DOCKER_REGISTRY=localhost:5000 make helm-install
```

> **Note:** `OPENAI_API_KEY` must be set — the chart (as of main) creates a `kagent-openai` Secret from it. If the variable is empty the secret is skipped and all agent pods will fail with `secret "kagent-openai" not found`.

This builds all kagent images, pushes them to the local registry, and deploys via helm to the `kagent` namespace.

### Enable the AgentHarness controller

The AgentHarness controller is disabled by default — it requires `--openshell-gateway-url`. Patch the deployment after install:

```shell
kubectl patch deployment kagent-controller -n kagent --type=json -p='[
  {
    "op": "add",
    "path": "/spec/template/spec/containers/0/args",
    "value": [
      "--openshell-gateway-url=dns:///openshell.openshell.svc.cluster.local:8080",
      "--openshell-insecure=true"
    ]
  }
]'

kubectl rollout status deployment/kagent-controller -n kagent --timeout=60s
```

Verify the controller started the AgentHarness controller:

```shell
kubectl logs -n kagent -l app.kubernetes.io/component=controller --tail=30 | grep -i "harness\|agentharness"
# Expected: {"msg":"Starting Controller","controller":"agentharness",...}
```

---

## Step 5 — Create the AgentHarness CRD

> **Note:** As of commit `1939749e`, the `Sandbox` CRD has been renamed to `AgentHarness` (`agentharnesses.kagent.dev/v1alpha2`).

> **Gotcha:** The image referenced in older docs (`pj3677/nemoclaw-sandbox-base:2026.5.4`) is `linux/arm64` only. On an `amd64` kind node, use the official NVIDIA image which has both architectures:

```yaml
apiVersion: kagent.dev/v1alpha2
kind: AgentHarness
metadata:
  name: my-claw
  namespace: kagent
spec:
  backend: openclaw
  image: ghcr.io/nvidia/nemoclaw/sandbox-base:latest
  modelConfigRef: default-model-config
  description: "my openclaw agent"
```

```shell
kubectl apply -f <above-yaml>
```

Wait for it to be ready:

```shell
kubectl get agentharness.kagent.dev -n kagent -w
# NAME      BACKEND    READY   ID               AGE
# my-claw   openclaw   True    kagent-my-claw   ...
```

The kagent controller automatically:
1. Sets up an OpenAI provider pointing at the cluster inference proxy
2. Writes `~/.openclaw/openclaw.json` into the sandbox pod
3. Starts `openclaw gateway run --port 18800` inside the pod

---

## Step 6 — Build the OpenShell CLI (optional, for troubleshooting)

Requires Rust 1.88+ and z3:

```shell
brew install z3
rustup update stable

cd ~/go/src/github.com/kagent-dev/OpenShell
cargo build -p openshell-cli --bin openshell
# binary at: target/debug/openshell
```

---

## Step 7 — Port-forward and use the CLI

> **Gotcha:** Port-forward to the **pod directly**, not the service. `svc/openshell` intercepts gRPC traffic incorrectly and returns `route not found`.

```shell
# Correct:
kubectl -n openshell port-forward pod/openshell-0 8080:8080 &

# NOT this (gRPC broken via service):
# kubectl -n openshell port-forward svc/openshell 8080:8080
```

Then use the CLI:

```shell
cd ~/go/src/github.com/kagent-dev/OpenShell

./target/debug/openshell --gateway-endpoint http://127.0.0.1:8080 sandbox list
./target/debug/openshell --gateway-endpoint http://127.0.0.1:8080 sandbox get kagent-my-claw

# Connect a shell into the sandbox:
./target/debug/openshell --gateway-endpoint http://127.0.0.1:8080 sandbox connect kagent-my-claw
```

Or register the gateway to avoid passing `--gateway-endpoint` every time:

```shell
./target/debug/openshell gateway add --name local --endpoint http://127.0.0.1:8080
./target/debug/openshell gateway select local
./target/debug/openshell sandbox list
```

> **Note:** The gateway registration is stored in `~/.config/openshell/gateways.json`.

---

## Step 8 — Use the OpenClaw TUI inside the sandbox

The openclaw gateway runs inside the sandbox pod on port 18800. To access it:

```shell
kubectl exec -it -n openshell kagent-my-claw -- bash
# then inside the pod:
openclaw tui --local
```

> **Gotcha:** `openclaw tui` (without flags) tries to connect to a remote cloud gateway and fails with "Missing gateway auth token". `openclaw tui --url ws://127.0.0.1:18800` fails with "gateway url override requires explicit credentials". Use `--local` which reads the local config directly (`gateway.mode: local`, port 18800).

> **Gotcha:** The openclaw gateway may crash on first startup due to the `bonjour` plugin calling `networkInterfaces()`, which is blocked by the sandbox's network policy. If the TUI can't connect, check and restart:
>
> ```shell
> # inside the sandbox pod:
> cat /tmp/openclaw-gateway.log | tail -30
>
> # If crashed, disable bonjour and restart:
> openclaw plugins disable bonjour
> HOME=/sandbox openclaw gateway run --port 18800 > /tmp/gw2.log 2>&1 &
> ```

---

## Step 9 — Add Telegram (optional)

1. Create a bot via `@BotFather` on Telegram, save the token
2. Get your Telegram user ID from `@RawDataBot`
3. Patch the AgentHarness:

```shell
kubectl patch agentharness.kagent.dev my-claw -n kagent --type=merge -p '{
  "spec": {
    "channels": [{
      "name": "telegram",
      "type": "telegram",
      "telegram": {
        "allowedUserIDs": ["<your-telegram-user-id>"],
        "botToken": {"value": "<bot-token>"}
      }
    }]
  }
}'
```

> Note: Use the custom image `pj3677/nemoclaw-sandbox-base:2026.5.4` (arm64) for Telegram support as it contains a newer openclaw build with the Telegram fix. On amd64, wait for an updated multi-arch image.

---

## Current Status

| Component | Status | Namespace |
|-----------|--------|-----------|
| Kind cluster `kagent` | Running | — |
| MetalLB | Running | `metallb-system` |
| Local registry | Running | `localhost:5000` |
| OpenShell gateway (`openshell-0`) | Running | `openshell` |
| kagent controller + agents | Running | `kagent` |
| AgentHarness `my-claw` (openclaw) | `READY=True` | `kagent` |
| OpenClaw gateway | Running (port 18800) | inside pod |

---

## CRD Reference

| CRD | API | Purpose |
|-----|-----|---------|
| `agentharnesses.kagent.dev` | `v1alpha2` | User-facing: create openclaw/openshell agent VMs (renamed from `sandboxes.kagent.dev` in commit `1939749e`) |
| `sandboxes.agents.x-k8s.io` | `v1alpha1` | Internal: OpenShell native Sandbox CRD, created by kagent controller |

---

## Useful Commands

```shell
# Check AgentHarness status
kubectl get agentharness.kagent.dev -n kagent

# Check all pods
kubectl get pods -n kagent
kubectl get pods -n openshell

# Gateway logs (OpenShell)
kubectl logs openshell-0 -n openshell -f

# Controller logs (kagent)
kubectl logs -n kagent -l app.kubernetes.io/component=controller -f

# OpenClaw gateway log (inside sandbox pod)
kubectl exec -n openshell kagent-my-claw -- tail -f /tmp/gw2.log

# Shell into the sandbox
kubectl exec -it -n openshell kagent-my-claw -- bash
```
