"""The observer's pass, with Sandboxes in memory and the fake Metronome."""
import asyncio
import logging

import pytest

from billing_operator.catalogue import CatalogueFile
from billing_operator.events import AWAKE, KEPT
from billing_operator.memory import MemoryKube
from billing_operator.meter import ts
from billing_operator.passes import NoCatalogue, Observer
from billing_operator.sender import FakeMetronome, Rejected, Retry

from conftest import acct, asleep, sandbox

OTHER = "v@example.com"
T0 = ts("2026-10-02T10:00:00Z")


@pytest.fixture
def make(catalogue_file):
    def make(*sandboxes, **kwargs):
        kube, sink = MemoryKube(list(sandboxes)), FakeMetronome()
        return Observer(kube, sink, catalogue_file, holder="test", **kwargs), kube, sink
    return make


async def ticks(observer, *times):
    return [await observer.run_once(t) for t in times]


def sent(sink, kind=None):
    return [(e.transaction_id, e.customer_id, e.properties) for e in sink.events.values() if kind in (None, e.event_type)]


async def test_one_event_per_awake_session_per_tick(make):
    observer, kube, sink = make(sandbox("s-aaaaa", ready_since="2026-10-02T09:59:40Z"), sandbox("s-bbbbb", owner=OTHER))
    results = await ticks(observer, T0, T0 + 60, T0 + 120)
    assert [(r.awake_seconds, r.events, r.sent) for r in results] == [(20, 1, 1), (120, 2, 2), (120, 2, 2)]
    assert sent(sink) == [
        (f"awake/s-aaaaa/{T0}", acct(), {"session_id": "s-aaaaa", "seconds": "20"}),  # since Ready; s-bbbbb was Ready long before
        (f"awake/s-aaaaa/{T0 + 60}", acct(), {"session_id": "s-aaaaa", "seconds": "60"}),
        (f"awake/s-bbbbb/{T0 + 60}", acct(OTHER), {"session_id": "s-bbbbb", "seconds": "60"}),
        (f"awake/s-aaaaa/{T0 + 120}", acct(), {"session_id": "s-aaaaa", "seconds": "60"}),
        (f"awake/s-bbbbb/{T0 + 120}", acct(OTHER), {"session_id": "s-bbbbb", "seconds": "60"}),
    ]
    assert kube.lists == 3  # one list of Sandboxes a pass


async def test_one_event_per_kept_session_per_hour(make):
    observer, kube, sink = make(sandbox("s-aaaaa"), asleep("s-bbbbb"))
    results = await ticks(observer, *(T0 + 60 * i for i in range(121)))
    kept = sent(sink, KEPT)
    assert kept == [
        (f"kept/s-aaaaa/{T0}", acct(), {"session_id": "s-aaaaa", "gb_seconds": "18000"}),
        (f"kept/s-bbbbb/{T0}", acct(), {"session_id": "s-bbbbb", "gb_seconds": "18000"}),
        (f"kept/s-aaaaa/{T0 + 3600}", acct(), {"session_id": "s-aaaaa", "gb_seconds": "18000"}),
        (f"kept/s-bbbbb/{T0 + 3600}", acct(), {"session_id": "s-bbbbb", "gb_seconds": "18000"}),
    ]
    assert results[60].events == 3 and results[59].events == 1   # at the hour, not before
    # A sleeping session sends disk only; the awake one a minute at a time.
    assert sink.total(AWAKE, "seconds") == 7200 and len(sent(sink, AWAKE)) == 120
    assert all(p["session_id"] == "s-aaaaa" for _, _, p in sent(sink, AWAKE))


@pytest.mark.parametrize("warm", [sandbox("s-warm1", owner=None), sandbox("s-warm1", owner=None, mode="Suspended", ready="False")])
async def test_a_warm_pool_sandbox_sends_nothing(make, warm):
    observer, kube, sink = make(warm)
    results = await ticks(observer, *(T0 + 60 * i for i in range(62)))
    assert sink.events == {} and all(r.sessions == 0 and r.events == 0 for r in results)


async def test_a_warm_pod_adopted_just_before_a_tick_counts_only_since_it_was_adopted(make):
    observer, kube, sink = make(sandbox("s-warm1", owner=None, ready_since="2026-10-02T09:58:00Z"))
    await observer.run_once(T0 - 60)
    adopted = sandbox("s-warm1", ready_since="2026-10-02T09:58:00Z")   # Ready for two minutes, in the pool
    adopted["metadata"]["annotations"]["browserjs.dev/created"] = "2026-10-02T09:59:55Z"
    kube.put(adopted)
    await observer.run_once(T0)
    assert sent(sink) == [(f"awake/s-warm1/{T0}", acct(), {"session_id": "s-warm1", "seconds": "5"})]


