"""Stand-ins for test/billing/run.sh: a process with the name, the labels, the
ServiceAccount, the volumes and the place in the network of the real one,
which answers its probes and does nothing else.

  stub.py backend    answers 200 on :8080 (the readiness and liveness path)
  stub.py operator   answers 200 on :8081 (kopf's liveness address)
"""
import http.server
import sys

PORT = {"backend": 8080, "operator": 8081}[sys.argv[1]]


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok\n")

    def log_message(self, *args):
        pass


http.server.ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
