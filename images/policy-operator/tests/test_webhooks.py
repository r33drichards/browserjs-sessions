import asyncio
import dataclasses
import gzip
import hashlib
import hmac
import json
from unittest.mock import AsyncMock

import pytest

from policy_operator.operator import Operator
from policy_operator.server import make_app
from policy_operator.webhooks import Webhooks, configuration, PublicResolver
from policy_operator.outbox import Outbox
from conftest import ALLOW_ALL, H, resource

SID = "s-aaaaa"
DEST = {"url": "https://example.com/hook", "batch_size": 2, "flush_interval_seconds": 1}


def event(i, tool="exec"):
    return {"id": str(i), "session_id": SID, "type": "tool_call", "stage": "request", "tool": tool}


async def drained(hooks, seconds=3):
    async def wait():
        while hooks.outbox.groups():
            await asyncio.sleep(0.01)
    await asyncio.wait_for(wait(), seconds)


@pytest.mark.parametrize("doc", [
    {"url": "http://example.com"}, {"url": "https://127.0.0.1"}, {"url": "https://[::1]"},
    {"url": "https://169.254.169.254"}, {"url": "https://user:password@example.com"},
    {"url": "https://example.com:8443"}, {**DEST, "batch_size": 0}, {**DEST, "batch_size": True},
    {**DEST, "flush_interval_seconds": 61}, {**DEST, "signing_secret": "short"},
    {**DEST, "filter": 42}, {**DEST, "unknown": 1},
])
def test_invalid_settings(doc):
    with pytest.raises(ValueError):
        configuration(doc)


async def test_dns_blocks_mixed_public_and_private_answers(monkeypatch):
    from aiohttp.resolver import DefaultResolver
    monkeypatch.setattr(DefaultResolver, "resolve", AsyncMock(return_value=[{"host": "8.8.8.8"}, {"host": "10.0.0.1"}]))
    resolver = PublicResolver()
    with pytest.raises(OSError):
        await resolver.resolve("example.com", 443)
    await resolver.close()


async def test_filter_validation_and_isolation(cfg):
    hooks = Webhooks(cfg)
    for source in ["package wrong\nallow_tool_call := true", H + "allow_tool_call := data.secret", H + 'allow_tool_call := http.send({"url":"https://example.com"})']:
        _, result = await hooks.validate({**DEST, "filter": source})
        assert not result["ok"]
    _, result = await hooks.validate({**DEST, "filter": H + 'allow_tool_call if input.tool == "exec"'})
    assert result["ok"]
    await hooks.close()


async def test_batch_filter_duplicates_and_session_isolation(cfg):
    hooks = Webhooks(cfg)
    await hooks.configure(SID, {**DEST, "filter": H + 'allow_tool_call if input.tool == "exec"'})
    hooks._send = AsyncMock(return_value=True)
    assert hooks.ingest([event(1), event(1), event(2, "browser_execute"), {**event(3), "session_id": "s-bbbbb"}])
    await drained(hooks)
    sent = json.loads(hooks._send.await_args.args[1])
    assert [e["id"] for e in sent["events"]] == ["1"]
    assert hooks.ingest([event(1)]) and not hooks.outbox.groups()
    await hooks.close()


async def test_partial_batch_and_retries_keep_identical_body(cfg):
    hooks = Webhooks(cfg)
    await hooks.configure(SID, DEST)
    hooks._send = AsyncMock(side_effect=[False, True])
    assert hooks.ingest([event(1)])
    await drained(hooks, 4)
    first, second = hooks._send.await_args_list
    assert first.args[1:] == second.args[1:]
    await hooks.close()


async def test_queue_pressure_is_atomic(cfg, monkeypatch):
    import policy_operator.outbox as module
    hooks = Webhooks(cfg)
    await hooks.configure(SID, DEST)
    monkeypatch.setattr(module, "MAX_PENDING_BYTES", 1)
    assert not hooks.ingest([event(1), event(2)])
    assert not hooks.outbox.groups()
    assert not hooks.outbox.db.hlen(hooks.outbox.key("receipts"))
    await hooks.close()


async def test_first_pass_restores_settings(cfg):
    op = Operator(cfg)
    body = resource(SID, "rego", ALLOW_ALL)
    body["spec"]["webhook"] = DEST
    await op.first_pass([body])
    assert op.webhooks.settings[SID]["url"] == DEST["url"]
    await op.close()


async def test_opa_gzip_ingestion_auth_and_denied_decision(cfg, aiohttp_client):
    op = Operator(cfg)
    op.ready = True
    await op.webhooks.configure(SID, DEST)
    client = await aiohttp_client(make_app(op))
    raw = {"decision_id": "decision-1", "path": f"browserjs/decision/{SID}/mcp_tools",
           "timestamp": "2026-10-03T12:00:00Z", "input": {"operation": "mcp_call_tool", "server": "exec", "tool": "exec", "arguments": {}},
           "result": {"allow": False}}
    headers = {"Content-Encoding": "gzip", "Content-Type": "application/json"}
    body = gzip.compress(json.dumps([raw, {"path": "browserjs/loaded"}]).encode())
    assert (await client.post("/logs", data=body, headers=headers)).status == 401
    assert (await client.post("/logs", data=body, headers={**headers, "Authorization": "Bearer api-secret"})).status == 401
    assert (await client.post("/logs", data=body, headers={**headers, "Authorization": "Bearer bundle-secret"})).status == 204
    group = op.webhooks.outbox.groups()[0]
    assert op.webhooks.outbox.events(group, 10, 100000)[0][1]["allowed"] is False
    await op.close()


