#!/usr/bin/env bash
# aauth-agent-to-llm.sh — guided walkthrough of Part C (agent → gateway → extauth → LLM).
#
# Prerequisites: Part B from demo/aauth/README.md is running (port-forwards, extauth,
# agentgateway). This script does not start those services.
#
# Usage:
#   ./demo/aauth/aauth-agent-to-llm.sh
#
# Options (environment):
#   FAST=1          Skip typing animation; print commands instantly.
#   TYPE_DELAY=0.02 Base scale for single-line typing speed (default 0.028).
#   NO_PAUSE=1      Run all steps without "Press Enter" between them.
#   CONTROLLER_URL  Default http://localhost:8083
#   GATEWAY_URL     Default http://localhost:3030
#   AGENT_URL       Default http://localhost:18080

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Use $'...' so escapes are real bytes; plain '\033' in quotes prints literally.
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

PROMPT="${PROMPT:-\$}"
# shellcheck source=demo/aauth/_typing.sh
source "${SCRIPT_DIR}/_typing.sh"

say()     { printf '\n%s▸ %s%s\n' "$CYAN" "$*" "$NC"; }
banner()  { printf '\n%s%s%s\n' "$BOLD" "$*" "$NC"; }
note()    { printf '%s  %s%s\n' "$DIM" "$*" "$NC"; }
success() { printf '%s✓ %s%s\n' "$GREEN" "$*" "$NC"; }

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

check_reachable() {
  local url="$1"
  local label="$2"
  if ! curl -sf --connect-timeout 2 "$url" >/dev/null 2>&1; then
    printf '%sWarning:%s %s is not reachable at %s\n' \
      "$YELLOW" "$NC" "$label" "$url"
    printf '  Make sure Part B from %s/README.md is running.\n' "$SCRIPT_DIR"
    return 1
  fi
  return 0
}

run_typed() {
  local cmd="$1"
  type_command "$cmd"
  # shellcheck disable=SC2086
  eval "$cmd"
}

CONTROLLER="${CONTROLLER_URL:-http://localhost:8083}"
GATEWAY="${GATEWAY_URL:-http://localhost:3030}"
AGENT="${AGENT_URL:-http://localhost:18080}"

# ---------------------------------------------------------------------------
banner "AAuth demo — agent outbound path to LLM (Part C)"
note "Assumes Part B is already up: controller :8083, extauth, agentgateway :3030, agent :18080."
note "See ${SCRIPT_DIR}/README.md for setup."

require_cmd curl
require_cmd jq

pause_step

# ---------------------------------------------------------------------------
# Step 1 — Verify the controller's AAuth Agent Provider (issuer) is published
# ---------------------------------------------------------------------------
banner "Step 1 — Verify AAuth Agent Provider on kagent controller"

say "Well-known agent metadata (issuer document):"
check_reachable "${CONTROLLER}/.well-known/aauth-agent.json" "controller AAuth issuer" || true
run_typed "curl -s ${CONTROLLER}/.well-known/aauth-agent.json | jq ."

pause_step

say "Issuer JWKS (extauth will fetch this to verify agent JWTs):"
run_typed "curl -s ${CONTROLLER}/.well-known/jwks.json | jq ."

note "Expect keys[0] with alg=EdDSA, kty=OKP, kid=kagent-issuer-1 (see README §B1)."

pause_step

# ---------------------------------------------------------------------------
# Step 2 — Unsigned request to agentgateway (should be rejected)
# ---------------------------------------------------------------------------
banner "Step 2 — Unsigned curl to agentgateway (§C1)"

say "Direct POST to the gateway without AAuth signatures — gate should be closed:"

GW_CMD=$(cat <<EOF
curl -si -X POST ${GATEWAY}/openai/v1/chat/completions \\
  -H "Content-Type: application/json" \\
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' \\
  | head -20
EOF
)
type_command "$GW_CMD"
# shellcheck disable=SC2086
eval "$GW_CMD"

note "Expected: HTTP/1.1 401 Unauthorized"
note "  aauth-requirement: requirement=auth-token"
note "  www-authenticate: AAuth"
note "  signature-error: error=invalid_signature"
note "  body: {\"error\":\"missing_signature\"}"
note "This proves agentgateway delegates to extauth and unsigned traffic is blocked."

pause_step

# ---------------------------------------------------------------------------
# Step 3 — A2A message to agent (triggers signed outbound LLM call)
# ---------------------------------------------------------------------------
banner "Step 3 — Send A2A message to agent (§C2)"

say "Unsigned inbound to the agent; the agent signs outbound LLM traffic through the gateway:"

AGENT_CMD=$(cat <<EOF
curl -s -X POST ${AGENT}/ \\
  -H "Content-Type: application/json" \\
  -d '{
        "jsonrpc": "2.0",
        "id":      "req-1",
        "method":  "message/send",
        "params": {
          "message": {
            "kind":       "message",
            "messageId":  "msg-1",
            "role":       "user",
            "parts":      [{"kind": "text", "text": "hello from the aauth demo"}]
          }
        }
      }' | jq '.result.history[-1]'
EOF
)
type_command "$AGENT_CMD"

curl -s -X POST "${AGENT}/" \
  -H "Content-Type: application/json" \
  -d '{
        "jsonrpc": "2.0",
        "id":      "req-1",
        "method":  "message/send",
        "params": {
          "message": {
            "kind":       "message",
            "messageId":  "msg-1",
            "role":       "user",
            "parts":      [{"kind": "text", "text": "hello from the aauth demo"}]
          }
        }
      }' | jq '.result.history[-1]'

note "With a placeholder OPENAI_API_KEY you may see an OpenAI 401 in the agent reply —"
note "that still means AAuth verification succeeded (request reached OpenAI)."
note "With a real key you get a normal completion (see README §C2)."

pause_step

# ---------------------------------------------------------------------------
# Step 4 — Prompt to inspect extauth logs
# ---------------------------------------------------------------------------
banner "Step 4 — Confirm in extauth log (§C3)"

printf '\n'
printf '%sSwitch to the terminal where extauth is running (README Part B, Terminal 2).%s\n' "$YELLOW" "$NC"
printf 'The most recent log line should look like:\n\n'
cat <<'EOF'
{
  "time":         "2026-05-12T02:52:30Z",
  "resource_id":  "kagent-agents",
  "level":        "identified",
  "agent_server": "http://localhost:8083",
  "delegate":     "aauth:aauth-test-agent@kagent.kagent.local",
  "result":       "allowed",
  "latency_ms":   9
}
EOF

printf '\n'
note "level=identified — controller aa-agent+jwt verified via JWKS; request signature verified with cnf.jwk."
note "delegate — agent sub from SA TokenReview; only this pod can mint that identity."
note "Full chain explained in README § \"What this demo proves\"."

printf '\n%sWhen you see level=identified, the agent → gateway → extauth → LLM path succeeded.%s\n' \
  "$GREEN" "$NC"
printf '\n'
