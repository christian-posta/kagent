#!/usr/bin/env bash
#
# GKE spike: does openclaw golden-snapshot in substrate on a real Linux/KVM host?
#
# Answers the question: "is the runsc checkpoint: exit 128 failure we hit on
# kind specific to docker-desktop's LinuxKit VM, or does it happen on GKE too?"
#
# Apply spike-B (openclaw gateway in nemoclaw image) on a fresh GKE cluster.
# If it reaches Ready, kind is the culprit and the harness port becomes
# viable for GKE users. If it hits the same runsc checkpoint: exit 128,
# substrate has a fundamental bug independent of host.
#
# Usage:
#   $0 spike      # default — provision + install + run spike (leaves cluster running for inspection)
#   $0 cleanup    # tear down cluster + bucket + GAR repo
#
# COST: ~$0.25/hr (1× e2-standard-4 + GKE control plane). ~$1-2 per session.
# RUNTIME: ~15-20 min to provision + install, ~3 min for the test.

set -euo pipefail

# ----------------------------------------------------------------------------
# Configuration — edit BEFORE running
# ----------------------------------------------------------------------------

export PROJECT_ID="ceposta-solo-testing"
export CLUSTER_LOCATION="us-west2-a"                 # zonal cluster (cheaper than regional)
export GCE_REGION="us-west2"
export CLUSTER_NAME="substrate-spike"
export CLUSTER_VERSION="1.35.3-gke.1389002"          # need k8s 1.35+ for PodCertificateRequest v1beta1 (substrate dependency)
export MACHINE_TYPE="e2-standard-4"                  # 4 vCPU / 16 GB; cheapest viable for substrate

export BUCKET_NAME="${PROJECT_ID}-substrate-spike-snapshots"
export GAR_LOCATION="us-west2"
export GAR_REPO="substrate-spike"
export KO_DOCKER_REPO="${GAR_LOCATION}-docker.pkg.dev/${PROJECT_ID}/${GAR_REPO}"
export KO_DEFAULTPLATFORMS="linux/amd64"

export NETWORK="default"
export SUBNETWORK="default"

SUBSTRATE_DIR="${HOME}/go/src/github.com/agent-substrate/substrate"

SPIKE_NS="spike"
WP_NAME="spike-pool"
AT_NAME="spike-b-openclaw"

# Public on ghcr.io; GKE nodes can pull without auth setup.
NEMOCLAW_IMAGE="ghcr.io/kagent-dev/nemoclaw/sandbox-base:2026.5.4"

# ----------------------------------------------------------------------------
# Helpers
# ----------------------------------------------------------------------------

C='\033[1;36m' ; R='\033[1;31m' ; G='\033[1;32m' ; Y='\033[1;33m' ; N='\033[0m'
log()  { echo -e "${C}[spike]${N} $*"; }
ok()   { echo -e "${G}[ok]${N} $*"; }
warn() { echo -e "${Y}[warn]${N} $*"; }
die()  { echo -e "${R}[err]${N} $*" >&2 ; exit 1; }

require() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }

confirm() {
  local prompt="$1"
  read -p "${prompt} [y/N] " -n 1 -r
  echo
  [[ $REPLY =~ ^[Yy]$ ]] || die "aborted by user"
}

# ----------------------------------------------------------------------------
# Spike action
# ----------------------------------------------------------------------------

