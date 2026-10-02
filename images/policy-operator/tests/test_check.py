import hashlib
import json
import subprocess

import pytest

from policy_operator import opa
from policy_operator.check import BYPASS, KNOWN_TOOLS, check, evaluate, policy_hash

from conftest import EXAMPLES, H, cases, example


def decide(cfg, tmp_path, validation, sid, input_doc):
    """As OPA is asked: the tenant module, through the decision module."""
    d = tmp_path / "d"
    d.mkdir(exist_ok=True)
    (d / "tenant.rego").write_text(validation.tenant_module)
    (d / "decision.rego").write_text(cfg.decision_template.read_text().replace("{{SESSION_ID}}", sid))
    (d / "in.json").write_text(json.dumps(input_doc))
    out = subprocess.run(
        [cfg.opa_bin, "eval", "--capabilities", str(cfg.capabilities), "-d", "tenant.rego", "-d", "decision.rego",
         "-i", "in.json", "-f", "json", f'data.browserjs.decision["{sid}"].mcp_tools'],
        cwd=d, check=True, capture_output=True, text=True).stdout
    return json.loads(out)["result"][0]["expressions"][0]["value"].get("allow")


@pytest.mark.parametrize("name", EXAMPLES)
def test_every_case_of_every_example_through_real_opa(cfg, tmp_path, name):
    sid = "s-ab2cd"
    v = check(cfg, "rego", example(name, "rego"), sid)
    assert v.ok, v.errors
    # The presets are what people start from: none earns a warning.
    assert v.warnings == []
    all_cases = cases(name)
    assert all_cases
    wrong = [c["name"] for c in all_cases if decide(cfg, tmp_path, v, sid, c["input"]) != c["allow"]]
    assert wrong == []


def test_all_222_cases_are_run():
    assert EXAMPLES == ["browser-only", "form-filling", "no-scripting", "observe-only", "one-site", "read-only-shell", "unrestricted"]
    assert sum(len(cases(n)) for n in EXAMPLES) == 222


def test_examples_begin_with_what_they_are(cfg):
    # The backend takes a preset's description from its first comment.
    for name in EXAMPLES:
        first, rest = example(name, "rego").split("package browserjs.policy\n", 1)
        assert first.startswith("# ") and all(l.startswith("# ") for l in first.splitlines()), name


@pytest.mark.parametrize("name", EXAMPLES)
def test_example_rego_as_kind_rego(cfg, name):
    v = check(cfg, "rego", example(name, "rego"), "s-abcdefghij")
    assert v.ok, v.errors
    assert v.rego == example(name, "rego")
    assert v.tenant_module == example(name, "rego").replace(
        "package browserjs.policy\n", 'package browserjs.tenant["s-abcdefghij"]\n')


def test_hash_is_of_the_module_before_the_rewrite(cfg):
    src = example("one-site", "rego")
    v = check(cfg, "rego", src, "s-ab2cd")
    assert v.hash == "sha256:" + hashlib.sha256(src.encode()).hexdigest() == policy_hash(v.rego)


def test_to_api_shape(cfg):
    ok = check(cfg, "rego", H + "allow_tool_call := true\n").to_api()
    assert set(ok) == {"ok", "errors", "warnings", "rego", "hash"} and ok["ok"] is True
    bad = check(cfg, "rego", "package x\n").to_api()
    assert set(bad) == {"ok", "errors", "warnings"} and bad["ok"] is False


# --- the tenant checks ------------------------------------------------------

