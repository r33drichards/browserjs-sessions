"""The operator's HTTP API: docs/contracts/policy/operator-api.yaml."""
from __future__ import annotations

import asyncio
import hmac
import json
import uuid
from datetime import datetime, timezone

import aiohttp

from aiohttp import web

from .check import check, evaluate
from .config import MAX_EVALUATE_BODY, MAX_INPUT_BYTES, MAX_LONG_POLL_SECONDS, MAX_VALIDATE_BODY
from .operator import Operator

BUNDLE_TYPE = "application/vnd.openpolicyagent.bundles"
OPERATOR = web.AppKey("operator", Operator)


def _authorized(request: web.Request, token: str) -> bool:
    # An unset token admits nobody.
    if not token:
        return False
    given = request.headers.get("Authorization", "")
    return hmac.compare_digest(given.encode(), f"Bearer {token}".encode())


def _unauthorized() -> web.Response:
    return web.Response(status=401)


def _error(message: str, status: int = 400) -> web.Response:
    return web.json_response({"error": message}, status=status)


def _wait_seconds(prefer: str) -> float:
    """N of `Prefer: modes=snapshot,delta;wait=N`; 0 when there is none."""
    for part in prefer.replace(",", ";").split(";"):
        name, _, value = part.strip().partition("=")
        if name == "wait":
            try:
                return max(0.0, min(float(value), MAX_LONG_POLL_SECONDS))
            except ValueError:
                return 0.0
    return 0.0


async def healthz(request: web.Request) -> web.Response:
    return web.Response(text="ok\n")


async def readyz(request: web.Request) -> web.Response:
    op = request.app[OPERATOR]
    if op.ready and op.publisher.etag:
        return web.Response(text="ok\n")
    return web.Response(status=503, text="the first pass over the existing policies is not complete\n")


async def bundle(request: web.Request) -> web.Response:
    op = request.app[OPERATOR]
    if not _authorized(request, op.cfg.bundle_token):
        return _unauthorized()
    pub = op.publisher
    if not op.ready or pub.etag is None:
        return web.Response(status=503)
    known = request.headers.get("If-None-Match", "").strip()
    if known == pub.etag:
        wait = _wait_seconds(request.headers.get("Prefer", ""))
        if wait:
            await pub.wait_change(known, wait)
    # One read of the pair: a publish between two reads must not mix them.
    etag, body = pub.etag, pub.body
    if known == etag:
        return web.Response(status=304, headers={"ETag": etag})
    return web.Response(body=body, headers={"ETag": etag, "Content-Type": BUNDLE_TYPE})


async def _policy_source(request: web.Request, limit: int):
    """(document, None) or (None, the response that refuses the request)."""
    try:
        raw = await request.read()
    except web.HTTPRequestEntityTooLarge:
        return None, web.Response(status=413)
    if len(raw) > limit:
        return None, web.Response(status=413)
    try:
        doc = json.loads(raw)
    except (ValueError, RecursionError):
        return None, _error("the body is not JSON")
    if not isinstance(doc, dict):
        return None, _error("the body must be a JSON object")
    # Rego is the only kind there is; the field may be left out.
    if doc.setdefault("kind", "rego") != "rego":
        return None, _error("kind must be rego")
    if not isinstance(doc.get("source"), str) or not doc["source"]:
        return None, _error("source must be a string that is not empty")
    return doc, None


async def validate(request: web.Request) -> web.Response:
    op = request.app[OPERATOR]
    if not _authorized(request, op.cfg.api_token):
        return _unauthorized()
    doc, refusal = await _policy_source(request, MAX_VALIDATE_BODY)
    if refusal:
        return refusal
    v = await asyncio.to_thread(check, op.cfg, doc["kind"], doc["source"])
    return web.json_response(v.to_api())


async def evaluate_(request: web.Request) -> web.Response:
    op = request.app[OPERATOR]
    if not _authorized(request, op.cfg.api_token):
        return _unauthorized()
    doc, refusal = await _policy_source(request, MAX_EVALUATE_BODY)
    if refusal:
        return refusal
    if "input" not in doc:
        return _error("input is required")
    if len(json.dumps(doc["input"]).encode()) > MAX_INPUT_BYTES:
        return _error("input is larger than 1 MiB")
    out = await asyncio.to_thread(evaluate, op.cfg, doc["kind"], doc["source"], doc["input"])
    return web.json_response(out)


async def validate_webhook(request: web.Request) -> web.Response:
    op = request.app[OPERATOR]
    if not _authorized(request, op.cfg.api_token):
        return _unauthorized()
    try:
        doc = await request.json()
        _, verdict = await op.webhooks.validate(doc)
    except (ValueError, TypeError):
        return _error("invalid webhook configuration")
    return web.json_response(verdict)


