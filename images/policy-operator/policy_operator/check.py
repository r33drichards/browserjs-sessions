"""Is this policy valid: the one implementation.

`check` is what the reconcile of a SessionPolicy runs and what
POST /v1/validate runs: size, JSON schema, JSON to Rego, the tenant checks
of rego-contract.md, the hash, the package rewrite.
"""
from __future__ import annotations

import base64
import functools
import hashlib
import json
import math
import re
from dataclasses import dataclass, field
from typing import Any

import jsonschema

from . import opa
from .config import EVAL_DEADLINE_SECONDS, MAX_DIAGNOSTICS, MAX_SOURCE_BYTES, Config
from .jsonpos import locate, positions
from .translate import translate

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


def _from_opa(errors: list[dict], positioned: bool) -> list[dict]:
    out = []
    for e in errors:
        loc = e.get("location") or {}
        message = str(e.get("message", "")).strip() or "error"
        code = str(e.get("code") or "rego_compile_error")
        if positioned:
            out.append(diagnostic(code, message, loc.get("row"), loc.get("col")))
        else:
            # The rows of generated Rego mean nothing in the JSON it came from.
            out.append(diagnostic(code, "in the generated Rego: " + message))
    return out


def _fail(errors: list[dict]) -> Validation:
    return Validation(ok=False, errors=errors[:MAX_DIAGNOSTICS])


@functools.lru_cache(maxsize=4)
def _validator(schema_path: str) -> jsonschema.Draft202012Validator:
    with open(schema_path, encoding="utf-8") as f:
        schema = json.load(f)
    jsonschema.Draft202012Validator.check_schema(schema)
    return jsonschema.Draft202012Validator(schema)


def _path_text(path) -> str:
    out = ""
    for p in path:
        out += f"[{p}]" if isinstance(p, int) else ("." if out else "") + str(p)
    return out


def _reject_constant(name: str):
    raise ValueError(f"{name} is not JSON")


def _finite(text: str) -> float:
    value = float(text)
    if not math.isfinite(value):
        raise ValueError(f"the number {text} is out of range")
    return value


def _parse_json(cfg: Config, source: str) -> tuple[dict | None, dict, list[dict]]:
    """The document, the positions of its values, and the errors."""
    try:
        doc = json.loads(source, parse_constant=_reject_constant, parse_float=_finite)
    except json.JSONDecodeError as e:
        return None, {}, [diagnostic("json_parse_error", e.msg, e.lineno, e.colno)]
    except (ValueError, RecursionError) as e:
        return None, {}, [diagnostic("json_parse_error", str(e) or "the document is nested too deeply")]
    pos = positions(source)
    errors = []
    found = sorted(_validator(str(cfg.schema)).iter_errors(doc), key=lambda e: (list(map(str, e.absolute_path)), e.message))
    for e in found:
        where = _path_text(e.absolute_path)
        message = e.message if len(e.message) <= 300 else e.message[:300] + "…"
        at = locate(pos, tuple(e.absolute_path)) or (None, None)
        errors.append(diagnostic("schema_error", f"{where}: {message}" if where else message, *at))
    return doc, pos, errors


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


# --- check ----------------------------------------------------------------

def check(cfg: Config, kind: str, source: str, session_id: str | None = None) -> Validation:
    try:
        return _check(cfg, kind, source, session_id)
    except opa.OpaTimeout as e:
        return _fail([diagnostic("rego_compile_error", str(e))])


def _check(cfg: Config, kind: str, source: str, session_id: str | None) -> Validation:
    if session_id is not None and not SESSION_ID.fullmatch(session_id):
        return _fail([diagnostic(GUARD, "the policy is not named after a session")])
    if kind not in ("json", "rego"):
        return _fail([diagnostic("schema_error", "kind must be json or rego")])
    if not isinstance(source, str) or not source:
        return _fail([diagnostic("size_error", "the policy is empty")])
    if len(source.encode("utf-8")) > MAX_SOURCE_BYTES:
        return _fail([diagnostic("size_error", f"the policy is larger than {MAX_SOURCE_BYTES} bytes")])

    warnings: list[dict] = []
    positioned = kind == "rego"
    if kind == "json":
        doc, pos, errors = _parse_json(cfg, source)
        if errors:
            return _fail(errors)
        rego, found = translate(doc)
        for w in found[:MAX_DIAGNOSTICS]:
            warnings.append(diagnostic(w["code"], w["message"], *(locate(pos, w["path"]) or (None, None))))
        if len(rego.encode("utf-8")) > MAX_SOURCE_BYTES:
            return _fail([diagnostic("size_error", f"the Rego generated from the policy is larger than {MAX_SOURCE_BYTES} bytes")])
    else:
        rego = source

    ast, errors = opa.parse(cfg.opa_bin, rego)
    if errors or ast is None:
        return _fail(_from_opa(errors, positioned))
    guard = _guard(ast)
    if guard:
        if not positioned:
            guard = [diagnostic(GUARD, "in the generated Rego: " + g["message"]) for g in guard]
        return _fail(guard)
    errors = opa.check(cfg.opa_bin, cfg.capabilities, rego)
    if errors:
        return _fail(_from_opa(errors, positioned))
    sid = session_id or PLACEHOLDER_SESSION
    tenant = rewrite_package(cfg, rego, ast, sid)
    if tenant is None:
        row, col = _loc(ast["package"])
        return _fail([diagnostic(GUARD, "the package clause must be written as: package browserjs.policy",
                                 *((row, col) if positioned else (None, None)))])
    return Validation(ok=True, warnings=warnings, rego=rego, hash=policy_hash(rego),
                      tenant_module=tenant, session_id=sid)


def evaluate(cfg: Config, kind: str, source: str, input_doc) -> dict:
    """POST /v1/evaluate: {ok, allow?, errors}."""
    v = check(cfg, kind, source)
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
                at = (loc.get("row"), loc.get("col")) if kind == "rego" else (None, None)
                out.append(diagnostic("eval_error", message or "the evaluation failed", *at))
        return {"ok": False, "errors": out}
    # As the decision module has it: true and nothing else.
    return {"ok": True, "allow": value is True, "errors": []}
