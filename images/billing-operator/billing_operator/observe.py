"""What the meter sees of a Sandbox (metering.md, "What is observed")."""
from __future__ import annotations

import hashlib

from .meter import ts

LABEL_OWNER = "browserjs.dev/owner"   # sessions.OwnerLabel(owner)
ANN_OWNER = "browserjs.dev/owner-id"  # the owner's email address
ANN_CREATED = "browserjs.dev/created"  # when a warm-pool Sandbox became this owner's session
ANN_SIZE = "browserjs.dev/size"  # the size its pod has (sessions.AnnSize); none is small
SMALL = "small"


def owner_label(owner: str) -> str:
    """sessions.OwnerLabel in the backend: Account.spec.ownerHash."""
    return hashlib.sha256(owner.encode("utf-8")).hexdigest()[:32]


def _ready(sandbox: dict) -> dict:
    for condition in (sandbox.get("status") or {}).get("conditions") or []:
        if isinstance(condition, dict) and condition.get("type") == "Ready":
            return condition
    return {}


def awake(sandbox: dict) -> bool:
    """sessions.FromSandbox(...).State == Running, the same rule: not being
    deleted, not Suspended, and Ready."""
    if (sandbox.get("metadata") or {}).get("deletionTimestamp"):
        return False
    if (sandbox.get("spec") or {}).get("operatingMode") == "Suspended":
        return False
    return _ready(sandbox).get("status") == "True"


def ready_since(sandbox: dict) -> str | None:
    """Since when the session has been awake and its owner's: Ready's
    lastTransitionTime, or the time it was taken from the warm pool if that
    is later. A pod's time in the pool is nobody's."""
    ready = _ready(sandbox).get("lastTransitionTime")
    adopted = ((sandbox.get("metadata") or {}).get("annotations") or {}).get(ANN_CREATED)
    if not ready or not adopted:
        return ready
    try:
        return adopted if ts(adopted) > ts(ready) else ready
    except (ValueError, TypeError):
        return ready


def observe(sandboxes: list[dict], disk_gb: int, sizes: frozenset[str] = frozenset()) -> dict[str, dict[str, dict]]:
    """{owner hash: {session ID: {"awake", "readySince", "diskGB", "size"}}}
    for every Sandbox that has an owner and is not being deleted. A
    warm-pool Sandbox before adoption has no owner and is not in it.

    `sizes` is the sizes the catalogue has a rate for. A session of any
    other size, like one with none, is "small": charged at the base rate,
    which is the lowest.
    """
    out: dict[str, dict[str, dict]] = {}
    for sandbox in sandboxes:
        meta = sandbox.get("metadata") or {}
        owner = (meta.get("annotations") or {}).get(ANN_OWNER)
        if not owner or meta.get("deletionTimestamp") or not meta.get("name"):
            continue
        owner_hash = (meta.get("labels") or {}).get(LABEL_OWNER) or owner_label(owner)
        is_awake = awake(sandbox)
        out.setdefault(owner_hash, {})[meta["name"]] = {
            "awake": is_awake,
            "readySince": ready_since(sandbox) if is_awake else None,
            "diskGB": disk_gb,
            "size": size if (size := (meta.get("annotations") or {}).get(ANN_SIZE)) in sizes else SMALL,
        }
    return out
