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

The transaction_id is the idempotency key: the same window always has the
same one, and Metronome ignores a repeat.
"""
from __future__ import annotations

from dataclasses import dataclass, field

from .meter import iso

AWAKE = "session.awake"
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


def awake_event(session: str, customer: str, window_start: int, at: int, secs: int) -> Event:
    return Event(f"awake/{session}/{window_start}", customer, AWAKE, iso(at), {"session_id": session, "seconds": str(secs)})


def kept_event(session: str, customer: str, window_start: int, at: int, gb_seconds: int) -> Event:
    return Event(f"kept/{session}/{window_start}", customer, KEPT, iso(at),
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


class Windows:
    """Each session's sum over the open window, in memory. Losing it (a
    restart) loses the open windows, which are then free."""

    def __init__(self, size: int, make) -> None:
        self.size = size
        self._make = make  # awake_event or kept_event
        self._open: dict[str, _Sum] = {}

    def __len__(self) -> int:
        return len(self._open)

    def tick(self, now: int, counted: dict[str, int], customers: dict[str, str], going: set[str]) -> list[Event]:
        """Add this tick's amounts (by session; `customers` names the
        customer of each) and return the events that are due: a window that
        is over, and a session that is not in `going` (it is no longer
        awake, or no longer there), which is sent at once."""
        due: list[Event] = []

        def close(session: str) -> None:
            w = self._open.pop(session)
            if w.total > 0:
                # At the window's end when it ran to its end; otherwise at its last sight.
                over = now >= w.start + self.size
                due.append(self._make(session, w.customer, w.start, w.start + self.size if over else w.last, w.total))

        for session in sorted(counted):
            start = window_of(now, self.size)
            w = self._open.get(session)
            if w is not None and w.start != start:
                close(session)
                w = None
            if w is None:
                w = self._open[session] = _Sum(customers[session], start)
            w.total += counted[session]
            w.last = now
        for session in sorted(self._open):
            if now >= self._open[session].start + self.size or session not in going:
                close(session)
        return due
