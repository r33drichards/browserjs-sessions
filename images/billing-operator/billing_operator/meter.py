"""The seconds of metering.md ("What is observed", "Seconds"): which time
of a session is counted, a pure function.

The rule is the gap rule: a session is charged the time between two looks
only when they were at most MAX_GAP apart, and time that was not observed
is never charged. Nothing here is money: Metronome turns seconds into
money (docs/contracts/billing/metronome.md).

The answers are those of docs/contracts/billing/spike/meter_ref.py for
`awakeSeconds` and `diskGBSeconds`, case for case (tests/test_vectors.py).
"""
from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timezone

MAX_GAP = 150  # seconds; a longer gap between two observations is not counted


@dataclass
class Seconds:
    """What one tick counted, by session."""
    awake: dict[str, int] = field(default_factory=dict)
    disk_gb: dict[str, int] = field(default_factory=dict)  # GB-seconds

    def as_expect(self) -> dict:
        """The two keys of an `expect` entry of metering-vectors.json that are seconds."""
        return {"awakeSeconds": self.awake, "diskGBSeconds": self.disk_gb}


def ts(value: str) -> int:
    """An RFC 3339 time as whole seconds since the epoch (rounded down)."""
    t = datetime.fromisoformat(value.replace("Z", "+00:00") if value.endswith(("Z", "z")) else value)
    if t.tzinfo is None:
        t = t.replace(tzinfo=timezone.utc)
    return int(t.timestamp() // 1)


def iso(seconds: int) -> str:
    return datetime.fromtimestamp(seconds, timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _ts_or_none(value) -> int | None:
    if not isinstance(value, str) or not value:
        return None
    try:
        return ts(value)
    except ValueError:
        return None


def seconds(sessions: dict[str, dict], observed: dict[str, dict], now: str,
            max_gap: int = MAX_GAP) -> tuple[dict[str, dict], Seconds]:
    """One tick.

    `sessions` is what the last tick left: {id: {"lastSeen", "awake"}}. It
    is not changed: the sessions after this tick are returned. `observed`
    maps the ID of every session that exists and is not being deleted to
    {"awake": bool, "readySince": time or None, "diskGB": int}. `now` is the
    tick's time, RFC 3339.
    """
    t = ts(now)
    counted = Seconds()
    after: dict[str, dict] = {}
    for sid in sorted(observed):
        o = observed[sid]
        prev = sessions.get(sid)
        last = _ts_or_none(prev.get("lastSeen")) if prev else None
        gap = t - last if last is not None else None
        seen = gap is not None and 0 < gap <= max_gap
        if seen:
            counted.disk_gb[sid] = gap * max(0, int(o.get("diskGB") or 0))  # the disk existed at both sights
        awake = bool(o.get("awake"))
        if awake:
            if seen and prev.get("awake"):
                a = gap
            else:
                # First sight of this run, or the observer was away too long,
                # or the clock went backwards: count only from when the pod
                # became Ready, and only if that was recent enough to have
                # been seen.
                ready = _ts_or_none(o.get("readySince"))
                since = t - ready if ready is not None else -1
                a = since if 0 <= since <= max_gap else 0
                if gap is not None:
                    # It was looked at before and was not awake then (or the
                    # clock has not moved on since): it cannot have been awake
                    # for longer than the time since that look. Without this
                    # a tick repeated with the same `now` would count the
                    # time since Ready twice.
                    a = min(a, max(gap, 0))
            if a:
                counted.awake[sid] = a
        after[sid] = {"lastSeen": now, "awake": awake}
    # A session that is not observed is dropped: the time since its lastSeen
    # is never counted.
    return after, counted
