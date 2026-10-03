"""With a real OPA server, and no cluster: the bundle the operator
publishes is one OPA loads, long-polls for, and answers from as mcp-js asks.
"""
import asyncio
import io
import json
import os
import subprocess
import time
import urllib.error
import urllib.request

import pytest
import yaml

from policy_operator import cli, handlers, server
from policy_operator.check import policy_hash
from policy_operator.operator import Operator

from conftest import ALLOW_ALL, BROKEN, CONTRACTS, DENY_ALL, free_port, resource


def wait_until(predicate, seconds=20.0, step=0.05):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if predicate():
            return True
        time.sleep(step)
    return False


def http(url, data=None, headers=None, method=None):
    request = urllib.request.Request(url, data=data, headers=headers or {}, method=method)
    try:
        with urllib.request.urlopen(request, timeout=5) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()
    except OSError:
        return 0, b""


def decide(opa_url, sid, input_doc):
    status, body = http(f"{opa_url}/v1/data/browserjs/decision/{sid}/mcp_tools",
                        json.dumps({"input": input_doc}).encode(), method="POST")
    assert status == 200
    # The whole answer but OPA's decision_id: {} when the document is undefined.
    answer = json.loads(body)
    answer.pop("decision_id", None)
    return answer


CALL = {"operation": "mcp_call_tool", "server": "browser", "tool": "browser_execute",
        "arguments": {"operations": [{"type": "url"}]}}


def test_documented_command_then_a_second_opa_answers_the_cases(cfg, tmp_path, capsys, monkeypatch):
    """docs/policy-operator.md, "Without a cluster"."""
    monkeypatch.setenv("OPA_BIN", cfg.opa_bin)
    monkeypatch.setenv("POLICY_CONTRACT_DIR", str(cfg.contract_dir))
    assert cli.main(["example-resources", str(tmp_path / "policies")]) == 0
    assert len(list((tmp_path / "policies").glob("*.yaml"))) == 7
    out = tmp_path / "browserjs.tar.gz"
    assert cli.main(["bundle", str(tmp_path / "policies"), "-o", str(out)]) == 0
    printed = capsys.readouterr().out
    assert "s-exam1: in the bundle, sha256:" in printed and "--- /tenant/s-exam5.rego" in printed
    assert 'package browserjs.decision["s-exam3"].mcp_tools' in printed

    port = free_port()
    opa = subprocess.Popen([cfg.opa_bin, "run", "--server", "--addr", f"127.0.0.1:{port}", "-b", str(out)],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        url = f"http://127.0.0.1:{port}"
        assert wait_until(lambda: http(url + "/health")[0] == 200)
        buffer = io.StringIO()
        assert cli.run_cases(cfg, url, buffer) == 0, buffer.getvalue()
        assert buffer.getvalue().strip() == "264/264 cases pass"
        assert cli.main(["run-cases", url]) == 0
        # A session that is not in the bundle: no result, which mcp-js denies.
        assert decide(url, "s-zzzzz", CALL) == {}
    finally:
        opa.terminate()
        opa.wait(10)


def test_bundle_command_reports_what_does_not_compile(cfg, tmp_path, capsys, monkeypatch):
    monkeypatch.setenv("OPA_BIN", cfg.opa_bin)
    monkeypatch.setenv("POLICY_CONTRACT_DIR", str(cfg.contract_dir))
    docs = [resource("s-aaaaa", "rego", BROKEN), resource("s-bbbbb", "rego", ALLOW_ALL),
            resource("s-ccccc", "rego", BROKEN, status={"rego": DENY_ALL}),
            {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "ignored"}}]
    (tmp_path / "all.yaml").write_text(yaml.safe_dump_all(docs))
    assert cli.main(["bundle", str(tmp_path)]) == 0
    printed = capsys.readouterr().out
    assert "s-aaaaa: does not compile; left out (denied)" in printed
    assert "  4: rego_parse_error: unexpected eof token" in printed
    assert f"s-bbbbb: in the bundle, {policy_hash(ALLOW_ALL)}" in printed
    assert f"s-ccccc: does not compile; status.rego is in the bundle, {policy_hash(DENY_ALL)}" in printed
    assert "--- /tenant/s-aaaaa.rego" not in printed and "--- /tenant/s-ccccc.rego" in printed


@pytest.fixture
async def stack(cfg, tmp_path, monkeypatch):
    """The operator's HTTP API on a real port, and two real OPA servers
    configured with the contract's opa-config.yaml and system-authz.rego."""
    import dataclasses
    port = free_port()
    op = Operator(dataclasses.replace(cfg, http_port=port))
    monkeypatch.setattr(handlers, "OPERATOR", op)
    runner = await server.start(op, "127.0.0.1")

    config = yaml.safe_load((CONTRACTS / "opa-config.yaml").read_text())
    config["services"]["operator"]["url"] = f"http://127.0.0.1:{port}"
    processes, addresses = [], []
    for i in range(2):
        directory = tmp_path / f"opa{i}"
        directory.mkdir()
        config["persistence_directory"] = str(directory / "persist")
        (directory / "opa-config.yaml").write_text(yaml.safe_dump(config))
        opa_port = free_port()
        processes.append(subprocess.Popen(
            [cfg.opa_bin, "run", "--server", "--addr", f"127.0.0.1:{opa_port}", "--authentication=token",
             "--authorization=basic", "--config-file", str(directory / "opa-config.yaml"),
             str(CONTRACTS / "system-authz.rego")],
            env={**os.environ, "BUNDLE_TOKEN": cfg.bundle_token, "OPERATOR_TOKEN": cfg.opa_token},
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL))
        addresses.append(f"127.0.0.1:{opa_port}")
    op.cfg = dataclasses.replace(op.cfg, opa_decision_url="http://" + addresses[0])
    try:
        yield op, addresses
    finally:
        for p in processes:
            p.kill()  # OPA's graceful shutdown waits ten seconds for its connections
        for p in processes:
            p.wait(10)
        await op.close()
        await server.stop(runner, op)


