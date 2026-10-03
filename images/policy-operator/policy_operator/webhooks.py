"""Session tool-call exports with a durable at-least-once outbox.

The same tenant guard and capabilities as enforcement apply to filters. A
filter uses browserjs.policy.allow_tool_call; its input is the export event.
"""
from __future__ import annotations

import asyncio
import hashlib
import hmac
import ipaddress
import json
import logging
import time
import uuid
import redis

import aiohttp
from aiohttp.resolver import DefaultResolver
from yarl import URL

from . import opa
from .check import check, SESSION_ID
from .config import Config, EVAL_DEADLINE_SECONDS
from .outbox import Outbox

log = logging.getLogger("policy_operator.webhooks")
MAX_BATCH_BYTES = 2 * 1024 * 1024


def configuration(doc: object) -> dict:
    if not isinstance(doc, dict) or set(doc) - {"url", "filter", "batch_size", "flush_interval_seconds", "signing_secret"}:
        raise ValueError("webhook must be an object with url, filter, batch_size, flush_interval_seconds and signing_secret")
    url = doc.get("url")
    if not isinstance(url, str) or len(url) > 2048:
        raise ValueError("url must be an HTTPS URL")
    parsed = URL(url)
    if parsed.scheme != "https" or not parsed.host or parsed.user is not None or parsed.fragment or parsed.port != 443:
        raise ValueError("url must be HTTPS on port 443, without credentials or a fragment")
    try:
        address = ipaddress.ip_address(parsed.host)
    except ValueError:
        pass
    else:
        if not address.is_global:
            raise ValueError("url must use a public address")
    source = doc.get("filter", "")
    secret = doc.get("signing_secret", "")
    if not isinstance(source, str) or len(source.encode()) > 65536:
        raise ValueError("filter must be a string of at most 65536 bytes")
    if not isinstance(secret, str) or len(secret.encode()) > 256 or (secret and len(secret.encode()) < 16):
        raise ValueError("signing_secret must be empty or 16 to 256 bytes")
    size = doc.get("batch_size", 100)
    interval = doc.get("flush_interval_seconds", 5)
    if type(size) is not int or not 1 <= size <= 500:
        raise ValueError("batch_size must be between 1 and 500")
    if type(interval) is not int or not 1 <= interval <= 60:
        raise ValueError("flush_interval_seconds must be between 1 and 60")
    return {"url": str(parsed), "filter": source, "batch_size": size,
            "flush_interval_seconds": interval, "signing_secret": secret}


class PublicResolver(DefaultResolver):
    """Validate the actual DNS answers used to connect, on every resolution."""
    async def resolve(self, host, port=0, family=0):
        answers = await super().resolve(host, port, family)
        if not answers or any(not ipaddress.ip_address(a["host"]).is_global for a in answers):
            raise OSError("webhook DNS resolved to a non-public address")
        return answers


