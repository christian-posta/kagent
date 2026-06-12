#!/usr/bin/env bash
# up.sh — bring up the substrate demo from a clean slate.
#
# Default behavior (platform only):
#   - kind cluster ${KIND_CLUSTER_NAME} with substrate + kagent installed
#   - localhost:5001 holding the openclaw image mirror (so you can apply
#     demo/agents/peterj-claw.yaml at any time without re-pulling from ghcr)
#   - NO demo agents applied — you create those yourself via the UI or
#     `kubectl apply -f demo/agents/...`
#
# With --full also:
#   - SandboxAgent/hello-substrate Ready (declarative, Go ADK)
#   - AgentHarness/peterj-claw Ready (openclaw on substrate)
#
# Usage:
#   bash demo/up.sh              # platform only (default)
#   bash demo/up.sh --full       # platform + both demo agents
#
# Optional env vars (with defaults):
#   KIND_CLUSTER_NAME   default: kagent-substrate
#   KAGENT_DIR          default: $HOME/go/src/github.com/kagent-dev/kagent
#   SUBSTRATE_DIR       default: $HOME/go/src/github.com/kagent-dev/substrate
#   OPENAI_KEY_FILE     default: $HOME/bin/openai-key
#   WORKER_REPLICAS     default: 2
#   PULL_REPOS          default: 1 (set to 0 to skip git fetch/reset)
#
# Companion: down.sh tears the cluster down.

set -euo pipefail

# -------------------- args --------------------
FULL=0
for arg in "$@"; do
  case "$arg" in
    --full) FULL=1 ;;
    -h|--help)
      # print the leading comment header (skip the shebang)
      awk 'NR>1 && /^#/ {sub(/^# ?/, ""); print; next} NR>1 {exit}' "$0"
      exit 0 ;;
    *) echo "unknown argument: $arg (try --help)" >&2; exit 2 ;;
  esac
done

# -------------------- config --------------------
: "${KIND_CLUSTER_NAME:=kagent-substrate}"
: "${KAGENT_DIR:=$HOME/go/src/github.com/kagent-dev/kagent}"
: "${SUBSTRATE_DIR:=$HOME/go/src/github.com/kagent-dev/substrate}"
: "${OPENAI_KEY_FILE:=$HOME/bin/openai-key}"
: "${WORKER_REPLICAS:=2}"
: "${PULL_REPOS:=1}"

# Pinned upstream OpenClaw sandbox base; mirrored to localhost:5001.
OPENCLAW_UPSTREAM_REF="ghcr.io/kagent-dev/nemoclaw/sandbox-base@sha256:d52bee415dc4c0dba7164f9eabe727574c056d4f211781f20af249707883a3b4"
OPENCLAW_LOCAL_TAG="localhost:5001/nemoclaw/sandbox-base:demo"

KCTX="kind-${KIND_CLUSTER_NAME}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

bold() { printf '\033[1m%s\033[0m\n' "$*"; }
step() { printf '\n\033[1;36m▶ %s\033[0m\n' "$*"; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; }
warn() { printf '  \033[33m!\033[0m %s\n' "$*"; }
die()  { printf '\n\033[1;31m✗ %s\033[0m\n' "$*" >&2; exit 1; }

# -------------------- preflight --------------------
step "Preflight ($([[ ${FULL} == 1 ]] && echo 'mode: --full' || echo 'mode: platform-only'))"
[[ -d "${KAGENT_DIR}" ]]    || die "KAGENT_DIR not found: ${KAGENT_DIR}"
[[ -d "${SUBSTRATE_DIR}" ]] || die "SUBSTRATE_DIR not found: ${SUBSTRATE_DIR}"
[[ -f "${OPENAI_KEY_FILE}" ]] || die "OPENAI_KEY_FILE not found: ${OPENAI_KEY_FILE}"
for cmd in kind kubectl helm docker go; do
  command -v "$cmd" >/dev/null || die "missing required tool: $cmd"
done
ok "tools, paths, key file all present"

