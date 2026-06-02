import type { AgentResponse } from "@/types";

/** True only for openshell-backed AgentHarness rows. Substrate-backed
 *  harnesses ALSO populate `openshellAgentHarness` (legacy field name), but
 *  the controller now sets `runtime` on the response so we can tell them
 *  apart. Empty `runtime` defaults to "openshell" for legacy compatibility. */
export function isOpenshellSandboxRow(item: AgentResponse): boolean {
  const harness = item.openshellAgentHarness;
  if (!harness?.gatewaySandboxName) return false;
  const rt = harness.runtime ?? "openshell";
  return rt === "openshell";
}

/** True for substrate-backed AgentHarness rows (`spec.runtime: substrate`).
 *  These open via the controller's gateway proxy at
 *  `/api/agentharnesses/<ns>/<name>/gateway/`, not the openshell SSH path. */
export function isSubstrateHarnessRow(item: AgentResponse): boolean {
  return item.openshellAgentHarness?.runtime === "substrate";
}

/** Gateway URL for a substrate-backed AgentHarness. Mirrors the path the
 *  kagent controller's HTTP proxy registers in server.go
 *  (`APIPathAgentHarnesses + "/{namespace}/{name}/"`). */
export function substrateHarnessGatewayHref(item: AgentResponse): string | null {
  if (!isSubstrateHarnessRow(item)) return null;
  const ns = item.agent?.metadata?.namespace;
  const name = item.agent?.metadata?.name;
  if (!ns || !name) return null;
  return `/api/agentharnesses/${ns}/${name}/gateway/`;
}

export type OpenshellTerminalLinkParams = {
  gatewaySandboxName: string;
  namespace?: string;
  /** Sandbox CR name (Kubernetes metadata.name). */
  crName?: string;
  modelConfigRef?: string;
  /**
   * OpenClaw / NemoClaw harness: terminal offers “Launch plain shell” vs default session (e.g. `openclaw tui`).
   */
  clawHarness?: boolean;
};

/** Opens `/openshell` with auto-connect when the page loads (`connect=1`). */
export function openshellTerminalHref(params: OpenshellTerminalLinkParams): string {
  const q = new URLSearchParams({
    sandbox: params.gatewaySandboxName,
    connect: "1",
  });
  if (params.clawHarness) {
    q.set("clawHarness", "1");
  }
  const ns = params.namespace?.trim();
  const name = params.crName?.trim();
  const mc = params.modelConfigRef?.trim();
  if (ns) q.set("ns", ns);
  if (name) q.set("name", name);
  if (mc) q.set("modelConfigRef", mc);
  return `/openshell?${q.toString()}`;
}
