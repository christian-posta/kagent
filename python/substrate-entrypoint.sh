#!/bin/sh
#
# Substrate config-via-env shim. Lives in the kagent app's substrate variant
# (see python/Dockerfile.substrate). substrate's ActorTemplate container
# schema doesn't allow volume mounts, so the kagent translator's
# `/config` Secret can't be carried across. Instead the substrate backend
# inlines the JSON config + agent card as env vars; this shim materializes
# them on disk before exec'ing the agent runtime.
#
# Required env (always set by the substrate sandbox backend):
#   KAGENT_CONFIG_JSON       — contents of config.json
#   KAGENT_AGENT_CARD_JSON   — contents of agent-card.json
#
# Optional env:
#   KAGENT_SRT_SETTINGS_JSON — contents of srt-settings.json (only when the
#                              agent has sandbox/skills settings)
#
# Everything else (KAGENT_NAME / KAGENT_NAMESPACE / KAGENT_URL / model
# provider secrets / OTEL_*) flows from the standard kagent translator env
# block unchanged.
#
# After materializing config we exec kagent-adk static --local. --local
# swaps the controller-backed SessionService for the in-memory one, so the
# agent can serve requests without reaching the kagent controller — useful
# when the controller is out-of-cluster or otherwise unroutable from the
# sandbox. Sessions don't persist across suspend cycles; that's acceptable
# for a PoC.

set -eu

CONFIG_DIR="${CONFIG_DIR:-/tmp/config}"
mkdir -p "$CONFIG_DIR"

if [ -z "${KAGENT_CONFIG_JSON:-}" ]; then
  echo "substrate-entrypoint: FATAL: KAGENT_CONFIG_JSON is not set" >&2
  exit 64
fi
if [ -z "${KAGENT_AGENT_CARD_JSON:-}" ]; then
  echo "substrate-entrypoint: FATAL: KAGENT_AGENT_CARD_JSON is not set" >&2
  exit 64
fi

printf '%s' "$KAGENT_CONFIG_JSON"     >"$CONFIG_DIR/config.json"
printf '%s' "$KAGENT_AGENT_CARD_JSON" >"$CONFIG_DIR/agent-card.json"
if [ -n "${KAGENT_SRT_SETTINGS_JSON:-}" ]; then
  printf '%s' "$KAGENT_SRT_SETTINGS_JSON" >"$CONFIG_DIR/srt-settings.json"
fi

echo "substrate-entrypoint: materialized config in $CONFIG_DIR (config.json=$(wc -c <"$CONFIG_DIR/config.json")B, agent-card.json=$(wc -c <"$CONFIG_DIR/agent-card.json")B)"

# Substrate's OCI spec resets the container ENV to a minimal PATH, dropping
# the kagent-adk image's `/.kagent/.venv/bin` (where the kagent-adk console
# script lives). Add it back so plain `kagent-adk …` resolves, and also
# resolve the absolute path defensively in case the venv layout changes.
export PATH="/.kagent/.venv/bin:/.kagent/bin:$PATH"
KAGENT_ADK_BIN="${KAGENT_ADK_BIN:-/.kagent/.venv/bin/kagent-adk}"

# The default CMD is just `--host 0.0.0.0 --port 80`; forward everything
# the caller passed, plus the static-mode args and --local.
# --loop asyncio / --http h11 force pure-Python event loop + HTTP parser
# implementations. uvloop's epoll/eventfd usage has historically tripped
# gVisor's runsc checkpoint (exit 128); the pure-Python defaults are slower
# but checkpoint cleanly. See SUBSTRATE.md §11.
exec "$KAGENT_ADK_BIN" static --local --loop asyncio --http h11 --filepath "$CONFIG_DIR" "$@"
