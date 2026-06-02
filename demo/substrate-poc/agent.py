"""
Minimal stand-in for a kagent agent, used to validate that agent-substrate
can host a Python web server (gVisor sandboxing, suspend/resume of memory
state, request routing via atenet, outbound connectivity).

Surface (intentionally close to kagent):
  GET  /.well-known/agent-card.json   readiness; used by substrate's golden-snapshot probe
  GET  /info                          startup UUID + bump counter (proves RAM survives resume)
  GET  /connectivity                  active outbound probes: DNS, in-cluster HTTP, external HTTP
  POST /echo                          trivial request/response
  GET  /healthz                       liveness

The startup UUID and counter are kept ONLY in memory. If substrate's gVisor
checkpoint/restore preserves process memory across suspend cycles, the UUID
must NOT change after a resume.
"""
from __future__ import annotations

import os
import sys
import socket
import time
import uuid
from urllib import request as urlrequest
from urllib.error import URLError

# Eager unbuffered logging so we can see exactly how far we get under gVisor.
print("=== agent.py: module top reached ===", flush=True)
sys.stdout.flush()

from fastapi import FastAPI
from fastapi.responses import JSONResponse
print("=== agent.py: imports complete ===", flush=True)


STARTUP_UUID = str(uuid.uuid4())
STARTUP_TIME = time.time()
print(f"=== agent.py: STARTUP_UUID={STARTUP_UUID} ===", flush=True)

_counter = 0


app = FastAPI()


@app.get("/healthz")
def healthz():
    return {"ok": True}


@app.get("/.well-known/agent-card.json")
def agent_card():
    return {
        "name": "substrate_poc_agent",
        "description": "Phase 0 stand-in. Validates substrate can host a Python web server.",
        "url": "http://substrate-poc:8080",
        "version": "0.0.1",
        "capabilities": {"streaming": False},
        "defaultInputModes": ["text"],
        "defaultOutputModes": ["text"],
        "skills": [],
        "preferredTransport": "JSONRPC",
    }


@app.get("/info")
def info():
    global _counter
    _counter += 1
    return {
        "startup_uuid": STARTUP_UUID,
        "startup_time": STARTUP_TIME,
        "uptime_seconds": time.time() - STARTUP_TIME,
        "request_count": _counter,
        "pod_hostname": socket.gethostname(),
        "env_kagent": {k: v for k, v in os.environ.items() if k.startswith("KAGENT_")},
    }


def _probe_dns(host: str) -> dict:
    try:
        addrs = socket.getaddrinfo(host, None)
        return {"ok": True, "addrs": list({a[4][0] for a in addrs})}
    except Exception as e:
        return {"ok": False, "error": f"{type(e).__name__}: {e}"}


def _probe_http(url: str, timeout: float = 4.0) -> dict:
    try:
        req = urlrequest.Request(url, method="GET", headers={"User-Agent": "substrate-poc/0.0.1"})
        with urlrequest.urlopen(req, timeout=timeout) as resp:
            return {"ok": True, "status": resp.status}
    except URLError as e:
        return {"ok": False, "error": f"URLError: {e.reason}"}
    except Exception as e:
        return {"ok": False, "error": f"{type(e).__name__}: {e}"}


@app.get("/connectivity")
def connectivity():
    """Active outbound probes from inside the sandbox.

    Each probe runs independently so a single failure doesn't shadow others.
    The probe set is designed to differentiate failure modes:
      * dns_kube_api  : in-cluster DNS resolves kubernetes.default
      * dns_external  : external DNS resolves a public hostname
      * http_substrate: in-cluster HTTP to substrate's own router
      * http_external : external HTTPS to a public endpoint
    """
    return {
        "dns_kube_api":   _probe_dns("kubernetes.default.svc.cluster.local"),
        "dns_external":   _probe_dns("example.com"),
        "http_substrate": _probe_http("http://atenet-router.ate-system.svc/.well-known/agent-card.json"),
        "http_external":  _probe_http("https://example.com"),
    }


@app.post("/echo")
async def echo(payload: dict):
    return JSONResponse({"received": payload, "from": STARTUP_UUID})


