"""Properties of the seconds function (the tracks document's that are about
seconds), over generated histories: sessions that come, wake, sleep and go,
and a clock that jumps forwards and backwards.
"""
import importlib.util
import sys

from hypothesis import given, settings, strategies as st

from billing_operator.meter import MAX_GAP, iso, seconds, ts

from conftest import CONTRACTS

BASE = ts("2026-10-02T10:00:00Z")
SESSIONS = ["s-aaaaa", "s-bbbbb", "s-ccccc"]


@st.composite
def observation(draw, now):
    out = {}
    for sid in draw(st.lists(st.sampled_from(SESSIONS), unique=True)):
        awake = draw(st.booleans())
        ready = draw(st.one_of(st.none(), st.integers(-400, 200).map(lambda d: iso(now + d))))
        out[sid] = {"awake": awake, "readySince": ready if awake else None, "diskGB": draw(st.integers(0, 10))}
    return out


@st.composite
def history(draw):
    """[(now, observed)]: mostly a tick apart, sometimes late, away, repeated or backwards."""
    ticks, now = [], BASE
    for _ in range(draw(st.integers(1, 12))):
        now += draw(st.one_of(st.integers(1, MAX_GAP), st.integers(MAX_GAP + 1, 4000), st.just(0), st.integers(-300, -1)))
        ticks.append((now, draw(observation(now))))
    return ticks


@settings(max_examples=300, deadline=None)
@given(history())
def test_every_tick_of_any_history(ticks):
    sessions = {}
    for now, observed in ticks:
        after, counted = seconds(sessions, observed, iso(now))
        # Awake seconds never exceed now - lastSeen, nor MAX_GAP; nothing is negative.
        for sid, secs in counted.awake.items():
            assert observed[sid]["awake"] and 0 < secs <= MAX_GAP
            if sid in sessions:
                assert secs <= now - ts(sessions[sid]["lastSeen"])
        # The disk is counted only between two sights at most MAX_GAP apart.
        for sid, gbs in counted.disk_gb.items():
            gap = now - ts(sessions[sid]["lastSeen"])
            assert 0 < gap <= MAX_GAP and gbs == gap * observed[sid]["diskGB"] >= 0
        # The sessions remembered are exactly those observed.
        assert set(after) == set(observed) and all(s["lastSeen"] == iso(now) for s in after.values())
        sessions = after


@settings(max_examples=300, deadline=None)
@given(history())
def test_a_repeated_tick_with_the_same_now_counts_nothing(ticks):
    sessions = {}
    for now, observed in ticks:
        sessions, _ = seconds(sessions, observed, iso(now))
        again, counted = seconds(sessions, observed, iso(now))
        assert counted.awake == {} and not any(counted.disk_gb.values()) and again == sessions


@settings(max_examples=300, deadline=None)
@given(st.lists(st.integers(1, MAX_GAP), min_size=1, max_size=200), st.integers(0, 10))
def test_an_hour_awake_in_any_pattern_of_ticks_is_exactly_an_hour(gaps, disk_gb):
    pattern, total = [], 0
    for gap in gaps * 3600:
        gap = min(gap, 3600 - total)
        pattern.append(gap)
        total += gap
        if total == 3600:
            break
    now = BASE
    sessions = {"s-aaaaa": {"lastSeen": iso(now), "awake": True}}
    awake = disk = 0
    for gap in pattern:
        now += gap
        sessions, counted = seconds(sessions, {"s-aaaaa": {"awake": True, "readySince": iso(BASE - 86400), "diskGB": disk_gb}}, iso(now))
        awake += sum(counted.awake.values())
        disk += sum(counted.disk_gb.values())
    assert awake == 3600 and disk == 3600 * disk_gb


@settings(max_examples=200, deadline=None)
@given(st.integers(MAX_GAP + 1, 10**7), st.booleans(), st.integers(-10**6, 10**6))
def test_a_gap_over_the_limit_counts_at_most_the_limit(gap, was_awake, ready_offset):
    now = BASE + gap
    sessions = {"s-aaaaa": {"lastSeen": iso(BASE), "awake": was_awake}}
    observed = {"s-aaaaa": {"awake": True, "readySince": iso(now + ready_offset), "diskGB": 5}}
    _, counted = seconds(sessions, observed, iso(now))
    assert counted.disk_gb == {} and sum(counted.awake.values()) <= MAX_GAP


# --- against the contract's reference implementation --------------------------

def reference():
    spec = importlib.util.spec_from_file_location("meter_ref", CONTRACTS / "spike" / "meter_ref.py")
    module = importlib.util.module_from_spec(spec)
    written, sys.dont_write_bytecode = sys.dont_write_bytecode, True  # nothing is left in the contract's directory
    try:
        spec.loader.exec_module(module)
    finally:
        sys.dont_write_bytecode = written
    return module


def departs(sessions, observed, now):
    """Where the observer counts less than spike/meter_ref.py: a session
    seen before, not counted the gap, whose time since Ready is longer than
    the time since it was last seen (docs/billing-operator.md, "Where the
    seconds are stricter than the reference")."""
    for sid, o in observed.items():
        prev = sessions.get(sid)
        if not prev or not o["awake"] or not o.get("readySince"):
            continue
        gap, since = now - ts(prev["lastSeen"]), now - ts(o["readySince"])
        if not (0 < gap <= MAX_GAP and prev["awake"]) and 0 <= since <= MAX_GAP and since > max(gap, 0):
            return True
    return False


@settings(max_examples=300, deadline=None)
@given(history())
def test_the_seconds_are_the_references(ticks):
    ref = reference()
    assert ref.MAX_GAP == MAX_GAP
    sessions, state = {}, {}
    for now, observed in ticks:
        strict = departs(sessions, observed, now)
        after, mine = seconds(sessions, observed, iso(now))
        theirs = ref.step(state, [], observed, iso(now))
        if strict:
            assert sum(mine.awake.values()) < sum(theirs["awakeSeconds"].values())  # only ever in the user's favour
            assert mine.disk_gb == theirs["diskGBSeconds"]
        else:
            assert mine.as_expect() == {k: theirs[k] for k in ("awakeSeconds", "diskGBSeconds")}
        assert after == state["sessions"]
        sessions = after
