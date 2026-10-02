"""Usage events, as Metronome takes them (metronome.md, "Usage events").

  session.awake   one per awake session per tick that counted more than
                  zero seconds; transaction_id awake/<session>/<tick>
  session.kept    a session's GB-seconds added up over an hour and sent
                  once, at the hour (or at the session's last sight);
                  transaction_id kept/<session>/<hour start>

The transaction_id is the idempotency key: the same tick, or the same
hour, always has the same one, and Metronome ignores a repeat.
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


def awake_event(session: str, customer: str, now: int, secs: int) -> Event:
    return Event(f"awake/{session}/{now}", customer, AWAKE, iso(now), {"session_id": session, "seconds": str(secs)})


def kept_event(session: str, customer: str, hour_start: int, last: int, gb_seconds: int) -> Event:
    return Event(f"kept/{session}/{hour_start}", customer, KEPT, iso(last),
                 {"session_id": session, "gb_seconds": str(gb_seconds)})


def hour_of(now: int) -> int:
    """The start of the hour a tick's seconds belong to: the hour that ends
    at or after the tick, so that the tick at 11:00:00 completes 10:00."""
    return (now - 1) // HOUR * HOUR


@dataclass
class _Hour:
    customer: str
    start: int
    gb_seconds: int = 0
    last: int = 0  # the last tick that added to it


class HourlyDisk:
    """The hour's GB-seconds of each session, in memory. Losing it (a
    restart) loses that hour's disk, which is then free."""

    def __init__(self) -> None:
        self._hours: dict[str, _Hour] = {}

    def __len__(self) -> int:
        return len(self._hours)

    def tick(self, now: int, counted: dict[str, int], customers: dict[str, str]) -> list[Event]:
        """Add this tick's GB-seconds (by session; `customers` names each
        observed session's customer) and return the events that are due: an
        hour that is over, and a session that is no longer there."""
        due: list[Event] = []

        def close(session: str) -> None:
            h = self._hours.pop(session)
            if h.gb_seconds > 0:
                due.append(kept_event(session, h.customer, h.start, h.last, h.gb_seconds))

        for session in sorted(counted):
            start = hour_of(now)
            h = self._hours.get(session)
            if h is not None and h.start != start:
                close(session)
                h = None
            if h is None:
                h = self._hours[session] = _Hour(customers[session], start)
            h.gb_seconds += counted[session]
            h.last = now
        for session in sorted(self._hours):
            h = self._hours[session]
            # Over (this tick is at or past the hour's end), or the session
            # was not observed: sent at its last sight.
            if now >= h.start + HOUR or session not in customers:
                close(session)
        return due
