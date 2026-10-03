"""The whole operator under `kopf run`, as deploy.md starts it, against an
API server faked in memory (fake_kube.py) and two real OPA servers. Not a
cluster: no RBAC, no admission, no NetworkPolicy. That run is track B's.
"""
import asyncio
import json
import os
import subprocess
import threading
import time

import pytest
import yaml
from aiohttp import web
from kopf.testing import KopfRunner

from policy_operator import handlers, kube
from policy_operator.check import policy_hash

from conftest import ALLOW_ALL, BROKEN, CONTRACTS, DENY_ALL, free_port, resource
from fake_kube import FakeKube
from test_integration import CALL, decide, http, wait_until

NS = "browserjs-sessions"


class Cluster:
    """The fake API server on a thread of its own: kopf brings its own loop."""

    def __init__(self):
        self.kube = FakeKube()
        self.port = free_port()
        self.loop = asyncio.new_event_loop()
        self.thread = threading.Thread(target=self._serve, daemon=True)
        self.started = threading.Event()

    def _serve(self):
        asyncio.set_event_loop(self.loop)
        runner = web.AppRunner(self.kube.app(), handler_cancellation=True)
        self.loop.run_until_complete(runner.setup())
        self.loop.run_until_complete(web.TCPSite(runner, "127.0.0.1", self.port).start())
        self.started.set()
        self.loop.run_forever()

    def start(self):
        self.thread.start()
        assert self.started.wait(10)

    def call(self, fn, *args):
        """Run something on the API server's loop, from the test's thread."""
        done = threading.Event()
        out = []

        def run():
            out.append(fn(*args))
            done.set()

        self.loop.call_soon_threadsafe(run)
        assert done.wait(10)
        return out[0]

    def policy(self, name):
        return self.call(self.kube.get, "sessionpolicies", name)

    def conditions(self, name):
        obj = self.policy(name) or {}
        return {c["type"]: c["status"] for c in (obj.get("status") or {}).get("conditions") or []}


@pytest.fixture
def cluster(cfg, tmp_path, monkeypatch):
    c = Cluster()
    c.start()
    kubeconfig = tmp_path / "kubeconfig"
    kubeconfig.write_text(yaml.safe_dump({
        "apiVersion": "v1", "kind": "Config", "current-context": "fake",
        "clusters": [{"name": "fake", "cluster": {"server": f"http://127.0.0.1:{c.port}"}}],
        "users": [{"name": "fake", "user": {"token": "fake"}}],
        "contexts": [{"name": "fake", "context": {"cluster": "fake", "user": "fake", "namespace": NS}}]}))
    operator_port = free_port()
    monkeypatch.setenv("KUBECONFIG", str(kubeconfig))
    monkeypatch.setenv("OPA_BIN", cfg.opa_bin)
    monkeypatch.setenv("POLICY_CONTRACT_DIR", str(CONTRACTS))
    monkeypatch.setenv("HTTP_PORT", str(operator_port))
    monkeypatch.setenv("WEBHOOK_REDIS_URL", cfg.webhook_redis_url)
    monkeypatch.setenv("WEBHOOK_REDIS_PREFIX", cfg.webhook_redis_prefix)
    monkeypatch.setenv("BUNDLE_TOKEN", cfg.bundle_token)
    monkeypatch.setenv("OPA_TOKEN", cfg.opa_token)
    monkeypatch.setenv("OPERATOR_API_TOKEN", cfg.api_token)
    monkeypatch.setattr(handlers, "LOADED_INTERVAL_SECONDS", 1.0)
    # In a pod this reads the ServiceAccount; here, the fake.
    real = kube.list_session_policies
    monkeypatch.setattr(kube, "list_session_policies",
                        lambda namespace: real(namespace, f"http://127.0.0.1:{c.port}", "fake", False))

    config = yaml.safe_load((CONTRACTS / "opa-config.yaml").read_text())
    config["services"]["operator"]["url"] = f"http://127.0.0.1:{operator_port}"
    processes, endpoints = [], []
    for i in range(2):
        directory = tmp_path / f"opa{i}"
        directory.mkdir()
        config["persistence_directory"] = str(directory / "persist")
        (directory / "opa-config.yaml").write_text(yaml.safe_dump(config))
        port = free_port()
        processes.append(subprocess.Popen(
            [cfg.opa_bin, "run", "--server", "--addr", f"127.0.0.1:{port}", "--authentication=token",
             "--authorization=basic", "--config-file", str(directory / "opa-config.yaml"),
             str(CONTRACTS / "system-authz.rego")],
            env={**os.environ, "BUNDLE_TOKEN": cfg.bundle_token, "OPERATOR_TOKEN": cfg.opa_token},
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL))
        endpoints.append(port)
    c.opa_urls = [f"http://127.0.0.1:{p}" for p in endpoints]
    c.operator_url = f"http://127.0.0.1:{operator_port}"
    # One EndpointSlice per replica, as two ports on one address cannot share a slice.
    for i, port in enumerate(endpoints):
        c.call(c.kube.put, "endpointslices", {
            "apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice",
            "metadata": {"name": f"opa-{i}", "namespace": NS, "labels": {"kubernetes.io/service-name": "opa-engine"}},
            "addressType": "IPv4", "ports": [{"name": "http", "port": port, "protocol": "TCP"}],
            "endpoints": [{"addresses": ["127.0.0.1"], "conditions": {"ready": True}}]})
    # A slice of another Service: its endpoints are not OPA's.
    c.call(c.kube.put, "endpointslices", {
        "apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice",
        "metadata": {"name": "backend-x", "namespace": NS, "labels": {"kubernetes.io/service-name": "backend"}},
        "addressType": "IPv4", "ports": [{"port": 1}], "endpoints": [{"addresses": ["127.0.0.1"]}]})
    try:
        yield c
    finally:
        for p in processes:
            p.kill()
        for p in processes:
            p.wait(10)
        c.loop.call_soon_threadsafe(c.loop.stop)