# -------------------- step 1: refresh repos --------------------
if [[ "${PULL_REPOS}" == "1" ]]; then
  step "Refreshing repos to latest"
  # kagent: PR #1981 is merged to main as of commit 32e72210. The original
  # peterj/substrate-declar branch was deleted upstream — use main.
  ( cd "${KAGENT_DIR}" && \
      git fetch upstream main >/dev/null 2>&1 && \
      git checkout main >/dev/null 2>&1 && \
      git reset --hard upstream/main >/dev/null )
  ok "kagent at $(cd "${KAGENT_DIR}" && git log --oneline -1)"

  ( cd "${SUBSTRATE_DIR}" && \
      git fetch origin main >/dev/null 2>&1 && \
      git checkout main >/dev/null 2>&1 && \
      git reset --hard origin/main >/dev/null )
  ok "substrate at $(cd "${SUBSTRATE_DIR}" && git log --oneline -1)"
else
  step "Skipping repo refresh (PULL_REPOS=0)"
fi

# -------------------- step 2: create cluster + install substrate --------------------
step "Creating kind cluster '${KIND_CLUSTER_NAME}'"
(
  cd "${SUBSTRATE_DIR}"
  KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME}" ./hack/create-kind-cluster.sh \
    | sed 's/^/  /' \
    | tail -10
)
kubectl config use-context "${KCTX}" >/dev/null
ok "cluster up, context=${KCTX}"

step "Installing substrate control plane (this can take a few minutes)"
# The install script's own wait times out; we ignore that and wait properly below.
(
  cd "${SUBSTRATE_DIR}"
  KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME}" ./hack/install-ate-kind.sh --deploy-ate-system 2>&1 \
    | tail -5 \
    | sed 's/^/  /'
) || warn "install-ate-kind reported a wait timeout — that's expected, continuing"

step "Waiting for ate-api-server to be Ready"
kubectl --context "${KCTX}" wait deploy/ate-api-server-deployment -n ate-system \
  --for=condition=Available --timeout=600s
ok "ate-api-server Ready"

# -------------------- step 3: build ateom-gvisor --------------------
step "Building ateom-gvisor"
(
  cd "${SUBSTRATE_DIR}"
  export KO_DOCKER_REPO=localhost:5001
  export KO_DEFAULTPLATFORMS="linux/$(go env GOARCH)"
  ./hack/run-tool.sh ko build -B ./cmd/ateom-gvisor 2>&1 | tail -1 | sed 's/^/  /'
)
ok "ateom-gvisor pushed to localhost:5001"

# -------------------- step 4: mirror OpenClaw image --------------------
# Always mirror so demo/agents/peterj-claw.yaml is ready to `kubectl apply`
# whether or not we run --full now.
step "Mirroring OpenClaw image to localhost:5001 (works around ghcr PROTOCOL_ERROR)"
docker pull "${OPENCLAW_UPSTREAM_REF}" 2>&1 | tail -3 | sed 's/^/  /'
docker tag  "${OPENCLAW_UPSTREAM_REF}" "${OPENCLAW_LOCAL_TAG}"
# `docker push` outputs the *just-pushed* digest on a line like
#   "demo: digest: sha256:abc... size: N"
# which we parse — docker inspect's RepoDigests is unreliable because the
# image's local cache lists ALL refs it's ever known (including the original
# ghcr.io one, which we explicitly don't want here).
push_out=$(docker push "${OPENCLAW_LOCAL_TAG}" 2>&1 | tee >(sed 's/^/  /' >&2))
digest_hash=$(printf '%s\n' "${push_out}" | awk '/digest: sha256:/ {for (i=1;i<=NF;i++) if ($i ~ /^sha256:/) {print $i; exit}}')
[[ -n "${digest_hash}" ]] || die "could not parse digest from docker push output"
OPENCLAW_LOCAL_DIGEST="localhost:5001/nemoclaw/sandbox-base@${digest_hash}"
ok "OpenClaw mirror: ${OPENCLAW_LOCAL_DIGEST}"

