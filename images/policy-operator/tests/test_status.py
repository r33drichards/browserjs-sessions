from policy_operator.bundle import Tenant
from policy_operator.check import Validation
from policy_operator.operator import Loaded, Outcome
from policy_operator.status import after_loaded_check, after_reconcile

T0, T1, T2 = "2026-10-02T00:00:00Z", "2026-10-02T00:01:00Z", "2026-10-02T00:02:00Z"
OK = Validation(ok=True, rego="R1", hash="sha256:1", warnings=[{"code": "allow_empty", "message": "w"}])
BAD = Validation(ok=False, errors=[{"row": 3, "col": 1, "code": "rego_parse_error", "message": "unexpected eof token"},
                                   {"code": "policy_guard_error", "message": "x"}])


def conditions(status):
    return {c["type"]: c for c in status["conditions"]}


def test_compiled_and_loaded_is_ready():
    s = after_reconcile({}, 4, Outcome(OK, Tenant("M", "sha256:1"), [], "17-2"), Loaded(2, 2), T0)
    assert s["observedGeneration"] == 4 and s["regoGeneration"] == 4
    assert s["rego"] == "R1" and s["hash"] == "sha256:1" and s["errors"] == []
    assert s["warnings"] == [{"code": "allow_empty", "message": "w"}]
    assert s["loaded"] == {"replicas": 2, "total": 2, "revision": "17-2"} and s["lastAppliedTime"] == T0
    c = conditions(s)
    assert [c[t]["status"] for t in ("Compiled", "Loaded", "Ready")] == ["True", "True", "True"]
    assert c["Loaded"]["message"] == "2/2 replicas"
    for cond in s["conditions"]:
        assert cond["observedGeneration"] == 4 and cond["lastTransitionTime"] == T0 and cond["reason"]
    # The CRD: the three types, each once.
    assert [cond["type"] for cond in s["conditions"]] == ["Compiled", "Loaded", "Ready"]


def test_compiled_but_not_loaded_yet():
    s = after_reconcile({}, 1, Outcome(OK, Tenant("M", "sha256:1"), [], "17-2"), Loaded(1, 2), T0)
    c = conditions(s)
    assert [c[t]["status"] for t in ("Compiled", "Loaded", "Ready")] == ["True", "False", "False"]
    assert c["Loaded"]["reason"] == "Pending" and c["Loaded"]["message"] == "1/2 replicas"
    assert "lastAppliedTime" not in s
    s = after_reconcile({}, 1, Outcome(OK, Tenant("M", "sha256:1"), [], "17-2"), Loaded(0, 0), T0)
    assert conditions(s)["Loaded"]["reason"] == "NoReplicas"


def test_a_spec_that_does_not_compile_leaves_rego_and_hash_alone():
    old = after_reconcile({}, 1, Outcome(OK, Tenant("M", "sha256:1"), [], "17-2"), Loaded(2, 2), T0)
    s = after_reconcile(old, 2, Outcome(BAD, Tenant("M", "sha256:1"), [], "17-2"), Loaded(2, 2), T1)
    assert "rego" not in s and "hash" not in s and "regoGeneration" not in s
    assert s["observedGeneration"] == 2 and s["errors"] == BAD.errors and s["warnings"] == []
    c = conditions(s)
    # The last good policy is still served by every replica; the spec is not in force.
    assert [c[t]["status"] for t in ("Compiled", "Loaded", "Ready")] == ["False", "True", "False"]
    assert c["Compiled"]["reason"] == "CompileError"
    assert c["Compiled"]["message"] == "unexpected eof token (and 1 more)"
    assert c["Ready"]["reason"] == "CompileError" and c["Ready"]["message"] == "the previous policy stays in force"
    assert c["Compiled"]["lastTransitionTime"] == T1 and c["Loaded"]["lastTransitionTime"] == T0
    assert "lastAppliedTime" not in s  # Loaded did not become True: it was
    assert all(cond["observedGeneration"] == 2 for cond in s["conditions"])


def test_never_compiled_means_nothing_in_force():
    s = after_reconcile({}, 1, Outcome(BAD, None, [], None), Loaded(0, 2), T0)
    c = conditions(s)
    assert [c[t]["status"] for t in ("Compiled", "Loaded", "Ready")] == ["False", "False", "False"]
    assert c["Loaded"]["reason"] == "NoPolicy" and s["loaded"] == {"replicas": 0, "total": 2}


