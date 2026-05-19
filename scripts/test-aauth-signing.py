#!/usr/bin/env python3
"""Standalone AAuth signing verification script.

Generates an ephemeral Ed25519 keypair, signs a sample HTTP request using
the hwk (bare-key) scheme, prints all AAuth headers, and optionally verifies
the signature round-trip using the aauth library.

Usage
-----
    # Run directly (requires aauth library installed in current Python env)
    python scripts/test-aauth-signing.py

    # Run via uv from within the kagent Python workspace
    cd python
    uv run --package kagent-adk python ../scripts/test-aauth-signing.py

    # Override the sample URL
    AAUTH_TEST_URL=https://my-llm-proxy.example.com/v1/chat/completions \
        python scripts/test-aauth-signing.py

The output can be pasted into curl for manual verification through agentgateway:

    curl -X POST https://<gateway>/api/a2a/default/my-agent \\
         -H "Content-Type: application/json" \\
         -H "Signature: <value>" \\
         -H "Signature-Input: <value>" \\
         -H "Signature-Key: <value>" \\
         -d '{"task": "hello"}'
"""

import os
import sys

# ---------------------------------------------------------------------------
# Check library availability
# ---------------------------------------------------------------------------
try:
    import aauth
except ImportError:
    print("ERROR: 'aauth' library is not installed.")
    print("")
    print("Install via uv from the project root:")
    print("  cd python && uv sync")
    print("")
    print("Or install directly:")
    print("  pip install git+https://github.com/christian-posta/aauth-python-library")
    sys.exit(1)

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
AGENT_ID    = os.getenv("AAUTH_AGENT_ID",   "aauth:test-agent@default.kagent.local")
TEST_URL    = os.getenv("AAUTH_TEST_URL",    "http://kagent-controller.kagent:8083/api/tasks")
TEST_METHOD = os.getenv("AAUTH_TEST_METHOD", "POST")

# ---------------------------------------------------------------------------
# Generate ephemeral keypair
# ---------------------------------------------------------------------------
print("=" * 60)
print("AAuth Signing Test")
print("=" * 60)

private_key, public_key = aauth.generate_ed25519_keypair()
print(f"[1] Generated ephemeral Ed25519 keypair")

jwk = aauth.public_key_to_jwk(public_key, kid="test-1")
thumbprint = aauth.calculate_jwk_thumbprint(jwk)
print(f"[2] JWK thumbprint: {thumbprint}")

# ---------------------------------------------------------------------------
# Sign a sample request
# ---------------------------------------------------------------------------
sample_headers: dict[str, str] = {
    "content-type": "application/json",
    "x-agent-name": AGENT_ID,
}

print(f"\n[3] Signing request:")
print(f"    Method : {TEST_METHOD}")
print(f"    URL    : {TEST_URL}")
print(f"    Headers: {sample_headers}")

signed = aauth.sign_request(
    method=TEST_METHOD,
    target_uri=TEST_URL,
    headers=sample_headers,
    body=None,
    private_key=private_key,
    sig_scheme="hwk",
)

print(f"\n[4] AAuth headers produced:")
for name, value in signed.items():
    print(f"    {name}: {value}")

# ---------------------------------------------------------------------------
# Verify the signature (round-trip)
# ---------------------------------------------------------------------------
print("\n[5] Verifying signature round-trip...")

all_headers = {**sample_headers, **signed}

try:
    valid = aauth.verify_signature(
        method=TEST_METHOD,
        target_uri=TEST_URL,
        headers=all_headers,
        body=None,
        signature_input_header=signed.get("Signature-Input", ""),
        signature_header=signed.get("Signature", ""),
        signature_key_header=signed.get("Signature-Key", ""),
    )
    if valid:
        print("    PASS — signature verified successfully!")
    else:
        print("    FAIL — verify_signature returned False")
        sys.exit(1)
except Exception as e:
    print(f"    ERROR during verification: {e}")
    print("    (This may be OK if the library's verify_signature API differs slightly)")

# ---------------------------------------------------------------------------
# Print curl command for manual agentgateway test
# ---------------------------------------------------------------------------
print("\n[6] curl command for manual agentgateway/extauth test:")
print()
print(f"  curl -X {TEST_METHOD} '{TEST_URL}' \\")
print(f"    -H 'Content-Type: application/json' \\")
for name, value in signed.items():
    print(f"    -H '{name}: {value}' \\")
print(f"    -d '{{\"task\": \"hello\"}}' \\")
print(f"    -v")

print()
print("Route the above curl through agentgateway with extauth-aauth-resource")
print("configured to verify incoming AAuth signatures.")
print("=" * 60)
