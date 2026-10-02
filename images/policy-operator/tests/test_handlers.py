"""The kopf handlers, called as functions. OPA's replicas are a small HTTP
server that answers what a replica that had just fetched the bundle would."""
import io
import json
import tarfile

import kopf
import pytest
from aiohttp import web

from policy_operator import handlers
from policy_operator.check import policy_hash
from policy_operator.operator import Operator

from conftest import ALLOW_ALL, BROKEN, DENY_ALL, example, resource


class Patch:
    def __init__(self):
        self.status = {}


class Replicas:
    """Two OPA replicas that always have the published bundle, or lag."""

    def __init__(self, op):
        self.op, self.lagging, self.frozen = op, False, None

    def document(self):
        if self.op.publisher.body is None:
            return {}
        with tarfile.open(fileobj=io.BytesIO(self.op.publisher.body), mode="r:gz") as tar:
            return json.loads(tar.extractfile("/data.json").read())["browserjs"]["loaded"]

    async def handle(self, request):
        if request.headers.get("Authorization") != "Bearer opa-secret":
            return web.Response(status=401)
        return web.json_response({"result": {} if self.lagging else self.document()})


@pytest.fixture
async def world(cfg, aiohttp_server, monkeypatch):
    op = Operator(cfg)
    monkeypatch.setattr(handlers, "OPERATOR", op)
    monkeypatch.setattr(handlers, "LOADED_WAIT_SECONDS", 0.3)
    replicas = Replicas(op)
    addresses = []
    for _ in range(2):
        app = web.Application()
        app.router.add_get("/v1/data/browserjs/loaded", replicas.handle)
        addresses.append(f"127.0.0.1:{(await aiohttp_server(app)).port}")
    # The shape of kopf's index: key to the values stored under it.
    index = {"opa-abcde": [addresses[:1]], "opa-fghij": [addresses[1:]]}
    yield op, replicas, index
    await op.close()


async def reconcile(index, body, status=None):
    patch = Patch()
    await handlers.reconcile(name=body["metadata"]["name"], spec=body["spec"], status=status or body.get("status") or {},
                             meta=body["metadata"], patch=patch, opa_endpoints=index)
    return patch.status


def conds(status):
    return {c["type"]: c["status"] for c in status["conditions"]}


async def test_create_writes_status_as_the_crd_describes_it(world):
    op, _, index = world
    await op.first_pass([])
    source = example("one-site", "rego")
    status = await reconcile(index, resource("s-aaaaa", "rego", source, generation=1))
    assert set(status) == {"observedGeneration", "rego", "hash", "regoGeneration", "errors", "warnings",
                           "loaded", "lastAppliedTime", "conditions"}
    assert status["observedGeneration"] == 1 and status["regoGeneration"] == 1
    assert status["rego"] == example("one-site", "rego") and status["hash"] == policy_hash(status["rego"])
    assert status["errors"] == [] and status["warnings"] == []
    assert status["loaded"] == {"replicas": 2, "total": 2, "revision": op.publisher.revision}
    assert conds(status) == {"Compiled": "True", "Loaded": "True", "Ready": "True"}
    for c in status["conditions"]:
        assert set(c) == {"type", "status", "reason", "message", "observedGeneration", "lastTransitionTime"}
        assert c["observedGeneration"] == 1 and c["lastTransitionTime"].endswith("Z")
    json.dumps(status)  # it is a patch: plain JSON


