import os

# kopf decides when it is imported whether it can log in with a kubeconfig:
# only if KUBECONFIG is set or ~/.kube/config exists. On a machine with
# neither (CI) the tests that run the operator under kopf would have no
# login handler. The fixtures point KUBECONFIG at their own file later.
os.environ.setdefault("KUBECONFIG", "/nonexistent/kubeconfig-set-by-the-test")

import json
import shutil
from pathlib import Path

import pytest

from policy_operator.config import Config

CONTRACTS = Path(__file__).resolve().parents[3] / "docs" / "contracts" / "policy"
EXAMPLES = sorted(p.name[: -len(".rego")] for p in (CONTRACTS / "examples").glob("*.rego"))


def example(name: str, suffix: str) -> str:
    return (CONTRACTS / "examples" / f"{name}.{suffix}").read_text(encoding="utf-8")


def cases(name: str) -> list[dict]:
    return json.loads(example(name, "cases.json"))


@pytest.fixture(scope="session")
def cfg() -> Config:
    opa = shutil.which("opa")
    assert opa, "the opa binary must be on PATH (nix develop provides it)"
    return Config(opa_bin=opa, contract_dir=CONTRACTS, bundle_token="bundle-secret",
                  opa_token="opa-secret", api_token="api-secret")


H = "package browserjs.policy\nimport rego.v1\n"


def free_port() -> int:
    import socket
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def resource(name: str, kind: str, source: str, status: dict | None = None, generation: int = 1) -> dict:
    body = {"apiVersion": "browserjs.dev/v1alpha1", "kind": "SessionPolicy",
            "metadata": {"name": name, "namespace": "browserjs-sessions", "generation": generation},
            "spec": {"sessionRef": {"name": name}, "kind": kind, "source": source}}
    if status is not None:
        body["status"] = status
    return body


ALLOW_ALL = H + "allow_tool_call := true\n"
DENY_ALL = H + "allow_tool_call := false\n"
BROKEN = H + "allow_tool_call if {\n"
