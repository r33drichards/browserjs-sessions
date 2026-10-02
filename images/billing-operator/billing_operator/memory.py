"""The calls of kube.Client answered from memory: for `python -m
billing_operator simulate`, which runs the observer's pass over a directory
of YAML, and for the tests.
"""
from __future__ import annotations

import copy

from .kube import LEASE, micro_time


class MemoryKube:
    def __init__(self, sandboxes: list[dict] | None = None):
        self.sandboxes: dict[str, dict] = {}
        self.lease: dict | None = None
        self.lists = 0
        self.renewals: list[int] = []
        for sandbox in sandboxes or []:
            self.put(sandbox)

    def put(self, sandbox: dict) -> None:
        self.sandboxes[sandbox["metadata"]["name"]] = copy.deepcopy(sandbox)

    def remove(self, name: str) -> None:
        self.sandboxes.pop(name, None)

    async def close(self) -> None:
        pass

    async def list_sandboxes(self) -> list[dict]:
        self.lists += 1
        return [copy.deepcopy(s) for _, s in sorted(self.sandboxes.items())]

    async def renew_lease(self, now: int, holder: str) -> None:
        self.renewals.append(now)
        self.lease = {"apiVersion": "coordination.k8s.io/v1", "kind": "Lease", "metadata": {"name": LEASE},
                      "spec": {"holderIdentity": holder, "renewTime": micro_time(now)}}