run_spike() {
  require gcloud
  require kubectl
  require docker
  require go
  [[ -d "$SUBSTRATE_DIR" ]] || die "substrate checkout not found at $SUBSTRATE_DIR"

  log "Project: ${PROJECT_ID}"
  log "Cluster: ${CLUSTER_NAME} in ${CLUSTER_LOCATION}"
  log "Bucket:  gs://${BUCKET_NAME}"
  log "GAR:     ${KO_DOCKER_REPO}"
  confirm "Proceed with cluster + bucket + GAR creation?"

  # --- Stage 1: provision infra ---

  log "Enabling required APIs"
  gcloud services enable \
    container.googleapis.com \
    artifactregistry.googleapis.com \
    storage.googleapis.com \
    compute.googleapis.com \
    --project="${PROJECT_ID}"

  log "Creating Artifact Registry repo (if missing)"
  gcloud artifacts repositories describe "${GAR_REPO}" \
    --location="${GAR_LOCATION}" --project="${PROJECT_ID}" >/dev/null 2>&1 \
    || gcloud artifacts repositories create "${GAR_REPO}" \
        --repository-format=docker \
        --location="${GAR_LOCATION}" \
        --project="${PROJECT_ID}"

  log "Configuring docker auth for GAR"
  gcloud auth configure-docker "${GAR_LOCATION}-docker.pkg.dev" --quiet

  log "Creating GCS bucket for snapshots (if missing)"
  gcloud storage buckets describe "gs://${BUCKET_NAME}" --project="${PROJECT_ID}" >/dev/null 2>&1 \
    || gcloud storage buckets create "gs://${BUCKET_NAME}" \
        --project="${PROJECT_ID}" \
        --location="${GCE_REGION}" \
        --uniform-bucket-level-access

  log "Creating GKE cluster (~5 min)"
  log "NOTE: --enable-kubernetes-alpha required for substrate's certificates.k8s.io/v1beta1"
  log "      PodCertificateRequest/ClusterTrustBundle APIs (GKE gates these by default)."
  log "      Alpha cluster has a hard 30-day TTL — fine for a spike."
  # Alpha clusters are mutually exclusive with --release-channel and --workload-pool.
  # No release channel + no workload identity, both acceptable for a spike.
  gcloud container clusters describe "${CLUSTER_NAME}" \
    --location="${CLUSTER_LOCATION}" --project="${PROJECT_ID}" >/dev/null 2>&1 \
    || gcloud container clusters create "${CLUSTER_NAME}" \
        --location="${CLUSTER_LOCATION}" \
        --project="${PROJECT_ID}" \
        --machine-type="${MACHINE_TYPE}" \
        --num-nodes=1 \
        --network="${NETWORK}" \
        --subnetwork="${SUBNETWORK}" \
        --cluster-version="${CLUSTER_VERSION}" \
        --enable-kubernetes-alpha \
        --enable-ip-alias \
        --no-enable-autoupgrade \
        --no-enable-autorepair \
        --quiet

  log "Fetching cluster credentials"
  gcloud container clusters get-credentials "${CLUSTER_NAME}" \
    --location="${CLUSTER_LOCATION}" --project="${PROJECT_ID}"

  local kctx="gke_${PROJECT_ID}_${CLUSTER_LOCATION}_${CLUSTER_NAME}"
  kubectl config use-context "${kctx}"
  ok "Cluster ready: $(kubectl get nodes -o name | wc -l | tr -d ' ') node(s)"

  log "Granting cluster-admin to current user (idempotent)"
  kubectl create clusterrolebinding "spike-admin-$(whoami)" \
    --clusterrole=cluster-admin \
    --user="$(gcloud config get-value account)" \
    --dry-run=client -o yaml | kubectl apply -f -

  # --- Stage 2: install substrate ---

  log "Installing substrate (builds ateom-gvisor/ateapi/atelet/atenet/dns via ko, pushes to GAR)"
  log "First-time builds: ~10 min. Cached rebuilds: ~1 min."

  pushd "${SUBSTRATE_DIR}" >/dev/null

  if ! git diff --quiet cmd/servers/atelet/oci.go; then
    warn "Local uncommitted patch in atelet/oci.go (device-tar fix) — ko will include it. Expected."
  fi

  NO_DEV_ENV=true ./hack/install-ate.sh --deploy-ate-system

  popd >/dev/null

  log "Waiting for ate-system pods to be Ready (max 5 min)"
  kubectl -n ate-system wait --for=condition=Ready pod --all --timeout=300s \
    || die "ate-system pods did not become Ready"

  ok "Substrate installed"
  kubectl -n ate-system get pods

  # --- Stage 3: spike namespace + WorkerPool ---

  log "Creating spike namespace + WorkerPool"
  kubectl create namespace "${SPIKE_NS}" --dry-run=client -o yaml | kubectl apply -f -

  local ateom_image
  ateom_image=$(kubectl -n ate-system get ds atelet \
    -o jsonpath='{.spec.template.spec.containers[?(@.name=="ateom-gvisor")].image}' 2>/dev/null || true)
  if [[ -z "${ateom_image}" ]]; then
    ateom_image=$(kubectl -n ate-system get pods -o yaml \
      | grep -oE "${KO_DOCKER_REPO}/[^\"' ]*ateom-gvisor[^\"' ]*" | head -1 || true)
  fi
  [[ -n "${ateom_image}" ]] || die "could not resolve ateom-gvisor image from deployed manifests"
  log "ateom-gvisor image: ${ateom_image}"

  kubectl apply -f - <<YAML
apiVersion: ate.dev/v1alpha1
kind: WorkerPool
metadata:
  name: ${WP_NAME}
  namespace: ${SPIKE_NS}
spec:
  replicas: 1
  ateomImage: ${ateom_image}
YAML

  log "Waiting for WorkerPool deployment to be Ready"
  kubectl -n "${SPIKE_NS}" rollout status deployment "${WP_NAME}-deployment" --timeout=180s

  # --- Stage 4: apply spike-B ActorTemplate ---

  log "Applying spike-B ActorTemplate (openclaw gateway)"
  kubectl apply -f - <<YAML
apiVersion: ate.dev/v1alpha1
kind: ActorTemplate
metadata:
  name: ${AT_NAME}
  namespace: ${SPIKE_NS}
spec:
  pauseImage: registry.k8s.io/pause:3.10.2@sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4
  runsc:
    amd64:
      sha256Hash: a397be1abc2420d26bce6c70e6e2ff96c73aaaab929756c56f5e2089ea842b63
      url: gs://gvisor/releases/nightly/2026-05-19/x86_64/runsc
    arm64:
      sha256Hash: 1ba2366ae2efceba166046f51a4104f9261c9cb72c6db8f5b3fe2dc57dea86b9
      url: gs://gvisor/releases/nightly/2026-05-19/aarch64/runsc
    authentication: {}
  containers:
  - name: openclaw
    image: ${NEMOCLAW_IMAGE}
    command:
    - /bin/sh
    - -c
    - |
      set -e
      mkdir -p "\${HOME}/.openclaw"
      cat > "\${HOME}/.openclaw/openclaw.json" <<'EOF'
      {
        "gateway": {
          "port": 80,
          "bind": "lan",
          "auth": {"mode": "token", "token": "test-token"},
          "controlUi": {"allowedOrigins": ["*"], "dangerouslyDisableDeviceAuth": true}
        }
      }
      EOF
      openclaw gateway run --port 80 --allow-unconfigured >>/tmp/openclaw-gateway.log 2>&1 &
      for i in \$(seq 1 60); do
        curl -sf http://127.0.0.1:80/ >/dev/null 2>&1 && echo "gateway up" && break
        sleep 1
      done
      tail -f /tmp/openclaw-gateway.log /dev/null
    env:
    - name: HOME
      value: /root
    ports:
    - containerPort: 80
  workerPoolRef:
    name: ${WP_NAME}
    namespace: ${SPIKE_NS}
  snapshotsConfig:
    location: gs://${BUCKET_NAME}/${SPIKE_NS}/${AT_NAME}/
YAML

  # --- Stage 5: poll for verdict ---

  log "Polling ActorTemplate phase (verdict in ~3 min)"
  local deadline=$(( $(date +%s) + 240 ))
  local verdict="UNKNOWN"
  local phase=""

  while [[ $(date +%s) -lt $deadline ]]; do
    phase=$(kubectl -n "${SPIKE_NS}" get actortemplate "${AT_NAME}" \
              -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    echo "$(date +%H:%M:%S) phase=${phase:-<empty>}"

    if [[ "${phase}" == "Ready" ]]; then
      verdict="PASS"
      break
    fi

    if kubectl logs -n ate-system deploy/ate-controller --tail=50 2>/dev/null \
         | grep -q "runsc checkpoint.*exit status 128"; then
      verdict="FAIL — runsc checkpoint: exit 128 (same as kind)"
      break
    fi

    sleep 5
  done

  echo
  echo "===================== VERDICT ====================="
  case "${verdict}" in
    PASS)
      echo -e "${G}PASS${N}: openclaw golden-snapshotted on GKE."
      echo "Kind/LinuxKit is the kind-cluster blocker. Harness port is viable for GKE."
      ;;
    FAIL*)
      echo -e "${R}${verdict}${N}"
      echo "GKE hits the same runsc checkpoint failure. Substrate has a fundamental bug"
      echo "independent of host. Do NOT start the harness port until substrate fixes this."
      ;;
    *)
      echo -e "${Y}TIMEOUT${N}: did not reach Ready or known failure within 4 min"
      echo "Investigate:"
      echo "  kubectl -n ${SPIKE_NS} get actortemplate ${AT_NAME} -o yaml"
      echo "  kubectl logs -n ate-system deploy/ate-controller --tail=200"
      echo "  kubectl logs -n ${SPIKE_NS} -l app=ate-worker-pool --tail=500"
      ;;
  esac
  echo "==================================================="
  echo
  echo "When done investigating, run:  $0 cleanup"
}

