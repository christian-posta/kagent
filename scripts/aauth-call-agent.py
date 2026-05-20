#!/usr/bin/env python3
"""Sign an inbound A2A request to a kagent agent and POST it.

This is a small debugging helper for the Phase 3 AAuth verification demo.
It is *not* shipped as a kagent runtime feature.

What it does:
  1. Reads the caller's projected ServiceAccount token (default location, or
     the file specified by AAUTH_SA_TOKEN_PATH).
  2. Generates a fresh Ed25519 keypair.
  3. Mints an aa-agent+jwt at the controller's POST /aauth/agent-jwt
     endpoint, presenting the SA token as a Bearer credential. The
     controller's TokenReview derives sub from the SA's identity, so the
     resulting JWT belongs to whichever workload is running this script.
  4. Signs an A2A message/send JSON-RPC request with the keypair + JWT.
  5. POSTs it to the target agent and prints the response.

Intended for use inside the cluster — run it from a curl/python probe pod
that has the audience-scoped SA token mounted. The agent runtime's Phase 3
verifier should then log `aauth: verified caller=aauth:<this-sa>@<ns>.kagent.local`.

Usage (inside a pod):
    python aauth-call-agent.py \
        --controller http://kagent-controller.kagent:8083 \
        --agent     http://aauth-test-agent.kagent:8080 \
        --message   "hello from probe"

Outside a pod, supply an SA token file via --sa-token-file.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import uuid

import httpx

try:
    import aauth as aauth_lib
except ImportError:
    sys.stderr.write(
        "ERROR: the 'aauth' Python library is not installed in this interpreter. "
        "Install it (e.g. pip install aauth) or run this from a pod whose image "
        "includes kagent-adk.\n"
    )
    sys.exit(2)


DEFAULT_SA_TOKEN_PATH = "/var/run/secrets/kubernetes.io/serviceaccount/token"


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--controller", required=True, help="Base URL of the kagent controller (e.g. http://kagent-controller.kagent:8083)")
    p.add_argument("--agent", required=True, help="Base URL of the target agent (e.g. http://aauth-test-agent.kagent:8080)")
    p.add_argument("--message", default="probe message", help="Text payload for the A2A message/send call")
    p.add_argument("--sa-token-file", default=os.getenv("AAUTH_SA_TOKEN_PATH", DEFAULT_SA_TOKEN_PATH),
                   help="Path to the SA token to present at /aauth/agent-jwt")
    p.add_argument("--no-message", action="store_true",
                   help="Only mint the JWT and print it (do not call the agent)")
    return p.parse_args()


def read_sa_token(path: str) -> str:
    with open(path) as fh:
        token = fh.read().strip()
    if not token:
        raise SystemExit(f"SA token at {path} is empty")
    return token


def mint_jwt(controller_url: str, sa_token: str, public_jwk: dict) -> tuple[str, dict]:
    url = controller_url.rstrip("/") + "/aauth/agent-jwt"
    resp = httpx.post(
        url,
        headers={"Authorization": f"Bearer {sa_token}", "Content-Type": "application/json"},
        json={"public_key_jwk": public_jwk},
        timeout=10.0,
    )
    if resp.status_code != 200:
        raise SystemExit(f"JWT mint failed: HTTP {resp.status_code} body={resp.text[:400]!r}")
    body = resp.json()
    token = body.get("token")
    if not token:
        raise SystemExit(f"controller response missing 'token': {body!r}")
    return token, body


def main() -> None:
    args = parse_args()
    sa_token = read_sa_token(args.sa_token_file)

    priv, pub = aauth_lib.generate_ed25519_keypair()
    pub_jwk = aauth_lib.public_key_to_jwk(pub)

    print(f"==> minting aa-agent+jwt at {args.controller}", flush=True)
    jwt, mint_body = mint_jwt(args.controller, sa_token, pub_jwk)
    print(f"    expires_in={mint_body.get('expires_in')}")

    if args.no_message:
        print(jwt)
        return

    agent_url = args.agent.rstrip("/") + "/"
    rpc_id = "probe-" + uuid.uuid4().hex[:8]
    msg_id = "msg-" + uuid.uuid4().hex[:8]
    body = {
        "jsonrpc": "2.0",
        "id": rpc_id,
        "method": "message/send",
        "params": {
            "message": {
                "kind": "message",
                "messageId": msg_id,
                "role": "user",
                "parts": [{"kind": "text", "text": args.message}],
            }
        },
    }
    body_bytes = json.dumps(body).encode("utf-8")

    # Headers the verifier needs to see; aauth library sets Signature-Key etc.
    base_headers = {
        "Content-Type": "application/json",
    }

    sig_headers = aauth_lib.sign_request(
        method="POST",
        target_uri=agent_url,
        headers=base_headers,
        body=None,
        private_key=priv,
        sig_scheme="jwt",
        jwt=jwt,
    )

    headers = {**base_headers, **sig_headers}
    print(f"==> POST {agent_url}", flush=True)
    print(f"    Signature-Input: {headers.get('Signature-Input')}")
    resp = httpx.post(agent_url, headers=headers, content=body_bytes, timeout=30.0)
    print(f"<== HTTP {resp.status_code}")
    try:
        print(json.dumps(resp.json(), indent=2)[:2000])
    except Exception:
        print(resp.text[:2000])


if __name__ == "__main__":
    main()
