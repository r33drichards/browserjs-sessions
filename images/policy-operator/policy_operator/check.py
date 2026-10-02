"""Is this policy valid: the one implementation.

`check` is what the reconcile of a SessionPolicy runs and what
POST /v1/validate runs: size, the tenant checks of rego-contract.md, the
hash, the package rewrite, and the warnings about tools that undo each
other's rules.
"""
from __future__ import annotations

import base64
import hashlib
import json
import re
from dataclasses import dataclass, field
from typing import Any

from . import opa
from .config import EVAL_DEADLINE_SECONDS, MAX_DIAGNOSTICS, MAX_SOURCE_BYTES, Config

GUARD = "policy_guard_error"
POLICY_PACKAGE = ["data", "browserjs", "policy"]
# The session a module is rewritten for when nobody asked for one (validate):
# the rewrite is part of the verdict, so it always runs.
PLACEHOLDER_SESSION = "s-aaaaa"
IMPORT_ROOTS = ("rego", "future", "input")
# rego-contract.md. The CRD enforces it on the resource's name; the name
# goes into a package clause and two file names, so it is not taken on trust.
SESSION_ID = re.compile(r"^s-([a-z2-7]{10}|[a-z0-9]{5})$")


@dataclass
class Validation:
    ok: bool
    errors: list[dict] = field(default_factory=list)
    warnings: list[dict] = field(default_factory=list)
    # When ok: the module with package browserjs.policy, and its hash.
    rego: str | None = None
    hash: str | None = None
    # When ok: the module as it goes into the bundle for `session_id`.
    tenant_module: str | None = None
    session_id: str | None = None

    def to_api(self) -> dict:
        """The Validation of operator-api.yaml."""
        out: dict[str, Any] = {"ok": self.ok, "errors": self.errors, "warnings": self.warnings}
        if self.ok:
            out["rego"] = self.rego
            out["hash"] = self.hash
        return out


def policy_hash(rego: str) -> str:
    return "sha256:" + hashlib.sha256(rego.encode("utf-8")).hexdigest()


def diagnostic(code: str, message: str, row: int | None = None, col: int | None = None) -> dict:
    d: dict[str, Any] = {}
    if isinstance(row, int) and row >= 1:
        d["row"] = row
        if isinstance(col, int) and col >= 1:
            d["col"] = col
    d["code"] = code
    d["message"] = message
    return d


def _from_opa(errors: list[dict]) -> list[dict]:
    out = []
    for e in errors:
        loc = e.get("location") or {}
        message = str(e.get("message", "")).strip() or "error"
        out.append(diagnostic(str(e.get("code") or "rego_compile_error"), message, loc.get("row"), loc.get("col")))
    return out


def _fail(errors: list[dict]) -> Validation:
    return Validation(ok=False, errors=errors[:MAX_DIAGNOSTICS])


# --- The tenant checks (rego-contract.md, 1 to 4) -------------------------

def _loc(node: dict) -> tuple[int | None, int | None]:
    loc = node.get("location") or {}
    return loc.get("row"), loc.get("col")


