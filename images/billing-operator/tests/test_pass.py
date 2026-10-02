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


def minutes(n, start=T0):
    return [start + 60 * i for i in range(n + 1)]


async def test_five_ticks_of_sixty_awake_seconds_make_one_event_of_three_hundred(make):
    observer, kube, sink = make(sandbox("s-aaaaa", ready_since="2026-10-02T09:59:40Z"), sandbox("s-bbbbb", owner=OTHER))
    results = await ticks(observer, *minutes(10))
    assert [r.awake_seconds for r in results] == [20] + [120] * 10      # observed every tick
    assert [r.events for r in results] == [1, 0, 0, 0, 0, 2, 0, 0, 0, 0, 2]   # sent at the window's end
    assert [(e.transaction_id, e.customer_id, e.timestamp, e.properties) for e in sink.events.values()] == [
        # The 20 s since Ready before 10:00:00 belong to the window that ended then.
        (f"awake/s-aaaaa/{T0 - 300}", acct(), "2026-10-02T10:00:00Z", {"session_id": "s-aaaaa", "seconds": "20"}),
        (f"awake/s-aaaaa/{T0}", acct(), "2026-10-02T10:05:00Z", {"session_id": "s-aaaaa", "seconds": "300"}),
        (f"awake/s-bbbbb/{T0}", acct(OTHER), "2026-10-02T10:05:00Z", {"session_id": "s-bbbbb", "seconds": "300"}),
        (f"awake/s-aaaaa/{T0 + 300}", acct(), "2026-10-02T10:10:00Z", {"session_id": "s-aaaaa", "seconds": "300"}),
        (f"awake/s-bbbbb/{T0 + 300}", acct(OTHER), "2026-10-02T10:10:00Z", {"session_id": "s-bbbbb", "seconds": "300"}),
    ]
    assert kube.lists == 11  # one list of Sandboxes a pass


async def test_six_hours_of_disk_are_one_event_per_kept_session(make):
    six = ts("2026-10-02T06:00:00Z")
    observer, kube, sink = make(sandbox("s-aaaaa"), asleep("s-bbbbb"))
    results = await ticks(observer, *minutes(360, six))
    assert sent(sink, KEPT) == [
        (f"kept/s-aaaaa/{six}", acct(), {"session_id": "s-aaaaa", "gb_seconds": "108000"}),
        (f"kept/s-bbbbb/{six}", acct(), {"session_id": "s-bbbbb", "gb_seconds": "108000"}),
    ]
    assert [e.timestamp for e in sink.events.values() if e.event_type == KEPT] == ["2026-10-02T12:00:00Z"] * 2
    assert results[-1].events == 3 and results[-2].events == 0
    # A sleeping session sends disk only; the awake one a window at a time.
    assert sink.total(AWAKE, "seconds") == 21600 and len(sent(sink, AWAKE)) == 72
    assert all(p["session_id"] == "s-aaaaa" for _, _, p in sent(sink, AWAKE))


async def test_the_windows_are_settings(make):
    observer, kube, sink = make(sandbox("s-aaaaa"), awake_window=120, kept_window=600)
    await ticks(observer, *minutes(10))
    assert [(e.event_type, e.transaction_id.split("/")[2], e.timestamp[11:19]) for e in sink.events.values()] == [
        (AWAKE, str(T0), "10:02:00"), (AWAKE, str(T0 + 120), "10:04:00"), (AWAKE, str(T0 + 240), "10:06:00"),
        (AWAKE, str(T0 + 360), "10:08:00"), (AWAKE, str(T0 + 480), "10:10:00"), (KEPT, str(T0), "10:10:00")]
    assert sink.total(AWAKE, "seconds") == 600 and sink.total(KEPT, "gb_seconds") == 3000


@pytest.mark.parametrize("warm", [sandbox("s-warm1", owner=None), sandbox("s-warm1", owner=None, mode="Suspended", ready="False")])
async def test_a_warm_pool_sandbox_sends_nothing(make, warm):
    observer, kube, sink = make(warm)
    results = await ticks(observer, *minutes(370, ts("2026-10-02T06:00:00Z")))
    assert sink.events == {} and all(r.sessions == 0 and r.events == 0 for r in results)


async def test_a_warm_pod_adopted_just_before_a_tick_counts_only_since_it_was_adopted(make):
    observer, kube, sink = make(sandbox("s-warm1", owner=None, ready_since="2026-10-02T09:58:00Z"))
    await observer.run_once(T0 - 60)
    adopted = sandbox("s-warm1", ready_since="2026-10-02T09:58:00Z")   # Ready for two minutes, in the pool
    adopted["metadata"]["annotations"]["browserjs.dev/created"] = "2026-10-02T09:59:55Z"
    kube.put(adopted)
    await observer.run_once(T0)
    assert sent(sink) == [(f"awake/s-warm1/{T0 - 300}", acct(), {"session_id": "s-warm1", "seconds": "5"})]