# spike/tenant-guard.py's corpus, and more.
REFUSED = {
    "data ref": H + 'allow_tool_call if data.browserjs.tenant["s-other"].allow_tool_call\n',
    "data alias": H + "allow_tool_call if { d := data; d.browserjs }\n",
    "import data": "package browserjs.policy\nimport rego.v1\nimport data.browserjs.tenant as t\nallow_tool_call if t\n",
    "with": H + 'allow_tool_call if { helper with input as {"tool": "x"} }\nhelper if input.tool == "x"\n',
    "with data": H + "allow_tool_call if { helper with data.x as 1 }\nhelper := true\n",
    "wrong package": 'package browserjs.decision["s-other"].mcp_tools\nimport rego.v1\nallow := true\n',
    "system pkg": "package system.authz\nimport rego.v1\nallow := true\n",
    "data in comprehension": H + "allow_tool_call if { count([x | x := data.browserjs.loaded[_]]) > 0 }\n",
    "data in rule head ref": H + "data.x := 1\nallow_tool_call := true\n",
    # More.
    "second package clause": H + "allow_tool_call := true\npackage browserjs.tenant\nx := 1\n",
    "data in a rule head": H + "allow_tool_call := true\ndata.browserjs.decision.x.mcp_tools.allow := true\n",
    "data in a default": H + "default allow_tool_call := data.x\n",
    "data in a default of a function": H + "default f(_) := data\nallow_tool_call if f(1)\n",
    "data in a function argument": H + "allow_tool_call if count(data.browserjs.loaded) > 0\n",
    "data as a function's own argument": H + "f(data) := 1\nallow_tool_call if f(1) == 1\n",
    "data in an every": H + "allow_tool_call if { every k, v in data.browserjs.tenant { v } }\n",
    "data in an every body": H + "allow_tool_call if { every x in input.xs { data.browserjs.loaded[x] } }\n",
    "with on a built-in": H + "allow_tool_call if { is_string(input.x) with is_string as helper }\nhelper(_) := true\n",
    "with in a comprehension": H + "allow_tool_call if { count([1 | helper with input.x as 1]) > 0 }\nhelper if input.x\n",
    "data in an else": H + "allow_tool_call := false if { input.x } else := data.y\n",
    "data in a set comprehension head": H + "allow_tool_call if { count({data | input.x}) > 0 }\n",
    "data as an object key": H + "allow_tool_call if { {data: 1} }\n",
    "data in a rule ref key": H + "x[data.y] := 1\nallow_tool_call := true\n",
    "data in a negation": H + "allow_tool_call if { not data.browserjs.loaded.x }\n",
    "data in a some": H + "allow_tool_call if { some k in data.browserjs.loaded; k }\n",
    "data in a call of a call": H + "allow_tool_call if { object.get(object.get(data, \"browserjs\", {}), \"loaded\", 1) }\n",
    "data bracket ref": H + 'allow_tool_call if data["browserjs"]\n',
    "data in a template-less string call": H + 'allow_tool_call if json.marshal(data) != ""\n',
    "import of data root": "package browserjs.policy\nimport rego.v1\nimport data\nallow_tool_call if data\n",
    "import of another root": "package browserjs.policy\nimport rego.v1\nimport other.x\nallow_tool_call := true\n",
    "sub-package": "package browserjs.policy.sub\nimport rego.v1\nallow_tool_call := true\n",
    "parent package": "package browserjs\nimport rego.v1\nallow_tool_call := true\n",
    "no entry rule": H + "allow := true\n",
    "entry rule only as a ref head": H + "allow_tool_call.x := true\n",
    "http.send": H + 'allow_tool_call if http.send({"method": "get", "url": "http://x"}).status_code == 200\n',
    "opa.runtime": H + "allow_tool_call if opa.runtime().env.OPERATOR_TOKEN\n",
    "numbers.range": H + "allow_tool_call if count(numbers.range(1, 1000000)) > 0\n",
    "print": H + "allow_tool_call if print(input)\n",
    "walk": H + "allow_tool_call if { walk(input, [p, v]); v == 1 }\n",
    "rego.metadata": H + "allow_tool_call if rego.metadata.rule()\n",
    "trace": H + 'allow_tool_call if trace("x")\n',
    "does not parse": H + "allow_tool_call if {\n",
    "recursion": H + "allow_tool_call if a\na if b\nb if a\n",
    "unsafe var": H + "allow_tool_call if x == y\n",
}


@pytest.mark.parametrize("name", sorted(REFUSED))
def test_hostile_modules_are_refused(cfg, name):
    v = check(cfg, "rego", REFUSED[name], "s-ab2cd")
    assert not v.ok
    assert v.errors and v.rego is None and v.hash is None and v.tenant_module is None
    for e in v.errors:
        assert set(e) <= {"row", "col", "code", "message"} and e["code"] and e["message"]


