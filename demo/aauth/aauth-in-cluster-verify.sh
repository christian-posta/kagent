#!/usr/bin/env bash
# aauth-in-cluster-verify.sh — guided walkthrough of Part E + F (in-cluster, log-only).
#
# Prerequisites:
#   - Part A applied (aauth-test-agent with kagent-builtin-mcp tool, model config,
#     AAUTH_ISSUER_URL on controller).
#   - Part B port-forward on agent :18080 (for F2 message/send to trigger MCP).
#   - extauth and agentgateway are NOT required for E3 or F.
#
# Usage:
#   ./demo/aauth/aauth-in-cluster-verify.sh
#
# Options (environment):
#   FAST=1              Skip typing animation.
#   TYPE_DELAY=0.02     Base scale for single-line typing speed (default 0.028).
#   NO_PAUSE=1          Run without "Press Enter" between steps.
#   KUBE_CONTEXT        Default kind-kagent
#   KUBE_NS             Default kagent
#   AGENT_URL           Host port-forward for F2 curl (default http://localhost:18080)
#   CONTROLLER_CLUSTER  In-cluster controller URL for aauth-call-agent.py
#   AGENT_CLUSTER       In-cluster agent URL for aauth-call-agent.py

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

if [[ -t 1 ]]; then
  BOLD=$'\033[1m'
  CYAN=$'\033[0;36m'
  GREEN=$'\033[0;32m'
  YELLOW=$'\033[0;33m'
  DIM=$'\033[2m'
  NC=$'\033[0m'
else
  BOLD='' CYAN='' GREEN='' YELLOW='' DIM='' NC=''
fi

TYPE_DELAY="${TYPE_DELAY:-0.028}"
PROMPT="${PROMPT:-\$}"

KUBE_CONTEXT="${KUBE_CONTEXT:-kind-kagent}"
KUBE_NS="${KUBE_NS:-kagent}"
AGENT_URL="${AGENT_URL:-http://localhost:18080}"
CONTROLLER_CLUSTER="${CONTROLLER_CLUSTER:-http://kagent-controller.kagent:8083}"
AGENT_CLUSTER="${AGENT_CLUSTER:-http://aauth-test-agent.kagent:8080}"
AAUTH_HELPER="${REPO_ROOT}/scripts/aauth-call-agent.py"

KUBECTL=(kubectl --context "$KUBE_CONTEXT" -n "$KUBE_NS")

# shellcheck source=demo/aauth/_typing.sh
source "${SCRIPT_DIR}/_typing.sh"

say()      { printf '\n%s▸ %s%s\n' "$CYAN" "$*" "$NC"; }
banner()   { printf '\n%s%s%s\n' "$BOLD" "$*" "$NC"; }
note()     { printf '%s  %s%s\n' "$DIM" "$*" "$NC"; }
success()  { printf '%s✓ %s%s\n' "$GREEN" "$*" "$NC"; }

pause_step() {
  if [[ -z "${NO_PAUSE:-}" ]]; then
    printf '\n%sPress Enter to continue…%s ' "$YELLOW" "$NC"
    read -r _
  fi
}

require_cmd() {
  local c="$1"
  if ! command -v "$c" >/dev/null 2>&1; then
    printf 'Required command not found: %s\n' "$c" >&2
    exit 1
  fi
}

run_typed() {
  local cmd="$1"
  type_command "$cmd"
  # shellcheck disable=SC2086
  eval "$cmd"
}

# ---------------------------------------------------------------------------
banner "AAuth demo — in-cluster verification (Part E + F)"
note "E3: signed inbound to the agent (door 3). F: signed agent → controller /mcp (door 2)."
note "No unsigned host→agent curl demo — see aauth-agent-to-llm.sh for the outbound LLM path."
note "Assumes Part B agent port-forward (${AGENT_URL}) is up; extauth/gateway not needed."
note "See ${SCRIPT_DIR}/README.md § Part E and § Part F."

require_cmd kubectl
require_cmd jq
require_cmd curl

if [[ ! -f "$AAUTH_HELPER" ]]; then
  printf 'Helper script not found: %s\n' "$AAUTH_HELPER" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Agent aa-agent+jwt — credential used on every signed outbound call
# ---------------------------------------------------------------------------
banner "Agent credential — minted aa-agent+jwt (README §F3)"

say "Controller minted this at agent startup; it rides in Signature-Key on signed requests:"

TOKEN_CMD="${KUBECTL[*]} logs deploy/aauth-test-agent | sed -n '/AAuth: minted/,/AAuth signing enabled/p'"
run_typed "$TOKEN_CMD"

note "Check typ=aa-agent+jwt, iss=http://localhost:8083, sub=aauth:aauth-test-agent@kagent.kagent.local, cnf.jwk."
note "Optional JWT decode: README §F3 (python base64url decode)."

pause_step

# ---------------------------------------------------------------------------
# E3 — Agent inbound verifier (positive, signed)
# ---------------------------------------------------------------------------
banner "E3 — Agent inbound verifier — positive (door 3)"

say "Copy the signed-call helper into the agent pod and POST a signed message/send:"

