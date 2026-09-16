"""Stable shared types for imported and `python -m` SBX service entrypoints."""
from dataclasses import dataclass


@dataclass(frozen=True)
class NetworkProof:
    """Trusted in-process verifier result; never decoded from worker messages."""
    binding: str
    phase: str
    policy_digest: str
    expires_at: float
    evidence_id: str
    gateway_port: int