def test_errors_and_warnings_are_capped_at_the_crds_fifty():
    many = Validation(ok=False, errors=[{"code": "c", "message": str(i)} for i in range(80)])
    assert len(after_reconcile({}, 1, Outcome(many, None), Loaded(), T0)["errors"]) == 50
    noisy = Validation(ok=True, rego="R", hash="sha256:1", warnings=[{"code": "c", "message": str(i)} for i in range(80)])
    assert len(after_reconcile({}, 1, Outcome(noisy, Tenant("M", "sha256:1")), Loaded(), T0)["warnings"]) == 50


def test_bundle_build_failure_is_reported_on_the_resource():
    out = Outcome(OK, None, [{"code": "rego_compile_error", "message": "boom"}], None)
    s = after_reconcile({}, 1, out, Loaded(0, 2), T0)
    assert "rego" not in s and s["errors"] == [{"code": "rego_compile_error", "message": "the bundle does not build: boom"}]
    assert conditions(s)["Compiled"]["reason"] == "BundleBuildFailed"


def test_transition_times_and_last_applied_move_only_on_a_change():
    first = after_reconcile({}, 1, Outcome(OK, Tenant("M", "sha256:1"), [], "17-2"), Loaded(2, 2), T0)
    again = after_reconcile(first, 1, Outcome(OK, Tenant("M", "sha256:1"), [], "17-2"), Loaded(2, 2), T1)
    assert all(c["lastTransitionTime"] == T0 for c in again["conditions"]) and "lastAppliedTime" not in again
    # A new policy, loaded: Loaded stays True, but it was applied anew.
    new = Validation(ok=True, rego="R2", hash="sha256:2")
    changed = after_reconcile(first, 2, Outcome(new, Tenant("M2", "sha256:2"), [], "17-3"), Loaded(2, 2), T2)
    assert changed["lastAppliedTime"] == T2 and changed["loaded"]["revision"] == "17-3"


def test_revision_is_kept_across_an_operator_restart():
    first = after_reconcile({}, 1, Outcome(OK, Tenant("M", "sha256:1"), [], "17-2"), Loaded(2, 2), T0)
    # A new process publishes the same hash under a revision of its own.
    resumed = after_reconcile(first, 1, Outcome(OK, Tenant("M", "sha256:1"), [], "99-1"), Loaded(2, 2), T1)
    assert resumed["loaded"]["revision"] == "17-2"


def test_timer_no_change_no_patch():
    s = after_reconcile({}, 1, Outcome(OK, Tenant("M", "sha256:1"), [], "17-2"), Loaded(2, 2), T0)
    assert after_loaded_check(s, 1, Loaded(2, 2), when=T1) == {}


def test_timer_follows_the_replicas():
    s = after_reconcile({}, 1, Outcome(OK, Tenant("M", "sha256:1"), [], "17-2"), Loaded(1, 2), T0)
    change = after_loaded_check(s, 1, Loaded(2, 2), when=T1)
    c = conditions(change)
    assert [c[t]["status"] for t in ("Compiled", "Loaded", "Ready")] == ["True", "True", "True"]
    assert c["Compiled"]["lastTransitionTime"] == T0 and c["Loaded"]["lastTransitionTime"] == T1
    assert change["lastAppliedTime"] == T1 and change["loaded"] == {"replicas": 2, "total": 2, "revision": "17-2"}
    assert set(change) == {"conditions", "loaded", "lastAppliedTime"}  # never rego, hash or errors
    s.update(change)
    # A third replica starts and has not loaded the bundle yet.
    change = after_loaded_check(s, 1, Loaded(2, 3), when=T2)
    c = conditions(change)
    assert c["Loaded"]["status"] == "False" and c["Ready"]["status"] == "False" and c["Loaded"]["message"] == "2/3 replicas"
    assert "lastAppliedTime" not in change


def test_timer_on_a_spec_that_does_not_compile():
    good = after_reconcile({}, 1, Outcome(OK, Tenant("M", "sha256:1"), [], "17-2"), Loaded(2, 2), T0)
    bad = dict(good, **after_reconcile(good, 2, Outcome(BAD, Tenant("M", "sha256:1"), [], "17-2"), Loaded(1, 2), T1))
    change = after_loaded_check(bad, 2, Loaded(2, 2), when=T2)
    c = conditions(change)
    assert [c[t]["status"] for t in ("Compiled", "Loaded", "Ready")] == ["False", "True", "False"]
    assert c["Ready"]["reason"] == "CompileError"
