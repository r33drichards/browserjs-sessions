"""kube.Client over HTTP, against the fake API server (fake_api.py): the
calls deploy.md's Role allows, and nothing else."""
import pytest

from billing_operator.kube import Client
from billing_operator.meter import ts

from conftest import NS, sandbox
from fake_api import FakeAPI

T0 = ts("2026-10-02T10:00:00Z")
LEASE = f"/apis/coordination.k8s.io/v1/namespaces/{NS}/leases"


@pytest.fixture
async def api(aiohttp_server):
    fake = FakeAPI([sandbox(f"s-{i:05d}") for i in range(7)], page_size=3)
    server = await aiohttp_server(fake.app())
    fake.url = f"http://127.0.0.1:{server.port}"
    return fake


@pytest.fixture
async def client(api):
    c = Client(NS, api.url, "sa-token", False)
    yield c
    await c.close()


async def test_the_list_follows_continue_tokens(api, client):
    assert [s["metadata"]["name"] for s in await client.list_sandboxes()] == [f"s-{i:05d}" for i in range(7)]
    assert api.requests == [("GET", f"/apis/agents.x-k8s.io/v1beta1/namespaces/{NS}/sandboxes", "Bearer sa-token")] * 3


async def test_the_lease_is_made_then_renewed(api, client):
    await client.renew_lease(T0, "pod-1")
    assert [r[:2] for r in api.requests] == [("GET", f"{LEASE}/billing-observer"), ("POST", LEASE)]
    lease = api.leases["billing-observer"]
    assert lease["metadata"]["namespace"] == NS
    assert lease["spec"] == {"holderIdentity": "pod-1", "renewTime": "2026-10-02T10:00:00.000000Z"}
    lease["spec"]["leaseDurationSeconds"] = 600   # somebody's field is kept
    await client.renew_lease(T0 + 60, "pod-2")
    assert [r[:2] for r in api.requests[2:]] == [("GET", f"{LEASE}/billing-observer"), ("PUT", f"{LEASE}/billing-observer")]
    assert api.leases["billing-observer"]["spec"] == {"holderIdentity": "pod-2", "renewTime": "2026-10-02T10:01:00.000000Z",
                                                      "leaseDurationSeconds": 600}


async def test_a_conflict_on_the_lease_is_tried_once_more(api, client):
    await client.renew_lease(T0, "pod-1")
    api.conflicts = 1
    await client.renew_lease(T0 + 60, "pod-1")
    assert api.leases["billing-observer"]["spec"]["renewTime"] == "2026-10-02T10:01:00.000000Z"
    api.conflicts = 2
    with pytest.raises(RuntimeError, match="billing-observer"):
        await client.renew_lease(T0 + 120, "pod-1")


async def test_the_service_account_token_is_read_again_for_every_request(api, tmp_path):
    token = tmp_path / "token"
    token.write_text("first\n")
    c = Client(NS, api.url, token, False)
    try:
        await c.renew_lease(T0, "p")
        token.write_text("second\n")  # the kubelet rotated it
        await c.renew_lease(T0 + 60, "p")
    finally:
        await c.close()
    assert [r[2] for r in api.requests] == ["Bearer first"] * 2 + ["Bearer second"] * 2


async def test_in_cluster_reads_the_pods_environment(monkeypatch, tmp_path):
    from billing_operator import kube
    (tmp_path / "token").write_text("t")
    monkeypatch.setattr(kube, "SERVICE_ACCOUNT", tmp_path)
    monkeypatch.setattr(kube.ssl, "create_default_context", lambda cafile: ("ca", cafile))
    monkeypatch.setenv("KUBERNETES_SERVICE_HOST", "fd00::1")
    monkeypatch.setenv("KUBERNETES_SERVICE_PORT", "443")
    c = kube.Client.in_cluster(NS)
    assert c.base_url == "https://[fd00::1]:443" and c._headers()["Authorization"] == "Bearer t"
    assert c._ssl == ("ca", str(tmp_path / "ca.crt"))