async def test_signature_and_redirect_refusal(cfg):
    hooks = Webhooks(cfg)
    captured = {}
    class Response:
        status = 302
        async def __aenter__(self): return self
        async def __aexit__(self, *args): pass
    class HTTP:
        def post(self, url, **kwargs):
            captured.update(kwargs)
            return Response()
    hooks.http = HTTP()
    settings = configuration({**DEST, "signing_secret": "secret-of-sixteen-bytes"})
    assert not await hooks._send(settings, b'{"events":[]}', "batch-1")
    assert captured["allow_redirects"] is False
    headers = captured["headers"]
    digest = hmac.new(settings["signing_secret"].encode(), headers["X-Computer-Use-Timestamp"].encode() + b"." + captured["data"], hashlib.sha256).hexdigest()
    assert headers["X-Computer-Use-Signature"] == "sha256=" + digest
    hooks.http = None
    await hooks.close()


async def test_full_batch_wakes_partial_batch_timer(cfg):
    hooks = Webhooks(cfg)
    await hooks.configure(SID, {**DEST, "flush_interval_seconds": 60})
    hooks._send = AsyncMock(return_value=True)
    assert hooks.ingest([event(1)])
    await asyncio.sleep(0.01)
    assert hooks.ingest([event(2)])
    await drained(hooks, 1)
    hooks._send.assert_awaited_once()
    await hooks.close()


async def test_recovery_without_current_configuration_and_durable_duplicates(cfg, tmp_path):
    hooks = Webhooks(cfg)
    await hooks.configure(SID, {**DEST, "batch_size": 1})
    # Crash after persistence, before the worker runs.
    assert hooks.ingest([event(1)])
    await hooks.close()
    recovered = Webhooks(cfg)
    recovered._send = AsyncMock(return_value=True)
    recovered.start()
    await drained(recovered)
    assert json.loads(recovered._send.await_args.args[1])["events"][0]["id"] == "1"
    await recovered.configure(SID, {**DEST, "batch_size": 1})
    assert recovered.ingest([event(1)]) and not recovered.outbox.groups()
    await recovered.close()


async def test_crash_after_receiver_accepts_replays_same_batch(cfg, tmp_path):
    hooks = Webhooks(cfg)
    await hooks.configure(SID, {**DEST, "batch_size": 1})
    observed = asyncio.Event()
    payloads = []
    async def lost_ack(settings, body, bid):
        payloads.append((body, bid))
        observed.set()
        await asyncio.Future()  # process dies before observing acknowledgement
    hooks._send = lost_ack
    assert hooks.ingest([event(1)])
    await asyncio.wait_for(observed.wait(), 1)
    await hooks.close()
    recovered = Webhooks(cfg)
    recovered._send = AsyncMock(return_value=True)
    recovered.start()
    await drained(recovered)
    assert recovered._send.await_args.args[1:] == payloads[0]
    await recovered.close()


async def test_configuration_change_and_disable_preserve_original_destination(cfg):
    hooks = Webhooks(cfg)
    await hooks.configure(SID, {**DEST, "batch_size": 1})
    assert hooks.ingest([event(1)])
    await hooks.configure(SID, {**DEST, "url": "https://new.example.com", "batch_size": 1})
    assert hooks.ingest([event(2)])
    await hooks.remove(SID)
    hooks._send = AsyncMock(return_value=True)
    await drained(hooks)
    assert {args.args[0]["url"] for args in hooks._send.await_args_list} == {DEST["url"], "https://new.example.com"}
    await hooks.close()


async def test_filter_error_retains_events_for_retry(cfg, monkeypatch):
    hooks = Webhooks(cfg)
    await hooks.configure(SID, {**DEST, "batch_size": 1, "filter": H + "allow_tool_call := true"})
    import policy_operator.webhooks as module
    monkeypatch.setattr(module.opa, "eval_many", lambda *args: None)
    hooks._send = AsyncMock(return_value=True)
    assert hooks.ingest([event(1)])
    await asyncio.sleep(0.05)
    assert hooks.outbox.groups()
    hooks._send.assert_not_called()
    await hooks.close()


