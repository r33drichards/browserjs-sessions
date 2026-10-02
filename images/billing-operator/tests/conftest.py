import hashlib
from pathlib import Path

import pytest

from billing_operator.catalogue import CatalogueFile

CONTRACTS = Path(__file__).resolve().parents[3] / "docs" / "contracts" / "billing"
NS = "browserjs-sessions"
OWNER = "u@example.com"


def owner_hash(owner: str = OWNER) -> str:
    return hashlib.sha256(owner.encode()).hexdigest()[:32]


def acct(owner: str = OWNER) -> str:
    return "acct-" + owner_hash(owner)


def sandbox(name: str, owner: str | None = OWNER, *, ready: str | None = "True",
            ready_since: str = "2026-10-02T09:00:00Z", mode: str | None = "Running", deleting: bool = False) -> dict:
    """A session's Sandbox. owner None: a warm-pool Sandbox before adoption."""
    meta = {"name": name, "namespace": NS}
    if owner:
        meta["labels"] = {"browserjs.dev/owner": owner_hash(owner)}
        meta["annotations"] = {"browserjs.dev/owner-id": owner}
    if deleting:
        meta["deletionTimestamp"] = "2026-10-02T09:30:00Z"
    body = {"apiVersion": "agents.x-k8s.io/v1beta1", "kind": "Sandbox", "metadata": meta, "spec": {}, "status": {}}
    if mode:
        body["spec"]["operatingMode"] = mode
    if ready:
        body["status"]["conditions"] = [{"type": "Ready", "status": ready, "lastTransitionTime": ready_since}]
    return body


def asleep(name: str, owner: str = OWNER) -> dict:
    return sandbox(name, owner, mode="Suspended", ready="False")


@pytest.fixture
def catalogue_path(tmp_path):
    path = tmp_path / "catalogue.yaml"
    path.write_text((CONTRACTS / "catalogue.yaml").read_text(encoding="utf-8"), encoding="utf-8")
    return path


@pytest.fixture
def catalogue_file(catalogue_path):
    return CatalogueFile(catalogue_path)
