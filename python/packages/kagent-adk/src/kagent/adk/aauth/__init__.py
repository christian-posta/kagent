"""kagent.adk.aauth — opt-in AAuth signing for outbound agent requests.

Usage
-----
At agent startup (cli.py) call ``init_signer()`` once.  Every outbound call
site then checks ``get_signer()`` and attaches signing if the signer is not
None.  When AAUTH_ENABLED is false (the default) ``get_signer()`` returns
None and nothing changes.
"""

from ._signer import AAuthSigner

_signer: AAuthSigner | None = None


def init_signer() -> None:
    """Bootstrap the module-level signer from environment variables.

    Reads AAUTH_ENABLED and AAUTH_AGENT_ID.  Sets the global signer to None
    (no-op) when AAUTH_ENABLED != "true" or when the aauth library is absent.
    Call once at agent process startup.
    """
    global _signer
    _signer = AAuthSigner.from_env()


def get_signer() -> AAuthSigner | None:
    """Return the active signer, or None if AAuth is disabled."""
    return _signer


__all__ = ["AAuthSigner", "init_signer", "get_signer"]
