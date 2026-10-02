import asyncio
import json
import time

import pytest

from policy_operator.check import policy_hash
from policy_operator.operator import Operator
from policy_operator.server import BUNDLE_TYPE, make_app

from conftest import ALLOW_ALL, DENY_ALL, H, example, resource

BUNDLE = {"Authorization": "Bearer bundle-secret"}
API = {"Authorization": "Bearer api-secret"}
URL = "/bundles/browserjs.tar.gz"


@pytest.fixture
async def op(cfg):
    return Operator(cfg)


@pytest.fixture
async def client(op, aiohttp_client):
    return await aiohttp_client(make_app(op))


async def test_healthz_needs_nothing(client):
    assert (await client.get("/healthz")).status == 200


async def test_readyz_is_503_until_the_first_pass(client, op):
    assert (await client.get("/readyz")).status == 503
    await op.first_pass([])
    assert (await client.get("/readyz")).status == 200


@pytest.mark.parametrize("headers", [{}, {"Authorization": "Bearer wrong"}, {"Authorization": "bundle-secret"},
                                     {"Authorization": "Bearer "}])
async def test_missing_or_wrong_token_is_401_with_an_empty_body(client, op, headers):
    await op.first_pass([])
    for method, path in [("GET", URL), ("POST", "/v1/validate"), ("POST", "/v1/evaluate"), ("GET", "/v1/schema")]:
        r = await client.request(method, path, headers=headers, data=b"{}")
        assert r.status == 401 and await r.read() == b"", path


async def test_each_token_opens_only_its_own_endpoints(client, op):
    await op.first_pass([])
    assert (await client.get(URL, headers=API)).status == 401
    assert (await client.get("/v1/schema", headers=BUNDLE)).status == 401
    assert (await client.post("/v1/validate", headers=BUNDLE, json={"kind": "rego", "source": ALLOW_ALL})).status == 401


async def test_an_unset_token_admits_nobody(cfg, aiohttp_client):
    import dataclasses
    op = Operator(dataclasses.replace(cfg, bundle_token="", api_token=""))
    await op.first_pass([])
    client = await aiohttp_client(make_app(op))
    assert (await client.get(URL, headers={"Authorization": "Bearer "})).status == 401
    assert (await client.get("/v1/schema", headers={"Authorization": "Bearer "})).status == 401


async def test_bundle_is_503_before_the_first_pass(client, op):
    r = await client.get(URL, headers=BUNDLE)
    assert r.status == 503
    # Even with policies already taken in: nothing partial is ever served.
    await op.reconcile("s-aaaaa", {"kind": "rego", "source": ALLOW_ALL}, {})
    assert (await client.get(URL, headers=BUNDLE)).status == 503
    assert (await client.get(URL, headers={**BUNDLE, "Prefer": "modes=snapshot,delta;wait=30"})).status == 503


async def test_bundle_etag_and_content_type(client, op):
    await op.first_pass([resource("s-aaaaa", "rego", ALLOW_ALL)])
    r = await client.get(URL, headers=BUNDLE)
    assert r.status == 200
    assert r.headers["Content-Type"] == BUNDLE_TYPE == "application/vnd.openpolicyagent.bundles"
    etag = r.headers["ETag"]
    assert etag == op.publisher.etag and etag.startswith('"') and etag.endswith('"')
    assert await r.read() == op.publisher.body
    r = await client.get(URL, headers={**BUNDLE, "If-None-Match": etag})
    assert r.status == 304 and r.headers["ETag"] == etag and await r.read() == b""
    r = await client.get(URL, headers={**BUNDLE, "If-None-Match": '"stale"'})
    assert r.status == 200
    await op.reconcile("s-aaaaa", {"kind": "rego", "source": DENY_ALL}, {})
    r = await client.get(URL, headers={**BUNDLE, "If-None-Match": etag})
    assert r.status == 200 and r.headers["ETag"] != etag


async def test_long_poll_is_held_until_the_bundle_changes(client, op):
    await op.first_pass([resource("s-aaaaa", "rego", ALLOW_ALL)])
    etag = op.publisher.etag
    started = time.monotonic()
    request = asyncio.create_task(client.get(URL, headers={
        **BUNDLE, "If-None-Match": etag, "Prefer": "modes=snapshot,delta;wait=30"}))
    await asyncio.sleep(0.3)
    assert not request.done()
    await op.reconcile("s-bbbbb", {"kind": "rego", "source": DENY_ALL}, {})
    r = await asyncio.wait_for(request, 5)
    assert r.status == 200 and r.headers["ETag"] == op.publisher.etag != etag
    assert r.headers["Content-Type"] == BUNDLE_TYPE
    assert 0.3 <= time.monotonic() - started < 5
    assert await r.read() == op.publisher.body


async def test_long_poll_ends_with_304_after_the_wait(client, op):
    await op.first_pass([])
    etag = op.publisher.etag
    started = time.monotonic()
    r = await client.get(URL, headers={**BUNDLE, "If-None-Match": etag, "Prefer": "modes=snapshot,delta;wait=1"})
    assert r.status == 304 and 0.9 <= time.monotonic() - started < 3


async def test_long_poll_without_a_current_etag_answers_at_once(client, op):
    await op.first_pass([])
    started = time.monotonic()
    r = await client.get(URL, headers={**BUNDLE, "Prefer": "modes=snapshot,delta;wait=30"})
    assert r.status == 200 and time.monotonic() - started < 2
    r = await client.get(URL, headers={**BUNDLE, "If-None-Match": '"old"', "Prefer": "wait=30"})
    assert r.status == 200 and time.monotonic() - started < 2


