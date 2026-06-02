#!/usr/bin/env bash
# Boot the external-facing AAuth verification layer for the substrate demo:
#
#   1. kubectl port-forward kagent-controller :8083  → JWKS + agent-jwt mint
#   2. extauth-aauth-resource on :7070 (gRPC) + :8080 (HTTP)
#   3. agentgateway on :3030 (ext_authz → extauth → upstream LLM)
#
# Background: when `controller.defaultAAuthEnabled: true` is set
# (kagent-substrate-values.yaml does this), every declarative agent's
# httpx client signs outbound requests with RFC 9421. agents are
# configured to send LLM traffic at http://host.docker.internal:3030 so
# agentgateway can call extauth to verify the signature before the
# request reaches OpenAI / Anthropic / etc. See demo/aauth/README.md for
# the verifier-boundary breakdown.
#
# This script wraps the manual launch from demo/aauth/README.md so the
# substrate demo can stand the verification layer up in one command.
# Re-runnable: kills any previous instance it started before launching
# fresh ones. Logs land in /tmp/kagent-aauth-*.log.
#
# Required:
#   - agentgateway on $PATH
#   - extauth-aauth-resource cloned + built (./aauth-service binary).
#     Default location: ~/go/src/github.com/christian-posta/extauth-aauth-resource
#     Override with EXTAUTH_REPO env var.
#   - kubectl context pointing at the kagent kind cluster
#   - kagent-controller deployment Ready in the kagent namespace
#
# Exits non-zero on any failure. Use `--stop` to tear everything down.
# Use `--status` for a read-only report (does not start anything).
# Idempotent: if all three services are already running (pid files live and
# their ports listening), prints the current state and exits 0 without
# touching anything. Force a relaunch by running `--stop` first.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
EXTAUTH_REPO="${EXTAUTH_REPO:-$HOME/go/src/github.com/christian-posta/extauth-aauth-resource}"

CONTROLLER_PF_PORT="${CONTROLLER_PF_PORT:-8083}"
EXTAUTH_GRPC_PORT="7070"
EXTAUTH_HTTP_PORT="8080"
AGW_PORT="3030"

CONTROLLER_LOG="/tmp/kagent-aauth-controller-pf.log"
EXTAUTH_LOG="/tmp/kagent-aauth-extauth.log"
AGW_LOG="/tmp/kagent-aauth-agentgateway.log"

CONTROLLER_PID_FILE="/tmp/kagent-aauth-controller-pf.pid"
EXTAUTH_PID_FILE="/tmp/kagent-aauth-extauth.pid"
AGW_PID_FILE="/tmp/kagent-aauth-agentgateway.pid"

color()   { printf '\033[%sm%s\033[0m' "$1" "$2"; }
info()    { echo "$(color '1;34' '[info]') $*"; }
ok()      { echo "$(color '1;32' '[ ok ]') $*"; }
err()     { echo "$(color '1;31' '[err ]') $*" >&2; }

stop_pid_file() {
    local pid_file="$1"
    local label="$2"
    if [[ -f "$pid_file" ]]; then
        local pid
        pid=$(cat "$pid_file" 2>/dev/null || true)
        if [[ -n "${pid:-}" ]] && kill -0 "$pid" 2>/dev/null; then
            info "stopping $label (pid=$pid)"
            kill "$pid" 2>/dev/null || true
            # give it a beat to exit; force if still alive
            sleep 0.5
            kill -0 "$pid" 2>/dev/null && kill -9 "$pid" 2>/dev/null || true
        fi
        rm -f "$pid_file"
    fi
}

stop_all() {
    stop_pid_file "$AGW_PID_FILE"        "agentgateway"
    stop_pid_file "$EXTAUTH_PID_FILE"    "extauth"
    stop_pid_file "$CONTROLLER_PID_FILE" "controller port-forward"
    # Also reap anything else hogging our ports — handles the case where a
    # previous invocation crashed without writing a pid file.
    for port in "$AGW_PORT" "$EXTAUTH_GRPC_PORT" "$EXTAUTH_HTTP_PORT" "$CONTROLLER_PF_PORT"; do
        if pids=$(lsof -tiTCP:"$port" -sTCP:LISTEN 2>/dev/null); then
            for p in $pids; do
                info "killing stray listener on :$port (pid=$p)"
                kill -9 "$p" 2>/dev/null || true
            done
        fi
    done
}

if [[ "${1:-}" == "--stop" ]]; then
    stop_all
    ok "stopped"
    exit 0
fi

pid_alive_from_file() {
    local pid_file="$1"
    [[ -f "$pid_file" ]] || return 1
    local pid; pid=$(cat "$pid_file" 2>/dev/null || true)
    [[ -n "${pid:-}" ]] && kill -0 "$pid" 2>/dev/null
}
port_listening() {
    lsof -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1
}

