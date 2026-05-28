#!/usr/bin/env bash
#
# Dashboard view for the kagent-on-substrate demo. Prints:
#   - SandboxAgent count and Ready status
#   - WorkerPool replicas
#   - Worker pods currently Running
#   - Actors in substrate, grouped by status (RUNNING / SUSPENDED)
#   - Snapshot disk usage in rustfs
#
# Used by `make demo-substrate-status` and embedded between steps in DEMO.md.
# Intentionally chatty so it reads well on a recording.
set -euo pipefail

NS="${SUBSTRATE_DEMO_NS:-kagent-substrate-poc}"
POOL="${SUBSTRATE_DEMO_POOL:-poc-pool}"

# ANSI helpers; bash colors only when stdout is a tty.
if [[ -t 1 ]]; then
  bold=$'\033[1m'; dim=$'\033[2m'; reset=$'\033[0m'; cyan=$'\033[36m'
else
  bold=""; dim=""; reset=""; cyan=""
fi

section() { printf "\n%s── %s %s\n" "$cyan" "$1" "$reset"; }

section "SandboxAgents in $NS"
kubectl get sandboxagent -n "$NS" \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.conditions[?(@.type=="Ready")].status}{"\t"}{.status.conditions[?(@.type=="Ready")].message}{"\n"}{end}' \
  2>/dev/null | column -t -s $'\t' || echo "(none)"

section "WorkerPool $POOL"
kubectl get workerpool "$POOL" -n "$NS" -o jsonpath='replicas={.spec.replicas}{"\n"}' 2>/dev/null || echo "(missing)"
echo
echo "${dim}worker pods Running:${reset}"
kubectl get pods -n "$NS" --no-headers 2>/dev/null \
  | awk '{printf "  %-50s %s\n", $1, $3}' \
  | head -10

section "Substrate actors for $NS"
# kubectl ate get actors (list) is sometimes flaky against degraded Valkey
# clusters; iterate ListWorkers instead — every actor that has been touched
# by ResumeActor shows up there as ASSIGNED, and for SUSPENDED actors we read
# the kagent SandboxAgent.status message which already includes status.
kubectl ate get workers 2>/dev/null \
  | awk -v ns="$NS" 'NR==1 || ($1==ns && $2!="ate-demo-counter")' \
  | column -t

echo
echo "${dim}actor status (from SandboxAgent.status.conditions[Ready].message):${reset}"
kubectl get sandboxagent -n "$NS" -o jsonpath='{range .items[*]}{.metadata.name}{": "}{.status.conditions[?(@.type=="Ready")].message}{"\n"}{end}' 2>/dev/null \
  | sed 's/^/  /'

section "Snapshot storage (rustfs)"
kubectl exec -n ate-system deploy/rustfs -- du -sh /data/ate-snapshots 2>/dev/null \
  || echo "(rustfs not reachable)"
