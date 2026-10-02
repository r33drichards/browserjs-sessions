# Runs every tools/examples/<name>.cases.json against tools/examples/<name>.rego
# the way the cluster does (rego-contract.md): the operator's tenant checks
# (package, imports, no `data`, no `with`, the entry rule), `opa check` under
# the capabilities file, the module rewritten into a tenant package, and each
# case asked of the generated decision module. A case with `shell` is also
# given to a real `sh`, which must split its command into exactly the words
# listed: evidence that a command line a policy accepts as "one program with
# plain arguments" is that to the shell too.
#
#   python3 docs/contracts/policy/tools/run-cases.py <opa> docs/contracts/policy
import glob, json, os, subprocess, sys, tempfile
opa, d = sys.argv[1], sys.argv[2]
sid = "s-ab2cd"; bad = 0; total = 0
caps = os.path.join(d, "capabilities.json")
tmpl = open(os.path.join(d, "decision-module.rego.tmpl")).read()

def shell_words(command):
    # `set -- <command>` makes the shell parse the text as the words of one
    # simple command; anything it would expand, split or run differently
    # shows up as different words (or as output before the marker).
    with tempfile.TemporaryDirectory() as empty:
        out = subprocess.run(["sh", "-c", "set -- " + command + "\nprintf 'WORDS'; printf '\\0%s' \"$@\""], cwd=empty, capture_output=True, timeout=10)
    head, _, rest = out.stdout.partition(b"WORDS")
    if out.returncode or head or out.stderr: return None
    return [w.decode() for w in rest.split(b"\0")[1:]]

def guard(path):
    ast = json.loads(subprocess.run([opa, "parse", "--format", "json", "--json-include", "locations", path], check=True, capture_output=True, text=True).stdout)
    errs = []
    if [t["value"] for t in ast["package"]["path"]] != ["data", "browserjs", "policy"]: errs.append("the package must be browserjs.policy")
    for imp in ast.get("imports", []):
        if imp["path"]["value"][0]["value"] not in ("rego", "future", "input"): errs.append("import not allowed")
    def walk(n):
        if isinstance(n, dict):
            if n.get("type") == "var" and n.get("value") == "data": errs.append("reference to data")
            if n.get("with"): errs.append("with not allowed")
            for v in n.values(): walk(v)
        elif isinstance(n, list):
            for v in n: walk(v)
    walk(ast.get("rules", [])); walk(ast.get("imports", []))
    names = {r["head"]["ref"][0]["value"] for r in ast.get("rules", [])}
    if "allow_tool_call" not in names: errs.append("the policy must define allow_tool_call")
    return errs

for cases in sorted(glob.glob(os.path.join(d, "tools", "examples", "*.cases.json"))):
    name = os.path.basename(cases)[:-len(".cases.json")]
    src = os.path.join(d, "tools", "examples", name + ".rego")
    rego = open(src).read()
    assert len(rego.encode()) <= 65536
    assert rego.count("package browserjs.policy\n") == 1
    errs = guard(src)
    assert not errs, (name, errs)
    subprocess.run([opa, "check", "--capabilities", caps, src], check=True)
    n = 0; failed = 0; shells = 0
    with tempfile.TemporaryDirectory() as t:
        open(t + "/tenant.rego", "w").write(rego.replace("package browserjs.policy\n", f'package browserjs.tenant["{sid}"]\n'))
        open(t + "/decision.rego", "w").write(tmpl.replace("{{SESSION_ID}}", sid))
        subprocess.run([opa, "check", "--strict", "--capabilities", caps, t], check=True)
        for c in json.load(open(cases)):
            n += 1
            open(t + "/in.json", "w").write(json.dumps(c["input"]))
            out = json.loads(subprocess.run([opa, "eval", "--capabilities", caps, "-d", t + "/tenant.rego", "-d", t + "/decision.rego", "-i", t + "/in.json", "-f", "json", f'data.browserjs.decision["{sid}"].mcp_tools'], check=True, capture_output=True, text=True).stdout)
            got = out["result"][0]["expressions"][0]["value"].get("allow")
            if got != c["allow"]: failed += 1; print(f"FAIL {name}: {c['name']}: want {c['allow']}, got {got}")
            if "shell" in c:
                shells += 1
                words = shell_words(c["shell"]["command"])
                if words != c["shell"]["argv"]: failed += 1; print(f"FAIL {name}: {c['name']}: sh splits it into {words}, not {c['shell']['argv']}")
    print(f"{name}: {n - failed}/{n} cases pass ({sum(1 for c in json.load(open(cases)) if c['allow'])} allow, {sum(1 for c in json.load(open(cases)) if not c['allow'])} deny; {shells} checked against sh)")
    total += n; bad += failed
print(f"{total - bad}/{total} cases pass"); sys.exit(1 if bad else 0)