async def test_update_applies_and_a_broken_update_keeps_the_last_good(world):
    op, _, index = world
    await op.first_pass([])
    status = await reconcile(index, resource("s-aaaaa", "rego", ALLOW_ALL, generation=1))
    changed = await reconcile(index, resource("s-aaaaa", "rego", DENY_ALL, generation=2), status)
    status = {**status, **changed}
    assert status["hash"] == policy_hash(DENY_ALL) and status["regoGeneration"] == 2
    assert conds(status) == {"Compiled": "True", "Loaded": "True", "Ready": "True"}

    broken = await reconcile(index, resource("s-aaaaa", "rego", BROKEN, generation=3), status)
    assert "rego" not in broken and "hash" not in broken and "regoGeneration" not in broken
    status = {**status, **broken}
    assert status["observedGeneration"] == 3 and status["regoGeneration"] == 2
    assert status["hash"] == policy_hash(DENY_ALL) and status["rego"] == DENY_ALL
    assert status["errors"][0]["code"] == "rego_parse_error" and status["errors"][0]["row"] == 4
    assert conds(status) == {"Compiled": "False", "Loaded": "True", "Ready": "False"}
    # The bundle still carries the last good module.
    assert handlers and op.sessions["s-aaaaa"].tenant.hash == policy_hash(DENY_ALL)


async def test_resume_after_a_restart_changes_nothing(world):
    op, _, index = world
    await op.first_pass([])
    status = await reconcile(index, resource("s-aaaaa", "rego", ALLOW_ALL))

    # A new process: the first pass over what exists, then kopf's resume.
    again = Operator(op.cfg)
    handlers.OPERATOR = again
    existing = resource("s-aaaaa", "rego", ALLOW_ALL, status=status)
    await again.first_pass([existing])
    etag = again.publisher.etag
    resumed = await reconcile({}, existing)  # no replica has the new process's bundle... or any address
    assert again.publisher.etag == etag
    assert resumed["hash"] == status["hash"] and resumed["loaded"]["revision"] == status["loaded"]["revision"]
    assert conds(resumed)["Compiled"] == "True" and conds(resumed)["Loaded"] == "False"
    assert next(c for c in resumed["conditions"] if c["type"] == "Compiled")["lastTransitionTime"] == \
        next(c for c in status["conditions"] if c["type"] == "Compiled")["lastTransitionTime"]


async def test_replicas_that_lag_are_left_to_the_timer(world):
    op, replicas, index = world
    await op.first_pass([])
    replicas.lagging = True
    body = resource("s-aaaaa", "rego", ALLOW_ALL)
    status = await reconcile(index, body)
    assert conds(status) == {"Compiled": "True", "Loaded": "False", "Ready": "False"}
    assert status["loaded"]["replicas"] == 0 and status["loaded"]["total"] == 2 and "lastAppliedTime" not in status

    patch = Patch()
    await handlers.loaded(name="s-aaaaa", status=status, meta=body["metadata"], patch=patch, opa_endpoints=index)
    assert patch.status == {}  # still lagging: nothing to write

    replicas.lagging = False
    op._loaded_cache = None
    await handlers.loaded(name="s-aaaaa", status=status, meta=body["metadata"], patch=patch, opa_endpoints=index)
    assert conds(patch.status) == {"Compiled": "True", "Loaded": "True", "Ready": "True"}
    assert patch.status["loaded"]["replicas"] == 2 and "lastAppliedTime" in patch.status
    assert "rego" not in patch.status and "hash" not in patch.status

    status = {**status, **patch.status}
    patch = Patch()
    await handlers.loaded(name="s-aaaaa", status=status, meta=body["metadata"], patch=patch, opa_endpoints=index)
    assert patch.status == {}


async def test_timer_waits_for_the_reconcile_of_a_new_generation(world):
    op, _, index = world
    await op.first_pass([])
    body = resource("s-aaaaa", "rego", ALLOW_ALL, generation=1)
    status = await reconcile(index, body)
    patch = Patch()
    await handlers.loaded(name="s-aaaaa", status=status, meta={**body["metadata"], "generation": 2},
                          patch=patch, opa_endpoints=index)
    assert patch.status == {}


