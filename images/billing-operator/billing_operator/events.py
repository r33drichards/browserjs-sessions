"""Usage events, as Metronome takes them (metronome.md, "Usage events").

The observer looks every tick; what it sends is added up over a window,
because Metronome charges by the event.

  session.awake   a session's awake seconds over a window (AWAKE_WINDOW, 5
                  minutes, aligned to the clock), sent at the window's end,
                  or at once when the session stops being awake;
                  transaction_id awake/<session>/<window start>
  session.kept    a session's GB-seconds over a window (KEPT_WINDOW, 6
                  hours, aligned to UTC), sent at the window's end, or at
                  the session's last sight;
                  transaction_id kept/<session>/<window start>

A session of a size other than small (the catalogue's `sizes`) is charged
at that size's rate: its awake seconds go out as session.awake.<size>, an
event type with a metric and a rate of its own, and carry the size as a
property. The key is the same: a session has one size while it is awake.

The transaction_id is the idempotency key: the same window always has the
same one, and Metronome ignores a repeat.

A window that was sent early and then goes on (the session woke again
inside the same five minutes) is sent in parts: the later part has the
time of its first tick added to the key, awake/<session>/<window
start>/<first tick>, so that it is neither ignored (free) nor counted twice.
"""
from __future__ import annotations

from dataclasses import dataclass, field

from .meter import iso

AWAKE = "session.awake"
SMALL = "small"  # the size whose awake seconds are session.awake itself
KEPT = "session.kept"
HOUR = 3600


@dataclass(frozen=True)
class Event:
    transaction_id: str
    customer_id: str   # the Account's name, an ingest alias of its Metronome customer
    event_type: str
    timestamp: str
    properties: dict = field(compare=True, default_factory=dict)

    def body(self) -> dict:
        return {"transaction_id": self.transaction_id, "customer_id": self.customer_id,
                "event_type": self.event_type, "timestamp": self.timestamp, "properties": dict(self.properties)}


def _key(kind: str, session: str, window_start: int, part: int | None) -> str:
    return f"{kind}/{session}/{window_start}" + (f"/{part}" if part is not None else "")


def awake_type(size: str | None) -> str:
    """The event type of the awake seconds of a session of `size`."""
    return AWAKE if not size or size == SMALL else f"{AWAKE}.{size}"


def awake_event(session: str, customer: str, window_start: int, at: int, secs: int, part: int | None = None,
                size: str | None = None) -> Event:
    properties = {"session_id": session, "seconds": str(secs)}
    if size and size != SMALL:
        properties["size"] = size
    return Event(_key("awake", session, window_start, part), customer, awake_type(size), iso(at), properties)


def kept_event(session: str, customer: str, window_start: int, at: int, gb_seconds: int, part: int | None = None) -> Event:
    return Event(_key("kept", session, window_start, part), customer, KEPT, iso(at),
                 {"session_id": session, "gb_seconds": str(gb_seconds)})


def window_of(now: int, size: int) -> int:
    """The start of the window a tick's seconds belong to: the window that
    ends at or after the tick, so that the tick at 10:05:00 completes the
    window that began at 10:00:00."""
    return (now - 1) // size * size


@dataclass
class _Sum:
    customer: str
    start: int
    total: int = 0
    last: int = 0  # the last tick that added to it
    part: int | None = None  # its first tick, when an earlier part of the same window was sent
    size: str | None = None  # of an awake window: the session's size


class Windows:
    """Each session's sum over the open window, in memory. Losing it (a
    restart) loses the open windows, which are then free."""

    def __init__(self, size: int, make) -> None:
        self.size = size
        self._make = make  # awake_event or kept_event
        self._open: dict[str, _Sum] = {}
        self._early: dict[str, int] = {}  # session -> the window of which a part was sent early

    def __len__(self) -> int:
        return len(self._open)

    def tick(self, now: int, counted: dict[str, int], customers: dict[str, str], going: set[str],
             sizes: dict[str, str] | None = None) -> list[Event]:
        """Add this tick's amounts (by session; `customers` names the
        customer of each) and return the events that are due: a window that
        is over, and a session that is not in `going` (it is no longer
        awake, or no longer there), which is sent at once. `sizes`, for
        awake windows, is each session's size: a session met at another
        size than its open window's (it stopped and started between two
        ticks) has that window sent, and a new part begun."""
        due: list[Event] = []
        sizes = sizes or {}

        def close(session: str) -> None:
            w = self._open.pop(session)
            if w.total > 0:
                # At the window's end when it ran to its end; otherwise at its last sight.
                over = now >= w.start + self.size
                at = w.start + self.size if over else w.last
                if w.size is None:
                    due.append(self._make(session, w.customer, w.start, at, w.total, w.part))
                else:
                    due.append(self._make(session, w.customer, w.start, at, w.total, w.part, w.size))
                if not over:
                    self._early[session] = w.start

        for session in sorted(counted):
            start = window_of(now, self.size)
            w = self._open.get(session)
            if w is not None and (w.start != start or w.size != sizes.get(session)):
                close(session)
                w = None
            if w is None:
                # The window's key is taken if a part of it was sent early:
                # this part is keyed by its first tick as well.
                w = self._open[session] = _Sum(customers[session], start,
                                               part=now if self._early.get(session) == start else None,
                                               size=sizes.get(session))
            w.total += counted[session]
            w.last = now
        for session in sorted(self._open):
            if now >= self._open[session].start + self.size or session not in going:
                close(session)
        current = window_of(now, self.size)
        self._early = {s: start for s, start in self._early.items() if start == current}
        return due
