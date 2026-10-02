"""The observer's calls to the API server: exactly the verbs of deploy.md's
Role, over aiohttp. memory.py answers the same calls from memory.

  sandboxes                       list
  leases (billing-observer)       get, create, update
"""
from __future__ import annotations

import os
import ssl
from pathlib import Path

import aiohttp

SERVICE_ACCOUNT = Path("/var/run/secrets/kubernetes.io/serviceaccount")
SANDBOXES = "/apis/agents.x-k8s.io/v1beta1"
LEASES = "/apis/coordination.k8s.io/v1"
LEASE = "billing-observer"


def micro_time(seconds: int) -> str:
    """A Lease's renewTime: RFC 3339 with microseconds."""
    from .meter import iso
    return iso(seconds)[:-1] + ".000000Z"


class Client:
    def __init__(self, namespace: str, base_url: str, token: str | Path | None = None,
                 ssl_context: ssl.SSLContext | bool | None = None, timeout: float = 30.0):
        self.namespace = namespace
        self.base_url = base_url.rstrip("/")
        self._token = token
        self._ssl = ssl_context
        self._timeout = aiohttp.ClientTimeout(total=timeout)
        self._http: aiohttp.ClientSession | None = None

    @classmethod
    def in_cluster(cls, namespace: str) -> "Client":
        """As the pod's ServiceAccount."""
        host, port = os.environ["KUBERNETES_SERVICE_HOST"], os.environ.get("KUBERNETES_SERVICE_PORT", "443")
        base_url = f"https://[{host}]:{port}" if ":" in host else f"https://{host}:{port}"
        # The token's path, not its text: the kubelet replaces it within the hour.
        return cls(namespace, base_url, SERVICE_ACCOUNT / "token",
                   ssl.create_default_context(cafile=str(SERVICE_ACCOUNT / "ca.crt")))

    async def close(self) -> None:
        if self._http is not None and not self._http.closed:
            await self._http.close()

    def _headers(self) -> dict:
        token = self._token.read_text().strip() if isinstance(self._token, Path) else self._token
        headers = {"Accept": "application/json"}
        if token:
            headers["Authorization"] = f"Bearer {token}"
        return headers

    def _url(self, api: str, plural: str, name: str | None = None) -> str:
        url = f"{self.base_url}{api}/namespaces/{self.namespace}/{plural}"
        return f"{url}/{name}" if name else url

    async def _request(self, method: str, url: str, *, accept=(), params=None, body=None):
        """The response's JSON; None for a status in `accept`. Anything else raises."""
        if self._http is None or self._http.closed:
            self._http = aiohttp.ClientSession(timeout=self._timeout)
        kwargs = {"headers": self._headers(), "params": params, "ssl": self._ssl}
        if body is not None:
            kwargs["json"] = body
        async with self._http.request(method, url, **kwargs) as r:
            if r.status in accept:
                await r.read()
                return None
            if r.status not in (200, 201):
                text = (await r.text())[:300]
                raise aiohttp.ClientResponseError(r.request_info, r.history, status=r.status,
                                                  message=f"{method} {url}: {text}")
            return await r.json(content_type=None)

    async def list_sandboxes(self) -> list[dict]:
        items: list[dict] = []
        params = {"limit": "500"}
        while True:
            page = await self._request("GET", self._url(SANDBOXES, "sandboxes"), params=params)
            items += page.get("items") or []
            more = (page.get("metadata") or {}).get("continue")
            if not more:
                return items
            params = {"limit": "500", "continue": more}

    async def renew_lease(self, now: int, holder: str) -> None:
        """Lease billing-observer says when usage was last delivered: made
        if it is not there, its renewTime moved if it is. Somebody else's
        write in between (a 409) is tried once more."""
        for _ in range(2):
            lease = await self._request("GET", self._url(LEASES, "leases", LEASE), accept=(404,))
            if lease is None:
                body = {"apiVersion": "coordination.k8s.io/v1", "kind": "Lease",
                        "metadata": {"name": LEASE, "namespace": self.namespace},
                        "spec": {"holderIdentity": holder, "renewTime": micro_time(now)}}
                done = await self._request("POST", self._url(LEASES, "leases"), body=body, accept=(409,))
            else:
                lease["spec"] = {**(lease.get("spec") or {}), "holderIdentity": holder, "renewTime": micro_time(now)}
                done = await self._request("PUT", self._url(LEASES, "leases", LEASE), body=lease, accept=(409,))
            if done is not None:
                return
        raise RuntimeError(f"Lease {LEASE} changed under two renewals in a row")
