import io
import json
import tarfile

import pytest
from aiohttp import web

from policy_operator import bundle
from policy_operator.bundle import BundleError
from policy_operator.check import policy_hash
from policy_operator.operator import Operator

from conftest import ALLOW_ALL, BROKEN, DENY_ALL, H, resource


def loaded_document(op: Operator) -> dict:
    with tarfile.open(fileobj=io.BytesIO(op.publisher.body), mode="r:gz") as tar:
        return json.loads(tar.extractfile("/data.json").read())["browserjs"]["loaded"]


def spec(kind, source):
    return {"kind": kind, "source": source}


async def test_nothing_is_published_before_the_first_pass(cfg):
    op = Operator(cfg)
    await op.reconcile("s-aaaaa", spec("rego", ALLOW_ALL), {})
    assert not op.ready and op.publisher.body is None and op.publisher.etag is None


async def test_first_pass_publishes_everything_at_once(cfg):
    op = Operator(cfg)
    await op.first_pass([resource("s-aaaaa", "rego", ALLOW_ALL), resource("s-bbbbb", "rego", DENY_ALL)])
    assert op.ready
    assert loaded_document(op) == {"s-aaaaa": policy_hash(ALLOW_ALL), "s-bbbbb": policy_hash(DENY_ALL)}
    start, n = op.publisher.revision.split("-")
    assert n == "1" and start.isdigit()


async def test_first_pass_with_nothing_publishes_an_empty_bundle(cfg):
    op = Operator(cfg)
    await op.first_pass([])
    assert op.ready and loaded_document(op) == {}


async def test_first_pass_skips_resources_being_deleted(cfg):
    op = Operator(cfg)
    going = resource("s-aaaaa", "rego", ALLOW_ALL)
    going["metadata"]["deletionTimestamp"] = "2026-10-02T00:00:00Z"
    await op.first_pass([going])
    assert loaded_document(op) == {}


async def test_revision_increases_with_every_published_bundle(cfg):
    op = Operator(cfg)
    await op.first_pass([])
    start = op.publisher.revision.split("-")[0]
    out = await op.reconcile("s-aaaaa", spec("rego", ALLOW_ALL), {})
    assert op.publisher.revision == f"{start}-2" == out.revision
    out = await op.reconcile("s-aaaaa", spec("rego", DENY_ALL), {})
    assert op.publisher.revision == f"{start}-3" == out.revision
    assert out.tenant.hash == policy_hash(DENY_ALL) and loaded_document(op) == {"s-aaaaa": policy_hash(DENY_ALL)}


async def test_same_spec_again_publishes_nothing(cfg):
    op = Operator(cfg)
    await op.first_pass([resource("s-aaaaa", "rego", ALLOW_ALL)])
    etag, revision = op.publisher.etag, op.publisher.revision
    out = await op.reconcile("s-aaaaa", spec("rego", ALLOW_ALL), {})  # the resume after the first pass
    assert (op.publisher.etag, op.publisher.revision) == (etag, revision)
    assert out.revision == revision and out.validation.ok


async def test_a_spec_that_does_not_compile_and_no_last_good_is_left_out(cfg):
    op = Operator(cfg)
    await op.first_pass([resource("s-aaaaa", "rego", ALLOW_ALL)])
    out = await op.reconcile("s-bbbbb", spec("rego", BROKEN), {})
    assert not out.validation.ok and out.tenant is None and out.build_errors == []
    assert loaded_document(op) == {"s-aaaaa": policy_hash(ALLOW_ALL)}


async def test_last_good_in_memory_stays_in_force(cfg):
    op = Operator(cfg)
    await op.first_pass([resource("s-aaaaa", "rego", ALLOW_ALL)])
    etag = op.publisher.etag
    out = await op.reconcile("s-aaaaa", spec("rego", BROKEN), {"rego": ALLOW_ALL})
    assert not out.validation.ok and out.tenant.hash == policy_hash(ALLOW_ALL)
    assert op.publisher.etag == etag  # nothing changed in the bundle


async def test_last_good_from_status_after_a_restart(cfg):
    op = Operator(cfg)
    await op.first_pass([resource("s-aaaaa", "rego", BROKEN, status={"rego": DENY_ALL, "hash": policy_hash(DENY_ALL)}),
                         resource("s-bbbbb", "json", "{}", status={})])
    assert loaded_document(op) == {"s-aaaaa": policy_hash(DENY_ALL)}
    assert not op.sessions["s-aaaaa"].validation.ok


async def test_status_rego_is_checked_again(cfg):
    op = Operator(cfg)
    stolen = H + 'allow_tool_call if data.browserjs.tenant["s-other"].allow_tool_call\n'
    await op.first_pass([resource("s-aaaaa", "rego", BROKEN, status={"rego": stolen, "hash": policy_hash(stolen)})])
    assert loaded_document(op) == {}


async def test_remove(cfg):
    op = Operator(cfg)
    await op.first_pass([resource("s-aaaaa", "rego", ALLOW_ALL), resource("s-bbbbb", "rego", DENY_ALL)])
    await op.remove("s-aaaaa")
    assert loaded_document(op) == {"s-bbbbb": policy_hash(DENY_ALL)}
    revision = op.publisher.revision
    await op.remove("s-aaaaa")
    await op.remove("s-never")
    assert op.publisher.revision == revision