async def test_a_session_deleted_in_the_hour_sends_its_disk_at_its_last_sight(make):
    observer, kube, sink = make(sandbox("s-aaaaa"))
    await ticks(observer, T0, T0 + 60, T0 + 120)
    kube.put(sandbox("s-aaaaa", deleting=True))
    result = await observer.run_once(T0 + 180)
    assert result.awake_seconds == 0 and observer.sessions == {}   # its last minute is free
    kept = [e for e in sink.events.values() if e.event_type == KEPT]
    assert [(e.transaction_id, e.timestamp, e.properties["gb_seconds"]) for e in kept] == [
        (f"kept/s-aaaaa/{T0}", "2026-10-02T10:02:00Z", "600")]


async def test_the_same_tick_twice_has_the_same_transaction_ids_and_counts_once(make):
    observer, kube, sink = make(sandbox("s-aaaaa", ready_since="2026-10-02T09:59:40Z"))
    await ticks(observer, T0, T0 + 60)
    again = await observer.run_once(T0 + 60)
    assert again.events == 0 and sink.total(AWAKE, "seconds") == 80
    # And a second observer (a restart) looking at the same instant makes the same key.
    second, _, _ = make(sandbox("s-aaaaa", ready_since="2026-10-02T09:59:40Z"))
    second.sender.sink = sink
    await second.run_once(T0)
    assert list(sink.events) == [f"awake/s-aaaaa/{T0}", f"awake/s-aaaaa/{T0 + 60}"] and sink.total(AWAKE, "seconds") == 80


async def test_a_restart_counts_nothing_for_the_time_it_was_down_beyond_the_gap(make):
    observer, kube, sink = make(sandbox("s-aaaaa"), asleep("s-bbbbb"))
    await ticks(observer, *(T0 + 60 * i for i in range(11)))
    assert sink.total(AWAKE, "seconds") == 600
    # Killed; back ten minutes later with an empty memory.
    restarted = Observer(kube, sink, observer.catalogue, holder="test")
    first, second = await ticks(restarted, T0 + 1200, T0 + 1260)
    assert first.awake_seconds == 0 and first.disk_gb_seconds == 0       # Ready long ago, never seen by this process
    assert second.awake_seconds == 60 and sink.total(AWAKE, "seconds") == 660
    # The hour's disk that was in memory is gone, which is free; what it counts from now is sent at the hour.
    await ticks(restarted, *(T0 + 60 * i for i in range(22, 61)))
    assert sink.total(KEPT, "gb_seconds") == 2 * 5 * 60 * 40


async def test_the_observer_away_within_the_gap_counts_it_all(make):
    observer, kube, sink = make(sandbox("s-aaaaa"))
    await ticks(observer, T0, T0 + 60, T0 + 60 + 150)
    assert sink.total(AWAKE, "seconds") == 210
    await observer.run_once(T0 + 60 + 150 + 151)
    assert sink.total(AWAKE, "seconds") == 210


async def test_metronome_away_is_retried_and_the_lease_waits(make, caplog):
    observer, kube, sink = make(sandbox("s-aaaaa"), backoff=60)
    await ticks(observer, T0, T0 + 60)
    assert kube.renewals == [T0, T0 + 60]
    sink.fail = Retry("503")
    with caplog.at_level(logging.WARNING, logger="billing_operator"):
        down = await ticks(observer, T0 + 120, T0 + 180, T0 + 240)
    assert [(r.sent, r.pending, r.lease) for r in down] == [(0, 1, False), (0, 2, False), (0, 3, False)]
    assert kube.renewals == [T0, T0 + 60] and "lease=not-renewed" in down[0].line()
    sink.fail = None
    back = await observer.run_once(T0 + 420)   # past the backoff; 180 s since the last sight is not counted
    assert (back.sent, back.pending, back.lease) == (3, 0, True) and kube.renewals[-1] == T0 + 420
    assert sink.total(AWAKE, "seconds") == 240   # every minute observed, each once


async def test_metronome_away_for_over_an_hour_that_usage_is_free(make):
    observer, kube, sink = make(sandbox("s-aaaaa"), backoff=60)
    await observer.run_once(T0)
    sink.fail = Retry("503")
    results = await ticks(observer, *(T0 + 60 * i for i in range(1, 91)))
    assert sum(r.dropped for r in results) == 29 and results[-1].pending == 62   # 61 minutes awake and the hour's disk
    sink.fail = None
    observer.sender._not_before = 0
    await observer.run_once(T0 + 91 * 60)
    assert sink.total(AWAKE, "seconds") == 61 * 60   # the last hour's minutes; the half hour before is free
    assert sink.total(KEPT, "gb_seconds") == 18000