async def test_a_session_that_falls_asleep_mid_window_is_sent_at_once(make):
    observer, kube, sink = make(sandbox("s-aaaaa"))
    await ticks(observer, T0, T0 + 60, T0 + 120)
    assert sink.events == {}
    kube.put(asleep("s-aaaaa"))
    result = await observer.run_once(T0 + 180)
    assert result.events == 1 and result.sent == 1 and result.awake_seconds == 0   # the last minute before the sleep is free
    assert [(e.transaction_id, e.timestamp, e.properties["seconds"]) for e in sink.events.values()] == [
        (f"awake/s-aaaaa/{T0}", "2026-10-02T10:02:00Z", "120")]
    # Its disk goes on being counted, and waits for its own window.
    assert (await observer.run_once(T0 + 240)).disk_gb_seconds == 300 and len(sink.events) == 1


async def test_stopping_and_resuming_inside_a_window_is_neither_free_nor_counted_twice(make):
    """The loophole a single key per window would leave: stop, resume within
    the same five minutes, and the rest of the window is ignored by
    Metronome as a repeat."""
    observer, kube, sink = make(sandbox("s-aaaaa"))
    await ticks(observer, T0, T0 + 60, T0 + 120)
    kube.put(asleep("s-aaaaa"))
    await observer.run_once(T0 + 180)                                    # the first part: 120 s
    kube.put(sandbox("s-aaaaa", ready_since="2026-10-02T10:03:40Z"))     # resumed at once
    results = await ticks(observer, T0 + 240, T0 + 300, T0 + 360)
    assert [(e.transaction_id, e.properties["seconds"]) for e in sink.events.values()] == [
        (f"awake/s-aaaaa/{T0}", "120"),
        (f"awake/s-aaaaa/{T0}/{T0 + 240}", "80")]                         # 20 s since Ready, and a minute
    counted = 120 + sum(r.awake_seconds for r in results[:2])
    assert sink.total(AWAKE, "seconds") == counted == 200                 # everything observed, once
    assert len(sink.requests) == 2


async def test_a_session_deleted_mid_window_is_sent_at_its_last_sight(make):
    observer, kube, sink = make(sandbox("s-aaaaa"))
    await ticks(observer, T0, T0 + 60, T0 + 120)
    kube.put(sandbox("s-aaaaa", deleting=True))
    result = await observer.run_once(T0 + 180)
    assert result.awake_seconds == 0 and observer.sessions == {}   # its last minute is free
    assert [(e.transaction_id, e.timestamp, e.properties) for e in sink.events.values()] == [
        (f"awake/s-aaaaa/{T0}", "2026-10-02T10:02:00Z", {"session_id": "s-aaaaa", "seconds": "120"}),
        (f"kept/s-aaaaa/{ts('2026-10-02T06:00:00Z')}", "2026-10-02T10:02:00Z", {"session_id": "s-aaaaa", "gb_seconds": "600"})]


async def test_the_same_window_sent_twice_has_the_same_transaction_id(make):
    observer, kube, sink = make(sandbox("s-aaaaa"))
    await ticks(observer, T0, T0 + 60, T0 + 120)
    # A restart in the middle of the window: its 120 s were in memory and are gone.
    restarted = Observer(kube, sink, observer.catalogue, holder="test")
    await ticks(restarted, T0 + 180, T0 + 240, T0 + 300)
    assert [(e.transaction_id, e.properties["seconds"]) for e in sink.events.values()] == [(f"awake/s-aaaaa/{T0}", "120")]
    # The first one, had it lived, sends the same window under the same key: Metronome keeps one.
    await ticks(observer, T0 + 180, T0 + 240, T0 + 300)
    assert [[e.transaction_id for e in r] for r in sink.requests] == [[f"awake/s-aaaaa/{T0}"]] * 2
    assert len(sink.events) == 1
    # And a tick looked at twice counts once.
    assert (await observer.run_once(T0 + 300)).awake_seconds == 0