# Per-service status line. Reports pid-file state + port-listening state and
# returns 0 when fully healthy (pid alive AND every port listening), 1 when
# anything's off (down, partial, or stray listener with no pid file).
report_service() {
    local name="$1"; local pid_file="$2"; local log="$3"; shift 3
    local pid_state="—" port_state=""
    local healthy=0  # accumulate; non-zero means "this service has a problem"

    if [[ -f "$pid_file" ]]; then
        local pid; pid=$(cat "$pid_file" 2>/dev/null || true)
        if [[ -n "${pid:-}" ]] && kill -0 "$pid" 2>/dev/null; then
            pid_state="pid=$pid"
        else
            pid_state="pid=$pid (DEAD)"
            healthy=1
        fi
    else
        pid_state="(no pid file)"
        healthy=1
    fi

    for port in "$@"; do
        local listener
        listener=$(lsof -tiTCP:"$port" -sTCP:LISTEN 2>/dev/null | head -1)
        if [[ -n "$listener" ]]; then
            port_state+=" :$port=UP(pid=$listener)"
        else
            port_state+=" :$port=DOWN"
            healthy=1
        fi
    done

    if [[ $healthy -eq 0 ]]; then
        ok    "  $name  $pid_state ${port_state# }  ($log)"
    else
        err   "  $name  $pid_state ${port_state# }  ($log)"
    fi
    return $healthy
}

if [[ "${1:-}" == "--status" ]]; then
    info "current state:"
    overall=0
    report_service "controller PF " "$CONTROLLER_PID_FILE" "$CONTROLLER_LOG" "$CONTROLLER_PF_PORT"                       || overall=1
    report_service "extauth       " "$EXTAUTH_PID_FILE"    "$EXTAUTH_LOG"    "$EXTAUTH_GRPC_PORT" "$EXTAUTH_HTTP_PORT" || overall=1
    report_service "agentgateway  " "$AGW_PID_FILE"        "$AGW_LOG"        "$AGW_PORT"                                || overall=1
    echo ""
    if [[ $overall -eq 0 ]]; then
        ok "all three services healthy"
    else
        err "one or more services are down or partial — run '$0' to (re)start"
    fi
    exit $overall
fi

# ── Idempotency check ──────────────────────────────────────────────────────
# If a previous run left all three services healthy, print state and exit
# rather than tearing down + relaunching. The check is conservative: every
# pid file must exist, every pid must be alive, and every expected port
# must be listening. Any partial state falls through to a full relaunch.
if pid_alive_from_file "$CONTROLLER_PID_FILE" \
   && pid_alive_from_file "$EXTAUTH_PID_FILE" \
   && pid_alive_from_file "$AGW_PID_FILE" \
   && port_listening "$CONTROLLER_PF_PORT" \
   && port_listening "$EXTAUTH_GRPC_PORT" \
   && port_listening "$EXTAUTH_HTTP_PORT" \
   && port_listening "$AGW_PORT"; then
    ok "all three services already running — leaving them alone"
    echo "  controller PF  → :$CONTROLLER_PF_PORT  pid=$(cat "$CONTROLLER_PID_FILE")  ($CONTROLLER_LOG)"
    echo "  extauth        → :$EXTAUTH_GRPC_PORT (grpc) :$EXTAUTH_HTTP_PORT (http)  pid=$(cat "$EXTAUTH_PID_FILE")  ($EXTAUTH_LOG)"
    echo "  agentgateway   → :$AGW_PORT  pid=$(cat "$AGW_PID_FILE")  ($AGW_LOG)"
    echo ""
    echo "Force a relaunch with: $0 --stop && $0"
    exit 0
fi

# ── Preflight ──────────────────────────────────────────────────────────────
command -v agentgateway >/dev/null \
    || { err "'agentgateway' not on PATH (see demo/aauth/README.md prereqs)"; exit 1; }
command -v kubectl >/dev/null \
    || { err "'kubectl' not on PATH"; exit 1; }
[[ -x "$EXTAUTH_REPO/aauth-service" ]] \
    || { err "extauth binary not found at $EXTAUTH_REPO/aauth-service. Build with 'cd $EXTAUTH_REPO && go build -o aauth-service ./cmd/server' or set EXTAUTH_REPO."; exit 1; }
[[ -f "$REPO_ROOT/demo/aauth/resource_key.pem" ]] \
    || { err "resource keypair missing at demo/aauth/resource_key.pem. Generate with: openssl genpkey -algorithm ed25519 -out demo/aauth/resource_key.pem"; exit 1; }
[[ -f "$REPO_ROOT/demo/aauth/aauth-config.yaml" ]] \
    || { err "aauth-config.yaml missing at $REPO_ROOT/demo/aauth/aauth-config.yaml"; exit 1; }
[[ -f "$REPO_ROOT/demo/aauth/agw-config.yaml" ]] \
    || { err "agw-config.yaml missing at $REPO_ROOT/demo/aauth/agw-config.yaml"; exit 1; }

kubectl get deploy -n kagent kagent-controller >/dev/null 2>&1 \
    || { err "deploy/kagent-controller not found in namespace kagent"; exit 1; }

# Clean slate.
stop_all