E3_GET_POD="${KUBECTL[*]} get pods -l app.kubernetes.io/name=aauth-test-agent -o jsonpath='{.items[0].metadata.name}'"
run_typed "$E3_GET_POD"

AGENT_POD="$("${KUBECTL[@]}" get pods -l app.kubernetes.io/name=aauth-test-agent -o jsonpath='{.items[0].metadata.name}')"
if [[ -z "$AGENT_POD" ]]; then
  printf 'No aauth-test-agent pod found in namespace %s\n' "$KUBE_NS" >&2
  exit 1
fi
success "Using pod: ${AGENT_POD}"

E3_CP="${KUBECTL[*]} cp ${AAUTH_HELPER} ${AGENT_POD}:/tmp/aauth-call-agent.py"
run_typed "$E3_CP"

E3_T0_CMD='T0=$(date -u +%Y-%m-%dT%H:%M:%SZ)'
type_command "$E3_T0_CMD"
eval "$E3_T0_CMD"

E3_EXEC="${KUBECTL[*]} exec ${AGENT_POD} -- python /tmp/aauth-call-agent.py \\
  --controller ${CONTROLLER_CLUSTER} \\
  --agent ${AGENT_CLUSTER} \\
  --message 'signed inbound test'"
type_command "$E3_EXEC"
# shellcheck disable=SC2086
eval "$E3_EXEC"

pause_step

say "Check the agent log for the positive verification (since T0):"

E3_LOGS="${KUBECTL[*]} logs ${AGENT_POD} --since-time=\$T0 | grep 'aauth: verified' | tail -1"
run_typed "$E3_LOGS"

note "Expect: INFO - aauth: verified caller=aauth:aauth-test-agent@kagent.kagent.local method=POST path=/"
note "caller is derived from the SA token used to mint the JWT — not just \"signed\"."

pause_step

# ---------------------------------------------------------------------------
# Part F — Signed agent → controller MCP (door 2 on /mcp, no extauth)
# ---------------------------------------------------------------------------
banner "Part F — Signed agent → controller MCP path (§F)"

note "Agent calls kagent built-in MCP at controller /mcp; every hop is signed and verified by door 2."
note "Proof is the controller verifier log, not the agent's text reply (discard curl body)."

# F1 — Sanity-check MCP tool wiring
banner "F1 — Built-in MCP toolset wired on aauth-test-agent"

F1_CMD="${KUBECTL[*]} get agent aauth-test-agent -o jsonpath='{.spec.declarative.tools[*].mcpServer.name}{\"\\n\"}'"
run_typed "$F1_CMD"

MCP_TOOL="$("${KUBECTL[@]}" get agent aauth-test-agent -o jsonpath='{.spec.declarative.tools[*].mcpServer.name}')"
if [[ "$MCP_TOOL" == *kagent-builtin-mcp* ]]; then
  success "MCP tool: kagent-builtin-mcp"
else
  printf '%sWarning:%s expected kagent-builtin-mcp, got: %q\n' "$YELLOW" "$NC" "$MCP_TOOL"
  note "Re-apply: kubectl apply -f ${REPO_ROOT}/examples/aauth-test-agent.yaml"
fi

pause_step

# F2 — Trigger list_agents via MCP and filter controller /mcp verifier lines
banner "F2 — Trigger MCP tool call and watch door 2 on /mcp"

say "Record timestamp, send a prompt that forces list_agents, then filter controller logs:"

F2_T0_CMD='T0=$(date -u +%Y-%m-%dT%H:%M:%SZ)'
type_command "$F2_T0_CMD"
eval "$F2_T0_CMD"

F2_CURL_CMD=$(cat <<EOF
curl -sS -X POST ${AGENT_URL}/ \\
  -H 'Content-Type: application/json' \\
  -d '{"jsonrpc":"2.0","id":"f-1","method":"message/send","params":{"message":{"kind":"message","messageId":"f-msg-1","role":"user","parts":[{"kind":"text","text":"You MUST call the list_agents tool right now. Do not answer from memory. Just call list_agents and tell me the count."}]}}}' >/dev/null
EOF
)
type_command "$F2_CURL_CMD"
# shellcheck disable=SC2086
eval "$F2_CURL_CMD"

F2_LOGS_CMD="${KUBECTL[*]} logs deploy/kagent-controller --since-time=\$T0 | grep '\"aauth: verified\"' | grep '\"path\":\"/mcp\"' | jq -c '{ts, msg, caller, method, path}'"
type_command "$F2_LOGS_CMD"
# shellcheck disable=SC2086
eval "$F2_LOGS_CMD"

note "Expect ~5 verified lines, caller=aauth:aauth-test-agent@kagent.kagent.local, path=/mcp."
note "Streamable HTTP session: POST init → GET SSE → list_tools → call → DELETE."
note "If you grep only '\"aauth\"' you may also see unverified /mcp from the UI or other clients (caller null) — not the agent."
note "Second run may show fewer lines (MCP session reused). Restart agent pod for a clean session."
note "Zero lines: LLM skipped the tool — strengthen the prompt or rollout-restart the agent."

printf '\n%sComplete — signed inbound (E3) and signed /mcp on controller (F).%s\n' "$GREEN" "$NC"
printf '\n'