def _guard(ast: dict) -> list[dict]:
    errors: list[dict] = []
    seen = set()

    def add(message: str, node: dict | None = None) -> None:
        row, col = _loc(node) if node else (None, None)
        if (message, row, col) not in seen:
            seen.add((message, row, col))
            errors.append(diagnostic(GUARD, message, row, col))

    # 1. Package.
    package = ast.get("package") or {}
    path = [t.get("value") for t in package.get("path") or []]
    if path != POLICY_PACKAGE:
        add("the package must be browserjs.policy", package)

    # 2. Imports.
    for imp in ast.get("imports") or []:
        value = (imp.get("path") or {}).get("value")
        head = value[0].get("value") if isinstance(value, list) and value else value
        if head not in IMPORT_ROOTS:
            add(f"import of {head} is not allowed: only rego, future and input can be imported", imp)

    # 3. No data, no with: anywhere in the rules or the imports.
    def walk(node) -> None:
        if isinstance(node, dict):
            if node.get("type") == "var" and node.get("value") == "data":
                add("a policy must not refer to data", node)
            modifiers = node.get("with")
            if modifiers:
                first = modifiers[0] if isinstance(modifiers, list) and isinstance(modifiers[0], dict) else node
                add("with is not allowed", first if first.get("location") else node)
            for value in node.values():
                walk(value)
        elif isinstance(node, list):
            for value in node:
                walk(value)

    walk(ast.get("rules") or [])
    walk(ast.get("imports") or [])

    # 4. The entry rule.
    def is_entry(rule: dict) -> bool:
        head = rule.get("head") or {}
        ref = head.get("ref")
        if isinstance(ref, list) and ref:
            return len(ref) == 1 and ref[0].get("value") == "allow_tool_call"
        return head.get("name") == "allow_tool_call"

    if not any(is_entry(r) for r in ast.get("rules") or [] if isinstance(r, dict)):
        add("the policy must define allow_tool_call")
    return errors


# --- The rewrite (rego-contract.md, 6) ------------------------------------

def _strip_locations(node):
    if isinstance(node, dict):
        return {k: _strip_locations(v) for k, v in node.items() if k != "location"}
    if isinstance(node, list):
        return [_strip_locations(v) for v in node]
    return node


def _offset(data: bytes, row: int, col: int) -> int:
    """The byte offset of OPA's (row, col); OPA counts columns in bytes."""
    start = 0
    for _ in range(row - 1):
        start = data.index(b"\n", start) + 1
    return start + col - 1


def rewrite_package(cfg: Config, source: str, ast: dict, session_id: str) -> str | None:
    """`source` with its package clause replaced by the session's, or None
    when the clause is not where the AST says or the result is not the same
    module in the new package.
    """
    data = source.encode("utf-8")
    package = ast["package"]
    last = package["path"][-1]
    try:
        start = _offset(data, *_loc(package))
        text = base64.b64decode(last["location"]["text"])
        end = _offset(data, *_loc(last)) + len(text)
    except (KeyError, TypeError, ValueError):
        return None
    if data[start:start + 7] != b"package" or data[end - len(text):end] != text:
        return None
    if data[end:end + 1] == b"]":  # package browserjs["policy"]
        end += 1
    clause = f'package browserjs.tenant["{session_id}"]'.encode()
    rewritten = (data[:start] + clause + data[end:]).decode("utf-8")

    # Believe nothing: the result must parse to the same rules and imports,
    # in exactly the tenant's package.
    again, errors = opa.parse(cfg.opa_bin, rewritten, locations=False)
    if errors or again is None:
        return None
    path = [t.get("value") for t in (again.get("package") or {}).get("path") or []]
    if path != ["data", "browserjs", "tenant", session_id]:
        return None
    for key in ("rules", "imports"):
        if _strip_locations(again.get(key) or []) != _strip_locations(ast.get(key) or []):
            return None
    return rewritten


# --- Warnings: tools that undo each other's rules (rego-contract.md) -------

def _call(server: str, tool: str, arguments: dict) -> dict:
    return {"operation": "mcp_call_tool", "server": server, "tool": tool, "arguments": arguments}


def _operations(tool: str, *operations: tuple[str, dict]) -> list[dict]:
    return [_call("browser", tool, {"operations": [{"type": t, "params": p}]}) for t, p in operations]


# Calls the policy is asked about, to see what it does and not what it says.
# A policy restricts the browser when it refuses one of the first; it leaves
# the desktop, or the shell, open when it allows one of the others.
BROWSER_PROBES = _operations(
    "browser_execute",
    ("evaluate", {"script": "document.title"}),
    ("setContent", {"html": "<p>probe</p>"}),
    ("navigate", {"url": "http://policy-probe.invalid/"}),
    ("click", {"selector": "a"}),
    ("type", {"selector": "input", "text": "probe"}),
)
DESKTOP_PROBES = _operations(
    "desktop_execute",
    ("mouse.click", {"x": 10, "y": 10}),
    ("keyboard.type", {"text": "probe"}),
    ("keyboard.pressKey", {"keys": ["LeftControl", "L"]}),
)


