"""The pass (metering.md, "The tick", steps 1 and 2; metronome.md, "Usage
events"): one look at every Sandbox, the seconds of each session, the
events sent to Metronome, the Lease.

    1. read the time once
    2. list every Sandbox once
    3. the seconds function, over the observer's memory of the last tick
    4. one session.awake event per awake session that counted seconds;
       the GB-seconds added to the hour's sum, sent when the hour is over
    5. deliver; after a tick that was delivered, renew the Lease

The observer reads no Account and writes nothing but the Lease. Its memory
(each session's last sight, the hour's GB-seconds, events waiting to be
delivered) is not a ledger: losing it loses charges and nothing else.
"""
from __future__ import annotations

import asyncio
import logging
import socket
import time
from dataclasses import dataclass

from .catalogue import CatalogueFile
from .events import HourlyDisk, awake_event
from .meter import MAX_GAP, iso, seconds
from .observe import observe
from .sender import Sender

log = logging.getLogger("billing_operator")


class NoCatalogue(RuntimeError):
    """There is no catalogue to count disks by: nothing is counted."""


@dataclass
class PassResult:
    now: str
    sessions: int = 0          # Sandboxes with an owner
    awake_seconds: int = 0     # counted this tick
    disk_gb_seconds: int = 0   # counted this tick (sent with the hour)
    events: int = 0            # made this tick
    sent: int = 0              # delivered this tick, earlier ticks' included
    pending: int = 0           # waiting to be tried again
    dropped: int = 0           # given up or refused: free time
    lease: bool = False        # renewed
    duration: float = 0.0

    def line(self) -> str:
        return (f"pass now={self.now} sessions={self.sessions} awake_seconds={self.awake_seconds} "
                f"disk_gb_seconds={self.disk_gb_seconds} events={self.events} sent={self.sent} "
                f"pending={self.pending} dropped={self.dropped} lease={'renewed' if self.lease else 'not-renewed'} "
                f"duration_ms={int(self.duration * 1000)}")


class Observer:
    def __init__(self, kube, sink, catalogue: CatalogueFile, *, clock=time.time, max_gap: int = MAX_GAP,
                 backoff: int = 60, holder: str | None = None):
        self.kube = kube
        self.catalogue = catalogue
        self.clock = clock
        self.max_gap = max_gap
        self.sender = Sender(sink, backoff=backoff)
        self.holder = holder or socket.gethostname()
        self.sessions: dict[str, dict] = {}   # {id: {"lastSeen", "awake"}} as of the last tick
        self.disk = HourlyDisk()
        self.last: PassResult | None = None
        self.beat = time.monotonic()  # the loop was last seen alive
        self.passes = 0

    async def run_once(self, now: int | None = None) -> PassResult:
        """One pass. `now` (seconds) is read from the clock when not given."""
        started = time.monotonic()
        t = int(self.clock()) if now is None else int(now)
        result = PassResult(now=iso(t))
        catalogue = self.catalogue.current()
        if catalogue is None:
            raise NoCatalogue(f"no catalogue at {self.catalogue.path}: nothing is counted")

        # If the list fails nothing is remembered of this tick: the next
        # one's gap decides what is counted.
        by_owner = observe(await self.kube.list_sandboxes(), catalogue.session_disk_gb)
        observed, customers = {}, {}
        for owner_hash, sessions in by_owner.items():
            for sid, o in sessions.items():
                observed[sid] = o
                customers[sid] = f"acct-{owner_hash}"  # the Account's name: its customer's ingest alias
        result.sessions = len(observed)

        self.sessions, counted = seconds(self.sessions, observed, result.now, self.max_gap)
        events = [awake_event(sid, customers[sid], t, secs) for sid, secs in sorted(counted.awake.items())]
        events += self.disk.tick(t, counted.disk_gb, customers)
        result.awake_seconds = sum(counted.awake.values())
        result.disk_gb_seconds = sum(counted.disk_gb.values())
        result.events = len(events)

        self.sender.add(t, events)
        flush = await self.sender.flush(t)
        result.sent, result.pending, result.dropped = flush.sent, flush.pending, flush.dropped
        if flush.pending == 0:
            # The tick was delivered: the Lease says usage is reaching Metronome.
            try:
                await self.kube.renew_lease(t, self.holder)
                result.lease = True
            except Exception as e:  # noqa: BLE001 - the next tick renews it
                log.error("Lease not renewed: %s: %s", type(e).__name__, e)

        result.duration = time.monotonic() - started
        self.last = result
        self.passes += 1
        log.info(result.line())
        return result

    async def run(self, tick: float) -> None:
        """A pass every `tick` seconds, never two at once: one that overruns
        is followed at once by the next, not overlapped by it."""
        while True:
            started = time.monotonic()
            self.beat = started
            try:
                await self.run_once()
            except asyncio.CancelledError:
                raise
            except Exception as e:  # noqa: BLE001 - the next tick tries again
                log.error("pass failed, nothing counted: %s: %s", type(e).__name__, e)
            self.beat = time.monotonic()
            await asyncio.sleep(max(0.0, tick - (time.monotonic() - started)))