def test_outbox_retries_survive_redis_crash(cfg, redis_server):
    box = Outbox(cfg.webhook_redis_url, cfg.webhook_redis_prefix)
    assert box.ingest([(event(1), configuration(DEST))])
    group = box.groups()[0]
    box.prepare(group, box.events(group, 2, 10000), [True])
    bid, payload, _, _ = box.batch(group)
    box.retry(bid, 100000, 123)
    box.close()
    redis_server.crash()
    redis_server.start()
    recovered = Outbox(cfg.webhook_redis_url, cfg.webhook_redis_prefix)
    assert recovered.batch(group) == (bid, payload, 100000, 123)
    recovered.acknowledge(bid)
    assert not recovered.groups()
    recovered.close()


def test_nondurable_redis_is_rejected(cfg):
    import redis
    client = redis.Redis.from_url(cfg.webhook_redis_url)
    client.config_set('appendfsync', 'everysec')
    try:
        with pytest.raises(RuntimeError, match='appendfsync always'):
            Outbox(cfg.webhook_redis_url, cfg.webhook_redis_prefix)
    finally:
        client.config_set('appendfsync', 'always')


async def test_config_version_mismatch_is_not_acknowledged(cfg, aiohttp_client):
    op = Operator(cfg)
    op.ready = True
    await op.webhooks.configure(SID, DEST)
    client = await aiohttp_client(make_app(op))
    body = {"events": [event(1)], "webhook": {**DEST, "url": "https://new.example.com"}}
    response = await client.post("/v1/tool-events", json=body, headers={"Authorization": "Bearer api-secret"})
    assert response.status == 503
    assert not op.webhooks.outbox.groups()
    body["webhook"] = DEST
    response = await client.post("/v1/tool-events", json=body, headers={"Authorization": "Bearer api-secret"})
    assert response.status == 204
    assert op.webhooks.outbox.groups()
    await op.close()


async def test_native_pre_hook_never_allows_when_durable_commit_fails(cfg, aiohttp_client, monkeypatch):
    import redis
    op = Operator(cfg)
    op.ready = True
    await op.webhooks.configure(SID, DEST)
    def fail(events): raise redis.RedisError("Redis unavailable")
    monkeypatch.setattr(op.webhooks.outbox, "ingest", fail)
    client = await aiohttp_client(make_app(op))
    response = await client.post(f"/v1/data/browserjs/hooks/{SID}/mcp_tools/pre", json={"input": {"operation": "mcp_call_tool", "server": "exec", "tool": "exec", "arguments": {}}})
    assert response.status == 503
    assert "result" not in await response.json()
    await op.close()


async def test_native_pre_hook_persists_attempt_without_authorizing(cfg, aiohttp_client):
    op = Operator(cfg)
    op.ready = True
    await op.webhooks.configure(SID, DEST)
    client = await aiohttp_client(make_app(op))
    response = await client.post(f"/v1/data/browserjs/hooks/{SID}/mcp_tools/pre", json={"input": {"operation": "mcp_call_tool", "server": "exec", "tool": "exec", "arguments": {"bin": "ls"}}})
    assert response.status == 200 and await response.json() == {"result": True}
    group = op.webhooks.outbox.groups()[0]
    recorded = op.webhooks.outbox.events(group, 1, 10000)[0][1]
    assert recorded["stage"] == "attempt" and recorded["arguments"] == {"bin": "ls"}
    assert "allowed" not in recorded
    # The removed gateway route is not an alternate authorization path.
    assert (await client.post(f"/v1/data/browserjs/decision/{SID}/mcp_tools", json={})).status == 404
    await op.close()


async def test_native_pre_hook_unconfigured_session_needs_no_redis_write(cfg, aiohttp_client, monkeypatch):
    op = Operator(cfg)
    op.ready = True
    monkeypatch.setattr(op.webhooks.outbox, "ingest", lambda _: pytest.fail("unconfigured session wrote to Redis"))
    client = await aiohttp_client(make_app(op))
    response = await client.post(f"/v1/data/browserjs/hooks/{SID}/mcp_tools/pre", json={"input": {"operation": "mcp_call_tool"}})
    assert response.status == 200 and await response.json() == {"result": True}
    await op.close()


async def test_reconciliation_never_leaves_an_unrecorded_execution_window(cfg, monkeypatch):
    hooks = Webhooks(cfg)
    await hooks.configure(SID, DEST)
    entered, resume = asyncio.Event(), asyncio.Event()
    original = hooks.validate
    async def validate(doc):
        entered.set()
        await resume.wait()
        return await original(doc)
    monkeypatch.setattr(hooks, "validate", validate)
    update = asyncio.create_task(hooks.configure(SID, {**DEST, "url": "https://new.example.com"}))
    await entered.wait()
    assert not hooks.ingest([event(1)])
    assert not hooks.outbox.groups()
    resume.set()
    await update
    assert hooks.ingest([event(1)])
    await hooks.close()


async def test_invalid_direct_configuration_refuses_new_calls(cfg):
    hooks = Webhooks(cfg)
    await hooks.configure(SID, DEST)
    await hooks.configure(SID, {**DEST, "filter": "package INVALID"})
    assert not hooks.ingest([event(1)])
    await hooks.configure(SID, DEST)
    assert hooks.ingest([event(1)])
    await hooks.close()