def _exec(bin: str, *args: str, **more) -> dict:
    return _call("exec", "exec", {"bin": bin, "args": list(args), "timeout": 5, **more})


# Programs that run other programs named in their arguments: allowing one
# allows every program.
LAUNCHERS = ("sh", "bash", "env", "xargs")
SHELL_PROBES = [
    _exec("curl", "-s", "http://127.0.0.1:9222/json/version"),
    _exec("policy-probe"),
]
LAUNCHER_PROBES = [_exec("sh", "-c", "id"), _exec("bash", "-c", "id"), _exec("env", "id"), _exec("xargs", "id")]
# The same calls, and a few programs a restrictive policy is likely to
# allow, with a PATH of the caller's choosing.
ENV_PROBES = [
    {**probe, "arguments": {**probe["arguments"], "env": {"PATH": "/tmp/policy-probe"}}}
    for probe in SHELL_PROBES + LAUNCHER_PROBES + [
        _exec(program, *args) for program in ("git", "ls", "cat", "pwd", "echo") for args in ((), ("status",))]
]
_GROUPS = (BROWSER_PROBES, DESKTOP_PROBES, SHELL_PROBES, LAUNCHER_PROBES, ENV_PROBES)
_PROBES = [probe for group in _GROUPS for probe in group]

WARNINGS = {
    "browser_bypass_desktop": (
        "the policy refuses some browser_execute calls but allows desktop_execute to click or type: "
        "with the mouse and keyboard an agent can use the address bar and DevTools, so the rules on "
        "browser_execute can be walked around. Deny desktop_execute, or allow only its screen operations"),
    "browser_bypass_shell": (
        "the policy refuses some browser_execute calls but allows exec to run programs that can make requests "
        "or start other programs: a program can reach the browser's own control ports on 127.0.0.1 (8081, 9222), "
        "so the rules on browser_execute can be walked around. Deny exec, or allow only programs and arguments "
        "that can neither make requests nor start programs"),
    "shell_bypass_desktop": (
        "the policy refuses some exec calls but allows desktop_execute to click or type: an agent can open "
        "a terminal on the desktop and type any command. Deny desktop_execute, or allow only its screen operations"),
    "shell_launcher_allowed": (
        "the policy refuses some programs but allows sh, bash, env or xargs: each runs whatever program its "
        "arguments name, so every program is allowed. Leave them out, or pin their arguments exactly"),
    "shell_env_allowed": (
        "the policy refuses some programs but lets a call set PATH in env: PATH decides which file a program "
        "name means, so an allowed name can be made to run anything. Refuse env, or allow only listed names "
        "that are not PATH or LD_*"),
}


def lint(cfg: Config, rego: str) -> list[dict]:
    """The warnings of a module that passed the checks. None stops a save."""
    try:
        allowed = opa.eval_many(cfg.opa_bin, cfg.capabilities, rego, _PROBES, EVAL_DEADLINE_SECONDS)
    except opa.OpaTimeout:
        return []
    if allowed is None:
        return []
    answers = iter(allowed)
    browser, desktop, programs, launchers, env = ([next(answers) for _ in group] for group in _GROUPS)
    shell_open = any(programs) or any(launchers)
    shell_restricted = not all(programs + launchers)
    codes = []
    if not all(browser) and any(desktop):
        codes.append("browser_bypass_desktop")
    if not all(browser) and shell_open:
        codes.append("browser_bypass_shell")
    if shell_restricted and any(desktop):
        codes.append("shell_bypass_desktop")
    if shell_restricted and any(launchers):
        codes.append("shell_launcher_allowed")
    if shell_restricted and any(env):
        codes.append("shell_env_allowed")
    return [diagnostic(code, WARNINGS[code]) for code in codes]


