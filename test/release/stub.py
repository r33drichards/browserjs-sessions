"""Stand-ins for test/release/run.sh: what a Rollout releases, and what its
check runs. Python's standard library only.

  stub.py backend   answers on :8080: /healthz, /version (VERSION), /verdict
                    (VERDICT: what the stand-in canary is to conclude about
                    this backend), /role (the pod's role label, read now from
                    the downward API file ACTIVE_FILE, as the real backend does)
  stub.py site      answers on :8080: /healthz, and / as HTML with VERSION in
                    it, or 500 when BROKEN is set
  stub.py canary    the stand-in for test/canary.py, which the analysis runs
                    as /canary/canary.py: asks API_URL for /verdict with the
                    Host header API_HOST, and fails unless it says "pass"
"""
import http.server
import os
import sys
import urllib.request


def role():
    try:
        for line in open(os.environ.get("ACTIVE_FILE", "/etc/podinfo/labels")):
            if line.startswith("browserjs.dev/role="):
                return line.split("=", 1)[1].strip().strip('"')
    except OSError:
        pass
    return "none"


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        status, body = 200, "ok"
        if MODE == "backend":
            body = {"/version": os.environ.get("VERSION", ""), "/verdict": os.environ.get("VERDICT", "pass"),
                    "/role": role(), "/host": self.headers.get("Host", "")}.get(self.path, "ok")
        elif self.path == "/":
            if os.environ.get("BROKEN"):
                status, body = 500, "broken"
            else:
                body = "<html><title>site %s</title></html>" % os.environ.get("VERSION", "")
        self.send_response(status)
        self.end_headers()
        self.wfile.write(body.encode() + b"\n")

    def log_message(self, *args):
        pass


MODE = sys.argv[1] if len(sys.argv) > 1 else "canary"
if MODE == "canary":
    request = urllib.request.Request(os.environ["API_URL"] + "/verdict", headers={"Host": os.environ.get("API_HOST", "")})
    with urllib.request.urlopen(request, timeout=20) as response:
        verdict = response.read().decode().strip()
    print("the backend at %s says: %s" % (os.environ["API_URL"], verdict))
    sys.exit(0 if verdict == "pass" else 1)
http.server.ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
