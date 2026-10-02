"""Stand-ins for what test/policy/run.sh does not run for real. Python's
standard library only; one file, mounted into three pods.

  stub.py operator   the policy operator, as OPA sees it: the bundle endpoint
                     of docs/contracts/policy/operator-api.yaml on 8080
                     (bearer token, ETag, long polling, 503 until there is a
                     bundle), /readyz, and kopf's liveness on 8081. The
                     bundle is the file /tmp/bundle.tar.gz, which the test
                     writes.
  stub.py browser    the browser container, as mcp-js sees it: an MCP server
                     named "browser" on 8081 with one tool, browser_execute,
                     and desktop_execute beside it, which run nothing and
                     say what they were asked.
                     Beside it on 8082, the "exec" server (mcp-exec), the
                     second server mcp-js connects to at start: its three
                     tools, which run nothing either.
  stub.py backend    something listening where the backend does, on 8080.
"""
import hashlib
import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

BUNDLE = "/tmp/bundle.tar.gz"
cond = threading.Condition()
state = {"etag": None, "body": None}


def watch_bundle():
    while True:
        try:
            with open(BUNDLE, "rb") as f:
                body = f.read()
            etag = '"' + hashlib.sha256(body).hexdigest()[:16] + '"'
            with cond:
                if etag != state["etag"]:
                    state.update(etag=etag, body=body)
                    cond.notify_all()
        except FileNotFoundError:
            pass
        time.sleep(0.05)


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        sys.stderr.write("%s %s\n" % (self.address_string(), fmt % args))

    def answer(self, status, body=b"", ctype=None, headers=()):
        self.send_response(status)
        if ctype:
            self.send_header("Content-Type", ctype)
        for name, value in headers:
            self.send_header(name, value)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def body(self):
        return self.rfile.read(int(self.headers.get("Content-Length") or 0))


class Operator(Handler):
    def do_GET(self):
        if self.path == "/healthz":
            return self.answer(200, b"ok")
        if self.path == "/readyz":
            return self.answer(200 if state["etag"] else 503)
        if self.path != "/bundles/browserjs.tar.gz":
            return self.answer(404)
        if self.headers.get("Authorization") != "Bearer " + os.environ["BUNDLE_TOKEN"]:
            return self.answer(401)
        if state["etag"] is None:
            return self.answer(503)
        seen = self.headers.get("If-None-Match")
        wait = 0
        for part in self.headers.get("Prefer", "").replace(";", ",").split(","):
            if part.strip().startswith("wait="):
                wait = int(part.strip()[5:])
        with cond:
            if seen == state["etag"] and wait:
                cond.wait_for(lambda: state["etag"] != seen, timeout=wait)
            etag, body = state["etag"], state["body"]
        if seen == etag:
            return self.answer(304)
        self.answer(200, body, "application/vnd.openpolicyagent.bundles", [("ETag", etag)])


class Browser(Handler):
    server_name = "browser"
    tools = ("browser_execute", "desktop_execute")

    def ran(self, tool, arguments):
        types = [op.get("type") for op in arguments.get("operations") or [] if isinstance(op, dict)]
        return "stub browser ran: " + ",".join(map(str, types))

    def do_GET(self):
        if self.path == "/healthz":
            return self.answer(200, b"ok")
        self.answer(405, headers=[("Allow", "POST")])

    def do_POST(self):
        message = json.loads(self.body() or b"null")
        if not self.path.startswith("/mcp") or not isinstance(message, dict):
            return self.answer(404)
        if "id" not in message:  # a notification
            return self.answer(202)
        method, params = message.get("method"), message.get("params") or {}
        sys.stderr.write("mcp %s\n" % method)
        if method == "initialize":
            result = {"protocolVersion": params.get("protocolVersion", "2025-03-26"),
                      "capabilities": {"tools": {}},
                      "serverInfo": {"name": self.server_name, "version": "stub"}}
        elif method == "tools/list":
            result = {"tools": [{"name": name, "description": "Says what it was asked to do.",
                                 "inputSchema": {"type": "object", "properties": {"operations": {"type": "array"}}}}
                                for name in self.tools]}
        elif method == "tools/call":
            result = {"content": [{"type": "text", "text": self.ran(params.get("name"), params.get("arguments") or {})}]}
        elif method == "ping":
            result = {}
        else:
            reply = {"jsonrpc": "2.0", "id": message["id"], "error": {"code": -32601, "message": "no such method"}}
            return self.answer(200, json.dumps(reply).encode(), "application/json")
        reply = {"jsonrpc": "2.0", "id": message["id"], "result": result}
        self.answer(200, json.dumps(reply).encode(), "application/json")


class Exec(Browser):
    server_name = "exec"
    tools = ("exec", "stream_logs", "search_logs")

    def ran(self, tool, arguments):
        return "stub exec ran: %s %s" % (tool, json.dumps(arguments, sort_keys=True))


class Backend(Handler):
    def do_GET(self):
        self.answer(200, b"ok")


def serve(port, handler):
    ThreadingHTTPServer(("0.0.0.0", port), handler).serve_forever()


role = sys.argv[1]
if role == "operator":
    threading.Thread(target=watch_bundle, daemon=True).start()
    threading.Thread(target=serve, args=(8081, Operator), daemon=True).start()
    serve(8080, Operator)
elif role == "browser":
    threading.Thread(target=serve, args=(8082, Exec), daemon=True).start()
    serve(8081, Browser)
elif role == "backend":
    serve(8080, Backend)
else:
    sys.exit("stub.py operator|browser|backend")
