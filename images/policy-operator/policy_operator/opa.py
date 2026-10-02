"""The `opa` binary, as subprocesses.

Untrusted text only ever reaches OPA through here: a temporary directory,
a bounded run time, nothing inherited but PATH.
"""
from __future__ import annotations

import json
import os
import subprocess
import tempfile
from pathlib import Path

# opa parse, check and build on a 64 KiB module take tens of milliseconds.
TIMEOUT_SECONDS = 20


class OpaTimeout(Exception):
    pass


def run(opa_bin: str, args: list[str], cwd: str | Path, timeout: float = TIMEOUT_SECONDS,
        stdin: bytes | None = None) -> subprocess.CompletedProcess:
    try:
        return subprocess.run(
            [opa_bin, *args], cwd=cwd, input=stdin, capture_output=True, timeout=timeout,
            env={"PATH": os.environ.get("PATH", ""), "HOME": str(cwd)},
        )
    except subprocess.TimeoutExpired as e:
        raise OpaTimeout(f"opa {args[0]} did not finish in {timeout} s") from e


def errors_of(proc: subprocess.CompletedProcess) -> list[dict]:
    """OPA's `--format json` errors: [{message, code, location: {file, row, col}}]."""
    for stream in (proc.stdout, proc.stderr):
        try:
            doc = json.loads(stream)
        except ValueError:
            continue
        if isinstance(doc, dict) and isinstance(doc.get("errors"), list):
            return [e for e in doc["errors"] if isinstance(e, dict)]
    if proc.returncode == 0:
        return []
    # Not a verdict on the policy: opa itself could not run as asked.
    text = (proc.stderr or proc.stdout).decode("utf-8", "replace").strip().split("\n", 1)[0]
    return [{"code": "rego_compile_error", "message": text or f"opa exited with {proc.returncode}"}]


def parse(opa_bin: str, source: str, locations: bool = True) -> tuple[dict | None, list[dict]]:
    """The module's AST (None when it does not parse) and OPA's errors."""
    with tempfile.TemporaryDirectory(prefix="policy-") as d:
        Path(d, "policy.rego").write_text(source, encoding="utf-8")
        args = ["parse", "--format", "json"]
        if locations:
            args += ["--json-include", "locations"]
        proc = run(opa_bin, args + ["policy.rego"], d)
    errors = errors_of(proc)
    if errors:
        return None, errors
    try:
        ast = json.loads(proc.stdout)
    except ValueError:
        return None, [{"code": "rego_parse_error", "message": "opa parse printed no module"}]
    return ast, []


def check(opa_bin: str, capabilities: Path, source: str) -> list[dict]:
    """`opa check --capabilities` on one module: OPA's errors, empty when it compiles."""
    with tempfile.TemporaryDirectory(prefix="policy-") as d:
        Path(d, "policy.rego").write_text(source, encoding="utf-8")
        proc = run(opa_bin, ["check", "--format", "json", "--capabilities", str(capabilities), "policy.rego"], d)
    return errors_of(proc)


def eval_rule(opa_bin: str, capabilities: Path, source: str, input_doc, deadline: float) -> tuple[object, list[dict]]:
    """The value of data.browserjs.policy.allow_tool_call for one input
    (None when undefined) and OPA's errors. Raises OpaTimeout past the deadline.
    """
    with tempfile.TemporaryDirectory(prefix="policy-") as d:
        Path(d, "policy.rego").write_text(source, encoding="utf-8")
        Path(d, "input.json").write_text(json.dumps(input_doc), encoding="utf-8")
        proc = run(
            opa_bin,
            ["eval", "--format", "json", "--capabilities", str(capabilities),
             "--timeout", f"{deadline}s", "-d", "policy.rego", "-i", "input.json",
             "data.browserjs.policy.allow_tool_call"],
            d,
            # OPA's own deadline answers first; this one is for an OPA that hangs.
            timeout=deadline + 3,
        )
    errors = errors_of(proc)
    if errors:
        return None, errors
    try:
        result = json.loads(proc.stdout).get("result") or []
        return result[0]["expressions"][0]["value"], []
    except (ValueError, IndexError, KeyError, AttributeError):
        return None, []


def eval_many(opa_bin: str, capabilities: Path, source: str, inputs: list, deadline: float) -> list[bool] | None:
    """For each input, whether data.browserjs.policy.allow_tool_call is true;
    None when OPA reports an error. Raises OpaTimeout past the deadline.
    """
    query = "{i | some i, probe in input.probes; data.browserjs.policy.allow_tool_call == true with input as probe}"
    with tempfile.TemporaryDirectory(prefix="policy-") as d:
        Path(d, "policy.rego").write_text(source, encoding="utf-8")
        Path(d, "input.json").write_text(json.dumps({"probes": inputs}), encoding="utf-8")
        proc = run(
            opa_bin,
            ["eval", "--format", "json", "--capabilities", str(capabilities),
             "--timeout", f"{deadline}s", "-d", "policy.rego", "-i", "input.json", query],
            d, timeout=deadline + 3,
        )
    if errors_of(proc):
        return None
    try:
        result = json.loads(proc.stdout).get("result") or []
        allowed = set(result[0]["expressions"][0]["value"])
    except (ValueError, IndexError, KeyError, AttributeError, TypeError):
        return None
    return [i in allowed for i in range(len(inputs))]