async def test_a_restart_counts_nothing_for_the_time_it_was_down_beyond_the_gap(make):
    observer, kube, sink = make(sandbox("s-aaaaa"), asleep("s-bbbbb"))
    await ticks(observer, *minutes(10))
    assert sink.total(AWAKE, "seconds") == 600
    # Killed; back ten minutes later with an empty memory.
    restarted = Observer(kube, sink, observer.catalogue, holder="test")
    results = await ticks(restarted, *minutes(5, T0 + 1200))
    assert results[0].awake_seconds == 0 and results[0].disk_gb_seconds == 0   # Ready long ago, never seen by this process
    assert [r.awake_seconds for r in results[1:]] == [60] * 5
    assert sink.total(AWAKE, "seconds") == 900 and f"awake/s-aaaaa/{T0 + 600}" not in sink.events


async def test_the_observer_away_within_the_gap_counts_it_all(make):
    observer, kube, sink = make(sandbox("s-aaaaa"))
    await ticks(observer, T0, T0 + 60, T0 + 210, T0 + 361)   # 150 s is counted; 151 s is not
    assert [(e.transaction_id, e.timestamp, e.properties["seconds"]) for e in sink.events.values()] == [
        (f"awake/s-aaaaa/{T0}", "2026-10-02T10:05:00Z", "210")]


async def test_the_lease_is_renewed_after_each_window_that_was_delivered(make, caplog):
    observer, kube, sink = make(sandbox("s-aaaaa"), backoff=60)
    await ticks(observer, *minutes(6))
    # At start, then once a window: when it begins with nothing waiting, and when its event is delivered.
    assert kube.renewals == [T0, T0 + 60, T0 + 300, T0 + 360]
    sink.fail = Retry("503")
    with caplog.at_level(logging.WARNING, logger="billing_operator"):
        down = await ticks(observer, *minutes(3, T0 + 420))
    assert [(r.events, r.pending, r.lease) for r in down] == [(0, 0, False)] * 3 + [(1, 1, False)]
    assert "lease=kept" in down[-1].line()
    sink.fail = None
    back = await observer.run_once(T0 + 660)
    assert (back.sent, back.pending, back.lease) == (1, 0, True) and kube.renewals[-1] == T0 + 660
    assert sink.total(AWAKE, "seconds") == 600   # every minute observed, each once


async def test_metronome_away_for_over_an_hour_that_usage_is_free(make):
    observer, kube, sink = make(sandbox("s-aaaaa"), backoff=60)
    sink.fail = Retry("503")
    results = await ticks(observer, *minutes(90))
    # Eighteen windows were made; the five made more than an hour ago are given up.
    assert sum(r.events for r in results) == 18 and sum(r.dropped for r in results) == 5 and results[-1].pending == 13
    assert not any(r.lease for r in results[5:])
    sink.fail = None
    observer.sender._not_before = 0
    await observer.run_once(T0 + 91 * 60)
    assert sink.total(AWAKE, "seconds") == 12 * 300   # one more passed its hour meanwhile
    assert f"awake/s-aaaaa/{T0}" not in sink.events


async def test_a_refused_window_is_dropped_and_counts_as_delivered(make, caplog):
    observer, kube, sink = make(sandbox("s-aaaaa"))
    await ticks(observer, *minutes(4))
    sink.fail = Rejected("400: unknown customer")
    with caplog.at_level(logging.ERROR, logger="billing_operator"):
        result = await observer.run_once(T0 + 300)
    assert (result.dropped, result.pending, result.lease) == (1, 0, True)
    assert f"awake/s-aaaaa/{T0}" in caplog.text


async def test_a_failed_lease_does_not_fail_the_tick_and_is_tried_again(make, caplog):
    observer, kube, sink = make(sandbox("s-aaaaa"))
    real = kube.renew_lease
    calls = []

    async def refuse(now, holder):
        calls.append(now)
        if len(calls) < 3:
            raise RuntimeError("403")
        await real(now, holder)

    kube.renew_lease = refuse
    with caplog.at_level(logging.ERROR, logger="billing_operator"):
        results = await ticks(observer, *minutes(3))
    assert [r.lease for r in results] == [False, False, True, False] and "Lease not renewed" in caplog.text
    assert calls == [T0, T0 + 60, T0 + 120]


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
        await ticks(observer, *minutes(5))
    lines = [r.getMessage() for r in caplog.records if r.getMessage().startswith("pass ")]
    assert len(lines) == 6 and lines[-1].startswith(
        "pass now=2026-10-02T10:05:00Z sessions=1 awake_seconds=60 disk_gb_seconds=300 events=1 sent=1 pending=0 "
        "dropped=0 lease=renewed duration_ms=")
    assert "events=0 sent=0 pending=0 dropped=0 lease=kept" in lines[2]
    assert kube.lease["spec"] == {"holderIdentity": "test", "renewTime": "2026-10-02T10:05:00.000000Z"}


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