# ---------------------------------------------------------------------------
# Minimal A2A JSON-RPC handler so the kagent A2A mux can drive this agent
# end-to-end from the UI chat panel. The Phase 0 stand-in doesn't run an LLM;
# it echoes the prompt back as the artifact text. That's enough to prove the
# full path: UI → kagent /api/a2a → atenet → substrate resume → agent process
# → reply rendered in the chat panel.
#
# Supports both:
#   - message/send   → returns a single application/json response
#   - message/stream → returns text/event-stream with SSE-framed task chunks
# The UI defaults to message/stream regardless of capabilities.streaming, so
# we have to handle it.

import asyncio
import json
from fastapi.responses import StreamingResponse


def _build_task(req_text: str, req_counter: int, context_id: str) -> dict:
    reply = (
        f"echo from substrate-poc agent ({STARTUP_UUID[:8]}, "
        f"request #{req_counter}): {req_text}"
    )
    return {
        "kind": "task",
        "id": str(uuid.uuid4()),
        # A2A: the task's contextId MUST echo the inbound message's contextId
        # so the client (UI / curl) can correlate the response to its session.
        # The UI's chat panel keys rendered history by contextId — a fresh
        # random one here makes the response invisible in the chat.
        "contextId": context_id,
        "status": {"state": "completed"},
        "artifacts": [{
            "artifactId": str(uuid.uuid4()),
            "parts": [{"kind": "text", "text": reply}],
        }],
        "history": [],
        "metadata": {},
    }


def _extract_text(payload: dict) -> str:
    msg = payload.get("params", {}).get("message", {}) or {}
    parts = msg.get("parts") or []
    return next(
        (p.get("text", "") for p in parts if p.get("kind") == "text"),
        "(no text)",
    )


def _extract_context_id(payload: dict) -> str:
    """Echo back the client's contextId; generate one if absent."""
    msg = payload.get("params", {}).get("message", {}) or {}
    cid = msg.get("contextId")
    return cid if cid else str(uuid.uuid4())


@app.post("/")
async def a2a_rpc(payload: dict):
    global _counter
    _counter += 1
    method = payload.get("method", "")
    req_id = payload.get("id")

    ctx_id = _extract_context_id(payload)

    if method == "message/send":
        return JSONResponse({
            "jsonrpc": "2.0",
            "id": req_id,
            "result": _build_task(_extract_text(payload), _counter, ctx_id),
        })

    if method == "message/stream":
        # Build the task but DON'T attach the artifact to the initial task
        # event — the kagent UI's `handleMessageEvent` treats `kind: task`
        # events as a "streaming started" signal and discards anything
        # inside. The actual reply must arrive as a separate
        # `kind: artifact-update` event. See
        # ui/src/lib/messageHandlers.ts:1086 and 1096.
        task = _build_task(_extract_text(payload), _counter, ctx_id)
        artifact = task.pop("artifacts")[0]  # detach for the artifact-update

        async def _events():
            # A2A streaming sequence the kagent UI expects (mirrors what the
            # real kagent ADK emits — see
            # python/packages/kagent-adk/src/kagent/adk/_agent_executor.py):
            #
            #   1. `kind: task`           — opens the task (no artifact yet)
            #   2. `kind: artifact-update` — carries the actual reply text
            #   3. `kind: status-update`   — final=true, closes the stream
            task_event = {"jsonrpc": "2.0", "id": req_id, "result": task}
            yield f"data: {json.dumps(task_event)}\n\n"

            artifact_update = {
                "jsonrpc": "2.0",
                "id": req_id,
                "result": {
                    "kind": "artifact-update",
                    "taskId": task["id"],
                    "contextId": task["contextId"],
                    "artifact": artifact,
                    "lastChunk": True,
                },
            }
            yield f"data: {json.dumps(artifact_update)}\n\n"

            status_update = {
                "jsonrpc": "2.0",
                "id": req_id,
                "result": {
                    "kind": "status-update",
                    "taskId": task["id"],
                    "contextId": task["contextId"],
                    "status": {"state": "completed"},
                    "final": True,
                },
            }
            yield f"data: {json.dumps(status_update)}\n\n"
            await asyncio.sleep(0)

        return StreamingResponse(_events(), media_type="text/event-stream")

    return JSONResponse({
        "jsonrpc": "2.0",
        "id": req_id,
        "error": {"code": -32601, "message": f"method not implemented: {method}"},
    })
