#!/usr/bin/env bash
# down.sh — tear down the demo cluster created by up.sh.
#
# Optional env vars:
#   KIND_CLUSTER_NAME   default: kagent-substrate

set -euo pipefail

: "${KIND_CLUSTER_NAME:=kagent-substrate}"

if kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
  echo "Deleting kind cluster '${KIND_CLUSTER_NAME}'..."
  kind delete cluster --name "${KIND_CLUSTER_NAME}"
  echo "Done."
else
  echo "No cluster named '${KIND_CLUSTER_NAME}' to delete."
fi

# We deliberately leave:
#   - the kind-registry container (shared across kind clusters)
#   - localhost:5001 images (cached for next run)
#   - your other kind clusters