# Cross-check the mirror digest against the hardcoded one in
# demo/agents/peterj-claw.yaml. They're expected to match (the push is
# deterministic for a pinned upstream digest); if they don't, applying the
# YAML by hand will fail or pull the wrong image.
if [[ -f "${SCRIPT_DIR}/agents/peterj-claw.yaml" ]]; then
  yaml_digest=$(awk '/workloadImage:/ {print $2; exit}' "${SCRIPT_DIR}/agents/peterj-claw.yaml" || true)
  if [[ -n "${yaml_digest}" && "${yaml_digest}" != "${OPENCLAW_LOCAL_DIGEST}" ]]; then
    warn "demo/agents/peterj-claw.yaml workloadImage doesn't match the freshly-pushed mirror digest."
    warn "  yaml:   ${yaml_digest}"
    warn "  pushed: ${OPENCLAW_LOCAL_DIGEST}"
    warn "  → update the yaml, or re-apply via --full (which uses the live digest)."
  fi
fi

# -------------------- step 5: helm-install kagent --------------------
step "Installing kagent via helm (this builds 6 images; ~5–10 min on first run)"
(
  cd "${KAGENT_DIR}"
  OPENAI_API_KEY="$(cat "${OPENAI_KEY_FILE}")" \
  KAGENT_DEFAULT_MODEL_PROVIDER=openAI \
  KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME}" \
  make helm-install KAGENT_HELM_EXTRA_ARGS="\
    --set controller.substrate.enabled=true \
    --set controller.substrate.ateApiEndpoint=dns:///api.ate-system.svc:443 \
    --set controller.substrate.ateApiInsecure=true \
    --set substrateWorkerPool.create=true \
    --set substrateWorkerPool.replicas=${WORKER_REPLICAS} \
    --set substrateWorkerPool.ateomImage=localhost:5001/ateom-gvisor:latest" 2>&1 \
    | tail -3 \
    | sed 's/^/  /'
) || warn "helm-install reported a timeout — that's expected on cold install, continuing"

step "Waiting for kagent-controller (may restart 2–3 times while postgres comes up)"
kubectl --context "${KCTX}" wait deploy/kagent-controller -n kagent \
  --for=condition=Available --timeout=600s
ok "kagent-controller Ready"

# -------------------- step 6: optionally apply demo CRs (--full) --------------------
if [[ "${FULL}" == 1 ]]; then
  step "Applying SandboxAgent/hello-substrate"
  kubectl --context "${KCTX}" apply -f "${SCRIPT_DIR}/agents/hello-substrate.yaml" | sed 's/^/  /'

  step "Applying AgentHarness/peterj-claw (workloadImage from live docker push digest)"
  # Substitute the live digest into the YAML so we're never out of sync with the
  # actual mirror push, even if the hardcoded digest in the file drifts.
  sed -E "s|^( *workloadImage: ).*$|\1${OPENCLAW_LOCAL_DIGEST}|" \
    "${SCRIPT_DIR}/agents/peterj-claw.yaml" \
    | kubectl --context "${KCTX}" apply -f - | sed 's/^/  /'

  step "Waiting for demo CRs to reach Ready (golden snapshots ~60–90s each)"
  kubectl --context "${KCTX}" wait sandboxagent/hello-substrate -n kagent \
    --for=condition=Ready --timeout=300s
  ok "hello-substrate Ready"

  kubectl --context "${KCTX}" wait agentharness/peterj-claw -n kagent \
    --for=condition=Ready --timeout=300s
  ok "peterj-claw Ready"
fi

# -------------------- done --------------------
bold ""
bold "✅ Platform is up. Next steps:"
echo ""
echo "  Port-forward the UI:"
echo "    kubectl port-forward -n kagent svc/kagent-ui 8001:8080 --context ${KCTX}"
echo ""
echo "  Then open http://localhost:8001"
echo ""
if [[ "${FULL}" == 1 ]]; then
  echo "  Already deployed (use the UI to interact):"
  echo "    - Agents → hello-substrate     (chat directly)"
  echo "    - Agents → peterj-claw         (opens OpenClaw Control; token: test-token)"
  echo "    - View   → Substrate           (the new operator panel)"
else
  echo "  Deploy the demo agents whenever you want (UI or kubectl):"
  echo "    kubectl --context ${KCTX} apply -f demo/agents/hello-substrate.yaml"
  echo "    kubectl --context ${KCTX} apply -f demo/agents/peterj-claw.yaml"
  echo ""
  echo "  Or re-run with everything pre-applied:"
  echo "    bash demo/up.sh --full"
fi
echo ""
echo "  Tear down:"
echo "    KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME} bash demo/down.sh"
echo ""
