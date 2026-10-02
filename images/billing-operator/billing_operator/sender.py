"""Sending usage events (metronome.md, "Usage events", "Delivery").

The observer hands events to a Sender, which delivers them through a sink:
anything with `async ingest(events)`. Metronome is the sink in a pod;
FakeMetronome the one in tests and on the command line.

  - at most 100 events a request
  - a 429, a 5xx or no answer: kept in memory and tried again with backoff,
    the same events with the same keys, for at most an hour; then dropped
  - any other 4xx: logged with the batch's keys and dropped
  - every loss is free time for the user
"""
from __future__ import annotations

import asyncio
import logging
from dataclasses import dataclass

import aiohttp

from .events import Event

log = logging.getLogger("billing_operator")

BATCH = 100
KEEP_SECONDS = 3600
MAX_BACKOFF = 300


class Retry(Exception):
    """The sink did not take the batch now: 429, 5xx, or no answer."""


class Rejected(Exception):
    """The sink refused the batch for good: a 4xx other than 429."""


class Metronome:
    """POST /v1/ingest. The token is sent and never logged or put in an error."""

    def __init__(self, url: str, token: str, timeout: float = 30.0):
        self.url = url.rstrip("/") + "/v1/ingest"
        self._token = token
        self._timeout = aiohttp.ClientTimeout(total=timeout)
        self._http: aiohttp.ClientSession | None = None

    async def close(self) -> None:
        if self._http is not None and not self._http.closed:
            await self._http.close()

    async def ingest(self, events: list[Event]) -> None:
        if self._http is None or self._http.closed:
            self._http = aiohttp.ClientSession(timeout=self._timeout)
        try:
            async with self._http.post(self.url, json=[e.body() for e in events],
                                       headers={"Authorization": f"Bearer {self._token}"}) as r:
                status = r.status
                text = (await r.text())[:300] if status >= 400 else ""
        except (aiohttp.ClientError, asyncio.TimeoutError) as e:
            raise Retry(type(e).__name__) from None
        if status == 429 or status >= 500:
            raise Retry(f"{status}")
        if status >= 400:
            raise Rejected(f"{status}: {text}")


class FakeMetronome:
    """An ingest endpoint in memory: it keeps what it was sent, ignores a
    transaction_id it has seen, and fails when told to."""

    def __init__(self) -> None:
        self.events: dict[str, Event] = {}   # by transaction_id, first one wins
        self.requests: list[list[Event]] = []
        self.fail: Exception | None = None   # raised by the next calls while set

    async def close(self) -> None:
        pass

    async def ingest(self, events: list[Event]) -> None:
        self.requests.append(list(events))
        if self.fail is not None:
            raise self.fail
        for e in events:
            self.events.setdefault(e.transaction_id, e)

    def total(self, event_type: str, key: str) -> int:
        return sum(int(e.properties[key]) for e in self.events.values() if e.event_type == event_type)


@dataclass
class Flush:
    sent: int = 0
    dropped: int = 0   # given up after an hour, or refused
    pending: int = 0


class Sender:
    def __init__(self, sink, *, keep: int = KEEP_SECONDS, backoff: int = 60):
        self.sink = sink
        self.keep = keep
        self.backoff = backoff          # after the first failure; doubled up to MAX_BACKOFF
        self._pending: list[tuple[int, Event]] = []  # (when it was made, the event), oldest first
        self._failures = 0
        self._not_before = 0

    @property
    def pending(self) -> int:
        return len(self._pending)

    def add(self, now: int, events: list[Event]) -> None:
        self._pending += [(now, e) for e in events]

    async def flush(self, now: int) -> Flush:
        """Deliver what is waiting, oldest first, in batches of 100."""
        out = Flush()
        old = [e for made, e in self._pending if now - made > self.keep]
        if old:
            self._pending = [(made, e) for made, e in self._pending if now - made <= self.keep]
            out.dropped += len(old)
            log.error("%d usage events could not be delivered for an hour and are dropped (that time is free): %s",
                      len(old), _keys(old))
        if now < self._not_before:
            out.pending = len(self._pending)
            return out
        while self._pending:
            batch = [e for _, e in self._pending[:BATCH]]
            try:
                await self.sink.ingest(batch)
            except Retry as e:
                self._failures += 1
                delay = min(self.backoff * 2 ** (self._failures - 1), MAX_BACKOFF)
                self._not_before = now + delay
                log.warning("ingest did not take %d events (%s); %d kept, tried again in %ds",
                            len(batch), e, len(self._pending), delay)
                break
            except Rejected as e:
                out.dropped += len(batch)
                log.error("ingest refused %d usage events, dropped (that time is free): %s: %s", len(batch), e, _keys(batch))
            else:
                out.sent += len(batch)
                self._failures, self._not_before = 0, 0
            del self._pending[:len(batch)]
        out.pending = len(self._pending)
        return out


def _keys(events: list[Event]) -> str:
    keys = [e.transaction_id for e in events]
    return ", ".join(keys[:20]) + (f", and {len(keys) - 20} more" if len(keys) > 20 else "")