ACCEPTED = {
    "helper in its own package": H + 'allow_tool_call if { helper(input.tool) }\nhelper(t) if t == "browser_execute"\n',
    "data as a string key": H + 'allow_tool_call if input["data"] == 1\n',
    "data as a field name": H + "allow_tool_call if input.data.with == 1\n",
    "future keywords": "package browserjs.policy\nimport future.keywords.if\nimport future.keywords.in\nallow_tool_call if 1 in [1]\n",
    "import of input": "package browserjs.policy\nimport rego.v1\nimport input.arguments as args\nallow_tool_call if args.x\n",
    "reserved names": H + "allow_tool_call := true\nallow_fetch := true\nallow_module := true\n",
    "comment before package": "# mine\n\n  package browserjs.policy\n\nimport rego.v1\nallow_tool_call := true\n",
    "non-ascii before and after": "# é𝄞\npackage browserjs.policy\nimport rego.v1\nallow_tool_call if input.x == \"é𝄞\"\n",
    "the word data in a string and a comment": H + '# data.browserjs with\nallow_tool_call if input.x == "data.x with y"\n',
    "unused variable (not strict)": H + "allow_tool_call if { x := 1 }\n",
}


@pytest.mark.parametrize("name", sorted(ACCEPTED))
def test_legitimate_modules_are_accepted(cfg, name):
    src = ACCEPTED[name]
    v = check(cfg, "rego", src, "s-ab2cd")
    assert v.ok, v.errors
    assert v.rego == src
    assert v.tenant_module == src.replace("package browserjs.policy", 'package browserjs.tenant["s-ab2cd"]', 1)
    assert opa.check(cfg.opa_bin, cfg.capabilities, v.tenant_module) == []


def test_bracketed_package_is_rewritten_whole(cfg):
    v = check(cfg, "rego", 'package browserjs["policy"]\nimport rego.v1\nallow_tool_call := true\n', "s-ab2cd")
    assert v.ok, v.errors
    assert v.tenant_module == 'package browserjs.tenant["s-ab2cd"]\nimport rego.v1\nallow_tool_call := true\n'


def test_guard_errors_carry_codes_and_locations(cfg):
    v = check(cfg, "rego", H + "allow_tool_call if {\n\tinput.x\n\tdata.y\n}\n")
    assert v.errors == [{"row": 5, "col": 2, "code": "policy_guard_error", "message": "a policy must not refer to data"}]
    v = check(cfg, "rego", "package system.authz\nallow_tool_call := true\n")
    assert v.errors == [{"row": 1, "col": 1, "code": "policy_guard_error", "message": "the package must be browserjs.policy"}]
    v = check(cfg, "rego", H + "allow := true\n")
    assert v.errors == [{"code": "policy_guard_error", "message": "the policy must define allow_tool_call"}]
    v = check(cfg, "rego", H + "allow_tool_call if { helper with input as 1 }\nhelper := true\n")
    assert [(e["code"], e["message"], e["row"]) for e in v.errors] == [("policy_guard_error", "with is not allowed", 3)]


def test_opa_errors_carry_opas_codes_and_locations(cfg):
    v = check(cfg, "rego", H + 'allow_tool_call if http.send({"url": input.x})\n')
    assert v.errors == [{"row": 3, "col": 20, "code": "rego_type_error", "message": "undefined function http.send"}]
    v = check(cfg, "rego", H + "allow_tool_call if {\n")
    assert v.errors[0]["code"] == "rego_parse_error" and v.errors[0]["row"] == 4 and "col" not in v.errors[0]
    v = check(cfg, "rego", H + "allow_tool_call if a\na if b\nb if a\n")
    assert {e["code"] for e in v.errors} == {"rego_recursion_error"}
    v = check(cfg, "rego", H + "allow_tool_call if x == y\n")
    assert {e["code"] for e in v.errors} == {"rego_unsafe_var_error"}