async def test_delete_removes_the_session_from_the_bundle(world):
    op, replicas, index = world
    await op.first_pass([])
    await reconcile(index, resource("s-aaaaa", "rego", ALLOW_ALL))
    await reconcile(index, resource("s-bbbbb", "rego", DENY_ALL))
    await handlers.delete(name="s-aaaaa")
    assert replicas.document() == {"s-bbbbb": policy_hash(DENY_ALL)}
    await handlers.gone(event={"type": "DELETED"}, name="s-bbbbb")
    assert replicas.document() == {}
    revision = op.publisher.revision
    await handlers.gone(event={"type": "MODIFIED"}, name="s-ccccc")
    await handlers.gone(event={"type": None}, name="s-ccccc")
    assert op.publisher.revision == revision


def test_ready_addresses():
    body = {"addressType": "IPv4", "ports": [{"name": "http", "port": 8181}], "endpoints": [
        {"addresses": ["10.0.0.1"], "conditions": {"ready": True}},
        {"addresses": ["10.0.0.2"], "conditions": {"ready": False}},
        {"addresses": ["10.0.0.3"]},
        {"addresses": ["10.0.0.4"], "conditions": {"ready": True, "terminating": False}},
    ]}
    assert handlers.ready_addresses(body, 1) == ["10.0.0.1:8181", "10.0.0.3:8181", "10.0.0.4:8181"]
    assert handlers.ready_addresses({"addressType": "IPv6", "endpoints": [{"addresses": ["fd00::1"]}]}, 8181) == ["[fd00::1]:8181"]
    assert handlers.ready_addresses({}, 8181) == []


def test_addresses_of_every_slice():
    assert handlers.addresses_of({"a": [["1:1", "2:1"]], "b": [["3:1"], ["1:1"]]}) == ["1:1", "2:1", "3:1"]
    assert handlers.addresses_of(None) == [] and handlers.addresses_of({}) == []


def test_the_index_function():
    out = handlers.opa_endpoints(name="opa-x", body={"endpoints": [{"addresses": ["10.0.0.1"]}]})
    assert out == {"opa-x": ["10.0.0.1:8181"]}


def test_the_diff_base_holds_a_digest_of_the_source_not_the_source():
    storage = handlers.SpecDigestDiffBase(prefix="policy-operator.browserjs.dev")
    big = "é" * 65536
    body = kopf.Body({"metadata": {"name": "s-aaaaa", "annotations": {"browserjs.dev/updated-by": "ui"}},
                      "spec": {"sessionRef": {"name": "s-aaaaa"}, "kind": "rego", "source": big},
                      "status": {"rego": big}})
    essence = storage.build(body=body)
    assert essence["spec"]["kind"] == "rego" and essence["spec"]["source"].startswith("sha256:")
    assert len(json.dumps(essence)) < 1024 and "status" not in essence
    assert body["spec"]["source"] == big  # the resource itself is untouched
    other = storage.build(body=kopf.Body({"metadata": {"name": "s-aaaaa"}, "spec": {"kind": "rego", "source": big + "x"}}))
    assert other["spec"]["source"] != essence["spec"]["source"]


def test_handlers_are_registered_for_sessionpolicies():
    registry = kopf.get_default_registry()
    resources = {(h.selector.group, h.selector.version, h.selector.any_name)
                 for h in registry._changing.get_all_handlers()}
    assert resources == {("browserjs.dev", "v1alpha1", "sessionpolicies")}
    mine = [h for h in registry._changing.get_all_handlers() if h.fn.__name__ == "reconcile"]
    # create, update of spec, and resume (which kopf marks as initial, with no reason).
    assert sorted(str(h.reason) for h in mine) == ["None", "create", "update"]
    assert [h.initial for h in mine if h.reason is None] == [True]
    assert [h.field for h in mine if str(h.reason) == "update"] == [("spec",)]
    assert [h.fn.__name__ for h in registry._changing.get_all_handlers() if str(h.reason) == "delete"] == ["delete"]
    assert [h.fn.__name__ for h in registry._spawning.get_all_handlers()] == ["loaded"]
    assert [h.fn.__name__ for h in registry._indexing.get_all_handlers()] == ["opa_endpoints"]