async def test_a_refused_batch_is_dropped_and_the_tick_counts_as_delivered(make, caplog):
    observer, kube, sink = make(sandbox("s-aaaaa"))
    await observer.run_once(T0)
    sink.fail = Rejected("400: unknown customer")
    with caplog.at_level(logging.ERROR, logger="billing_operator"):
        result = await observer.run_once(T0 + 60)
    assert (result.dropped, result.pending, result.lease) == (1, 0, True)
    assert f"awake/s-aaaaa/{T0 + 60}" in caplog.text


async def test_a_failed_lease_does_not_fail_the_tick(make, caplog):
    observer, kube, sink = make(sandbox("s-aaaaa"))

    async def refuse(now, holder):
        raise RuntimeError("403")

    kube.renew_lease = refuse
    with caplog.at_level(logging.ERROR, logger="billing_operator"):
        results = await ticks(observer, T0, T0 + 60)
    assert results[1].sent == 1 and not results[1].lease and "Lease not renewed" in caplog.text


async def test_a_failed_list_remembers_nothing_and_the_next_gap_decides(make):
    observer, kube, sink = make(sandbox("s-aaaaa"))
    await observer.run_once(T0)
    real = kube.list_sandboxes

    async def away():
        raise RuntimeError("the API server is away")

    kube.list_sandboxes = away
    with pytest.raises(RuntimeError):
        await observer.run_once(T0 + 60)
    kube.list_sandboxes = real
    assert (await observer.run_once(T0 + 120)).awake_seconds == 120


async def test_the_disk_size_is_the_catalogues_and_follows_it(make, catalogue_path, caplog):
    observer, kube, sink = make(asleep("s-aaaaa"))
    await ticks(observer, T0, T0 + 60)
    catalogue_path.write_text(catalogue_path.read_text().replace("sessionDiskGB: 5", "sessionDiskGB: 8"))
    assert (await observer.run_once(T0 + 120)).disk_gb_seconds == 480
    catalogue_path.write_text("sessionDiskGB: [")
    with caplog.at_level(logging.ERROR, logger="billing_operator"):
        assert (await observer.run_once(T0 + 180)).disk_gb_seconds == 480   # the last good one
    assert "keeping the last good one" in caplog.text


async def test_with_no_catalogue_nothing_is_counted_or_read(tmp_path):
    kube, sink = MemoryKube([sandbox("s-aaaaa")]), FakeMetronome()
    with pytest.raises(NoCatalogue):
        await Observer(kube, sink, CatalogueFile(tmp_path / "missing.yaml")).run_once(T0)
    assert kube.lists == 0 and sink.requests == []


async def test_a_log_line_per_pass(make, caplog):
    observer, kube, sink = make(sandbox("s-aaaaa"))
    with caplog.at_level(logging.INFO, logger="billing_operator"):
        await ticks(observer, T0, T0 + 60)
    lines = [r.getMessage() for r in caplog.records if r.getMessage().startswith("pass ")]
    assert len(lines) == 2 and lines[1].startswith(
        "pass now=2026-10-02T10:01:00Z sessions=1 awake_seconds=60 disk_gb_seconds=300 events=1 sent=1 pending=0 "
        "dropped=0 lease=renewed duration_ms=")
    assert kube.lease["spec"] == {"holderIdentity": "test", "renewTime": "2026-10-02T10:01:00.000000Z"}


async def test_the_clock_is_read_once_and_truncated_to_the_second(make):
    observer, kube, sink = make(sandbox("s-aaaaa"))
    reads = []

    def clock():
        reads.append(1)
        return T0 + 0.987

    observer.clock = clock
    assert (await observer.run_once()).now == "2026-10-02T10:00:00Z" and reads == [1]


async def test_the_loop_runs_passes_one_after_another_and_survives_a_failed_one(make):
    observer, kube, sink = make(sandbox("s-aaaaa"))
    running, most, calls = [0], [0], [0]
    real = observer.run_once

    async def slow():
        calls[0] += 1
        running[0] += 1
        most[0] = max(most[0], running[0])
        try:
            await asyncio.sleep(0.03)  # longer than the tick
            if calls[0] == 2:
                raise RuntimeError("the API server is away")
            return await real()
        finally:
            running[0] -= 1

    observer.run_once = slow
    task = asyncio.create_task(observer.run(0.01))
    await asyncio.sleep(0.2)
    task.cancel()
    await asyncio.gather(task, return_exceptions=True)
    assert most[0] == 1 and calls[0] >= 4 and observer.passes >= 2
