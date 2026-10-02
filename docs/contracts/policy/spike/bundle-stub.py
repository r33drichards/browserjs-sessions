# Bundle server stub: serves ./bundle.tar.gz with an ETag; long-polls when
# the client sends "Prefer: wait=N" and its If-None-Match is current.
import hashlib, os, sys, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
cond = threading.Condition()
state = {"etag": None, "body": None}
def load():
    while True:
        try:
            b = open("bundle.tar.gz", "rb").read()
            e = '"' + hashlib.sha256(b).hexdigest()[:16] + '"'
            with cond:
                if e != state["etag"]:
                    state.update(etag=e, body=b); cond.notify_all()
        except FileNotFoundError:
            pass
        time.sleep(0.01)
class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *a): pass
    def do_GET(self):
        if state["etag"] is None:
            self.send_response(503); self.send_header("Content-Length", "0"); self.end_headers(); return
        inm = self.headers.get("If-None-Match"); prefer = self.headers.get("Prefer", "")
        wait = 0
        for part in prefer.replace(";", ",").split(","):
            if part.strip().startswith("wait="): wait = int(part.strip()[5:])
        with cond:
            if inm == state["etag"] and wait:
                cond.wait_for(lambda: state["etag"] != inm, timeout=wait)
            etag, body = state["etag"], state["body"]
        if inm == etag:
            self.send_response(304); self.send_header("Content-Length", "0"); self.end_headers(); return
        self.send_response(200)
        self.send_header("Content-Type", "application/vnd.openpolicyagent.bundles")
        self.send_header("ETag", etag); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
threading.Thread(target=load, daemon=True).start()
ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
