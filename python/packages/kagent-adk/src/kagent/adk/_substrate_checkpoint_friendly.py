"""
Idle-driven httpx connection-pool cleanup, for kagent agents running inside
agent-substrate sandboxes.

Background
----------
substrate's gVisor `runsc checkpoint` can't reliably serialize a Python
process that's holding live keep-alive TLS sockets — most commonly the
``httpx.AsyncClient`` connection pool the OpenAI SDK (and similar LLM
clients) keep open to providers like ``api.openai.com``. Symptom is an
``exit status 128`` from ``runsc checkpoint`` and a torn-down sandbox
container (see ``SUBSTRATE.md §19``). The first golden snapshot of a
fresh idle process works fine; subsequent suspends after request handling
fail.

This module registers FastAPI middleware that records each request's
completion time, plus a background asyncio task that periodically walks
``gc.get_objects()`` for live ``httpx.AsyncClient`` instances and closes
them when the agent has been idle past a short threshold. By the time
substrate's IdleSuspender fires ``SuspendActor`` (default 30s idle), the
process has no live external connections and the checkpoint succeeds.

Activated explicitly by the ``kagent-adk static --local`` path. Deployment-
mode agents (long-lived Pods) don't need it; suspend is irrelevant there.

Costs
-----
Closing the httpx client invalidates the OpenAI SDK's cached client wrapper
that owns it. The next LLM call will get a fresh client + fresh TLS
handshake — measurable per-call latency increase on the first call after
each suspend cycle, but no behavior change otherwise.
"""

from __future__ import annotations

import asyncio
import gc
import logging
import time

import httpx
from fastapi import FastAPI

_logger = logging.getLogger(__name__)

# How long after the most recent request to proactively close idle clients.
# Must be < substrate's --substrate-idle-timeout (default 30s) so the close
# happens before substrate's checkpoint fires.
IDLE_CLOSE_THRESHOLD_SECONDS = 3.0

# How often the background task checks the idle timer. Cheap enough to run
# every second; the sweep itself is O(live-Python-objects).
SWEEP_INTERVAL_SECONDS = 1.0


def install(app: FastAPI) -> None:
    """Attach the request-activity middleware and the close-idle-clients
    background task to ``app``. Idempotent."""
    if getattr(app.state, "_substrate_checkpoint_friendly_installed", False):
        return
    app.state._substrate_checkpoint_friendly_installed = True

    state = {"last_request_monotonic": 0.0, "last_close_monotonic": 0.0}

    @app.middleware("http")
    async def _track_request_activity(request, call_next):
        try:
            return await call_next(request)
        finally:
            state["last_request_monotonic"] = time.monotonic()

    @app.on_event("startup")
    async def _spawn_close_loop() -> None:
        asyncio.create_task(_close_idle_clients_loop(state))


async def _close_idle_clients_loop(state: dict) -> None:
    """Background task: every SWEEP_INTERVAL_SECONDS, check whether the
    process has been idle past IDLE_CLOSE_THRESHOLD_SECONDS since the last
    request. If so AND we haven't already swept since that request, close
    every live ``httpx.AsyncClient`` in the heap.

    Gated on both "has there been a request" and "haven't already closed
    since the most recent request" so that:
      * The very first golden snapshot (taken before any request) runs
        without us closing the OpenAI client's pool prematurely.
      * After a request, we close once and then leave the heap alone
        rather than thrashing the pool every sweep tick.

    No shutdown handler — we want the task to survive gVisor suspend/resume
    cycles without the FastAPI lifespan ever cancelling it.
    """
    while True:
        try:
            await asyncio.sleep(SWEEP_INTERVAL_SECONDS)
            last_request = state["last_request_monotonic"]
            if last_request == 0:
                continue
            if time.monotonic() - last_request < IDLE_CLOSE_THRESHOLD_SECONDS:
                continue
            if state["last_close_monotonic"] >= last_request:
                continue
            await _close_all_idle_httpx_clients()
            state["last_close_monotonic"] = time.monotonic()
        except asyncio.CancelledError:
            return
        except Exception:
            _logger.exception("substrate-checkpoint-friendly: close loop iteration failed")


async def _close_all_idle_httpx_clients() -> int:
    """Walk the live object graph, close every open ``httpx.AsyncClient``.
    Returns the count actually closed (callers can log)."""
    closed = 0
    targets: list[httpx.AsyncClient] = []
    # Two-phase: collect then close. Mutating the heap (via aclose) while
    # iterating gc.get_objects() is risky.
    for obj in gc.get_objects():
        try:
            if isinstance(obj, httpx.AsyncClient) and not obj.is_closed:
                targets.append(obj)
        except Exception:
            # Some live objects raise on isinstance (e.g. weakly-referenced
            # proxies in unusual states); skip rather than abort.
            continue
    for client in targets:
        try:
            await client.aclose()
            closed += 1
        except Exception:
            _logger.exception("substrate-checkpoint-friendly: failed to aclose httpx.AsyncClient")
    if closed:
        _logger.info("substrate-checkpoint-friendly: closed %d idle httpx.AsyncClient(s)", closed)
    return closed