async def tool_events(request: web.Request) -> web.Response:
    op = request.app[OPERATOR]
    decisions = request.path == "/logs"
    if not _authorized(request, op.cfg.bundle_token if decisions else op.cfg.api_token):
        return _unauthorized()
    if not op.ready:
        return web.Response(status=503)
    try:
        # aiohttp decompresses Content-Encoding: gzip and enforces the
        # application body limit on the decompressed OPA upload.
        doc = await request.json()
    except (ValueError, TypeError):
        return _error("the body is not JSON")
    if not decisions and isinstance(doc, dict):
        from .webhooks import configuration
        try:
            expected = configuration(doc.get("webhook"))
        except ValueError:
            return _error("invalid capture configuration")
        items = doc.get("events")
        if not isinstance(items, list):
            return _error("events must be an array")
        # Wait for reconciliation rather than acknowledging an event under a
        # stale or absent webhook configuration.
        if any(not isinstance(item, dict) or op.webhooks.settings.get(item.get("session_id")) != expected for item in items):
            return _error("webhook configuration is still applying", 503)
        doc = items
    if not isinstance(doc, list) or len(doc) > 10000:
        return _error("expected an array of at most 10000 events")
    if decisions:
        from .webhooks import decision_event
        events = [event for item in doc if (event := decision_event(item)) is not None]
    else:
        events = [item for item in doc if isinstance(item, dict)]
    if not op.webhooks.ingest(events):
        return web.Response(status=503)
    return web.Response(status=204)


async def decision(request: web.Request) -> web.Response:
    """The session-facing OPA endpoint: never allow before durable capture.

    The opa Service routes here; only the operator can reach opa-engine.
    This keeps existing session URLs while removing the lossy log buffer.
    """
    from .check import SESSION_ID
    op = request.app[OPERATOR]
    sid = request.match_info["sid"]
    if not SESSION_ID.fullmatch(sid):
        return web.Response(status=404)
    if request.query:
        return web.Response(status=403)
    if not op.ready:
        return web.Response(status=503)
    try:
        raw = await request.read()
        if len(raw) > MAX_INPUT_BYTES + 1024:
            return web.Response(status=413)
        doc = json.loads(raw)
        args = doc.get("input") if isinstance(doc, dict) else None
        if not isinstance(args, dict) or args.get("operation") != "mcp_call_tool":
            return _error("input must describe an mcp_call_tool")
    except (ValueError, TypeError):
        return _error("the body is not JSON")
    event = {"id": str(uuid.uuid4()), "session_id": sid,
             "timestamp": datetime.now(timezone.utc).isoformat(), "type": "tool_call",
             "stage": "authorization", "server": args.get("server"),
             "tool": args.get("tool"), "arguments": args.get("arguments"), "allowed": False}
    target = op.cfg.opa_decision_url or f"http://{op.cfg.opa_service}.{op.cfg.namespace}.svc:{op.cfg.opa_port}"
    status, answer = 503, None
    try:
        client = await op._session()
        async with client.post(target.rstrip("/") + f"/v1/data/browserjs/decision/{sid}/mcp_tools",
                               json={"input": args}, allow_redirects=False) as response:
            status = response.status
            body = bytearray()
            async for chunk in response.content.iter_chunked(8192):
                body.extend(chunk)
                if len(body) > 65536:
                    raise ValueError("policy response is too large")
            if status == 200:
                answer = json.loads(body)
                result = answer.get("result") if isinstance(answer, dict) else None
                # During rollout, an old engine may still upload asynchronous
                # logs. Use its ID so both capture paths deduplicate durably.
                if isinstance(answer, dict) and isinstance(answer.get("decision_id"), str) and answer["decision_id"]:
                    event["id"] = answer["decision_id"]
                event["allowed"] = isinstance(result, dict) and result.get("allow") is True
            else:
                status = 503
    except (aiohttp.ClientError, asyncio.TimeoutError, ValueError):
        status = 503
    if status != 200:
        event["decision_error"] = "policy engine unavailable"
    # Redis AOF fsync finishes before a successful decision reaches mcp-js.
    if not op.webhooks.ingest([event]):
        return _error("tool event could not be durably recorded", 503)
    if status != 200 or not isinstance(answer, dict):
        return _error("policy engine unavailable", 503)
    return web.json_response(answer)


def make_app(op: Operator) -> web.Application:
    # aiohttp refuses larger bodies itself, with 413.
    app = web.Application(client_max_size=16 * 1024 * 1024)
    app[OPERATOR] = op
    app.router.add_post("/v1/data/browserjs/decision/{sid}/mcp_tools", decision)
    app.router.add_get("/health", readyz)
    app.router.add_get("/healthz", healthz)
    app.router.add_get("/readyz", readyz)
    app.router.add_get("/bundles/browserjs.tar.gz", bundle)
    app.router.add_post("/logs", tool_events)
    app.router.add_post("/v1/tool-events", tool_events)
    app.router.add_post("/v1/webhooks/validate", validate_webhook)
    app.router.add_post("/v1/validate", validate)
    app.router.add_post("/v1/evaluate", evaluate_)
    return app


async def start(op: Operator, host: str = "0.0.0.0") -> web.AppRunner:
    # A replica that goes away mid long poll takes its request with it.
    runner = web.AppRunner(make_app(op), access_log=None, handler_cancellation=True)
    await runner.setup()
    await web.TCPSite(runner, host, op.cfg.http_port).start()
    return runner


async def stop(runner: web.AppRunner, op: Operator) -> None:
    # Held long polls would otherwise keep the shutdown waiting for them.
    op.publisher.close()
    await runner.cleanup()
