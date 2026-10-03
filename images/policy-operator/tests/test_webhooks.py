import asyncio
import gzip
import hashlib
import hmac
import json
from unittest.mock import AsyncMock

import pytest

from policy_operator.operator import Operator
from policy_operator.server import make_app
from policy_operator.webhooks import Webhooks, configuration, decision_event, PublicResolver
from conftest import ALLOW_ALL, H, resource

SID = "s-aaaaa"
DEST = {"url": "https://example.com/hook", "batch_size": 2, "flush_interval_seconds": 1}


def event(i, tool="exec"):
    return {"id": str(i), "session_id": SID, "type": "tool_call", "stage": "request", "tool": tool}


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
    sent = []
    async def send(settings, body, bid):
        sent.append(json.loads(body))
        return True
    hooks._send = send
    assert hooks.ingest([event(1), event(1), event(2, "browser_execute"), {**event(3), "session_id": "s-bbbbb"}])
    task = hooks.tasks[SID]
    await asyncio.wait_for(task, 3)
    assert len(sent) == 1
    assert [e["id"] for e in sent[0]["events"]] == ["1"]
    assert hooks.bytes == 0
    assert hooks.ingest([event(1)]) and not hooks.tasks
    await hooks.close()


async def test_partial_batch_and_retries_keep_identical_body(cfg, monkeypatch):
    hooks = Webhooks(cfg)
    await hooks.configure(SID, DEST)
    sent = []
    async def send(settings, body, bid):
        sent.append((body, bid))
        return len(sent) == 2
    hooks._send = send
    assert hooks.ingest([event(1)])
    await asyncio.wait_for(hooks.tasks[SID], 4)
    assert sent[0] == sent[1]
    assert hooks.bytes == 0
    await hooks.close()


async def test_queue_pressure_is_atomic_and_config_change_discards(cfg, monkeypatch):
    import policy_operator.webhooks as module
    hooks = Webhooks(cfg)
    await hooks.configure(SID, DEST)
    monkeypatch.setattr(module, "MAX_QUEUE_BYTES", 1)
    assert not hooks.ingest([event(1), event(2)])
    assert not hooks.seen and hooks.bytes == 0
    monkeypatch.setattr(module, "MAX_QUEUE_BYTES", 10000)
    assert hooks.ingest([event(1)])
    await hooks.configure(SID, {**DEST, "url": "https://other.example.com"})
    assert hooks.bytes == 0 and not hooks.tasks and not hooks.queues
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
    assert op.webhooks.queues[SID][0].event["allowed"] is False
    assert len(op.webhooks.queues[SID]) == 1
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


async def test_unchanged_settings_keep_queued_events(cfg):
    hooks = Webhooks(cfg)
    await hooks.configure(SID, DEST)
    assert hooks.ingest([event(1)])
    task = hooks.tasks[SID]
    await hooks.configure(SID, DEST)  # omitted defaults normalize identically
    assert hooks.tasks[SID] is task and hooks.bytes > 0
    await hooks.close()


async def test_filter_runtime_failure_omits_events(cfg):
    hooks = Webhooks(cfg)
    await hooks.configure(SID, {**DEST, "batch_size": 1,
                                "filter": H + 'allow_tool_call := {"not": "a boolean"}'})
    hooks._send = AsyncMock(return_value=True)
    assert hooks.ingest([event(1)])
    await hooks.tasks[SID]
    hooks._send.assert_not_called()
    assert hooks.bytes == 0
    await hooks.close()


async def test_full_batch_wakes_partial_batch_timer(cfg):
    hooks = Webhooks(cfg)
    await hooks.configure(SID, {**DEST, "flush_interval_seconds": 60})
    hooks._send = AsyncMock(return_value=True)
    assert hooks.ingest([event(1)])
    task = hooks.tasks[SID]
    await asyncio.sleep(0.01)  # the worker is waiting for a partial-batch timeout
    assert hooks.ingest([event(2)])
    await asyncio.wait_for(task, 1)
    hooks._send.assert_awaited_once()
    assert hooks.bytes == 0
    await hooks.close()