def test_size(cfg):
    pad = "# " + "x" * 70000 + "\n"
    v = check(cfg, "rego", H + pad + "allow_tool_call := true\n")
    assert [e["code"] for e in v.errors] == ["size_error"]
    # Bytes, not characters.
    v = check(cfg, "rego", H + "# " + "é" * 33000 + "\nallow_tool_call := true\n")
    assert [e["code"] for e in v.errors] == ["size_error"]
    v = check(cfg, "rego", H + "# " + "x" * 60000 + "\nallow_tool_call := true\n")
    assert v.ok
    assert [e["code"] for e in check(cfg, "rego", "").errors] == ["size_error"]


def test_unknown_kind(cfg):
    assert not check(cfg, "yaml", "x").ok


def test_json_is_not_a_kind(cfg):
    v = check(cfg, "json", '{"version": 1, "allow": {"operations": ["*"]}}')
    assert not v.ok and v.errors == [{"code": "schema_error", "message": "kind must be rego"}]


# --- The decision module ------------------------------------------------------

def tool_call(server, tool):
    return {"operation": "mcp_call_tool", "server": server, "tool": tool, "arguments": {}}


def test_the_decision_module_refuses_servers_and_tools_it_has_not_heard_of(cfg, tmp_path):
    # Whatever the tenant says: a policy cannot allow a tool that did not
    # exist when it was written.
    v = check(cfg, "rego", H + "allow_tool_call := true\n", "s-ab2cd")
    assert v.ok, v.errors
    for server, tools in KNOWN_TOOLS.items():
        for tool in tools:
            assert decide(cfg, tmp_path, v, "s-ab2cd", tool_call(server, tool)) is True
            assert evaluate(cfg, "rego", v.rego, tool_call(server, tool))["allow"] is True
    for call in (tool_call("browser", "file_write"), tool_call("browser", "exec"), tool_call("exec", "browser_execute"),
                 tool_call("other", "browser_execute"), {"operation": "mcp_call_tool"}, tool_call(None, None),
                 tool_call(["browser"], "browser_execute"), tool_call("browser", {"browser_execute": 1})):
        assert decide(cfg, tmp_path, v, "s-ab2cd", call) is False, call
        # The editor's Test answers as a session would be answered.
        assert evaluate(cfg, "rego", v.rego, call) == {"ok": True, "allow": False, "errors": []}, call


def test_known_tools_are_the_decision_modules(cfg):
    template = cfg.decision_template.read_text(encoding="utf-8")
    for server, tools in KNOWN_TOOLS.items():
        assert f'"{server}": {{' + ", ".join(f'"{t}"' for t in sorted(tools)) + "}," in template
    assert template.count('": {"') == len(KNOWN_TOOLS)


# --- Warnings: tools that undo each other's rules -------------------------------

BROWSER_ONLY_SAFE = 'allow_tool_call if {\n\tinput.server == "browser"\n\tinput.tool == "browser_execute"\n\tevery op in input.arguments.operations { op.type != "evaluate" }\n}\n'
DESKTOP = 'allow_tool_call if input.tool == "desktop_execute"\n'
SCREEN = 'allow_tool_call if {\n\tinput.tool == "desktop_execute"\n\tevery op in input.arguments.operations { startswith(op.type, "screen.") }\n}\n'
SHELL = 'allow_tool_call if input.server == "exec"\n'
ONE_COMMAND = 'allow_tool_call if {\n\tinput.tool == "exec"\n\tinput.arguments.cmd == "git status"\n}\n'
ANY_BROWSER = 'allow_tool_call if input.tool == "browser_execute"\n'


@pytest.mark.parametrize("body, codes", [
    ("allow_tool_call := true\n", []),
    ("allow_tool_call := false\n", []),
    (ANY_BROWSER, []),
    (BROWSER_ONLY_SAFE, []),
    (BROWSER_ONLY_SAFE + SCREEN, []),
    (BROWSER_ONLY_SAFE + ONE_COMMAND, []),
    (BROWSER_ONLY_SAFE + DESKTOP, ["browser_bypass_desktop", "shell_bypass_desktop"]),
    (BROWSER_ONLY_SAFE + SHELL, ["browser_bypass_shell"]),
    (BROWSER_ONLY_SAFE + DESKTOP + SHELL, ["browser_bypass_desktop", "browser_bypass_shell"]),
    (ANY_BROWSER + DESKTOP, ["shell_bypass_desktop"]),
    (ANY_BROWSER + DESKTOP + ONE_COMMAND, ["shell_bypass_desktop"]),
    (ANY_BROWSER + SHELL, []),
    # The policy that looks at arguments and never at the tool.
    ('allow_tool_call if not "evaluate" in {op.type | some op in input.arguments.operations}\n',
     ["browser_bypass_desktop", "browser_bypass_shell"]),
])
def test_a_policy_whose_rules_can_be_walked_around_is_warned_about(cfg, body, codes):
    v = check(cfg, "rego", H + body)
    assert v.ok, v.errors
    assert [w["code"] for w in v.warnings] == codes
    assert all(set(w) == {"code", "message"} and w["message"] == BYPASS[w["code"]] for w in v.warnings)


