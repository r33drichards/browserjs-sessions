"""python -m policy_operator: what the operator would publish, without a cluster.

  bundle <dir> [-o browserjs.tar.gz]   the bundle for a directory of SessionPolicy YAML files
  example-resources <dir>              the contract's examples as SessionPolicy YAML files
  run-cases <opa url>                  the examples' cases, asked of a running OPA as mcp-js asks
"""
from __future__ import annotations

import argparse
import asyncio
import io
import json
import sys
import tarfile
import urllib.request
from pathlib import Path

import yaml

from .config import GROUP, VERSION, Config
from .operator import Operator


def read_resources(directory: Path) -> list[dict]:
    resources = []
    for path in sorted(p for p in directory.iterdir() if p.suffix in (".yaml", ".yml")):
        for doc in yaml.safe_load_all(path.read_text(encoding="utf-8")):
            if isinstance(doc, dict) and doc.get("kind") == "SessionPolicy":
                resources.append(doc)
    return resources


def build_from_directory(cfg: Config, directory: Path) -> tuple[Operator, bytes]:
    """The operator after its first pass over the directory, and its bundle."""
    async def go():
        op = Operator(cfg, offline=True)
        await op.first_pass(read_resources(directory))
        await op.close()
        return op, op.publisher.body
    return asyncio.run(go())


def describe(op: Operator, body: bytes, out) -> None:
    """The bundle, readably: who is in it, and every file."""
    print(f"revision {op.publisher.revision}, etag {op.publisher.etag}, {len(body)} bytes", file=out)
    for name in sorted(op.sessions):
        entry = op.sessions[name]
        v = entry.validation
        if v.ok:
            state = f"in the bundle, {entry.tenant.hash}"
        elif entry.tenant:
            state = f"does not compile; status.rego is in the bundle, {entry.tenant.hash}"
        else:
            state = "does not compile; left out (denied)"
        print(f"{name}: {state}", file=out)
        for d in v.errors + v.warnings:
            at = ":".join(str(d[k]) for k in ("row", "col") if k in d)
            at = at + ": " if at else ""
            print(f"  {at}{d['code']}: {d['message']}", file=out)
    with tarfile.open(fileobj=io.BytesIO(body), mode="r:gz") as tar:
        for member in tar.getmembers():
            print(f"\n--- {member.name} ({member.size} bytes)", file=out)
            print(tar.extractfile(member).read().decode("utf-8").rstrip("\n"), file=out)


def example_sessions(cfg: Config) -> dict[str, str]:
    """Example name to the session ID it is given: s-exam1, s-exam2, …"""
    names = sorted(p.name[: -len(".rego")] for p in (cfg.contract_dir / "examples").glob("*.rego"))
    return {name: f"s-exam{i}" for i, name in enumerate(names, 1)}


def write_example_resources(cfg: Config, directory: Path) -> dict[str, str]:
    directory.mkdir(parents=True, exist_ok=True)
    sessions = example_sessions(cfg)
    for name, sid in sessions.items():
        source = (cfg.contract_dir / "examples" / f"{name}.rego").read_text(encoding="utf-8")
        resource = {
            "apiVersion": f"{GROUP}/{VERSION}", "kind": "SessionPolicy",
            "metadata": {"name": sid, "namespace": cfg.namespace},
            "spec": {"sessionRef": {"name": sid}, "kind": "rego", "source": source},
        }
        (directory / f"{name}.yaml").write_text(yaml.safe_dump(resource, sort_keys=False), encoding="utf-8")
    return sessions


def run_cases(cfg: Config, opa_url: str, out) -> int:
    """Every case of every example, as mcp-js asks (rego-contract.md):
    POST /v1/data/browserjs/decision/<session>/mcp_tools, allowed only when
    result.allow is true. Returns how many gave the wrong answer."""
    total = wrong = 0
    for name, sid in example_sessions(cfg).items():
        cases = json.loads((cfg.contract_dir / "examples" / f"{name}.cases.json").read_text(encoding="utf-8"))
        for case in cases:
            request = urllib.request.Request(
                f"{opa_url.rstrip('/')}/v1/data/browserjs/decision/{sid}/mcp_tools",
                data=json.dumps({"input": case["input"]}).encode(), method="POST")
            with urllib.request.urlopen(request, timeout=5) as response:
                answer = json.loads(response.read())
            allowed = (answer.get("result") or {}).get("allow") is True
            total += 1
            if allowed != case["allow"]:
                wrong += 1
                print(f"FAIL {name} ({sid}): {case['name']}: want {case['allow']}, got {allowed}", file=out)
    print(f"{total - wrong}/{total} cases pass", file=out)
    return wrong


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="python -m policy_operator", description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    b = sub.add_parser("bundle")
    b.add_argument("directory", type=Path)
    b.add_argument("-o", "--output", type=Path, help="also write the bundle here")
    e = sub.add_parser("example-resources")
    e.add_argument("directory", type=Path)
    r = sub.add_parser("run-cases")
    r.add_argument("opa_url")
    args = parser.parse_args(argv)
    cfg = Config.from_env()
    if args.command == "bundle":
        op, body = build_from_directory(cfg, args.directory)
        describe(op, body, sys.stdout)
        if args.output:
            args.output.write_bytes(body)
        return 0
    if args.command == "example-resources":
        for name, sid in write_example_resources(cfg, args.directory).items():
            print(f"{sid}  {args.directory / (name + '.yaml')}")
        return 0
    return 1 if run_cases(cfg, args.opa_url, sys.stdout) else 0
