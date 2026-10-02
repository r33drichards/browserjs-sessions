"""The kopf handlers. `kopf run -m billing_operator` imports them.

The observer reconciles no resource: it samples. kopf gives it the process
(start, stop, signals) and the liveness endpoint; the work is the loop of
passes.py, started here.
"""
from __future__ import annotations

import asyncio
import logging
import os
import signal
import time

import kopf

from .catalogue import CatalogueFile
from .config import Config, ConfigError
from .kube import Client
from .passes import Observer
from .sender import Metronome

log = logging.getLogger("billing_operator")

# A loop that has not come round for this many ticks is stuck (every call to
# the API server has a time limit, so it should not be): the process is
# stopped, and the Deployment starts another.
STALLED_AFTER_TICKS = 5

# Set at startup; tests set them directly.
METER: Observer | None = None
_tasks: list[asyncio.Task] = []


def make_kube(cfg: Config):
    """In a pod, the ServiceAccount's client. Tests replace this."""
    return Client.in_cluster(cfg.namespace)


def make_sink(cfg: Config):
    """Metronome's ingest endpoint. Tests replace this."""
    return Metronome(cfg.metronome_url, cfg.metronome_token)


def stop_process() -> None:
    os.kill(os.getpid(), signal.SIGTERM)


def stalled(meter: Observer, tick: float, now: float | None = None) -> bool:
    return (time.monotonic() if now is None else now) - meter.beat > max(STALLED_AFTER_TICKS * tick, 300.0)


async def watchdog(meter: Observer, tick: float, loop_task: asyncio.Task, stop=stop_process) -> None:
    while True:
        await asyncio.sleep(tick)
        if loop_task.done() or stalled(meter, tick):
            log.critical("the observer has stopped (last pass: %s); stopping so that it is started again",
                         meter.last.now if meter.last else "none")
            stop()
            return


@kopf.on.startup()
async def startup(settings: kopf.OperatorSettings, **_):
    global METER
    try:
        cfg = Config.from_env()
    except ConfigError as e:
        raise kopf.PermanentError(str(e)) from e
    settings.posting.level = logging.WARNING
    # kopf would otherwise list and watch the cluster's namespaces, which the
    # operator's Role does not allow (deploy.md).
    settings.scanning.disabled = True
    if not cfg.metering:
        # The switch is off: deployed or not, the operator reads and writes nothing.
        log.info("BILLING is %s: the operator is idle", cfg.billing)
        return
    catalogue = CatalogueFile(cfg.catalogue)
    if catalogue.current() is None:
        raise kopf.PermanentError(f"no catalogue that parses at {cfg.catalogue} (BILLING_CATALOGUE)")
    METER = Observer(make_kube(cfg), make_sink(cfg), catalogue, max_gap=cfg.max_gap, backoff=cfg.tick,
                     awake_window=cfg.awake_window, kept_window=cfg.kept_window)
    log.info("BILLING is %s: observing %s every %ds, gaps of up to %ds, awake sent every %ds, disk every %ds, to %s",
             cfg.billing, cfg.namespace, cfg.tick, cfg.max_gap, cfg.awake_window, cfg.kept_window, cfg.metronome_url)
    loop_task = asyncio.create_task(METER.run(cfg.tick), name="billing-meter")
    _tasks[:] = [loop_task, asyncio.create_task(watchdog(METER, cfg.tick, loop_task), name="billing-watchdog")]


@kopf.on.cleanup()
async def cleanup(**_):
    global METER
    for task in _tasks:
        task.cancel()
    await asyncio.gather(*_tasks, return_exceptions=True)
    _tasks.clear()
    if METER is not None:
        await METER.kube.close()
        await METER.sender.sink.close()
        METER = None


@kopf.on.probe(id="meter")
def probe(**_):
    """In the answer of the liveness endpoint: what the last pass did."""
    if METER is None:
        return {"metering": False}
    last = METER.last
    return {"metering": True, "passes": METER.passes, "lastPass": last.now if last else None,
            "pending": METER.sender.pending, "leaseRenewed": bool(last and last.lease),
            "secondsSinceLoop": int(time.monotonic() - METER.beat)}
