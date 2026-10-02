"""JSON policy to Rego: docs/contracts/policy/json-to-rego.md, to the byte."""
from __future__ import annotations

import json
from typing import Any

# json-policy.schema.json, $defs.operation.
OPERATIONS = (
    "click", "evaluate", "navigate", "press", "screenshot", "select",
    "setContent", "setViewport", "type", "url", "wait",
)

# json-to-rego.md: the parameters of each operation.
PARAMETERS = {
    "setViewport": ("width", "height"),
    "navigate": ("url", "waitUntil"),
    "setContent": ("html",),
    "wait": ("ms", "selector"),
    "screenshot": ("fullPage",),
    "evaluate": ("script",),
    "click": ("selector",),
    "type": ("selector", "text", "delay"),
    "press": ("key",),
    "select": ("selector", "values"),
    "url": (),
}

HEADER = """\
# Generated from a browserjs JSON policy (version 1). Edit the JSON, not this file.
package browserjs.policy

import rego.v1"""

ENTRY = """\
allow_tool_call if {
	input.server == "browser"
	input.tool == "browser_execute"
	is_array(input.arguments.operations)
	every op in input.arguments.operations {
		operation_allowed(op)
	}
}"""

# Not in json-to-rego.md, which writes no operation_allowed at all when
# nothing is allowed; the module then does not compile ("undefined function
# operation_allowed"), although allow_empty is a warning. See
# docs/policy-operator.md, "Contract deviations".
NOTHING_ALLOWED = """\
# Nothing is allowed.
operation_allowed(_) := false"""

_STRING_KEYS = ("max_length", "pattern", "hosts", "schemes")


def _j(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False)


def _set(values) -> str:
    return "{" + ", ".join(sorted(_j(v) for v in values)) + "}"


def url_regex(constraint: dict) -> str:
    schemes = sorted(constraint.get("schemes") or ["http", "https"])
    hosts = constraint.get("hosts")
    if hosts:
        alternatives = []
        for host in sorted(hosts):
            if host.startswith("*."):
                alternatives.append(r"(?:[a-z0-9-]+\.)+" + host[2:].replace(".", r"\."))
            else:
                alternatives.append(host.replace(".", r"\."))
        host_re = "(?:" + "|".join(alternatives) + ")"
    else:
        host_re = "[a-z0-9.-]+"
    return "^(?:" + "|".join(schemes) + ")://" + host_re + "(?::[0-9]+)?(?:[/?#].*)?$"


def _checks(name: str, c: dict) -> list[str]:
    p = f"op.params[{_j(name)}]"
    lines = []
    if "min" in c or "max" in c:
        lines.append(f"is_number({p})")
        if "min" in c:
            lines.append(f"{p} >= {_j(c['min'])}")
        if "max" in c:
            lines.append(f"{p} <= {_j(c['max'])}")
    if any(k in c for k in _STRING_KEYS):
        lines.append(f"is_string({p})")
        if "max_length" in c:
            lines.append(f"count({p}) <= {_j(c['max_length'])}")
        if "pattern" in c:
            lines.append(f"regex.match({_j(c['pattern'])}, {p})")
        if "hosts" in c or "schemes" in c:
            lines.append(f"regex.match({_j(url_regex(c))}, lower({p}))")
    if "allowed" in c:
        lines.append(f"{p} in {_set(c['allowed'])}")
    return lines


def translate(policy: dict) -> tuple[str, list[dict]]:
    """The Rego module of a policy valid against json-policy.schema.json,
    and the warnings: dicts of code, message and path (into the document).
    """
    allow = policy.get("allow") or {}
    allowed_in = allow.get("operations") or []
    rules = allow.get("rules") or []
    denied = (policy.get("deny") or {}).get("operations") or []
    allowed = set(OPERATIONS) if "*" in allowed_in else set(allowed_in)

    deny_line = ["not op.type in denied_operations"] if denied else []

    def rule(lines: list[str], comment: str | None = None) -> str:
        head = [comment] if comment else []
        return "\n".join(head + ["operation_allowed(op) if {"] + ["\t" + l for l in lines] + ["}"])

    blocks = [HEADER, ENTRY]
    if denied:
        blocks.append("denied_operations := " + _set(denied))
    if allowed:
        blocks.append("allowed_operations := " + _set(allowed))
        blocks.append(rule(deny_line + ["op.type in allowed_operations"]))
    for i, r in enumerate(rules):
        lines = deny_line + [f"op.type == {_j(r['operation'])}"]
        constraints = r.get("constraints") or {}
        for name in sorted(constraints):
            lines += _checks(name, constraints[name])
        blocks.append(rule(lines, f"# allow.rules[{i}]"))
    if not allowed and not rules:
        blocks.append(NOTHING_ALLOWED)

    warnings = []

    def warn(code: str, message: str, *path) -> None:
        warnings.append({"code": code, "message": message, "path": path})

    if not allowed and not rules:
        warn("allow_empty", "nothing is allowed: the policy has no allow.operations and no allow.rules", "allow")
    for i, op in enumerate(denied):
        # Named in allow, that is: "*" with a deny list is how "everything
        # but" is written (examples/no-scripting), and is no mistake.
        if op in allowed_in or any(r["operation"] == op for r in rules):
            warn("denied_and_allowed", f"{op} is in deny.operations and in allow; deny wins", "deny", "operations", i)
    for i, r in enumerate(rules):
        op = r["operation"]
        if op in allowed:
            warn("rule_shadowed", f"allow.rules[{i}]: {op} is also in allow.operations, so the rule adds nothing", "allow", "rules", i)
        for name in sorted(r.get("constraints") or {}):
            if name not in PARAMETERS[op]:
                warn("unknown_parameter", f"allow.rules[{i}]: {op} takes no parameter {name}", "allow", "rules", i, "constraints", name)
    return "\n\n".join(blocks) + "\n", warnings