class Webhooks:
    def __init__(self, cfg: Config, offline: bool = False):
        self.cfg = cfg
        self.settings: dict[str, dict] = {}
        self.blocked: set[str] = set()
        self.outbox = None if offline else Outbox(cfg.webhook_redis_url, cfg.webhook_redis_prefix, cfg.webhook_redis_password)
        self.tasks: dict[str, asyncio.Task] = {}
        self.wake: dict[str, asyncio.Event] = {}
        self.http: aiohttp.ClientSession | None = None
        self.closed = False
        self.evaluations = asyncio.Semaphore(4)
        self.supervisor: asyncio.Task | None = None

    def start(self) -> None:
        if self.outbox is None:
            return
        if self.supervisor is None and not self.closed:
            self.supervisor = asyncio.create_task(self._supervise())
        self._schedule()

    def _schedule(self) -> None:
        for group in self.outbox.groups():
            wake = self.wake.setdefault(group, asyncio.Event())
            _, cfg = self.outbox.configuration(group)
            if self.outbox.count(group) >= cfg["batch_size"]:
                wake.set()
            if group not in self.tasks:
                self.tasks[group] = asyncio.create_task(self._worker(group))

    async def _supervise(self) -> None:
        while not self.closed:
            await asyncio.sleep(1)
            try:
                self._schedule()
            except redis.RedisError:
                log.exception("webhook outbox unavailable; accepted events retained")

    async def validate(self, doc: object) -> tuple[dict, dict]:
        cleaned = configuration(doc)
        if cleaned["filter"]:
            async with self.evaluations:
                v = await asyncio.to_thread(check, self.cfg, "rego", cleaned["filter"], None, False)
            return cleaned, v.to_api()
        return cleaned, {"ok": True, "errors": [], "warnings": []}

    async def configure(self, sid: str, doc: object) -> None:
        if doc is None:
            await self.remove(sid)
            return
        try:
            cleaned = configuration(doc)
        except ValueError as e:
            self.blocked.add(sid)
            log.warning("invalid webhook configuration", extra={"session": sid, "reason": str(e)})
            return
        if self.settings.get(sid) == cleaned:
            self.blocked.discard(sid)
            return
        # Capture must not see a transient absent configuration while its
        # replacement is being checked. Refuse new calls during validation.
        self.blocked.add(sid)
        cleaned, validation = await self.validate(cleaned)
        if not validation["ok"]:
            log.warning("invalid webhook filter", extra={"session": sid})
            return
        self.settings[sid] = cleaned
        self.blocked.discard(sid)

    async def remove(self, sid: str) -> None:
        # Disable new capture. Previously accepted events retain their original
        # destination and signing secret and continue until acknowledged.
        self.settings.pop(sid, None)
        self.blocked.discard(sid)

    def ingest(self, events: list[dict]) -> bool:
        if self.closed:
            return False
        pending = []
        for event in events:
            sid, eid = event.get("session_id"), event.get("id")
            if not isinstance(sid, str) or not SESSION_ID.fullmatch(sid):
                return False
            if not isinstance(eid, str) or not eid:
                return False
            if sid in self.blocked:
                return False
            if sid not in self.settings:
                continue
            # Keep the call itself, even when its arguments exceed the delivery
            # limit. This is an explicit event field, never a silent drop.
            if len(json.dumps(event, separators=(",", ":")).encode()) > MAX_BATCH_BYTES - 1024:
                event = {k: v for k, v in event.items() if k != "arguments"}
                event["arguments_truncated"] = True
                if len(json.dumps(event).encode()) > MAX_BATCH_BYTES - 1024:
                    return False
            pending.append((event, self.settings[sid]))
        try:
            accepted = self.outbox.ingest(pending)
            if accepted:
                self.start()
            return accepted
        except redis.RedisError:
            log.exception("webhook outbox write failed; ingestion not acknowledged")
            return False

    async def _send(self, cfg: dict, body: bytes, batch_id: str) -> bool:
        if self.http is None:
            self.http = aiohttp.ClientSession(
                connector=aiohttp.TCPConnector(resolver=PublicResolver(), use_dns_cache=False, limit=16),
                timeout=aiohttp.ClientTimeout(total=10), trust_env=False,
            )
        headers = {"Content-Type": "application/json", "X-Computer-Use-Batch-ID": batch_id}
        if cfg["signing_secret"]:
            stamp = str(int(time.time()))
            signature = hmac.new(cfg["signing_secret"].encode(), stamp.encode() + b"." + body, hashlib.sha256).hexdigest()
            headers.update({"X-Computer-Use-Timestamp": stamp, "X-Computer-Use-Signature": "sha256=" + signature})
        try:
            async with self.http.post(cfg["url"], data=body, headers=headers, allow_redirects=False) as response:
                return 200 <= response.status < 300
        except (aiohttp.ClientError, asyncio.TimeoutError, OSError):
            return False

    async def _worker(self, group: str) -> None:
        sid, cfg = self.outbox.configuration(group)
        try:
            while group in self.outbox.groups() and not self.closed:
                batch = self.outbox.batch(group)
                if batch is None:
                    wake = self.wake[group]
                    if self.outbox.count(group) < cfg["batch_size"]:
                        try:
                            await asyncio.wait_for(wake.wait(), cfg["flush_interval_seconds"])
                        except asyncio.TimeoutError:
                            pass
                    wake.clear()
                    rows = self.outbox.events(group, cfg["batch_size"], MAX_BATCH_BYTES - 1024)
                    events = [event for _, event in rows]
                    mask = [True] * len(events)
                    if cfg["filter"]:
                        try:
                            async with self.evaluations:
                                mask = await asyncio.to_thread(opa.eval_many, self.cfg.opa_bin, self.cfg.capabilities,
                                                               cfg["filter"], events, EVAL_DEADLINE_SECONDS)
                        except opa.OpaTimeout:
                            mask = None
                        if mask is None:
                            log.warning("webhook filter evaluation failed; events retained for retry", extra={"session": sid})
                            await asyncio.sleep(5)
                            continue
                    self.outbox.prepare(group, rows, mask)
                    batch = self.outbox.batch(group)
                    if batch is None:
                        continue
                bid, body, attempts, next_attempt = batch
                delay = next_attempt - time.time()
                if delay > 0:
                    await asyncio.sleep(delay)
                self.outbox.confirm()
                if await self._send(cfg, body, bid):
                    # A crash before this commit replays the identical batch.
                    self.outbox.acknowledge(bid)
                else:
                    attempts += 1
                    self.outbox.retry(bid, attempts, time.time() + min(2 ** min(attempts - 1, 9), 300))
                    log.warning("webhook delivery failed; retry scheduled", extra={"session": sid, "batch_id": bid, "attempts": attempts})
        except redis.RedisError:
            log.exception("webhook outbox unavailable; supervisor will retry")
        finally:
            self.tasks.pop(group, None)

    async def close(self) -> None:
        self.closed = True
        if self.supervisor:
            self.supervisor.cancel()
        tasks = list(self.tasks.values())
        for task in tasks:
            task.cancel()
        await asyncio.gather(*tasks, *([self.supervisor] if self.supervisor else []), return_exceptions=True)
        if self.http:
            await self.http.close()
        self.outbox.close() if self.outbox else None


def decision_event(doc: object) -> dict | None:
    if not isinstance(doc, dict):
        return None
    path = doc.get("path", "")
    if not isinstance(path, str):
        return None
    parts = path.strip("/").split("/")
    if len(parts) != 4 or parts[:2] != ["browserjs", "decision"] or parts[3] != "mcp_tools":
        return None
    args = doc.get("input")
    if not isinstance(args, dict) or args.get("operation") != "mcp_call_tool":
        return None
    return {"id": doc.get("decision_id"), "session_id": parts[2], "timestamp": doc.get("timestamp"),
            "type": "tool_call", "stage": "authorization", "server": args.get("server"),
            "tool": args.get("tool"), "arguments": args.get("arguments"),
            "allowed": isinstance(doc.get("result"), dict) and doc["result"].get("allow") is True}
