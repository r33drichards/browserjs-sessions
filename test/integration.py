#!/usr/bin/env python3
"""End-to-end test of the backend and the session pods on the local cluster.

    hack/local-up.sh        # once
    test/integration.py     # Python 3 standard library only; needs kubectl

Pomerium is left out: the test talks to the backend Service through a
port-forward, with the Host header a request through Pomerium would carry and
an X-Pomerium-Jwt-Assertion it signs itself. For that the backend is switched
to the deploy/local-test configuration, whose signing keys are this test's
(served in the cluster from a ConfigMap), and switched back to deploy/local at
the end. The normal local configuration is never weakened.

Each check prints PASS or FAIL with what was seen; the results are also
written to .local/integration-results.json. Exit status 1 if any failed.
"""
import base64
import hashlib
import http.client
import json
import os
import secrets
import socket
import subprocess
import sys
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
LOCAL = os.path.join(ROOT, ".local")
NS = "browserjs-sessions"
APP_HOST = "app.localtest.me"
SESSIONS_HOST = "sessions.localtest.me"  # every session is under it, by its ID
ALICE, BOB, ADMIN = "alice@example.com", "bob@example.com", "admin@example.com"
IDLE_AFTER = "30s"  # for the idle checks; the sweep runs once a minute

os.environ.setdefault("KUBECONFIG", os.path.join(LOCAL, "kubeconfig"))


# --- ES256, by hand: the standard library has no elliptic curves ------------

P = 0xFFFFFFFF00000001000000000000000000000000FFFFFFFFFFFFFFFFFFFFFFFF
N = 0xFFFFFFFF00000000FFFFFFFFFFFFFFFFBCE6FAADA7179E84F3B9CAC2FC632551
G = (0x6B17D1F2E12C4247F8BCE6E563A440F277037D812DEB33A0F4A13945D898C296,
     0x4FE342E2FE1A7F9B8EE7EB4A7C0F9E162BCE33576B315ECECBB6406837BF51F5)


def _add(a, b):
    if a is None:
        return b
    if b is None:
        return a
    if a[0] == b[0] and (a[1] + b[1]) % P == 0:
        return None
    if a == b:
        m = (3 * a[0] * a[0] - 3) * pow(2 * a[1], -1, P) % P
    else:
        m = (b[1] - a[1]) * pow(b[0] - a[0], -1, P) % P
    x = (m * m - a[0] - b[0]) % P
    return x, (m * (a[0] - x) - a[1]) % P


def _mul(k, point):
    out = None
    while k:
        if k & 1:
            out = _add(out, point)
        point = _add(point, point)
        k >>= 1
    return out


def b64url(data):
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()


class Signer:
    """The stand-in for Pomerium's signing key."""

    def __init__(self, d):
        self.d = d
        x, y = _mul(d, G)
        self.jwk = {"kty": "EC", "crv": "P-256", "alg": "ES256", "use": "sig",
                    "x": b64url(x.to_bytes(32, "big")), "y": b64url(y.to_bytes(32, "big"))}
        thumb = json.dumps({k: self.jwk[k] for k in ("crv", "kty", "x", "y")}, separators=(",", ":"))
        self.jwk["kid"] = hashlib.sha256(thumb.encode()).hexdigest()

    def jwt(self, claims):
        header = {"alg": "ES256", "typ": "JWT", "kid": self.jwk["kid"]}
        signed = b64url(json.dumps(header).encode()) + "." + b64url(json.dumps(claims).encode())
        z = int.from_bytes(hashlib.sha256(signed.encode()).digest(), "big")
        while True:
            k = secrets.randbelow(N - 1) + 1
            r = _mul(k, G)[0] % N
            s = pow(k, -1, N) * (z + r * self.d) % N
            if r and s:
                return signed + "." + b64url(r.to_bytes(32, "big") + s.to_bytes(32, "big"))

    def assertion(self, email, host, lifetime=300):
        """What Pomerium would say about email on a request for host."""
        now = int(time.time())
        return self.jwt({"iss": host, "aud": host, "sub": email, "user": email, "email": email,
                         "name": email.split("@")[0].title(), "iat": now, "exp": now + lifetime,
                         "jti": secrets.token_hex(8)})


def load_signer():
    """One key for every run, so a backend left on the test configuration by
    an interrupted run still trusts the next one."""
    path = os.path.join(LOCAL, "test-jwks-key")
    if not os.path.exists(path):
        os.makedirs(LOCAL, exist_ok=True)
        with open(os.open(path, os.O_WRONLY | os.O_CREAT, 0o600), "w") as f:
            f.write("%064x" % (secrets.randbelow(N - 1) + 1))
    with open(path) as f:
        return Signer(int(f.read().strip(), 16))