# ----------------------------------------------------------------------------
# Cleanup action
# ----------------------------------------------------------------------------

run_cleanup() {
  require gcloud
  require kubectl

  log "=== Cleanup: tearing down spike resources ==="
  warn "This will delete:"
  warn "  - GKE cluster:  ${CLUSTER_NAME} (${CLUSTER_LOCATION})"
  warn "  - GCS bucket:   gs://${BUCKET_NAME} (with all snapshot objects)"
  warn "  - GAR repo:     ${GAR_REPO} (with all images)"
  warn "  - kubeconfig context for the cluster"
  confirm "Confirm teardown?"

  # 1. GKE cluster — nukes all in-cluster resources (substrate, valkey, spike ns, workers).
  log "Deleting GKE cluster ${CLUSTER_NAME} (~3 min)"
  if gcloud container clusters describe "${CLUSTER_NAME}" \
       --location="${CLUSTER_LOCATION}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
    gcloud container clusters delete "${CLUSTER_NAME}" \
      --location="${CLUSTER_LOCATION}" \
      --project="${PROJECT_ID}" \
      --quiet \
      || warn "cluster delete reported failure — verify manually"
  else
    warn "cluster ${CLUSTER_NAME} not found, skipping"
  fi

  # 2. GCS bucket + contents.
  log "Deleting GCS bucket gs://${BUCKET_NAME}"
  if gcloud storage buckets describe "gs://${BUCKET_NAME}" \
       --project="${PROJECT_ID}" >/dev/null 2>&1; then
    gcloud storage rm --recursive "gs://${BUCKET_NAME}" \
      --project="${PROJECT_ID}" --quiet \
      || warn "bucket delete reported failure — verify manually"
  else
    warn "bucket gs://${BUCKET_NAME} not found, skipping"
  fi

  # 3. GAR repo + all images.
  log "Deleting GAR repo ${GAR_REPO}"
  if gcloud artifacts repositories describe "${GAR_REPO}" \
       --location="${GAR_LOCATION}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
    gcloud artifacts repositories delete "${GAR_REPO}" \
      --location="${GAR_LOCATION}" \
      --project="${PROJECT_ID}" \
      --quiet \
      || warn "GAR repo delete reported failure — verify manually"
  else
    warn "GAR repo ${GAR_REPO} not found, skipping"
  fi

  # 4. Local kubeconfig cleanup so we don't leave a dangling context.
  local kctx="gke_${PROJECT_ID}_${CLUSTER_LOCATION}_${CLUSTER_NAME}"
  kubectl config delete-context "${kctx}" 2>/dev/null || true
  kubectl config delete-cluster "${kctx}" 2>/dev/null || true
  kubectl config delete-user "${kctx}" 2>/dev/null || true

  ok "Cleanup complete."
  log "Sanity checks (should all be empty / not-found):"
  gcloud container clusters list --filter="name:${CLUSTER_NAME}" --project="${PROJECT_ID}" 2>/dev/null || true
  gcloud storage buckets list --filter="name:${BUCKET_NAME}" --project="${PROJECT_ID}" 2>/dev/null || true
  gcloud artifacts repositories list --location="${GAR_LOCATION}" \
    --filter="name~${GAR_REPO}" --project="${PROJECT_ID}" 2>/dev/null || true
}

# ----------------------------------------------------------------------------
# Dispatch
# ----------------------------------------------------------------------------

case "${1:-spike}" in
  spike)   run_spike   ;;
  cleanup) run_cleanup ;;
  *)       die "usage: $0 [spike|cleanup]   (default: spike)" ;;
esac
