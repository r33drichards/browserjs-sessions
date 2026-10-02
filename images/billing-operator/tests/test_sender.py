"""The events and their delivery: keys, the hour's disk, batches of 100,
retry for an hour with the same keys, a refusal dropped and logged."""
import logging

import pytest

from billing_operator.events import AWAKE, KEPT, HourlyDisk, awake_event, hour_of, kept_event
from billing_operator.meter import ts
from billing_operator.sender import FakeMetronome, Metronome, Rejected, Retry, Sender

from fake_api import FakeIngest

T0 = ts("2026-10-02T10:00:00Z")
C = {"s-aaaaa": "acct-a", "s-bbbbb": "acct-b"}


def events(n, now=T0):
    return [awake_event(f"s-{i:05d}", "acct-a", now, 60) for i in range(n)]


# --- the events ---------------------------------------------------------------------

def test_the_events_are_metronome_mds():
    assert awake_event("s-aaaaa", "acct-a", ts("2026-10-02T10:01:00Z"), 60).body() == {
        "transaction_id": "awake/s-aaaaa/1790935260", "customer_id": "acct-a", "event_type": "session.awake",
        "timestamp": "2026-10-02T10:01:00Z", "properties": {"session_id": "s-aaaaa", "seconds": "60"}}
    assert kept_event("s-aaaaa", "acct-a", T0, T0 + 3600, 18000).body() == {
        "transaction_id": f"kept/s-aaaaa/{T0}", "customer_id": "acct-a", "event_type": "session.kept",
        "timestamp": "2026-10-02T11:00:00Z", "properties": {"session_id": "s-aaaaa", "gb_seconds": "18000"}}


def test_the_tick_at_the_hour_completes_the_hour_before():
    assert hour_of(T0 + 1) == T0 == hour_of(T0 + 3600) and hour_of(T0) == T0 - 3600 and hour_of(T0 + 3601) == T0 + 3600


def test_an_hour_of_disk_is_one_event_at_the_hour():
    disk, sent = HourlyDisk(), []
    for i in range(1, 121):  # two hours of ticks
        sent += disk.tick(T0 + 60 * i, {"s-aaaaa": 300}, C)
    assert [e.body() for e in sent] == [kept_event("s-aaaaa", "acct-a", T0, T0 + 3600, 18000).body(),
                                        kept_event("s-aaaaa", "acct-a", T0 + 3600, T0 + 7200, 18000).body()]
    assert len(disk) == 0


def test_a_session_deleted_in_the_hour_is_sent_at_its_last_sight():
    disk = HourlyDisk()
    assert disk.tick(T0 + 60, {"s-aaaaa": 300, "s-bbbbb": 300}, C) == []
    assert disk.tick(T0 + 120, {"s-aaaaa": 300, "s-bbbbb": 300}, C) == []
    gone = disk.tick(T0 + 180, {"s-bbbbb": 300}, {"s-bbbbb": "acct-b"})
    assert [e.body() for e in gone] == [kept_event("s-aaaaa", "acct-a", T0, T0 + 120, 600).body()]
    assert len(disk) == 1


def test_an_hour_is_sent_when_it_is_over_even_if_the_tick_counted_nothing_and_late_ticks_split_by_hour():
    disk = HourlyDisk()
    disk.tick(T0 + 3540, {"s-aaaaa": 300}, C)
    # The observer was away over the hour's end: the next tick counts nothing for s-aaaaa.
    late = disk.tick(T0 + 3900, {}, C)
    assert [(e.transaction_id, e.properties["gb_seconds"]) for e in late] == [(f"kept/s-aaaaa/{T0}", "300")]
    assert disk.tick(T0 + 3960, {"s-aaaaa": 300}, C) == []
    nxt = disk.tick(T0 + 7260, {"s-aaaaa": 5}, C)   # an hour's sum closed by a tick of the next hour
    assert [(e.transaction_id, e.properties["gb_seconds"]) for e in nxt] == [(f"kept/s-aaaaa/{T0 + 3600}", "300")]


def test_nothing_counted_is_no_event():
    disk = HourlyDisk()
    disk.tick(T0 + 60, {"s-aaaaa": 0}, C)
    assert disk.tick(T0 + 3600, {"s-aaaaa": 0}, C) == []


# --- delivery -------------------------------------------------------------------------

async def test_batches_hold_at_most_a_hundred():
    sink = FakeMetronome()
    sender = Sender(sink)
    sender.add(T0, events(250))
    flush = await sender.flush(T0)
    assert [len(r) for r in sink.requests] == [100, 100, 50]
    assert (flush.sent, flush.pending, flush.dropped) == (250, 0, 0) and len(sink.events) == 250