def test_the_operator_under_kopf(cluster):
    c = cluster
    # A policy that exists before the operator starts, one of them broken with a last good in status.
    c.call(c.kube.put, "sessionpolicies", resource("s-aaaaa", "rego", ALLOW_ALL))
    c.call(c.kube.put, "sessionpolicies",
           resource("s-ccccc", "rego", BROKEN, status={"rego": DENY_ALL, "hash": policy_hash(DENY_ALL)}))

    # deploy.md's command, less the liveness endpoint.
    with KopfRunner(["run", "--standalone", f"--namespace={NS}", "-m", "policy_operator"], timeout=60) as runner:
        try:
            assert wait_until(lambda: http(c.operator_url + "/readyz")[0] == 200), "the first pass never completed"
            assert wait_until(lambda: all(http(u + "/health?bundles")[0] == 200 for u in c.opa_urls), 30)

            # Resume: status is written for what existed.
            assert wait_until(lambda: c.conditions("s-aaaaa") == {"Compiled": "True", "Loaded": "True", "Ready": "True"}, 30), \
                c.policy("s-aaaaa")
            status = c.policy("s-aaaaa")["status"]
            assert status["hash"] == policy_hash(ALLOW_ALL) and status["rego"] == ALLOW_ALL
            assert status["observedGeneration"] == 1 and status["loaded"]["replicas"] == 2 == status["loaded"]["total"]
            assert set(status) <= {"observedGeneration", "hash", "rego", "regoGeneration", "errors", "warnings",
                                   "loaded", "lastAppliedTime", "conditions"}  # nothing of kopf's in status
            assert wait_until(lambda: c.conditions("s-ccccc") == {"Compiled": "False", "Loaded": "True", "Ready": "False"}, 30), \
                c.policy("s-ccccc")
            assert c.policy("s-ccccc")["status"]["hash"] == policy_hash(DENY_ALL)
            for u in c.opa_urls:
                assert decide(u, "s-aaaaa", CALL) == {"result": {"allow": True}}
                assert decide(u, "s-ccccc", CALL) == {"result": {"allow": False}}

            # Create.
            c.call(c.kube.put, "sessionpolicies", resource("s-bbbbb", "rego", DENY_ALL))
            assert wait_until(lambda: c.conditions("s-bbbbb").get("Ready") == "True", 30), c.policy("s-bbbbb")
            created = c.policy("s-bbbbb")
            assert created["metadata"]["finalizers"] == ["browserjs.dev/policy-operator"]
            assert all(decide(u, "s-bbbbb", CALL) == {"result": {"allow": False}} for u in c.opa_urls)

            # Update: in force with nothing restarted.
            c.call(c.kube.put, "sessionpolicies", resource("s-bbbbb", "rego", ALLOW_ALL))
            assert wait_until(lambda: (c.policy("s-bbbbb")["status"].get("observedGeneration") == 2
                                       and c.conditions("s-bbbbb").get("Ready") == "True"), 30), c.policy("s-bbbbb")
            assert c.policy("s-bbbbb")["status"]["hash"] == policy_hash(ALLOW_ALL)
            assert all(decide(u, "s-bbbbb", CALL) == {"result": {"allow": True}} for u in c.opa_urls)

            # An update that does not compile: errors in status, the last good in force.
            c.call(c.kube.put, "sessionpolicies", resource("s-bbbbb", "rego", BROKEN))
            assert wait_until(lambda: c.policy("s-bbbbb")["status"].get("observedGeneration") == 3, 30)
            status = c.policy("s-bbbbb")["status"]
            assert c.conditions("s-bbbbb") == {"Compiled": "False", "Loaded": "True", "Ready": "False"}
            assert status["errors"][0]["code"] == "rego_parse_error" and status["regoGeneration"] == 2
            assert status["hash"] == policy_hash(ALLOW_ALL)
            assert all(decide(u, "s-bbbbb", CALL) == {"result": {"allow": True}} for u in c.opa_urls)

            # An annotation alone (the backend's updated-by) is not a change of spec.
            revision = json.loads(json.dumps(c.policy("s-aaaaa")["status"]["loaded"]["revision"]))
            before = http(c.operator_url + "/bundles/browserjs.tar.gz", headers={"Authorization": "Bearer bundle-secret"})[1]
            c.call(c.kube.put, "sessionpolicies", {**resource("s-aaaaa", "rego", ALLOW_ALL), "metadata": {
                "name": "s-aaaaa", "namespace": NS, "annotations": {"browserjs.dev/updated-by": "ui"}}})
            time.sleep(1.5)
            assert http(c.operator_url + "/bundles/browserjs.tar.gz", headers={"Authorization": "Bearer bundle-secret"})[1] == before
            assert c.policy("s-aaaaa")["status"]["loaded"]["revision"] == revision

            # Delete: out of the bundle, and the finalizer lets the resource go.
            c.call(c.kube.delete, "sessionpolicies", "s-bbbbb")
            assert wait_until(lambda: c.policy("s-bbbbb") is None, 30)
            assert wait_until(lambda: all(decide(u, "s-bbbbb", CALL) == {} for u in c.opa_urls))
            assert all(decide(u, "s-aaaaa", CALL) == {"result": {"allow": True}} for u in c.opa_urls)

            # status only ever goes through the status subresource.
            status_writes = [p for m, p in c.kube.requests if p.endswith("/status")]
            assert status_writes and all("/sessionpolicies/" in p for m, p in c.kube.requests)
        finally:
            output = runner  # kept for the assertion below
    assert output.exit_code == 0, output.output
    assert output.exception is None