async def test_a_bundle_that_does_not_build_leaves_the_previous_one(cfg, monkeypatch):
    op = Operator(cfg)
    await op.first_pass([resource("s-aaaaa", "rego", ALLOW_ALL)])
    etag = op.publisher.etag

    def fail(*a, **k):
        raise BundleError([{"code": "rego_compile_error", "message": "boom"}])

    monkeypatch.setattr(bundle, "build", fail)
    out = await op.reconcile("s-aaaaa", spec("rego", DENY_ALL), {})
    assert out.build_errors == [{"code": "rego_compile_error", "message": "boom"}]
    assert out.validation.ok and out.tenant.hash == policy_hash(ALLOW_ALL)
    assert op.publisher.etag == etag
    out = await op.reconcile("s-bbbbb", spec("rego", DENY_ALL), {})
    assert out.build_errors and out.tenant is None and "s-bbbbb" not in op.sessions
    monkeypatch.undo()
    # Nothing was cached from the failures: the next attempt goes through.
    out = await op.reconcile("s-aaaaa", spec("rego", DENY_ALL), {})
    assert not out.build_errors and loaded_document(op) == {"s-aaaaa": policy_hash(DENY_ALL)}


async def test_first_pass_fails_when_the_bundle_does_not_build(cfg, monkeypatch):
    monkeypatch.setattr(bundle, "build", lambda *a, **k: (_ for _ in ()).throw(BundleError([{"message": "boom"}])))
    op = Operator(cfg)
    with pytest.raises(BundleError):
        await op.first_pass([resource("s-aaaaa", "rego", ALLOW_ALL)])
    assert not op.ready and op.publisher.body is None


# --- what the replicas have loaded: OPA faked by a small HTTP server ---------

class FakeOpa:
    """GET /v1/data/browserjs/loaded, for the operator's token only."""

    def __init__(self, token="opa-secret"):
        self.token, self.document, self.requests = token, {}, 0

    async def handle(self, request):
        self.requests += 1
        if request.headers.get("Authorization") != f"Bearer {self.token}":
            return web.Response(status=401)
        return web.json_response({"result": self.document})

    async def start(self, aiohttp_server):
        app = web.Application()
        app.router.add_get("/v1/data/browserjs/loaded", self.handle)
        server = await aiohttp_server(app)
        self.address = f"127.0.0.1:{server.port}"
        return self


async def test_loaded_counts_the_replicas_that_serve_the_hash(cfg, aiohttp_server):
    a, b = await FakeOpa().start(aiohttp_server), await FakeOpa().start(aiohttp_server)
    op = Operator(cfg)
    try:
        a.document = {"s-aaaaa": "sha256:1"}
        b.document = {"s-aaaaa": "sha256:0"}
        state = await op.loaded("s-aaaaa", "sha256:1", [a.address, b.address])
        assert (state.replicas, state.total, state.all) == (1, 2, False)
        b.document = {"s-aaaaa": "sha256:1"}
        state = await op.loaded("s-aaaaa", "sha256:1", [a.address, b.address])
        assert (state.replicas, state.total, state.all) == (2, 2, True)
        assert (await op.loaded("s-zzzzz", "sha256:1", [a.address, b.address])).replicas == 0
        assert (await op.loaded("s-aaaaa", None, [a.address, b.address])).all is False
        none = await op.loaded("s-aaaaa", "sha256:1", [])
        assert (none.replicas, none.total, none.all) == (0, 0, False)
    finally:
        await op.close()


async def test_a_replica_that_refuses_or_is_gone_counts_as_not_loaded(cfg, aiohttp_server):
    good = await FakeOpa().start(aiohttp_server)
    refusing = await FakeOpa(token="another").start(aiohttp_server)
    good.document = refusing.document = {"s-aaaaa": "sha256:1"}
    op = Operator(cfg)
    try:
        state = await op.loaded("s-aaaaa", "sha256:1", [good.address, refusing.address, "127.0.0.1:1"])
        assert (state.replicas, state.total) == (1, 3)
    finally:
        await op.close()


async def test_loaded_is_cached_only_when_asked(cfg, aiohttp_server):
    a = await FakeOpa().start(aiohttp_server)
    op = Operator(cfg)
    try:
        for _ in range(3):
            await op.loaded("s-aaaaa", "sha256:1", [a.address], max_age=30)
        assert a.requests == 1
        await op.loaded("s-aaaaa", "sha256:1", [a.address])
        assert a.requests == 2
    finally:
        await op.close()


async def test_wait_loaded_returns_as_soon_as_every_replica_has_it(cfg, aiohttp_server):
    import asyncio
    a = await FakeOpa().start(aiohttp_server)
    op = Operator(cfg)
    try:
        async def later():
            await asyncio.sleep(0.2)
            a.document = {"s-aaaaa": "sha256:1"}
        task = asyncio.create_task(later())
        state = await op.wait_loaded("s-aaaaa", "sha256:1", lambda: [a.address], timeout=5)
        assert state.all
        await task
        state = await op.wait_loaded("s-aaaaa", "sha256:2", lambda: [a.address], timeout=0.2)
        assert not state.all and state.total == 1
    finally:
        await op.close()
