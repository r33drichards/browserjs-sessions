"""The one call the operator makes to the API server itself: the list of
SessionPolicy resources that exist when it starts. Everything after that
comes through kopf's watches.
"""
from __future__ import annotations

import os
import ssl
from pathlib import Path

import aiohttp

from .config import GROUP, PLURAL, VERSION

SERVICE_ACCOUNT = Path("/var/run/secrets/kubernetes.io/serviceaccount")


async def list_session_policies(namespace: str, base_url: str | None = None, token: str | None = None,
                                ssl_context: ssl.SSLContext | bool | None = None) -> list[dict]:
    """Every SessionPolicy of the namespace. In a pod the arguments default
    to the ServiceAccount's; they exist for tests."""
    if base_url is None:
        host, port = os.environ["KUBERNETES_SERVICE_HOST"], os.environ.get("KUBERNETES_SERVICE_PORT", "443")
        base_url = f"https://[{host}]:{port}" if ":" in host else f"https://{host}:{port}"
        token = (SERVICE_ACCOUNT / "token").read_text().strip()
        ssl_context = ssl.create_default_context(cafile=str(SERVICE_ACCOUNT / "ca.crt"))
    url = f"{base_url}/apis/{GROUP}/{VERSION}/namespaces/{namespace}/{PLURAL}"
    headers = {"Authorization": f"Bearer {token}"} if token else {}
    items: list[dict] = []
    params = {"limit": "500"}
    async with aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=60)) as http:
        while True:
            async with http.get(url, headers=headers, params=params, ssl=ssl_context) as r:
                r.raise_for_status()
                page = await r.json()
            items += page.get("items") or []
            token_ = (page.get("metadata") or {}).get("continue")
            if not token_:
                return items
            params = {"limit": "500", "continue": token_}
