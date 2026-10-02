"""The operator's state: which sessions have which policy, the bundle that
is published, and what each OPA replica has loaded.

Everything here is derived from the SessionPolicy resources; nothing
survives a restart, and nothing needs to.
"""
from __future__ import annotations

import asyncio
import hashlib
import logging
import time
from dataclasses import dataclass, field

import aiohttp

from . import bundle
from .bundle import BundleError, Tenant
from .check import Validation, check
from .config import Config

log = logging.getLogger("policy_operator")


@dataclass
class Entry:
    """One session, as last reconciled."""
    key: str                  # of the spec this was computed from
    validation: Validation    # of the current spec
    tenant: Tenant | None     # what is in force: the spec's module, or the last good one


@dataclass
class Outcome:
    """What a reconcile found, for status."""
    validation: Validation
    tenant: Tenant | None
    build_errors: list[dict] = field(default_factory=list)
    revision: str | None = None  # the bundle revision that first carried tenant.hash


@dataclass
class Loaded:
    replicas: int = 0
    total: int = 0

    @property
    def all(self) -> bool:
        return self.total > 0 and self.replicas == self.total


def spec_key(kind: str, source: str) -> str:
    return hashlib.sha256(f"{kind}\0{source}".encode("utf-8")).hexdigest()


class Publisher:
    """The published bundle, and a way to wait for the next one."""

    def __init__(self) -> None:
        self.body: bytes | None = None
        self.etag: str | None = None
        self.revision: str | None = None
        self._changed = asyncio.Event()
        self._closed = False

    def close(self) -> None:
        """Let go of every held request: the process is stopping."""
        self._closed = True
        self._changed.set()

    def publish(self, body: bytes, revision: str) -> None:
        self.body, self.revision = body, revision
        self.etag = '"' + hashlib.sha256(body).hexdigest() + '"'
        changed, self._changed = self._changed, asyncio.Event()
        changed.set()

    async def wait_change(self, etag: str, seconds: float) -> None:
        """Returns when the published ETag is not `etag`, or after `seconds`."""
        deadline = time.monotonic() + seconds
        while self.etag == etag and not self._closed:
            left = deadline - time.monotonic()
            if left <= 0:
                return
            try:
                await asyncio.wait_for(self._changed.wait(), left)
            except asyncio.TimeoutError:
                return