def test_warnings_never_fail_a_policy(cfg, monkeypatch):
    import policy_operator.check as c
    monkeypatch.setattr(c.opa, "eval_many", lambda *a: None)
    assert check(cfg, "rego", H + BROWSER_ONLY_SAFE + DESKTOP).warnings == []
    def late(*a):
        raise c.opa.OpaTimeout("late")
    monkeypatch.setattr(c.opa, "eval_many", late)
    v = check(cfg, "rego", H + BROWSER_ONLY_SAFE + DESKTOP)
    assert v.ok and v.warnings == []


# --- evaluate ---------------------------------------------------------------

SAMPLE = {"operation": "mcp_call_tool", "server": "browser", "tool": "browser_execute",
          "arguments": {"operations": [{"type": "navigate", "params": {"url": "https://example.com/"}}]}}


def test_evaluate(cfg):
    assert evaluate(cfg, "rego", example("one-site", "rego"), SAMPLE) == {"ok": True, "allow": True, "errors": []}
    other = {**SAMPLE, "arguments": {"operations": [{"type": "evaluate", "params": {"script": "1"}}]}}
    assert evaluate(cfg, "rego", example("one-site", "rego"), other) == {"ok": True, "allow": False, "errors": []}
    assert evaluate(cfg, "rego", H + "allow_tool_call if input.x\n", None) == {"ok": True, "allow": False, "errors": []}


def test_evaluate_allows_only_true(cfg):
    assert evaluate(cfg, "rego", H + 'allow_tool_call := "yes"\n', SAMPLE)["allow"] is False
    assert evaluate(cfg, "rego", H + "allow_tool_call := 1\n", SAMPLE)["allow"] is False
    assert evaluate(cfg, "rego", H + "allow_tool_call := true\n", SAMPLE)["allow"] is True


def test_evaluate_invalid_policy(cfg):
    out = evaluate(cfg, "rego", H + "allow_tool_call if data.x\n", {})
    assert out["ok"] is False and "allow" not in out and out["errors"][0]["code"] == "policy_guard_error"


def test_evaluate_error(cfg):
    out = evaluate(cfg, "rego", H + 'allow_tool_call := "a"\nallow_tool_call := "b" if input.x\n', {"x": 1})
    assert out["ok"] is False and [e["code"] for e in out["errors"]] == ["eval_error"]
    assert out["errors"][0]["row"] == 4


def test_evaluate_timeout(cfg):
    slow = H + ("allow_tool_call if {\n\txs := input.xs\n"
                "\tcount([1 | a := xs[_]; b := xs[_]; c := xs[_]; d := xs[_]; e := xs[_]; f := xs[_]; g := xs[_]; a + b + c + d + e + f + g == -1]) > 0\n}\n")
    out = evaluate(cfg, "rego", slow, {"xs": list(range(40))})
    assert out["ok"] is False and [e["code"] for e in out["errors"]] == ["eval_timeout"]


@pytest.mark.parametrize("name", ["x", "s-abc", "s-ABCDE", "s-abcde.rego", 's-ab"cd', "../s-abcde", "s-abcde\n", ""])
def test_a_name_that_is_not_a_session_id_is_refused(cfg, name):
    v = check(cfg, "rego", H + "allow_tool_call := true\n", name)
    assert not v.ok and v.errors == [{"code": "policy_guard_error", "message": "the policy is not named after a session"}]
