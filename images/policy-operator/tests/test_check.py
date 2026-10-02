import hashlib
import json
import subprocess

import pytest

from policy_operator import opa
from policy_operator.check import check, evaluate, policy_hash
from policy_operator.jsonpos import positions

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
    v = check(cfg, "json", example(name, "policy.json"), sid)
    assert v.ok, v.errors
    assert v.rego == example(name, "rego")
    all_cases = cases(name)
    assert all_cases
    wrong = [c["name"] for c in all_cases if decide(cfg, tmp_path, v, sid, c["input"]) != c["allow"]]
    assert wrong == []


def test_all_86_cases_are_run():
    assert sum(len(cases(n)) for n in EXAMPLES) == 86


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
    j = check(cfg, "json", example("one-site", "policy.json"), "s-ab2cd")
    assert j.hash == v.hash


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


# --- kind json --------------------------------------------------------------

def test_json_parse_error_has_a_position(cfg):
    v = check(cfg, "json", '{\n  "version": 1,\n  "allow": [,]\n}')
    assert v.errors == [{"row": 3, "col": 13, "code": "json_parse_error", "message": "Expecting value"}]
    assert [e["code"] for e in check(cfg, "json", '{"version": NaN}').errors] == ["json_parse_error"]
    assert [e["code"] for e in check(cfg, "json", '{"version": 1, "allow": {"rules": [{"operation": "wait", "constraints": {"ms": {"max": 1e999}}}]}}').errors] == ["json_parse_error"]
    assert [e["code"] for e in check(cfg, "json", "[" * 30000).errors] == ["json_parse_error"]


def test_schema_errors_point_into_the_json(cfg):
    src = '{\n  "version": 1,\n  "allow": {\n    "rules": [\n      {"operation": "fly"}\n    ]\n  },\n  "extra": true\n}'
    v = check(cfg, "json", src)
    assert not v.ok and {e["code"] for e in v.errors} == {"schema_error"}
    by_message = {e["message"].split(":")[0]: (e["row"], e["col"]) for e in v.errors}
    assert by_message["allow.rules[0].operation"] == (5, 8)
    assert (1, 1) in by_message.values()  # the additional property, reported on the document
    assert [e["code"] for e in check(cfg, "json", "[]").errors] == ["schema_error"]
    assert [e["code"] for e in check(cfg, "json", '{"version": 2}').errors] == ["schema_error"]


def test_positions():
    pos = positions('{"a": [1, {"b": null}],\n "c": "x"}')
    assert pos[()] == (1, 1) and pos[("a",)] == (1, 2) and pos[("a", 0)] == (1, 8)
    assert pos[("a", 1, "b")] == (1, 12) and pos[("c",)] == (2, 2)


def test_warnings_do_not_stop_a_policy_and_point_into_the_json(cfg, tmp_path):
    src = '{"version": 1,\n "allow": {"operations": ["*"],\n  "rules": [{"operation": "url", "constraints": {"x": {"min": 1}}}]},\n "deny": {"operations": ["evaluate"]}}'
    v = check(cfg, "json", src, "s-ab2cd")
    assert v.ok, v.errors
    assert [(w["code"], w["row"]) for w in v.warnings] == [("rule_shadowed", 3), ("unknown_parameter", 3)]


@pytest.mark.parametrize("source", ['{"version": 1}', '{"version": 1, "deny": {"operations": ["evaluate"]}}',
                                    '{"version": 1, "allow": {"operations": [], "rules": []}}'])
def test_a_policy_that_allows_nothing_compiles_and_denies(cfg, tmp_path, source):
    v = check(cfg, "json", source, "s-ab2cd")
    assert v.ok, v.errors
    assert [w["code"] for w in v.warnings] == ["allow_empty"]
    call = {"operation": "mcp_call_tool", "server": "browser", "tool": "browser_execute"}
    assert decide(cfg, tmp_path, v, "s-ab2cd", {**call, "arguments": {"operations": [{"type": "url"}]}}) is False
    assert decide(cfg, tmp_path, v, "s-ab2cd", {**call, "arguments": None}) is False
    # json-to-rego.md: an empty list of operations is allowed (it does nothing).
    assert decide(cfg, tmp_path, v, "s-ab2cd", {**call, "arguments": {"operations": []}}) is True


def test_generated_rego_errors_have_no_position(cfg):
    # Nothing the schema accepts is known to produce Rego that fails, so force it.
    import policy_operator.check as c
    real = c.translate
    c.translate = lambda doc: ("package browserjs.policy\nimport rego.v1\nallow_tool_call if http.send({})\n", [])
    try:
        v = check(cfg, "json", '{"version": 1}')
    finally:
        c.translate = real
    assert v.errors == [{"code": "rego_type_error", "message": "in the generated Rego: undefined function http.send"}]


# --- evaluate ---------------------------------------------------------------

SAMPLE = {"operation": "mcp_call_tool", "server": "browser", "tool": "browser_execute",
          "arguments": {"operations": [{"type": "navigate", "params": {"url": "https://example.com/"}}]}}


def test_evaluate(cfg):
    assert evaluate(cfg, "json", example("one-site", "policy.json"), SAMPLE) == {"ok": True, "allow": True, "errors": []}
    other = {**SAMPLE, "arguments": {"operations": [{"type": "evaluate", "params": {"script": "1"}}]}}
    assert evaluate(cfg, "json", example("one-site", "policy.json"), other) == {"ok": True, "allow": False, "errors": []}
    assert evaluate(cfg, "rego", H + "allow_tool_call if input.x\n", None) == {"ok": True, "allow": False, "errors": []}


def test_evaluate_allows_only_true(cfg):
    assert evaluate(cfg, "rego", H + 'allow_tool_call := "yes"\n', {})["allow"] is False
    assert evaluate(cfg, "rego", H + "allow_tool_call := 1\n", {})["allow"] is False
    assert evaluate(cfg, "rego", H + "allow_tool_call := true\n", {})["allow"] is True


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
