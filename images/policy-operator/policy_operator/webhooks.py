"""Session tool-call exports. Bounded, asynchronous, best-effort delivery.

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
from collections import OrderedDict, deque
from dataclasses import dataclass

import aiohttp
from aiohttp.resolver import DefaultResolver
from yarl import URL

from . import opa
from .check import check, SESSION_ID
from .config import Config, EVAL_DEADLINE_SECONDS

log = logging.getLogger("policy_operator.webhooks")
MAX_QUEUE_BYTES = 16 * 1024 * 1024
MAX_BATCH_BYTES = 2 * 1024 * 1024
MAX_ATTEMPTS = 8


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


@dataclass
class Pending:
    event: dict
    size: int


class Webhooks:
    def __init__(self, cfg: Config):
        self.cfg = cfg
        self.settings: dict[str, dict] = {}
        self.queues: dict[str, deque[Pending]] = {}
        self.tasks: dict[str, asyncio.Task] = {}
        self.wake: dict[str, asyncio.Event] = {}
        self.seen: OrderedDict[str, float] = OrderedDict()
        self.bytes = 0
        self.http: aiohttp.ClientSession | None = None
        self.closed = False
        self.evaluations = asyncio.Semaphore(4)

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
            await self.remove(sid)
            log.warning("invalid webhook configuration", extra={"session": sid, "reason": str(e)})
            return
        if self.settings.get(sid) == cleaned:
            return
        # Direct CRD writers must pass the same validation as API clients.
        await self.remove(sid)
        cleaned, validation = await self.validate(cleaned)
        if not validation["ok"]:
            log.warning("invalid webhook filter", extra={"session": sid})
            return
        self.settings[sid] = cleaned

    async def remove(self, sid: str) -> None:
        # Stop admission before awaiting cancellation; ingestion can run while
        # the old worker is shutting down.
        self.settings.pop(sid, None)
        task = self.tasks.pop(sid, None)
        if task:
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)
        for item in self.queues.pop(sid, ()):
            self.bytes -= item.size
        self.wake.pop(sid, None)

    def ingest(self, events: list[dict]) -> bool:
        """Atomic enqueue. False asks OPA to retry; no partial acceptance."""
        if self.closed:
            return False
        now = time.monotonic()
        while self.seen and (next(iter(self.seen.values())) < now - 3600 or len(self.seen) > 50000):
            self.seen.popitem(last=False)
        pending = []
        ids = set()
        for event in events:
            sid = event.get("session_id")
            eid = event.get("id")
            if not isinstance(sid, str) or not SESSION_ID.fullmatch(sid) or sid not in self.settings:
                continue
            if not isinstance(eid, str) or eid in self.seen or eid in ids:
                continue
            size = len(json.dumps(event, separators=(",", ":")).encode())
            if size > MAX_BATCH_BYTES - 1024:
                # An individual event must fit one delivery.
                log.warning("webhook event exceeds batch byte limit", extra={"session": sid})
                continue
            pending.append((sid, eid, Pending(event, size)))
            ids.add(eid)
        if self.bytes + sum(p.size for _, _, p in pending) > MAX_QUEUE_BYTES:
            return False
        for sid, eid, item in pending:
            self.seen[eid] = now
            self.queues.setdefault(sid, deque()).append(item)
            self.bytes += item.size
            wake = self.wake.setdefault(sid, asyncio.Event())
            if len(self.queues[sid]) >= self.settings[sid]["batch_size"]:
                wake.set()
            if sid not in self.tasks:
                self.tasks[sid] = asyncio.create_task(self._worker(sid))
        return True

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

    async def _worker(self, sid: str) -> None:
        cfg = self.settings[sid]
        try:
            while self.queues.get(sid):
                queue = self.queues[sid]
                wake = self.wake[sid]
                if len(queue) < cfg["batch_size"]:
                    try:
                        await asyncio.wait_for(wake.wait(), cfg["flush_interval_seconds"])
                    except asyncio.TimeoutError:
                        pass
                wake.clear()
                batch = []
                size = 0
                # Leave in the queue until delivered, so in-flight bytes stay bounded.
                for item in queue:
                    if len(batch) >= cfg["batch_size"] or size + item.size > MAX_BATCH_BYTES - 1024:
                        break
                    batch.append(item)
                    size += item.size
                events = [item.event for item in batch]
                if cfg["filter"]:
                    try:
                        async with self.evaluations:
                            mask = await asyncio.to_thread(opa.eval_many, self.cfg.opa_bin, self.cfg.capabilities,
                                                           cfg["filter"], events, EVAL_DEADLINE_SECONDS)
                    except opa.OpaTimeout:
                        mask = None
                    if mask is None:
                        log.warning("webhook filter evaluation failed; batch omitted", extra={"session": sid})
                        events = []
                    else:
                        events = [event for event, keep in zip(events, mask) if keep]
                if events:
                    bid = str(uuid.uuid4())
                    body = json.dumps({"version": 1, "batch_id": bid, "session_id": sid, "events": events},
                                      separators=(",", ":")).encode()
                    for attempt in range(MAX_ATTEMPTS):
                        if await self._send(cfg, body, bid):
                            break
                        if attempt == MAX_ATTEMPTS - 1:
                            log.warning("webhook delivery exhausted retries", extra={"session": sid, "batch_id": bid})
                        else:
                            await asyncio.sleep(min(2 ** attempt, 60))
                for _ in batch:
                    self.bytes -= queue.popleft().size
        finally:
            self.tasks.pop(sid, None)

    async def close(self) -> None:
        self.closed = True
        for sid in list(self.settings):
            await self.remove(sid)
        if self.http:
            await self.http.close()


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
