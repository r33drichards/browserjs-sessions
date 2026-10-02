import json, subprocess, sys
OPA = sys.argv[1]
def guard(src):
    p = subprocess.run([OPA, "parse", "--format", "json", "--json-include", "locations", "/dev/stdin"], input=src, text=True, capture_output=True)
    if p.returncode: return ["parse error"]
    ast = json.loads(p.stdout); errs = []
    path = [t["value"] for t in ast["package"]["path"]]
    if path != ["data", "browserjs", "policy"]: errs.append("package must be browserjs.policy")
    for imp in ast.get("imports", []):
        v = imp["path"]["value"]; head = v[0]["value"]
        if head not in ("rego", "future", "input"): errs.append(f"import of {head} not allowed")
    def walk(n, in_pkg=False):
        if isinstance(n, dict):
            if n.get("type") == "var" and n.get("value") == "data": errs.append("reference to data")
            if "with" in n and n["with"]: errs.append("with not allowed")
            for k, v in n.items(): walk(v)
        elif isinstance(n, list):
            for v in n: walk(v)
    walk(ast.get("rules", [])); walk(ast.get("imports", []))
    return sorted(set(errs))
H = "package browserjs.policy\nimport rego.v1\n"
cases = {
 "ok": H + 'allow_tool_call if { helper(input.tool) }\nhelper(t) if t == "browser_execute"\n',
 "data ref": H + 'allow_tool_call if data.browserjs.tenant["s-other"].allow_tool_call\n',
 "data alias": H + 'allow_tool_call if { d := data; d.browserjs }\n',
 "import data": "package browserjs.policy\nimport rego.v1\nimport data.browserjs.tenant as t\nallow_tool_call if t\n",
 "with": H + 'allow_tool_call if { helper with input as {"tool": "x"} }\nhelper if input.tool == "x"\n',
 "with data": H + 'allow_tool_call if { helper with data.x as 1 }\nhelper := true\n',
 "wrong package": 'package browserjs.decision["s-other"].mcp_tools\nimport rego.v1\nallow := true\n',
 "system pkg": 'package system.authz\nimport rego.v1\nallow := true\n',
 "data in comprehension": H + 'allow_tool_call if { count([x | x := data.browserjs.loaded[_]]) > 0 }\n',
 "data in rule head ref": H + 'data.x := 1\nallow_tool_call := true\n',
 "data as string key (fine)": H + 'allow_tool_call if input["data"] == 1\n',
}
for k, v in cases.items(): print(f"{k:28} {guard(v) or 'accepted'}")