async def test_many_long_polls_are_all_answered(client, op):
    await op.first_pass([])
    etag = op.publisher.etag
    requests = [asyncio.create_task(client.get(URL, headers={**BUNDLE, "If-None-Match": etag, "Prefer": "wait=30"}))
                for _ in range(2)]
    await asyncio.sleep(0.2)
    await op.reconcile("s-aaaaa", {"kind": "rego", "source": ALLOW_ALL}, {})
    for r in await asyncio.wait_for(asyncio.gather(*requests), 5):
        assert r.status == 200 and r.headers["ETag"] == op.publisher.etag


async def test_validate(client, cfg):
    r = await client.post("/v1/validate", headers=API, json={"kind": "json", "source": example("one-site", "policy.json")})
    assert r.status == 200
    body = await r.json()
    assert body == {"ok": True, "errors": [], "warnings": [], "rego": example("one-site", "rego"),
                    "hash": policy_hash(example("one-site", "rego"))}


async def test_validate_an_invalid_policy_is_a_200(client):
    r = await client.post("/v1/validate", headers=API, json={"kind": "rego", "source": H + "allow_tool_call if data.x\n"})
    assert r.status == 200
    assert await r.json() == {"ok": False, "warnings": [], "errors": [
        {"row": 3, "col": 20, "code": "policy_guard_error", "message": "a policy must not refer to data"}]}
    r = await client.post("/v1/validate", headers=API, json={"kind": "rego", "source": "# " + "x" * 70000})
    assert r.status == 200 and [e["code"] for e in (await r.json())["errors"]] == ["size_error"]


async def test_validate_is_what_the_reconcile_runs(client, op):
    source = '{"version": 1, "allow": {"operations": ["*"], "rules": [{"operation": "press"}]}}'
    api = await (await client.post("/v1/validate", headers=API, json={"kind": "json", "source": source})).json()
    await op.first_pass([])
    out = await op.reconcile("s-aaaaa", {"kind": "json", "source": source}, {})
    assert out.validation.to_api() == api and api["warnings"][0]["code"] == "rule_shadowed"


@pytest.mark.parametrize("body", [b"not json", b"[]", b'{"kind": "yaml", "source": "x"}', b'{"kind": "rego"}',
                                  b'{"kind": "rego", "source": ""}', b'{"kind": "rego", "source": 1}', b""])
async def test_validate_bad_request_is_400_with_an_error(client, body):
    for path in ("/v1/validate", "/v1/evaluate"):
        r = await client.post(path, headers=API, data=body)
        assert r.status == 400 and set(await r.json()) == {"error"}


async def test_validate_body_over_128_kib_is_413(client):
    r = await client.post("/v1/validate", headers=API, data=json.dumps({"kind": "rego", "source": "x" * (128 * 1024)}))
    assert r.status == 413
    r = await client.post("/v1/validate", headers=API, data=b"x" * (3 * 1024 * 1024))
    assert r.status == 413


async def test_evaluate(client):
    call = {"operation": "mcp_call_tool", "server": "browser", "tool": "browser_execute",
            "arguments": {"operations": [{"type": "navigate", "params": {"url": "https://example.com/"}}]}}
    body = {"kind": "json", "source": example("one-site", "policy.json"), "input": call}
    r = await client.post("/v1/evaluate", headers=API, json=body)
    assert r.status == 200 and await r.json() == {"ok": True, "allow": True, "errors": []}
    call["arguments"]["operations"][0]["params"]["url"] = "https://evil.example/"
    r = await client.post("/v1/evaluate", headers=API, json=body)
    assert await r.json() == {"ok": True, "allow": False, "errors": []}
    r = await client.post("/v1/evaluate", headers=API, json={"kind": "rego", "source": "package x\n", "input": {}})
    out = await r.json()
    assert r.status == 200 and out["ok"] is False and "allow" not in out and out["errors"]


async def test_evaluate_needs_an_input_of_at_most_1_mib(client):
    r = await client.post("/v1/evaluate", headers=API, json={"kind": "rego", "source": ALLOW_ALL})
    assert r.status == 400
    r = await client.post("/v1/evaluate", headers=API, json={"kind": "rego", "source": ALLOW_ALL, "input": None})
    assert r.status == 200 and (await r.json())["allow"] is True
    r = await client.post("/v1/evaluate", headers=API, json={"kind": "rego", "source": ALLOW_ALL, "input": "x" * (1024 * 1024)})
    assert r.status == 400
    r = await client.post("/v1/evaluate", headers=API, json={"kind": "rego", "source": ALLOW_ALL, "input": "x" * (1000 * 1000)})
    assert r.status == 200


async def test_schema(client, cfg):
    r = await client.get("/v1/schema", headers=API)
    assert r.status == 200 and r.headers["Content-Type"] == "application/schema+json"
    assert await r.read() == cfg.schema.read_bytes()


async def test_closing_lets_go_of_held_requests(client, op):
    await op.first_pass([])
    etag = op.publisher.etag
    request = asyncio.create_task(client.get(URL, headers={**BUNDLE, "If-None-Match": etag, "Prefer": "wait=30"}))
    await asyncio.sleep(0.2)
    op.publisher.close()
    r = await asyncio.wait_for(request, 5)
    assert r.status == 304
