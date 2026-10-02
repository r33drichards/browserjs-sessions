import json

import pytest

from policy_operator.translate import OPERATIONS, translate, url_regex

from conftest import EXAMPLES, example


def test_there_are_five_examples():
    assert len(EXAMPLES) == 5


@pytest.mark.parametrize("name", EXAMPLES)
def test_example_translates_byte_for_byte(name):
    rego, _ = translate(json.loads(example(name, "policy.json")))
    assert rego.encode() == example(name, "rego").encode()


@pytest.mark.parametrize("name", EXAMPLES)
def test_examples_have_no_warnings(name):
    _, warnings = translate(json.loads(example(name, "policy.json")))
    assert warnings == []


def test_star_is_all_eleven_operations():
    rego, _ = translate({"version": 1, "allow": {"operations": ["*", "click"]}})
    line = next(l for l in rego.splitlines() if l.startswith("allowed_operations"))
    assert line == "allowed_operations := {" + ", ".join(json.dumps(o) for o in sorted(OPERATIONS)) + "}"


def test_checks_are_in_parameter_order_and_table_order():
    rego, _ = translate({"version": 1, "allow": {"rules": [{"operation": "type", "constraints": {
        "text": {"allowed": ["b", "a", 2, True], "pattern": "^é\"\\d$", "max_length": 3, "min": 1.5, "max": 2},
        "delay": {"max": 10},
    }}]}})
    body = rego.split("# allow.rules[0]\n")[1]
    assert body == (
        "operation_allowed(op) if {\n"
        '\top.type == "type"\n'
        '\tis_number(op.params["delay"])\n'
        '\top.params["delay"] <= 10\n'
        '\tis_number(op.params["text"])\n'
        '\top.params["text"] >= 1.5\n'
        '\top.params["text"] <= 2\n'
        '\tis_string(op.params["text"])\n'
        '\tcount(op.params["text"]) <= 3\n'
        '\tregex.match("^é\\"\\\\d$", op.params["text"])\n'
        '\top.params["text"] in {"a", "b", 2, true}\n'
        "}\n"
    )


def test_url_regex():
    assert url_regex({"hosts": ["b.example", "*.a.example"]}) == \
        r"^(?:http|https)://(?:(?:[a-z0-9-]+\.)+a\.example|b\.example)(?::[0-9]+)?(?:[/?#].*)?$"
    assert url_regex({"schemes": ["https"]}) == r"^(?:https)://[a-z0-9.-]+(?::[0-9]+)?(?:[/?#].*)?$"


def test_deny_line_only_when_deny_is_written():
    with_deny, _ = translate({"version": 1, "allow": {"operations": ["click"], "rules": [{"operation": "press"}]},
                              "deny": {"operations": ["evaluate"]}})
    assert with_deny.count("\tnot op.type in denied_operations\n") == 2
    without, _ = translate({"version": 1, "allow": {"operations": ["click"]}, "deny": {"operations": []}})
    assert "denied_operations" not in without


def test_no_empty_set_is_written():
    rego, _ = translate({"version": 1, "allow": {"operations": [], "rules": []}, "deny": {"operations": []}})
    assert "{}" not in rego and "allowed_operations" not in rego


def codes(policy):
    return [(w["code"], w["path"]) for w in translate(policy)[1]]


def test_warnings():
    assert codes({"version": 1}) == [("allow_empty", ("allow",))]
    assert codes({"version": 1, "allow": {"operations": ["click"]}, "deny": {"operations": ["url", "click"]}}) == \
        [("denied_and_allowed", ("deny", "operations", 1))]
    assert codes({"version": 1, "allow": {"rules": [{"operation": "press"}]}, "deny": {"operations": ["press"]}}) == \
        [("denied_and_allowed", ("deny", "operations", 0))]
    assert codes({"version": 1, "allow": {"operations": ["*"], "rules": [{"operation": "press"}]}}) == \
        [("rule_shadowed", ("allow", "rules", 0))]
    assert codes({"version": 1, "allow": {"rules": [
        {"operation": "url", "constraints": {"x": {"min": 1}}},
        {"operation": "navigate", "constraints": {"url": {"hosts": ["a.b"]}, "selector": {"max_length": 1}}},
    ]}}) == [("unknown_parameter", ("allow", "rules", 0, "constraints", "x")),
             ("unknown_parameter", ("allow", "rules", 1, "constraints", "selector"))]
