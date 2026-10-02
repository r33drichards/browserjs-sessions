import io
import json
import tarfile

import pytest

from policy_operator import bundle
from policy_operator.bundle import BundleError, Tenant
from policy_operator.check import check

from conftest import ALLOW_ALL, EXAMPLES, example


def tenants_of(cfg, sources: dict[str, str]) -> dict[str, Tenant]:
    out = {}
    for sid, src in sources.items():
        v = check(cfg, "rego", src, sid)
        assert v.ok, v.errors
        out[sid] = Tenant(v.tenant_module, v.hash)
    return out


def members(body: bytes) -> dict[str, bytes]:
    with tarfile.open(fileobj=io.BytesIO(body), mode="r:gz") as tar:
        return {m.name: tar.extractfile(m).read() for m in tar.getmembers()}


def test_layout_and_loaded_document(cfg):
    sources = {"s-ab2cd": example("one-site", "rego"), "s-abcdefghij": ALLOW_ALL}
    tenants = tenants_of(cfg, sources)
    files = members(bundle.build(cfg, tenants, "1700000000-7"))
    assert sorted(files) == ["/.manifest", "/data.json", "/decision/s-ab2cd.rego", "/decision/s-abcdefghij.rego",
                             "/tenant/s-ab2cd.rego", "/tenant/s-abcdefghij.rego"]
    manifest = json.loads(files["/.manifest"])
    assert manifest["roots"] == ["browserjs"] and manifest["revision"] == "1700000000-7"
    # The hash each replica reports is the hash of status.rego.
    assert json.loads(files["/data.json"]) == {"browserjs": {"loaded": {
        sid: check(cfg, "rego", src).hash for sid, src in sources.items()}}}
    # opa build writes each module formatted, so not byte for byte the source.
    tenant = files["/tenant/s-ab2cd.rego"].decode()
    assert 'package browserjs.tenant["s-ab2cd"]\n' in tenant and "browserjs.policy" not in tenant
    decision = files["/decision/s-ab2cd.rego"].decode()
    assert 'package browserjs.decision["s-ab2cd"].mcp_tools\n' in decision
    assert 'data.browserjs.tenant["s-ab2cd"].allow_tool_call == true\n' in decision


def test_byte_stable_for_the_same_input(cfg):
    sources = {f"s-exam{i}": example(n, "rego") for i, n in enumerate(EXAMPLES)}
    a = bundle.build(cfg, tenants_of(cfg, sources), "1-1")
    b = bundle.build(cfg, tenants_of(cfg, dict(reversed(list(sources.items())))), "1-1")
    assert a == b
    assert bundle.build(cfg, tenants_of(cfg, sources), "1-2") != a


def test_no_sessions_is_still_a_bundle(cfg):
    files = members(bundle.build(cfg, {}, "1-1"))
    assert json.loads(files["/data.json"]) == {"browserjs": {"loaded": {}}}


def test_the_build_is_under_the_capabilities_file(cfg):
    # A module that never went through check: opa build itself refuses it.
    hostile = 'package browserjs.tenant["s-ab2cd"]\nimport rego.v1\nallow_tool_call if http.send({"url": "x"})\n'
    with pytest.raises(BundleError) as e:
        bundle.build(cfg, {"s-ab2cd": Tenant(hostile, "sha256:x"), **tenants_of(cfg, {"s-zzzzz": ALLOW_ALL})}, "1-1")
    assert e.value.errors == [{"code": "rego_type_error", "session": "s-ab2cd",
                               "message": "tenant/s-ab2cd.rego:3: undefined function http.send"}]
    assert e.value.sessions() == {"s-ab2cd"}