@pytest.mark.parametrize("failure", [Retry("503"), Retry("429"), Retry("ClientConnectorError")])
async def test_a_failure_is_retried_with_the_same_keys_and_backoff(failure, caplog):
    sink = FakeMetronome()
    sender = Sender(sink, backoff=60)
    sender.add(T0, events(150))
    sink.fail = failure
    with caplog.at_level(logging.WARNING, logger="billing_operator"):
        first = await sender.flush(T0)
    assert (first.sent, first.pending, first.dropped) == (0, 150, 0) and len(sink.requests) == 1  # it does not hammer
    assert "tried again in 60s" in caplog.text
    assert (await sender.flush(T0 + 30)).pending == 150 and len(sink.requests) == 1   # not before the backoff
    await sender.flush(T0 + 60)                                                        # 2nd failure: 120 s
    await sender.flush(T0 + 120)
    assert len(sink.requests) == 2
    await sender.flush(T0 + 180)                                                       # 3rd: 240 s
    await sender.flush(T0 + 420)                                                       # 4th: 300 s, the most
    assert len(sink.requests) == 4 and sender._not_before == T0 + 720
    sink.fail = None
    sender.add(T0 + 720, events(1, T0 + 720))
    done = await sender.flush(T0 + 720)
    assert (done.sent, done.pending) == (151, 0)
    # The very events, with the very keys, oldest first.
    assert [e.transaction_id for e in sink.requests[-3]] == [e.transaction_id for e in sink.requests[0]]
    assert sender._not_before == 0


async def test_given_up_after_an_hour(caplog):
    sink = FakeMetronome()
    sink.fail = Retry("503")
    sender = Sender(sink)
    sender.add(T0, events(3))
    sender.add(T0 + 1800, events(2, T0 + 1800))
    await sender.flush(T0 + 1800)
    assert (await sender.flush(T0 + 3600)).dropped == 0   # an hour exactly: still kept
    with caplog.at_level(logging.ERROR, logger="billing_operator"):
        late = await sender.flush(T0 + 3601)
    assert (late.dropped, late.pending) == (3, 2)
    assert "dropped (that time is free)" in caplog.text and f"awake/s-00000/{T0}" in caplog.text
    sink.fail = None
    sender._not_before = 0
    assert (await sender.flush(T0 + 3660)).sent == 2
    assert sorted(sink.events) == sorted(e.transaction_id for e in events(2, T0 + 1800))


async def test_a_refusal_is_dropped_and_logged_and_the_rest_goes_on(caplog):
    sink = FakeMetronome()
    sender = Sender(sink)
    sender.add(T0, events(120))
    calls = []
    real = sink.ingest

    async def refuse_the_first(batch):
        calls.append(len(batch))
        if len(calls) == 1:
            raise Rejected("400: bad event")
        await real(batch)

    sink.ingest = refuse_the_first
    with caplog.at_level(logging.ERROR, logger="billing_operator"):
        flush = await sender.flush(T0)
    assert (flush.sent, flush.dropped, flush.pending) == (20, 100, 0) and calls == [100, 20]
    assert "ingest refused 100 usage events" in caplog.text and f"awake/s-00000/{T0}" in caplog.text and "and 80 more" in caplog.text


async def test_the_fake_ignores_a_key_it_has_seen():
    sink = FakeMetronome()
    await sink.ingest(events(2))
    await sink.ingest(events(3))
    assert len(sink.events) == 3 and sink.total(AWAKE, "seconds") == 180 and sink.total(KEPT, "gb_seconds") == 0


# --- the real client, against an ingest endpoint over HTTP ---------------------------------

@pytest.fixture
async def ingest(aiohttp_server):
    fake = FakeIngest()
    server = await aiohttp_server(fake.app())
    fake.url = f"http://127.0.0.1:{server.port}"
    return fake


async def test_metronome_posts_the_events_with_the_token(ingest):
    m = Metronome(ingest.url + "/", "made-up-token")
    try:
        await m.ingest(events(2))
    finally:
        await m.close()
    assert ingest.requests == [("Bearer made-up-token", [f"awake/s-00000/{T0}", f"awake/s-00001/{T0}"])]
    assert ingest.events[f"awake/s-00000/{T0}"] == events(1)[0].body()


@pytest.mark.parametrize("status,error", [(429, Retry), (500, Retry), (503, Retry), (400, Rejected), (401, Rejected), (404, Rejected)])
async def test_metronomes_answers(ingest, status, error):
    ingest.status = status
    m = Metronome(ingest.url, "made-up-token")
    try:
        with pytest.raises(error) as e:
            await m.ingest(events(1))
    finally:
        await m.close()
    assert "made-up-token" not in str(e.value)


async def test_no_answer_is_a_retry_and_the_token_is_in_no_error():
    m = Metronome("http://127.0.0.1:1", "made-up-token", timeout=2)
    try:
        with pytest.raises(Retry) as e:
            await m.ingest(events(1))
    finally:
        await m.close()
    assert "made-up-token" not in str(e.value) and "made-up-token" not in repr(m.__dict__.get("url"))


async def test_the_sender_over_http_retries_a_5xx_with_the_same_keys(ingest):
    m = Metronome(ingest.url, "made-up-token")
    sender = Sender(m, backoff=60)
    try:
        sender.add(T0, events(101))
        ingest.status = 503
        assert (await sender.flush(T0)).pending == 101
        ingest.status = None
        assert (await sender.flush(T0 + 60)).sent == 101
    finally:
        await m.close()
    assert [len(keys) for _, keys in ingest.requests] == [100, 100, 1] and ingest.requests[0][1] == ingest.requests[1][1]
    assert len(ingest.events) == 101