# --- check ----------------------------------------------------------------

def check(cfg: Config, kind: str, source: str, session_id: str | None = None, warn: bool = True) -> Validation:
    try:
        return _check(cfg, kind, source, session_id, warn)
    except opa.OpaTimeout as e:
        return _fail([diagnostic("rego_compile_error", str(e))])


def _check(cfg: Config, kind: str, source: str, session_id: str | None, warn: bool) -> Validation:
    if session_id is not None and not SESSION_ID.fullmatch(session_id):
        return _fail([diagnostic(GUARD, "the policy is not named after a session")])
    if kind != "rego":
        return _fail([diagnostic("schema_error", "kind must be rego")])
    if not isinstance(source, str) or not source:
        return _fail([diagnostic("size_error", "the policy is empty")])
    if len(source.encode("utf-8")) > MAX_SOURCE_BYTES:
        return _fail([diagnostic("size_error", f"the policy is larger than {MAX_SOURCE_BYTES} bytes")])
    rego = source

    ast, errors = opa.parse(cfg.opa_bin, rego)
    if errors or ast is None:
        return _fail(_from_opa(errors))
    guard = _guard(ast)
    if guard:
        return _fail(guard)
    errors = opa.check(cfg.opa_bin, cfg.capabilities, rego)
    if errors:
        return _fail(_from_opa(errors))
    sid = session_id or PLACEHOLDER_SESSION
    tenant = rewrite_package(cfg, rego, ast, sid)
    if tenant is None:
        return _fail([diagnostic(GUARD, "the package clause must be written as: package browserjs.policy", *_loc(ast["package"]))])
    return Validation(ok=True, warnings=lint(cfg, rego) if warn else [], rego=rego, hash=policy_hash(rego),
                      tenant_module=tenant, session_id=sid)


# decision-module.rego.tmpl: the servers of a session and their tools.
KNOWN_TOOLS = {
    "browser": {"browser_execute", "desktop_execute"},
    "exec": {"exec", "kill", "search_logs", "stream_logs"},
}


def _known(input_doc) -> bool:
    if not isinstance(input_doc, dict):
        return False
    server, tool = input_doc.get("server"), input_doc.get("tool")
    return isinstance(server, str) and isinstance(tool, str) and tool in KNOWN_TOOLS.get(server, ())


def evaluate(cfg: Config, kind: str, source: str, input_doc) -> dict:
    """POST /v1/evaluate: {ok, allow?, errors}. What a session would be
    answered: the policy's allow_tool_call, behind the decision module's
    refusal of servers and tools it does not know."""
    v = check(cfg, kind, source, warn=False)
    if not v.ok:
        return {"ok": False, "errors": v.errors}
    try:
        value, errors = opa.eval_rule(cfg.opa_bin, cfg.capabilities, v.rego, input_doc, EVAL_DEADLINE_SECONDS)
    except opa.OpaTimeout:
        errors, value = [{"code": "eval_timeout"}], None
    if errors:
        out = []
        for e in errors[:MAX_DIAGNOSTICS]:
            message = str(e.get("message", ""))
            code = str(e.get("code", ""))
            if code in ("eval_timeout", "eval_cancel_error") or "deadline exceeded" in message or "cancel" in message:
                out.append(diagnostic("eval_timeout", f"the evaluation took longer than {EVAL_DEADLINE_SECONDS} seconds"))
            else:
                loc = e.get("location") or {}
                out.append(diagnostic("eval_error", message or "the evaluation failed", loc.get("row"), loc.get("col")))
        return {"ok": False, "errors": out}
    # As the decision module has it: true and nothing else.
    return {"ok": True, "allow": value is True and _known(input_doc), "errors": []}