# ── 1. Controller port-forward ─────────────────────────────────────────────
info "port-forward kagent-controller → :$CONTROLLER_PF_PORT"
kubectl port-forward -n kagent svc/kagent-controller "$CONTROLLER_PF_PORT:8083" \
    >"$CONTROLLER_LOG" 2>&1 &
echo $! > "$CONTROLLER_PID_FILE"

# Wait for the JWKS endpoint to be reachable — extauth fetches this on startup.
for i in $(seq 1 30); do
    if curl -sf "http://127.0.0.1:$CONTROLLER_PF_PORT/.well-known/jwks.json" >/dev/null 2>&1; then
        ok "controller JWKS reachable at http://localhost:$CONTROLLER_PF_PORT/.well-known/jwks.json"
        break
    fi
    [[ $i -eq 30 ]] && { err "controller JWKS not reachable; see $CONTROLLER_LOG"; stop_all; exit 1; }
    sleep 1
done

# ── 2. extauth-aauth-resource ──────────────────────────────────────────────
info "launching extauth-aauth-resource on :$EXTAUTH_GRPC_PORT (grpc) + :$EXTAUTH_HTTP_PORT (http)"
( cd "$EXTAUTH_REPO" \
    && AAUTH_CONFIG="$REPO_ROOT/demo/aauth/aauth-config.yaml" \
       ./aauth-service \
       >"$EXTAUTH_LOG" 2>&1 ) &
echo $! > "$EXTAUTH_PID_FILE"

for i in $(seq 1 30); do
    # extauth dispatches its /.well-known/* endpoints by Host header (see
    # aauth-config.yaml's `hosts:` list) — a 127.0.0.1 probe gets a 404
    # because that host isn't registered, so verify the listener is up
    # instead. Need both ports: 7070 (gRPC for agentgateway ext_authz) +
    # 8080 (HTTP for metadata/JWKS).
    if lsof -iTCP:"$EXTAUTH_GRPC_PORT" -sTCP:LISTEN >/dev/null 2>&1 \
       && lsof -iTCP:"$EXTAUTH_HTTP_PORT" -sTCP:LISTEN >/dev/null 2>&1; then
        ok "extauth listening on :$EXTAUTH_GRPC_PORT (grpc) + :$EXTAUTH_HTTP_PORT (http)"
        break
    fi
    [[ $i -eq 30 ]] && { err "extauth did not come up; see $EXTAUTH_LOG"; stop_all; exit 1; }
    sleep 1
done

# ── 3. agentgateway ────────────────────────────────────────────────────────
# agw-config.yaml references ${OPENAI_API_KEY} for the OpenAI backend's
# Authorization header. agentgateway is the *only* component in this demo
# that holds the real upstream key — the in-cluster `kagent-openai` Secret
# stays a placeholder so the agent pod never sees the real key. Resolution
# order:
#
#   1. OPENAI_API_KEY already exported in this shell → use it
#   2. else, if ~/bin/openai-key exists → load from there
#   3. else fall back to a placeholder so agentgateway can parse its config
#      (you'll get 401 from OpenAI; AAuth verification still happens, which
#      is enough to demo the door-1 path)
info "launching agentgateway on :$AGW_PORT"
if [[ -z "${OPENAI_API_KEY:-}" ]] && [[ -r "$HOME/bin/openai-key" ]]; then
    OPENAI_API_KEY="$(tr -d '\n' < "$HOME/bin/openai-key")"
    info "loaded OPENAI_API_KEY from $HOME/bin/openai-key (len=${#OPENAI_API_KEY})"
fi
: "${OPENAI_API_KEY:=sk-placeholder-not-real}"
export OPENAI_API_KEY
agentgateway -f "$REPO_ROOT/demo/aauth/agw-config.yaml" \
    >"$AGW_LOG" 2>&1 &
echo $! > "$AGW_PID_FILE"

for i in $(seq 1 30); do
    # agentgateway's admin/health surface isn't standardized — just confirm
    # the bind succeeded by checking the listener is up.
    if lsof -iTCP:"$AGW_PORT" -sTCP:LISTEN >/dev/null 2>&1; then
        ok "agentgateway listening on :$AGW_PORT"
        break
    fi
    [[ $i -eq 30 ]] && { err "agentgateway did not bind :$AGW_PORT; see $AGW_LOG"; stop_all; exit 1; }
    sleep 1
done

echo ""
ok "all three services up"
echo "  controller PF  → http://localhost:$CONTROLLER_PF_PORT     ($CONTROLLER_LOG)"
echo "  extauth HTTP   → http://localhost:$EXTAUTH_HTTP_PORT      ($EXTAUTH_LOG)"
echo "  extauth gRPC   → localhost:$EXTAUTH_GRPC_PORT             "
echo "  agentgateway   → http://localhost:$AGW_PORT               ($AGW_LOG)"
echo ""
echo "Agent pods reach agentgateway at http://host.docker.internal:$AGW_PORT."
echo "Point ModelConfig.openai.base_url at http://host.docker.internal:$AGW_PORT/openai."
echo ""
echo "Tear down with: $0 --stop"