class Operator:
    def __init__(self, cfg: Config):
        self.cfg = cfg
        self.sessions: dict[str, Entry] = {}
        self.publisher = Publisher()
        # True once every SessionPolicy that existed at start is in `sessions`
        # and a bundle of them is published. Until then nothing is published:
        # a bundle with sessions missing would deny them.
        self.ready = False
        self._started = int(time.time())
        self._counter = 0
        self._published_key: tuple | None = None
        self._carried: dict[str, tuple[str, str]] = {}  # session -> (hash, first revision)
        self._build_lock = asyncio.Lock()
        self._checks = asyncio.Semaphore(4)
        self._http: aiohttp.ClientSession | None = None
        self._loaded_cache: tuple[float, tuple[str, ...], dict] | None = None

    # --- policies -----------------------------------------------------------

    async def _check(self, kind: str, source: str, name: str) -> Validation:
        async with self._checks:
            return await asyncio.to_thread(check, self.cfg, kind, source, name)

    async def _entry(self, name: str, kind: str, source: str, last_good: str | None) -> Entry:
        key = spec_key(kind, source)
        entry = self.sessions.get(name)
        if entry and entry.key == key:
            return entry
        v = await self._check(kind, source, name)
        if v.ok:
            tenant = Tenant(v.tenant_module, v.hash)
        elif entry and entry.tenant:
            tenant = entry.tenant
        elif last_good:
            # status.rego is the last module that passed. It is checked again:
            # status is only a record, and OPA or the capabilities may have
            # changed since it was written.
            again = await self._check("rego", last_good, name)
            tenant = Tenant(again.tenant_module, again.hash) if again.ok else None
            if not again.ok:
                log.warning("%s: status.rego no longer passes the checks; the session is left out", name)
        else:
            tenant = None
        return Entry(key, v, tenant)

    async def reconcile(self, name: str, spec: dict, status: dict) -> Outcome:
        """Check a SessionPolicy's spec and bring the bundle up to date."""
        kind, source = str(spec.get("kind", "")), spec.get("source", "")
        previous = self.sessions.get(name)
        entry = await self._entry(name, kind, source if isinstance(source, str) else "", (status or {}).get("rego"))
        self.sessions[name] = entry
        errors: list[dict] = []
        if self.ready and entry is not previous:
            errors = await self.rebuild()
            if errors:
                # The previous bundle stays published; so does what it holds.
                if previous:
                    self.sessions[name] = previous
                else:
                    del self.sessions[name]
                return Outcome(entry.validation, previous.tenant if previous else None, errors,
                               self._revision_of(name, previous.tenant if previous else None))
        return Outcome(entry.validation, entry.tenant, [], self._revision_of(name, entry.tenant))

    async def remove(self, name: str) -> None:
        if self.sessions.pop(name, None) is not None and self.ready:
            await self.rebuild()
        self._carried.pop(name, None)

    async def first_pass(self, resources: list[dict]) -> None:
        """Take in every SessionPolicy that exists, then publish for the first time."""
        async def one(body: dict) -> None:
            name = body["metadata"]["name"]
            spec, status = body.get("spec") or {}, body.get("status") or {}
            source = spec.get("source", "")
            self.sessions[name] = await self._entry(name, str(spec.get("kind", "")),
                                                    source if isinstance(source, str) else "", status.get("rego"))

        await asyncio.gather(*(one(r) for r in resources if not (r.get("metadata") or {}).get("deletionTimestamp")))
        errors = await self.rebuild()
        if errors:
            raise BundleError(errors)
        self.ready = True
        log.info("first pass complete: %d policies, revision %s", len(self.sessions), self.publisher.revision)

    def _tenants(self) -> dict[str, Tenant]:
        return {name: e.tenant for name, e in self.sessions.items() if e.tenant}

    def _revision_of(self, name: str, tenant: Tenant | None) -> str | None:
        carried = self._carried.get(name)
        return carried[1] if tenant and carried and carried[0] == tenant.hash else None

    async def rebuild(self) -> list[dict]:
        """Build and publish, unless what is published already says the same.
        Returns OPA's errors when the build fails (and publishes nothing).
        """
        async with self._build_lock:
            tenants = self._tenants()
            key = tuple(sorted((name, t.hash) for name, t in tenants.items()))
            if key == self._published_key:
                return []
            revision = f"{self._started}-{self._counter + 1}"
            try:
                body = await asyncio.to_thread(bundle.build, self.cfg, tenants, revision)
            except BundleError as e:
                log.error("the bundle does not build; the previous one stays published: %s", e)
                return e.errors
            self._counter += 1
            self._published_key = key
            for name, t in tenants.items():
                if self._carried.get(name, ("", ""))[0] != t.hash:
                    self._carried[name] = (t.hash, revision)
            self.publisher.publish(body, revision)
            log.info("published revision %s: %d sessions, %d bytes", revision, len(tenants), len(body))
            return []

    # --- what the replicas have loaded --------------------------------------

    async def _session(self) -> aiohttp.ClientSession:
        if self._http is None or self._http.closed:
            self._http = aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=2))
        return self._http

    async def close(self) -> None:
        if self._http is not None:
            await self._http.close()

    async def _read_loaded(self, address: str) -> dict | None:
        """data.browserjs.loaded of one replica; None when it does not answer."""
        try:
            http = await self._session()
            async with http.get(f"http://{address}/v1/data/browserjs/loaded",
                                headers={"Authorization": f"Bearer {self.cfg.opa_token}"}) as r:
                if r.status != 200:
                    return None
                result = (await r.json(content_type=None)).get("result")
                return result if isinstance(result, dict) else {}
        except (aiohttp.ClientError, asyncio.TimeoutError, ValueError):
            return None

    async def replica_documents(self, addresses: list[str], max_age: float = 0.0) -> dict[str, dict | None]:
        addresses = sorted(set(addresses))
        cached = self._loaded_cache
        if max_age and cached and cached[1] == tuple(addresses) and time.monotonic() - cached[0] <= max_age:
            return cached[2]
        documents = dict(zip(addresses, await asyncio.gather(*(self._read_loaded(a) for a in addresses))))
        self._loaded_cache = (time.monotonic(), tuple(addresses), documents)
        return documents

    async def loaded(self, name: str, policy_hash: str | None, addresses: list[str], max_age: float = 0.0) -> Loaded:
        """How many of the ready replicas serve `policy_hash` for the session."""
        documents = await self.replica_documents(addresses, max_age)
        serving = sum(1 for d in documents.values() if policy_hash and d is not None and d.get(name) == policy_hash)
        return Loaded(replicas=serving, total=len(documents))

    async def wait_loaded(self, name: str, policy_hash: str | None, addresses, timeout: float,
                          step: float = 0.05) -> Loaded:
        """`loaded`, retried until every replica has it or `timeout` passes.
        `addresses` is a function: replicas come and go while waiting.
        """
        deadline = time.monotonic() + timeout
        while True:
            state = await self.loaded(name, policy_hash, addresses())
            if state.all or not policy_hash or time.monotonic() >= deadline:
                return state
            await asyncio.sleep(step)