async def until(predicate, seconds=20.0):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if await asyncio.to_thread(predicate):
            return True
        await asyncio.sleep(0.05)
    return False


class Patch:
    def __init__(self):
        self.status = {}


async def reconcile(index, body, status=None):
    patch = Patch()
    await handlers.reconcile(name=body["metadata"]["name"], spec=body["spec"], status=status or {},
                             meta=body["metadata"], patch=patch, opa_endpoints=index)
    return patch.status


def conds(status):
    return {c["type"]: c["status"] for c in status["conditions"]}


async def test_real_opa_replicas_follow_the_operator(stack):
    op, addresses = stack
    index = {"opa": [addresses]}
    urls = [f"http://{a}" for a in addresses]

    # Before the first pass the operator answers 503: OPA is up, and not ready.
    assert await until(lambda: all(http(u + "/health")[0] == 200 for u in urls))
    for u in urls:
        assert (await asyncio.to_thread(http, u + "/health?bundles"))[0] == 500

    await op.first_pass([resource("s-aaaaa", "rego", ALLOW_ALL)])
    assert await until(lambda: all(http(u + "/health?bundles")[0] == 200 for u in urls))
    for u in urls:
        assert await asyncio.to_thread(decide, u, "s-aaaaa", CALL) == {"result": {"allow": True}}
        assert await asyncio.to_thread(decide, u, "s-bbbbb", CALL) == {}

    # A new policy: the reconcile publishes, both replicas load it while the
    # handler waits (they long-poll), and status says Ready.
    started = time.monotonic()
    status = await reconcile(index, resource("s-bbbbb", "rego", DENY_ALL))
    took = time.monotonic() - started
    assert conds(status) == {"Compiled": "True", "Loaded": "True", "Ready": "True"}, status
    assert status["loaded"] == {"replicas": 2, "total": 2, "revision": op.publisher.revision}
    assert took < handlers.LOADED_WAIT_SECONDS
    for u in urls:
        assert await asyncio.to_thread(decide, u, "s-bbbbb", CALL) == {"result": {"allow": False}}

    # An edit applies with nothing restarted.
    status = {**status, **await reconcile(index, resource("s-bbbbb", "rego", ALLOW_ALL, generation=2), status)}
    assert conds(status)["Ready"] == "True" and status["hash"] == policy_hash(ALLOW_ALL)
    for u in urls:
        assert await asyncio.to_thread(decide, u, "s-bbbbb", CALL) == {"result": {"allow": True}}

    # An edit that does not compile: the previous policy keeps answering.
    status = {**status, **await reconcile(index, resource("s-bbbbb", "rego", BROKEN, generation=3), status)}
    assert conds(status) == {"Compiled": "False", "Loaded": "True", "Ready": "False"}
    for u in urls:
        assert await asyncio.to_thread(decide, u, "s-bbbbb", CALL) == {"result": {"allow": True}}

    # What a replica reports is the hash in status, and only to the operator.
    documents = await op.replica_documents(addresses)
    assert all(d == {"s-aaaaa": policy_hash(ALLOW_ALL), "s-bbbbb": status["hash"]} for d in documents.values())
    assert (await asyncio.to_thread(http, urls[0] + "/v1/data/browserjs/loaded"))[0] == 401

    # A deleted policy leaves the bundle: undefined, which is deny.
    await handlers.delete(name="s-bbbbb")
    assert await until(lambda: all(decide(u, "s-bbbbb", CALL) == {} for u in urls))
    assert await asyncio.to_thread(decide, urls[0], "s-aaaaa", CALL) == {"result": {"allow": True}}


async def test_real_opa_gateway_records_before_returning_decisions(stack):
    """A real OPA verdict cannot reach the caller before durable capture."""
    op, addresses = stack
    sent = []
    async def send(settings, body, bid):
        sent.append(json.loads(body))
        return True
    op.webhooks._send = send
    resources = [resource("s-aaaaa", "rego", ALLOW_ALL), resource("s-bbbbb", "rego", DENY_ALL)]
    for body in resources:
        body["spec"]["webhook"] = {"url": "https://example.com/hook", "batch_size": 1, "flush_interval_seconds": 1}
    await op.first_pass(resources)
    url = "http://" + addresses[0]
    assert await until(lambda: http(url + "/health")[0] == 200)
    assert await until(lambda: decide(url, "s-aaaaa", {}) == {"result": {"allow": False}})
    assert await until(lambda: decide(url, "s-bbbbb", {}) == {"result": {"allow": False}})
    gateway = f"http://127.0.0.1:{op.cfg.http_port}"
    assert await asyncio.to_thread(decide, gateway, "s-aaaaa", CALL) == {"result": {"allow": True}}
    assert await asyncio.to_thread(decide, gateway, "s-bbbbb", CALL) == {"result": {"allow": False}}
    deadline = time.monotonic() + 10
    while len(sent) < 2 and time.monotonic() < deadline:
        await asyncio.sleep(0.05)
    exported = {batch["session_id"]: batch["events"][0] for batch in sent}
    assert exported["s-aaaaa"]["allowed"] is True
    assert exported["s-bbbbb"]["allowed"] is False
    assert all(event["tool"] == "browser_execute" and event["id"] for event in exported.values())
