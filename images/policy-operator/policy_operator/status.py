"""status of a SessionPolicy, exactly as deploy/base/crd-sessionpolicy.yaml
describes it. Pure functions: what goes in is what was found, what comes
out is the patch.
"""
from __future__ import annotations

from datetime import datetime, timezone

from .config import MAX_DIAGNOSTICS
from .operator import Loaded, Outcome


def now() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _condition(old: list, type_: str, status: bool | None, reason: str, message: str,
               generation: int, when: str) -> dict:
    value = "Unknown" if status is None else str(bool(status))
    previous = next((c for c in old or [] if isinstance(c, dict) and c.get("type") == type_), None)
    since = previous.get("lastTransitionTime") if previous and previous.get("status") == value else None
    return {"type": type_, "status": value, "reason": reason, "message": message,
            "observedGeneration": generation, "lastTransitionTime": since or when}


def _is(old: dict, type_: str) -> bool:
    return any(c.get("type") == type_ and c.get("status") == "True" for c in old.get("conditions") or [] if isinstance(c, dict))


def _loaded_part(old: dict, compiled: bool, compiled_reason: str, policy_hash: str | None, loaded: Loaded,
                 revision: str | None, generation: int, when: str) -> dict:
    """Loaded, Ready, loaded and lastAppliedTime: shared by the reconcile and the timer."""
    conditions = old.get("conditions") or []
    if not policy_hash:
        is_loaded, reason, message = False, "NoPolicy", "no policy in force: the session is denied"
    elif loaded.total == 0:
        is_loaded, reason, message = False, "NoReplicas", "no OPA replica is ready"
    elif loaded.all:
        is_loaded, reason, message = True, "AllReplicas", f"{loaded.replicas}/{loaded.total} replicas"
    else:
        is_loaded, reason, message = False, "Pending", f"{loaded.replicas}/{loaded.total} replicas"
    ready = compiled and is_loaded
    if ready:
        ready_reason, ready_message = "Ready", "the policy is in force on every OPA replica"
    elif not compiled:
        ready_reason, ready_message = compiled_reason, (
            "the previous policy stays in force" if policy_hash else "no policy in force: the session is denied")
    else:
        ready_reason, ready_message = reason, message
    out: dict = {
        "loaded": {"replicas": loaded.replicas, "total": loaded.total},
    }
    old_loaded = old.get("loaded") or {}
    same_hash = old.get("hash") == policy_hash
    # The first revision to carry the hash: what status already names, when
    # the hash is the same, came before anything this process published.
    rev = (old_loaded.get("revision") if same_hash else None) or revision
    if rev:
        out["loaded"]["revision"] = rev
    if is_loaded and not (same_hash and _is(old, "Loaded") and old.get("lastAppliedTime")):
        out["lastAppliedTime"] = when
    out["_loaded"] = _condition(conditions, "Loaded", is_loaded, reason, message, generation, when)
    out["_ready"] = _condition(conditions, "Ready", ready, ready_reason, ready_message, generation, when)
    return out


def after_reconcile(old: dict, generation: int, outcome: Outcome, loaded: Loaded, when: str | None = None) -> dict:
    """The status to patch after a spec was checked."""
    when = when or now()
    old = old or {}
    v = outcome.validation
    compiled = v.ok and not outcome.build_errors
    out: dict = {"observedGeneration": generation}
    if compiled:
        out.update(rego=v.rego, hash=v.hash, regoGeneration=generation, errors=[],
                   warnings=v.warnings[:MAX_DIAGNOSTICS])
        reason, message = "Compiled", "the policy compiles"
    else:
        if v.ok:
            errors = [{"code": e.get("code", "rego_compile_error"), "message": "the bundle does not build: " + str(e.get("message"))}
                      for e in outcome.build_errors]
            reason = "BundleBuildFailed"
        else:
            errors, reason = v.errors, "CompileError"
        out.update(errors=errors[:MAX_DIAGNOSTICS], warnings=[])
        message = errors[0]["message"] if errors else "the policy does not compile"
        if len(errors) > 1:
            message += f" (and {len(errors) - 1} more)"
    policy_hash = outcome.tenant.hash if outcome.tenant else None
    part = _loaded_part(old, compiled, reason, policy_hash, loaded, outcome.revision, generation, when)
    conditions = old.get("conditions") or []
    out["conditions"] = [
        _condition(conditions, "Compiled", compiled, reason, message, generation, when),
        part.pop("_loaded"), part.pop("_ready"),
    ]
    out.update(part)
    return out


def after_loaded_check(old: dict, generation: int, loaded: Loaded, revision: str | None = None,
                       when: str | None = None) -> dict:
    """The status to patch when only what the replicas serve was looked at:
    empty when nothing would change. `old` must describe `generation`.
    """
    when = when or now()
    old = old or {}
    compiled_condition = next((c for c in old.get("conditions") or [] if c.get("type") == "Compiled"), {})
    compiled = compiled_condition.get("status") == "True"
    part = _loaded_part(old, compiled, compiled_condition.get("reason") or "CompileError", old.get("hash"),
                        loaded, revision, generation, when)
    conditions = [compiled_condition or _condition([], "Compiled", None, "Unknown", "", generation, when),
                  part.pop("_loaded"), part.pop("_ready")]
    out = dict(part, conditions=conditions)
    unchanged = (
        out["conditions"] == [c for c in old.get("conditions") or []]
        and out["loaded"] == (old.get("loaded") or {})
        and "lastAppliedTime" not in out
    )
    return {} if unchanged else out
