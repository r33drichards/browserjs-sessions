"""One run_js call to a session's mcp-js, as an MCP client would make it,
and what came back. Python's standard library only.

  mcp_call.py http://127.0.0.1:18080 <operation type> [count | <seconds>s]

The code run in the session calls the browser tool (or, with
TOOL=desktop_execute in the environment, that one) with one operation of
that type and prints what it got, or what was thrown: that is what an
agent's code sees. With SERVER=exec it calls the exec server's `exec` tool
instead, and the second argument is the command. Prints one JSON line a call: {"outcome": "ran" | "denied"
| "error", "seconds": <the whole run_js call>, "seen": <the text>}.
"""
import json
import os
import sys
import time
import urllib.request

base, operation = sys.argv[1], sys.argv[2]
repeat = sys.argv[3] if len(sys.argv) > 3 else "1"
# "20s": call after call for that long.
deadline = time.time() + float(repeat[:-1]) if repeat.endswith("s") else None
count = 10 ** 9 if deadline else int(repeat)

if os.environ.get("SERVER") == "exec":
    target = '"exec", "exec", %s' % json.dumps({"cmd": operation, "timeout": 5})
else:
    target = '"browser", %s, { operations: [{ type: %s, params: {} }] }' % (
        json.dumps(os.environ.get("TOOL", "browser_execute")), json.dumps(operation))
CODE = """
const started = Date.now();
try {
  const r = await mcp.callTool(%s);
  console.log(JSON.stringify({ returned: r, ms: Date.now() - started }));
} catch (e) {
  console.log(JSON.stringify({ thrown: String((e && e.message) || e), ms: Date.now() - started }));
}
""" % target

session, ids = None, 0


def post(message, timeout=60):
    global session
    headers = {"Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
    if session:
        headers["Mcp-Session-Id"] = session
    request = urllib.request.Request(base + "/mcp", json.dumps(message).encode(), headers)
    with urllib.request.urlopen(request, timeout=timeout) as response:
        session = response.headers.get("Mcp-Session-Id") or session
        return response.headers.get("Content-Type", ""), response.read().decode()


def rpc(method, params):
    global ids
    ids += 1
    ctype, text = post({"jsonrpc": "2.0", "id": ids, "method": method, "params": params})
    if ctype.startswith("text/event-stream"):
        replies = [json.loads(line[5:]) for line in text.splitlines() if line.startswith("data:") and line[5:].strip()]
        reply = [r for r in replies if r.get("id") == ids][-1]
    else:
        reply = json.loads(text)
    if "error" in reply:
        raise RuntimeError("%s: %s" % (method, reply["error"]))
    return reply["result"]


rpc("initialize", {"protocolVersion": "2025-03-26", "capabilities": {},
                   "clientInfo": {"name": "browserjs-policy-test", "version": "0"}})
post({"jsonrpc": "2.0", "method": "notifications/initialized"})
for _ in range(count):
    if deadline and time.time() > deadline:
        break
    started = time.time()
    try:
        result = rpc("tools/call", {"name": "run_js", "arguments": {"code": CODE}})
        seen = "\n".join(c.get("text", "") for c in result.get("content", []))
        if "stub browser ran" in seen or "stub exec ran" in seen:
            outcome = "ran"
        elif result.get("isError") or "thrown" in seen or "isError" in seen:
            outcome = "denied"
        else:
            outcome = "error"
    except Exception as e:  # the call itself failed: not a decision
        seen, outcome = repr(e), "error"
    print(json.dumps({"outcome": outcome, "seconds": round(time.time() - started, 3), "seen": seen[:600]}), flush=True)
