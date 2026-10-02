# Runs every examples/<name>.cases.json against examples/<name>.rego the way
# the cluster does: the module rewritten into a tenant package, queried
# through the generated decision module, checked under the capabilities file.
import glob, json, os, subprocess, sys, tempfile
opa, d = sys.argv[1], sys.argv[2]
sid = "s-ab2cd"; bad = 0; total = 0
tmpl = open(os.path.join(d, "decision-module.rego.tmpl")).read()
for cases in sorted(glob.glob(os.path.join(d, "examples", "*.cases.json"))):
    name = os.path.basename(cases)[:-len(".cases.json")]
    rego = open(os.path.join(d, "examples", name + ".rego")).read()
    assert rego.count("package browserjs.policy\n") == 1
    with tempfile.TemporaryDirectory() as t:
        open(t + "/tenant.rego", "w").write(rego.replace("package browserjs.policy\n", f'package browserjs.tenant["{sid}"]\n'))
        open(t + "/decision.rego", "w").write(tmpl.replace("{{SESSION_ID}}", sid))
        subprocess.run([opa, "check", "--strict", "--capabilities", d + "/capabilities.json", t], check=True)
        for c in json.load(open(cases)):
            total += 1
            open(t + "/in.json", "w").write(json.dumps(c["input"]))
            out = json.loads(subprocess.run([opa, "eval", "--capabilities", d + "/capabilities.json", "-d", t + "/tenant.rego", "-d", t + "/decision.rego", "-i", t + "/in.json", "-f", "json", f'data.browserjs.decision["{sid}"].mcp_tools'], check=True, capture_output=True, text=True).stdout)
            got = out["result"][0]["expressions"][0]["value"].get("allow")
            if got != c["allow"]: bad += 1; print(f"FAIL {name}: {c['name']}: want {c['allow']}, got {got}")
print(f"{total - bad}/{total} cases pass"); sys.exit(1 if bad else 0)