# --- the cluster ---------------------------------------------------------------

def kubectl(*args, input=None, check=True):
    done = subprocess.run(("kubectl",) + args, input=input, capture_output=True, text=True)
    if check and done.returncode != 0:
        raise RuntimeError("kubectl %s: %s" % (" ".join(args), done.stderr.strip()))
    return done.stdout


def apply_configuration(overlay):
    kubectl("apply", "-k", os.path.join(ROOT, "deploy", overlay))
    kubectl("-n", NS, "rollout", "status", "deploy/backend", "--timeout=180s")


def exists(kind, name):
    return kubectl("-n", NS, "get", kind, name, "--ignore-not-found", "-o", "name").strip() != ""


class PortForward:
    """kubectl port-forward to the backend Service. It follows one pod, so it
    is started again after the backend restarts."""

    def __init__(self):
        self.proc, self.port = None, None

    def start(self):
        self.stop()
        with socket.socket() as s:
            s.bind(("127.0.0.1", 0))
            self.port = s.getsockname()[1]
        self.proc = subprocess.Popen(
            ["kubectl", "-n", NS, "port-forward", "svc/backend", "%d:80" % self.port],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        deadline = time.time() + 30
        while time.time() < deadline:
            try:
                conn, resp = open_request("GET", APP_HOST, "/healthz", timeout=2)
                conn.close()
                if resp.status == 200:
                    return
            except OSError:
                pass
            time.sleep(0.3)
        raise RuntimeError("the backend did not answer through the port-forward")

    def stop(self):
        if self.proc:
            self.proc.terminate()
            self.proc.wait()
            self.proc = None


FORWARD = PortForward()
SIGNER = None


# --- requests --------------------------------------------------------------------

def connect(timeout):
    return http.client.HTTPConnection("127.0.0.1", FORWARD.port, timeout=timeout)


def open_request(method, host, path, user=None, body=None, headers=None, timeout=60, assertion=None):
    """Sends a request as it would arrive from Pomerium and returns the
    connection and the response, body unread."""
    h = {"Host": host, "Accept": "application/json"}
    if user:
        h["X-Pomerium-Jwt-Assertion"] = SIGNER.assertion(user, host)
    if assertion:
        h["X-Pomerium-Jwt-Assertion"] = assertion
    if body is not None and not isinstance(body, bytes):
        body = json.dumps(body).encode()
        h["Content-Type"] = "application/json"
    h.update(headers or {})
    conn = connect(timeout)
    conn.request(method, path, body=body, headers=h)
    return conn, conn.getresponse()


def request(method, host, path, **kw):
    try:
        conn, resp = open_request(method, host, path, **kw)
    except ConnectionRefusedError:
        FORWARD.start()  # the port-forward died; one more try on a new one
        conn, resp = open_request(method, host, path, **kw)
    try:
        return resp.status, resp.headers, resp.read()
    finally:
        conn.close()


def api(method, path, user, body=None):
    status, _, raw = request(method, APP_HOST, path, user=user, body=body)
    try:
        return status, json.loads(raw) if raw else None
    except ValueError:
        return status, raw.decode(errors="replace")


def session_host(sid):
    """The host a session is asked at: the same for all of them."""
    return SESSIONS_HOST


def session_path(sid, path):
    """path of session sid on that host."""
    return "/%s%s" % (sid, path)


def legacy_host(sid):
    """The host a session had to itself before, and still answers at."""
    return "%s.%s" % (sid, SESSIONS_HOST)


def wait_state(sid, want, user=ALICE, timeout=240):
    """Polls the API until the session is in state want; returns the seconds
    it took."""
    start, seen = time.time(), None
    while time.time() - start < timeout:
        status, s = api("GET", "/api/sessions/" + sid, user)
        seen = s.get("state") if status == 200 else status
        if seen == want:
            return time.time() - start
        if seen == "failed":
            break
        time.sleep(0.5)
    raise AssertionError("session %s is %r, not %r, after %.0fs" % (sid, seen, want, time.time() - start))


class MCP:
    """A minimal MCP client (Streamable HTTP) for one session, as one user."""

    def __init__(self, sid, user=ALICE):
        self.host, self.path, self.user, self.session, self.ids = session_host(sid), session_path(sid, "/mcp"), user, None, 0

    def post(self, message, timeout=120):
        headers = {"Accept": "application/json, text/event-stream"}
        if self.session:
            headers["Mcp-Session-Id"] = self.session
        status, resp_headers, raw = request("POST", self.host, self.path, user=self.user, body=message,
                                            headers=headers, timeout=timeout)
        if resp_headers.get("Mcp-Session-Id"):
            self.session = resp_headers["Mcp-Session-Id"]
        return status, resp_headers, raw

    def rpc(self, method, params=None, timeout=120):
        self.ids += 1
        message = {"jsonrpc": "2.0", "id": self.ids, "method": method}
        if params is not None:
            message["params"] = params
        status, headers, raw = self.post(message, timeout)
        if status != 200:
            raise AssertionError("%s answered %d: %s" % (method, status, raw[:200].decode(errors="replace")))
        text = raw.decode()
        if headers.get("Content-Type", "").startswith("text/event-stream"):
            replies = [json.loads(line[5:]) for line in text.splitlines()
                       if line.startswith("data:") and line[5:].strip()]
            replies = [r for r in replies if r.get("id") == self.ids]
            reply = replies[-1]
        else:
            reply = json.loads(text)
        if "error" in reply:
            raise AssertionError("%s: %s" % (method, reply["error"]))
        return reply["result"]

    def initialize(self):
        self.session = None
        result = self.rpc("initialize", {
            "protocolVersion": "2025-03-26", "capabilities": {},
            "clientInfo": {"name": "browserjs-integration", "version": "0"}})
        self.post({"jsonrpc": "2.0", "method": "notifications/initialized"})
        return result

    def tool(self, name, arguments, timeout=120):
        result = self.rpc("tools/call", {"name": name, "arguments": arguments}, timeout)
        text = "\n".join(c.get("text", "") for c in result.get("content", []))
        if result.get("isError"):
            raise AssertionError("%s failed: %s" % (name, text[:400]))
        return text

    def run_js(self, code, timeout=120):
        return self.tool("run_js", {"code": code}, timeout)


def websocket(host, path, timeout=15):
    """A websocket handshake by hand. Returns the status line, and for a 101
    the payload of the first frame the server sends."""
    sock = socket.create_connection(("127.0.0.1", FORWARD.port), timeout=timeout)
    try:
        sock.sendall((
            "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
            "Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: binary\r\n\r\n"
            % (path, host, base64.b64encode(secrets.token_bytes(16)).decode())).encode())
        buf = b""
        while b"\r\n\r\n" not in buf:
            chunk = sock.recv(4096)
            if not chunk:
                break
            buf += chunk
        head, _, rest = buf.partition(b"\r\n\r\n")
        status = head.split(b"\r\n")[0].decode()
        if " 101 " not in status:
            return status, b""

        def need(n):
            nonlocal rest
            while len(rest) < n:
                chunk = sock.recv(4096)
                if not chunk:
                    raise AssertionError("websocket closed before a frame arrived")
                rest += chunk
            out, rest = rest[:n], rest[n:]
            return out

        length = need(2)[1] & 0x7F
        if length == 126:
            length = int.from_bytes(need(2), "big")
        elif length == 127:
            length = int.from_bytes(need(8), "big")
        return status, need(length)
    finally:
        sock.close()


# --- checks ----------------------------------------------------------------------

RESULTS = []


def check(name, fn):
    """Runs one check. fn returns the evidence to record, or raises."""
    try:
        evidence = fn()
        RESULTS.append({"check": name, "result": "PASS", "evidence": evidence or ""})
        print("PASS  %-58s %s" % (name, evidence or ""), flush=True)
        return True
    except Exception as e:  # a failed check must not stop the ones after it
        RESULTS.append({"check": name, "result": "FAIL", "evidence": "%s: %s" % (type(e).__name__, e)})
        print("FAIL  %-58s %s: %s" % (name, type(e).__name__, e), flush=True)
        return False


def not_run(name, why):
    RESULTS.append({"check": name, "result": "NOT RUN", "evidence": why})
    print("SKIP  %-58s %s" % (name, why), flush=True)


def expect(condition, message):
    if not condition:
        raise AssertionError(message)


class Run:
    """The checks, in order; they share one session."""

    def __init__(self):
        self.sid = None
        self.mcp = None
        self.upload_key = "e2e-upload.txt"
        self.upload_body = b"uploaded by test/integration.py " + secrets.token_hex(8).encode()

    # identity

    def me(self):
        status, me = api("GET", "/api/me", ALICE)
        expect(status == 200 and me == {"email": ALICE, "name": "Alice", "admin": False}, "alice: %s %s" % (status, me))
        status, admin = api("GET", "/api/me", ADMIN)
        expect(status == 200 and admin["admin"] is True, "admin: %s %s" % (status, admin))
        status, _, body = request("GET", APP_HOST, "/api/me")
        expect(status == 401, "no assertion: %d" % status)
        status, _, _ = request("GET", APP_HOST, "/api/me", assertion=SIGNER.assertion(ALICE, "other.localtest.me"))
        expect(status == 401, "assertion for another host: %d" % status)
        return "alice %s; admin is admin; no assertion 401; assertion for another host 401" % json.dumps(me)

    # lifecycle

    def create(self):
        status, s = api("POST", "/api/sessions", ALICE, {"name": "integration"})
        expect(status == 201, "create: %s %s" % (status, s))
        self.sid = s["id"]
        expect(s["owner"] == ALICE and s["mcp_url"] == "https://%s/%s/mcp" % (SESSIONS_HOST, self.sid), "session: %s" % s)
        took = wait_state(self.sid, "running")
        self.timings["start"] = took
        return "%s running %.1fs after create; mcp_url %s" % (self.sid, took, s["mcp_url"])

    def ownership(self):
        path = "/api/sessions/" + self.sid
        status, _ = api("GET", path, BOB)
        expect(status == 404, "stranger GET: %d" % status)
        status, listed = api("GET", "/api/sessions", BOB)
        expect(status == 200 and listed == [], "stranger list: %s %s" % (status, listed))
        status, _ = api("PATCH", path, BOB, {"action": "stop"})
        expect(status == 404, "stranger PATCH: %d" % status)
        status, _ = api("DELETE", path, BOB)
        expect(status == 404, "stranger DELETE: %d" % status)
        status, _ = api("POST", path + "/vnc-ticket", BOB)
        expect(status == 404, "stranger vnc-ticket: %d" % status)
        status, s = api("GET", path, ADMIN)
        expect(status == 200 and s["owner"] == ALICE, "admin GET: %s %s" % (status, s))
        status, everyone = api("GET", "/api/sessions?all=1", ADMIN)
        expect(status == 200 and self.sid in [x["id"] for x in everyone], "admin list: %s" % status)
        status, own = api("GET", "/api/sessions", ADMIN)
        expect(status == 200 and self.sid not in [x["id"] for x in own], "admin's own list has it")
        return "stranger: GET/PATCH/DELETE/vnc-ticket 404, list []; admin: GET 200, listed with ?all=1 only"

    # MCP

    def mcp_initialize(self):
        self.mcp = MCP(self.sid)
        info = self.mcp.initialize()
        expect("serverInfo" in info, "initialize: %s" % info)
        return "serverInfo %s, protocol %s" % (json.dumps(info["serverInfo"]), info.get("protocolVersion"))

    def mcp_tools(self):
        names = sorted(t["name"] for t in self.mcp.rpc("tools/list")["tools"])
        expect("run_js" in names and "get_artifact_upload_url" in names, "tools: %s" % names)
        return "tools: " + ", ".join(names)

    def mcp_run_js(self):
        out = self.mcp.run_js("console.log('answer', 6 * 7)")
        expect("answer 42" in out, "output: %s" % out[:300])
        return "console.log('answer', 6 * 7) -> %s" % out.strip()[:80]

    def mcp_browser(self):
        out = self.mcp.run_js("""
            const r = await mcp.callTool("browser", "browser_execute", { operations: [
              { type: "navigate", params: { url: "https://example.com" } },
              { type: "evaluate", params: { script: "document.title" } },
              { type: "url", params: {} },
            ] });
            console.log(r.content[0].text);
        """)
        expect("Example Domain" in out and "https://example.com/" in out, "output: %s" % out[:500])
        return "tab default navigated to https://example.com/, document.title 'Example Domain'"

    def mcp_refusals(self):
        host, mcp = session_host(self.sid), session_path(self.sid, "/mcp")
        init = {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {
            "protocolVersion": "2025-03-26", "capabilities": {}, "clientInfo": {"name": "x", "version": "0"}}}
        seen = {}
        for label, kw in (("none", {}), ("garbage", {"assertion": "not.a.jwt"}),
                          ("for the app host", {"assertion": SIGNER.assertion(ALICE, APP_HOST)}),
                          ("expired", {"assertion": SIGNER.assertion(ALICE, host, lifetime=-600)})):
            for method in ("POST", "GET", "DELETE"):
                status, headers, _ = request(method, host, mcp, body=init if method == "POST" else None, **kw)
                expect(status == 403, "%s, assertion %s: %d" % (method, label, status))
                expect("WWW-Authenticate" not in headers, "a challenge was sent")
            seen[label] = 403
        status, _, _ = request("POST", host, mcp, user=BOB, body=init)
        expect(status == 404, "stranger: %d" % status)
        status, _, _ = request("POST", host, mcp, user=ADMIN, body=init,
                               headers={"Accept": "application/json, text/event-stream"})
        expect(status == 200, "admin: %d" % status)
        return "no/garbage/other-host/expired assertion 403 (POST, GET, DELETE; never 401); stranger 404; admin 200"

    def session_paths(self):
        host = session_host(self.sid)
        init = {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {
            "protocolVersion": "2025-03-26", "capabilities": {}, "clientInfo": {"name": "x", "version": "0"}}}
        stream = {"Accept": "application/json, text/event-stream"}
        # The sessions' host has sessions and nothing else.
        for path in ("/", "/mcp", "/api/me", "/api/sessions", "/s/%s/mcp" % self.sid, "/%sx/mcp" % self.sid,
                     session_path(self.sid, ""), session_path(self.sid, "/"), session_path(self.sid, "/api/artifacts")):
            status, _, raw = request("POST", host, path, user=ALICE, body=init, headers=stream)
            expect(status == 404 and b"<html" not in raw.lower(), "POST %s: %d" % (path, status))
        # No redirect leads out of the session's prefix, and a browser cannot
        # open a session route as a page.
        for path in ("//mcp", "/mcp//x", "/mcp/../mcp"):
            status, headers, _ = request("POST", host, session_path(self.sid, path), user=ALICE, body=init, headers=stream)
            expect(status == 404 and "Location" not in headers, "POST %s: %d" % (path, status))
        status, _, _ = request("GET", host, session_path(self.sid, "/mcp"), user=ALICE,
                               headers={"Accept": "text/html", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"})
        expect(status == 403, "a browser navigating to the MCP endpoint: %d" % status)
        return "nothing but sessions on the sessions' host (404); unclean paths 404, not redirected; a navigation 403"

    def legacy_host(self):
        """The host the session would have had to itself before still answers."""
        host = legacy_host(self.sid)
        init = {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {
            "protocolVersion": "2025-03-26", "capabilities": {}, "clientInfo": {"name": "x", "version": "0"}}}
        stream = {"Accept": "application/json, text/event-stream"}
        status, _, raw = request("POST", host, "/mcp", user=ALICE, body=init, headers=stream)
        expect(status == 200, "owner at the old host: %d %s" % (status, raw[:100]))
        status, _, _ = request("POST", host, "/mcp", user=BOB, body=init, headers=stream)
        expect(status == 404, "stranger at the old host: %d" % status)
        # An assertion is for one host: the sessions' is not the old one's.
        status, _, _ = request("POST", host, "/mcp", body=init, headers=stream,
                               assertion=SIGNER.assertion(ALICE, SESSIONS_HOST))
        expect(status == 403, "the sessions' host's assertion at the old host: %d" % status)
        status, _, raw = request("GET", host, "/.well-known/oauth-protected-resource/mcp")
        doc = json.loads(raw) if status == 200 else {}
        expect(doc.get("resource") == "https://%s/mcp" % host and doc.get("authorization_servers") == ["https://%s" % host],
               "OAuth metadata at the old host: %d %s" % (status, raw[:200]))
        status, _, _ = request("GET", session_host(self.sid), "/.well-known/oauth-protected-resource" + session_path(self.sid, "/mcp"))
        expect(status == 404, "the backend answered OAuth metadata for the sessions' host (Pomerium's to answer): %d" % status)
        return "owner 200, stranger 404, another host's assertion 403; its OAuth metadata names itself; none from the backend for the sessions' host"

    def mcp_stream_running(self):
        conn, resp = open_request("GET", session_host(self.sid), session_path(self.sid, "/mcp"), user=ALICE, timeout=20, headers={
            "Accept": "text/event-stream", "Mcp-Session-Id": self.mcp.session or ""})
        try:
            ctype = resp.headers.get("Content-Type", "")
            expect(resp.status == 200 and ctype.startswith("text/event-stream"), "GET /mcp: %d %s %s" % (
                resp.status, ctype, resp.read(200) if resp.status != 200 else ""))
            # The stream stays open: nothing more arrives, and it does not end.
            conn.sock.settimeout(3)
            try:
                first = resp.read1(1024)
                ended = first == b""
            except (socket.timeout, TimeoutError):
                first, ended = b"", False
            expect(not ended, "the stream ended at once")
            return "200 %s, still open after 3s (first bytes %r)" % (ctype, first[:40])
        finally:
            conn.close()

    # uploads

    def upload(self):
        host = session_host(self.sid)
        text = self.mcp.tool("get_artifact_upload_url", {"key": self.upload_key, "mime_type": "text/plain"})
        grant = json.loads(text)
        url = grant["url"]
        uploads = session_path(self.sid, "/api/artifact-uploads/")
        prefix = "https://%s%s" % (host, uploads)
        expect(url.startswith(prefix), "upload URL %s is not under the session's own prefix" % url)
        path = url[len("https://" + host):]
        status, _, raw = request("PUT", host, path, body=self.upload_body, headers={"Content-Type": "text/plain"})
        expect(status // 100 == 2, "PUT: %d %s" % (status, raw[:200]))
        again, _, _ = request("PUT", host, path, body=b"second time", headers={"Content-Type": "text/plain"})
        expect(again // 100 == 4, "second PUT with the same URL: %d" % again)
        out = self.mcp.run_js("""
            const f = artifact.get(%s);
            console.log(f === null ? "MISSING" : f.mime_type + "|" + new TextDecoder().decode(f.bytes));
        """ % json.dumps(self.upload_key))
        expect(self.upload_body.decode() in out, "artifact.get: %s" % out[:300])
        bad, _, _ = request("PUT", host, uploads + "0" * 64, body=b"x")
        expect(bad // 100 == 4, "PUT with an unknown token: %d" % bad)
        # Refused on its declared size, before any of the body is sent.
        conn = connect(30)
        try:
            conn.putrequest("PUT", uploads + "0" * 64, skip_host=True)
            conn.putheader("Host", host)
            conn.putheader("Content-Length", str(17 << 20))
            conn.endheaders()
            big = conn.getresponse().status
        finally:
            conn.close()
        expect(big == 413, "17 MiB PUT: %d" % big)
        return "URL under the session's prefix on the sessions' host; PUT with no credentials %d, again %d; artifact.get returns the bytes; unknown token %d; 17 MiB 413" % (
            status, again, bad)

    # VNC

    def vnc(self):
        host = session_host(self.sid)
        status, t = api("POST", "/api/sessions/%s/vnc-ticket" % self.sid, ALICE)
        expect(status == 200, "vnc-ticket: %s %s" % (status, t))
        prefix = "wss://%s%s?ticket=" % (host, session_path(self.sid, "/vnc"))
        expect(t["url"].startswith(prefix), "ticket URL: %s" % t["url"])
        path = t["url"][len("wss://" + host):]
        plain, headers, _ = request("GET", host, path)
        expect(plain == 426, "plain GET /vnc: %d" % plain)
        line, frame = websocket(host, path)
        expect(" 101 " in line, "upgrade: %s" % line)
        expect(frame.startswith(b"RFB 003.008"), "first frame: %r" % frame[:20])
        reused, _ = websocket(host, path)
        expect(" 401 " in reused, "ticket used twice: %s" % reused)
        none, _ = websocket(host, session_path(self.sid, "/vnc"))
        expect(" 401 " in none, "no ticket: %s" % none)
        return "plain GET 426; upgrade with ticket '%s', first frame %r; ticket again 401; no ticket 401" % (
            line, frame[:12])

    # stop and resume

    def remember(self):
        out = self.mcp.run_js("""
            await fs.mkdir("/data/memory", { recursive: true });
            await fs.writeFile("/data/memory/e2e.txt", "kept across a stop");
            console.log(await fs.readFile("/data/memory/e2e.txt", "utf8"));
        """)
        expect("kept across a stop" in out, "output: %s" % out[:300])
        time.sleep(3)  # Chromium writes its session file a moment after a navigation
        return "wrote /data/memory/e2e.txt"

    def stop(self):
        status, s = api("PATCH", "/api/sessions/" + self.sid, ALICE, {"action": "stop"})
        expect(status == 200, "stop: %s %s" % (status, s))
        took = wait_state(self.sid, "stopped")
        self.timings["stop"] = took
        expect(not exists("pod", self.sid), "the pod is still there")
        expect(exists("pvc", "data-" + self.sid) or kubectl("-n", NS, "get", "pvc", "-o", "name").strip() != "",
               "the disk is gone")
        host = session_host(self.sid)
        # For up to 2 s after it last saw the pod, the proxy still takes it to
        # be there; a call in that window is a 502, not the 409.
        time.sleep(2.5)
        mcp = session_path(self.sid, "/mcp")
        status, _, raw = request("POST", host, mcp, user=ALICE, body={"jsonrpc": "2.0", "id": 1, "method": "ping"},
                                 headers={"Accept": "application/json, text/event-stream"})
        expect(status == 409, "MCP POST while stopped: %d %s" % (status, raw[:100]))
        status, headers, _ = request("GET", host, mcp, user=ALICE, headers={"Accept": "text/event-stream"})
        expect(status == 405 and headers.get("Allow") == "POST, DELETE", "GET /mcp while stopped: %d" % status)
        time.sleep(3)
        _, s = api("GET", "/api/sessions/" + self.sid, ALICE)
        expect(s["state"] == "stopped", "state after those requests: %s" % s["state"])
        return "stopped %.1fs after the request; pod gone, disk kept; MCP POST 409; GET /mcp 405 (Allow: POST, DELETE); still stopped" % took

    def resume(self):
        status, s = api("PATCH", "/api/sessions/" + self.sid, ALICE, {"action": "resume"})
        expect(status == 200, "resume: %s %s" % (status, s))
        took = wait_state(self.sid, "running")
        self.timings["resume"] = took
        self.mcp = MCP(self.sid)
        self.mcp.initialize()
        return "running %.1fs after the request" % took

    def restored_tab(self):
        out = self.mcp.run_js("""
            const r = await mcp.callTool("browser", "browser_execute", { operations: [{ type: "url", params: {} }] });
            console.log(r.content[0].text);
        """)
        expect("https://example.com/" in out, "tab default after resume: %s" % out[:400])
        return "tab default is at https://example.com/ without navigating"

    def restored_files(self):
        out = self.mcp.run_js("""
            console.log(await fs.readFile("/data/memory/e2e.txt", "utf8"));
            const f = artifact.get(%s);
            console.log(f === null ? "MISSING" : new TextDecoder().decode(f.bytes));
        """ % json.dumps(self.upload_key))
        expect("kept across a stop" in out, "memory file: %s" % out[:300])
        expect(self.upload_body.decode() in out, "artifact: %s" % out[:300])
        return "/data/memory/e2e.txt and the uploaded artifact are both there"

    # idle

    def idle_sleep(self):
        kubectl("-n", NS, "set", "env", "deploy/backend", "IDLE_AFTER=" + IDLE_AFTER)
        kubectl("-n", NS, "rollout", "status", "deploy/backend", "--timeout=180s")
        FORWARD.start()
        self.mcp = MCP(self.sid)
        self.mcp.initialize()  # the last use of the session
        used = time.time()
        # A client that keeps its event stream open the whole time.
        self.stream = open_request("GET", session_host(self.sid), session_path(self.sid, "/mcp"), user=ALICE, timeout=600, headers={
            "Accept": "text/event-stream", "Mcp-Session-Id": self.mcp.session or ""})
        expect(self.stream[1].status == 200, "GET /mcp: %d" % self.stream[1].status)
        wait_state(self.sid, "asleep", timeout=300)
        took = time.time() - used
        self.timings["idle sleep"] = took
        return "asleep %.0fs after its last call (IDLE_AFTER=%s, swept once a minute), with a GET /mcp stream open throughout" % (
            took, IDLE_AFTER)

    def idle_stream_does_not_wake(self):
        try:
            self.stream[0].close()
        except Exception:
            pass
        host = session_host(self.sid)
        status, headers, _ = request("GET", host, session_path(self.sid, "/mcp"), user=ALICE, headers={"Accept": "text/event-stream"})
        expect(status == 405 and headers.get("Allow") == "POST, DELETE", "GET /mcp while asleep: %d" % status)
        time.sleep(8)
        _, s = api("GET", "/api/sessions/" + self.sid, ALICE)
        expect(s["state"] == "asleep", "state after GET /mcp: %s" % s["state"])
        expect(not exists("pod", self.sid), "a pod was started")
        return "GET /mcp on the sleeping session 405 (Allow: POST, DELETE); 8s later still asleep, no pod"

    def idle_wake(self):
        start = time.time()
        self.mcp = MCP(self.sid)
        self.mcp.initialize()
        took = time.time() - start
        self.timings["wake"] = took
        out = self.mcp.run_js("console.log('awake', 1 + 1)")
        expect("awake 2" in out, "run_js after waking: %s" % out[:200])
        _, s = api("GET", "/api/sessions/" + self.sid, ALICE)
        expect(s["state"] == "running", "state after the call: %s" % s["state"])
        return "an MCP POST was answered %.1fs later by the woken session" % took

    # delete

    def delete(self):
        status, _ = api("DELETE", "/api/sessions/" + self.sid, ALICE)
        expect(status == 204, "DELETE: %d" % status)
        start = time.time()
        while True:
            status, s = api("GET", "/api/sessions/" + self.sid, ALICE)
            if status == 404:
                break
            expect(time.time() - start < 120, "still %s %s after 120s" % (status, s))
            time.sleep(0.5)
        gone_api = time.time() - start
        left = None
        while time.time() - start < 180:
            left = [k for k, n in (("sandbox", self.sid), ("pod", self.sid)) if exists(k, n)]
            pvcs = [p for p in kubectl("-n", NS, "get", "pvc", "-o", "name").split() if self.sid in p]
            left += pvcs
            if not left:
                break
            time.sleep(1)
        expect(not left, "left behind after 180s: %s" % left)
        took = time.time() - start
        self.timings["delete"] = took
        time.sleep(2.5)  # the proxy remembers a session's owner for 2s
        status, _, _ = request("POST", session_host(self.sid), session_path(self.sid, "/mcp"), user=ALICE, body={
            "jsonrpc": "2.0", "id": 1, "method": "ping"})
        expect(status == 404, "MCP on the deleted session: %d" % status)
        sid, self.sid = self.sid, None
        return "API 404 after %.1fs; Sandbox, pod and PVC gone after %.1fs; MCP on %s 404" % (gone_api, took, sid)

    timings = {}

    def all(self):
        check("api: /api/me, and who is not signed in", self.me)
        if not check("create a session, wait for running", self.create):
            return
        check("stranger sees nothing, admin sees it", self.ownership)
        if check("mcp: initialize", self.mcp_initialize):
            check("mcp: tools/list has run_js and get_artifact_upload_url", self.mcp_tools)
            check("mcp: run_js", self.mcp_run_js)
            check("mcp: run_js drives the browser", self.mcp_browser)
            check("mcp: GET /mcp on a running session streams", self.mcp_stream_running)
            check("upload: one-time URL under the session's prefix", self.upload)
            check("memory: write a file", self.remember)
        check("mcp: refused without a good assertion (403, not 401)", self.mcp_refusals)
        check("paths: only sessions on the sessions' host, no redirects, no pages", self.session_paths)
        check("legacy: the session's old host still answers", self.legacy_host)
        check("vnc: ticket, websocket upgrade, RFB banner", self.vnc)
        if check("stop: stopped, MCP 409, GET /mcp 405", self.stop) and check("resume: running again", self.resume):
            check("resume: the tab is where it was", self.restored_tab)
            check("resume: memory file and artifact survived", self.restored_files)
        if check("idle: sleeps, an open GET /mcp stream does not prevent it", self.idle_sleep):
            check("idle: GET /mcp does not wake it", self.idle_stream_does_not_wake)
            check("idle: an MCP POST wakes it", self.idle_wake)
        else:
            not_run("idle: GET /mcp does not wake it", "the session did not go to sleep")
            not_run("idle: an MCP POST wakes it", "the session did not go to sleep")
        check("delete: 404, and Sandbox, pod and PVC are gone", self.delete)


def main():
    global SIGNER
    SIGNER = load_signer()
    run = Run()
    try:
        print("switching the backend to the test configuration (deploy/local-test)", flush=True)
        kubectl("apply", "-f", "-", input=kubectl(
            "-n", NS, "create", "configmap", "test-jwks", "--dry-run=client", "-o", "yaml",
            "--from-literal=jwks.json=" + json.dumps({"keys": [SIGNER.jwk]})))
        kubectl("apply", "-k", os.path.join(ROOT, "deploy", "local-test"))
        kubectl("-n", NS, "rollout", "status", "deploy/test-jwks", "--timeout=180s")
        kubectl("-n", NS, "rollout", "restart", "deploy/backend")  # so it reads the keys now
        kubectl("-n", NS, "rollout", "status", "deploy/backend", "--timeout=180s")
        FORWARD.start()
        run.all()
    finally:
        try:
            if run.sid:
                api("DELETE", "/api/sessions/" + run.sid, ALICE)
        except Exception:
            pass
        FORWARD.stop()
        print("switching the backend back (deploy/local)", flush=True)
        kubectl("-n", NS, "delete", "deploy/test-jwks", "svc/test-jwks", "configmap/test-jwks", "--ignore-not-found", check=False)
        # kubectl apply leaves alone what kubectl set env added.
        kubectl("-n", NS, "set", "env", "deploy/backend", "IDLE_AFTER-", check=False)
        apply_configuration("local")

    failed = [r for r in RESULTS if r["result"] == "FAIL"]
    with open(os.path.join(LOCAL, "integration-results.json"), "w") as f:
        json.dump({"results": RESULTS, "timings": {k: round(v, 1) for k, v in run.timings.items()}}, f, indent=2)
    print("\n%d checks: %d passed, %d failed, %d not run. Seconds: %s" % (
        len(RESULTS), sum(r["result"] == "PASS" for r in RESULTS), len(failed),
        sum(r["result"] == "NOT RUN" for r in RESULTS),
        ", ".join("%s %.1f" % kv for kv in run.timings.items())))
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
