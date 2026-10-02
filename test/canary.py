#!/usr/bin/env python3
"""The release canary: one session, end to end, through the public API host
with an API token. What a release must not break (docs/releases.md).

  CANARY_API_TOKEN=bjs_... test/canary.py

Python's standard library only. It prints one line a check and exits 0 only
if every check passed. The session it makes is deleted whatever happens, and
so is one a run before it left behind. Nothing it prints is secret: no token,
no email address.

The environment:

  CANARY_API_TOKEN     required. An API token with the scopes sessions:read,
                       sessions:write, sessions:connect, policies:read and
                       policies:write (the app's API tokens page). Never
                       printed.
  DOMAIN               computeruse.site. The API is https://api.<DOMAIN>, the
                       app https://app.<DOMAIN>, the site https://<DOMAIN>.
  API_URL, APP_URL,    each of those, to say otherwise. APP_URL or SITE_URL
  SITE_URL             empty: that check is left out.
  API_HOST             the Host header sent to API_URL, when API_URL is not
                       the public name: a backend asked directly, inside the
                       cluster (the rollout's check of a standby backend)
  CA_FILE              a CA certificate to trust (a local cluster's)
  CANARY_DIGESTS       "browser=sha256:...,mcp-js=sha256:...": the session is
                       a canary session, started cold on those digests of the
                       session images (the token's owner must be an admin of
                       the deployment). Unset: an ordinary session, from the
                       warm pool if there is one.
  EXPECT_STATE_SAVED   1 (default): sleep must save the session's state and
                       wake must bring it back as it was (Pod Snapshots).
                       0: a cluster without snapshots; wake starts it fresh.
  EXPECT_POLICIES      1 (default): the session's policy is changed and must
                       bind. 0: a deployment with session policies off.
  SESSION_HOOK         a command run once the session is running, with
                       SESSION_ID in its environment (the release workflow
                       checks the pod's images with it). Its failure fails
                       the run; its output is shown.
  START_TIMEOUT        seconds to wait for a session to run (420)
  SUMMARY              a file the results are appended to, as Markdown
"""
import base64
import json
import os
import re
import secrets
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

DOMAIN = os.environ.get("DOMAIN", "computeruse.site")
API = os.environ.get("API_URL", "https://api." + DOMAIN).rstrip("/")
APP = os.environ.get("APP_URL", "https://app." + DOMAIN).rstrip("/")
SITE = os.environ.get("SITE_URL", "https://" + DOMAIN).rstrip("/")
API_HOST = os.environ.get("API_HOST", "")
TOKEN = os.environ.get("CANARY_API_TOKEN", "")
EXPECT_STATE_SAVED = os.environ.get("EXPECT_STATE_SAVED", "1") != "0"
EXPECT_POLICIES = os.environ.get("EXPECT_POLICIES", "1") != "0"
START_TIMEOUT = int(os.environ.get("START_TIMEOUT", "420"))
# Sessions this script made are named so, and it deletes any it finds.
NAME_PREFIX = "release-canary-"
# Where a policy taken over by a token says it is managed: this page.
MANAGED_URL = "https://github.com/r33drichards/computer-use/blob/main/docs/releases.md"

CONTEXT = ssl.create_default_context(cafile=os.environ.get("CA_FILE") or None)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


OPENER = urllib.request.build_opener(urllib.request.HTTPSHandler(context=CONTEXT), NoRedirect)

results = []  # (ok, name, detail)
# Anything shaped like an email address: the log is public.
ADDRESS = re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}")


def scrub(text):
    """What is about to be printed, without the token (it never should be in
    an answer; this is the second lock)."""
    text = ADDRESS.sub("[address]", str(text))
    for secret in (TOKEN, ACCESS.get("token", "")):
        if secret:
            text = text.replace(secret, "[redacted]")
    return text


ACCESS = {}


def http(method, url, body=None, headers=None, timeout=60):
    """One request. Returns (status, headers, bytes); 0 when nothing answered."""
    data = None
    headers = dict(headers or {})
    if isinstance(body, (dict, list)):
        data = json.dumps(body).encode()
        headers.setdefault("Content-Type", "application/json")
    elif body is not None:
        data = body
    if API_HOST and url.startswith(API + "/"):
        headers["Host"] = API_HOST
    request = urllib.request.Request(url, data=data, method=method, headers=headers)
    try:
        with OPENER.open(request, timeout=timeout) as response:
            return response.status, response.headers, response.read()
    except urllib.error.HTTPError as e:
        return e.code, e.headers, e.read()
    except (urllib.error.URLError, OSError) as e:
        return 0, {}, str(e).encode()


def api(method, path, body=None, timeout=60):
    status, _, raw = http(method, API + path, body, {"Authorization": "Bearer " + ACCESS["token"]}, timeout)
    try:
        return status, json.loads(raw) if raw else None
    except ValueError:
        return status, raw[:300].decode(errors="replace")


class Failed(Exception):
    pass


def expect(condition, message):
    if not condition:
        raise Failed(message)


def check(name, fn):
    """Runs one check; returns whether it passed."""
    started = time.time()
    try:
        detail = fn() or ""
        ok = True
    except Failed as e:
        detail, ok = str(e), False
    except Exception as e:  # a bug here, or an answer in a shape nobody expected
        detail, ok = "%s: %s" % (type(e).__name__, e), False
    detail = scrub(detail)[:400]
    results.append((ok, name, detail))
    print("%s  %s (%.1fs)%s" % ("PASS" if ok else "FAIL", name, time.time() - started,
                                 "" if not detail else "\n      " + detail), flush=True)
    return ok


def skip(name, why):
    results.append((None, name, why))
    print("SKIP  %s\n      %s" % (name, why), flush=True)


class MCP:
    """A minimal MCP client (Streamable HTTP) for one session."""

    def __init__(self, sid):
        self.url, self.session, self.ids = "%s/%s/mcp" % (API, sid), None, 0

    def post(self, message, timeout):
        headers = {"Authorization": "Bearer " + ACCESS["token"], "Accept": "application/json, text/event-stream"}
        if self.session:
            headers["Mcp-Session-Id"] = self.session
        status, response_headers, raw = http("POST", self.url, message, headers, timeout)
        if response_headers.get("Mcp-Session-Id"):
            self.session = response_headers["Mcp-Session-Id"]
        return status, response_headers, raw

    def rpc(self, method, params, timeout=120):
        self.ids += 1
        status, headers, raw = self.post({"jsonrpc": "2.0", "id": self.ids, "method": method, "params": params}, timeout)
        expect(status == 200, "%s answered %d: %s" % (method, status, raw[:200].decode(errors="replace")))
        text = raw.decode()
        if headers.get("Content-Type", "").startswith("text/event-stream"):
            replies = [json.loads(line[5:]) for line in text.splitlines() if line.startswith("data:") and line[5:].strip()]
            replies = [r for r in replies if r.get("id") == self.ids]
            expect(replies, "%s: the event stream had no answer" % method)
            reply = replies[-1]
        else:
            reply = json.loads(text)
        expect("error" not in reply, "%s: %s" % (method, reply.get("error")))
        return reply["result"]

    def connect(self):
        self.session = None
        self.rpc("initialize", {"protocolVersion": "2025-03-26", "capabilities": {},
                                "clientInfo": {"name": "release-canary", "version": "0"}})
        self.post({"jsonrpc": "2.0", "method": "notifications/initialized"}, 30)

    def run_js(self, code, timeout=150):
        # Its own limit is 30 s by default; a browser that has just started
        # can take longer over its first page.
        result = self.rpc("tools/call", {"name": "run_js", "arguments": {"code": code, "execution_timeout_secs": 120}}, timeout)
        return "\n".join(c.get("text", "") for c in result.get("content", []))


# What the session's code runs: one tool call, and what it saw of it.
CALL = """
try {
  const r = await mcp.callTool(%s, %s, %s);
  console.log("CANARY-RETURNED " + JSON.stringify(r));
} catch (e) {
  console.log("CANARY-THROWN " + String((e && e.message) || e));
}
"""

EXEC = """
const call = async (tool, args) => JSON.parse((await mcp.callTool("exec", tool, args)).content[0].text);
try {
  const { id } = await call("exec", { bin: "echo", args: ["canary", %s], timeout: 20 });
  let r;
  for (let i = 0; i < 100; i++) {
    r = await call("stream_logs", { id, offset: 0 });
    if (r.status !== "running") break;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  console.log("CANARY-EXEC " + r.status + " " + JSON.stringify(r.logs));
} catch (e) {
  console.log("CANARY-THROWN " + String((e && e.message) || e));
}
"""


def browser(mcp, operations):
    return mcp.run_js(CALL % ('"browser"', '"browser_execute"', json.dumps({"operations": operations})))


def exec_echo(mcp, word):
    return mcp.run_js(EXEC % json.dumps(word))


def wait_state(sid, want, timeout):
    started, seen, message = time.time(), None, ""
    while time.time() - started < timeout:
        status, s = api("GET", "/v1/sessions/" + sid)
        seen = s.get("state") if status == 200 and isinstance(s, dict) else status
        message = s.get("message", "") if isinstance(s, dict) else ""
        if seen == want:
            return time.time() - started
        if seen == "failed":
            break
        time.sleep(2)
    raise Failed("the session is %r, not %r, after %.0fs%s" % (seen, want, time.time() - started,
                                                              "" if not message else ": " + message))


def wait_policy(sid, timeout=90):
    """Until the session's policy is in force."""
    started, state = time.time(), None
    while time.time() - started < timeout:
        status, p = api("GET", "/v1/sessions/%s/policy" % sid)
        state = p.get("state") if status == 200 and isinstance(p, dict) else status
        if state == "ready":
            return
        if state == "invalid":
            break
        time.sleep(2)
    raise Failed("the policy is %r after %.0fs" % (state, time.time() - started))


def until(what, fn, timeout=60):
    """fn() until it returns something true; OPA takes a bundle a moment
    after the policy is ready."""
    started, last = time.time(), None
    while time.time() - started < timeout:
        last = fn()
        if last:
            return last
        time.sleep(3)
    raise Failed("%s: not after %.0fs" % (what, time.time() - started))


def main():
    if not TOKEN:
        print("CANARY_API_TOKEN is not set: an API token of the deployment, with the sessions and policies scopes "
              "(docs/releases.md)", file=sys.stderr)
        return 2
    parts = TOKEN.split("_", 2)
    if len(parts) != 3 or parts[0] != "bjs":
        print("CANARY_API_TOKEN is not an API token (bjs_<id>_<secret>)", file=sys.stderr)
        return 2
    client_id = parts[1]
    digests = {}
    for pair in filter(None, os.environ.get("CANARY_DIGESTS", "").split(",")):
        name, _, digest = pair.partition("=")
        digests[name.strip()] = digest.strip()
    print("canary: %s, %s" % (API, "a canary session on " + ", ".join("%s@%s" % (n, d[:19]) for n, d in sorted(digests.items()))
                              if digests else "an ordinary session"), flush=True)

    state = {"sid": None, "mcp": None, "marker": "m" + secrets.token_hex(8)}

    # --- what answers without a session ---------------------------------------
    def reachable(url, want):
        def run():
            status, headers, _ = http("GET", url + "/", timeout=30)
            expect(status in want, "GET %s/ answered %d%s" % (url, status, "" if status else " (nothing answered)"))
            return "%d%s" % (status, "" if status == 200 else " to " + urllib.parse.urlsplit(headers.get("Location", "")).netloc)
        return run

    if SITE:
        check("the site answers", reachable(SITE, (200,)))
    if APP:
        # Nobody is signed in: Pomerium sends the browser to sign in.
        check("the app answers (a redirect to sign in)", reachable(APP, (302, 303)))

    def no_token():
        status, _, _ = http("GET", API + "/v1/sessions")
        expect(status == 401, "GET /v1/sessions without a token answered %d, want 401" % status)
    check("the API refuses a request with no token", no_token)

    def exchange():
        basic = base64.b64encode(("%s:%s" % (client_id, TOKEN)).encode()).decode()
        status, _, raw = http("POST", API + "/oauth/token", b"grant_type=client_credentials",
                              {"Authorization": "Basic " + basic, "Content-Type": "application/x-www-form-urlencoded"})
        expect(status == 200, "POST /oauth/token answered %d" % status)
        answer = json.loads(raw)
        expect(answer.get("token_type") == "Bearer" and answer.get("access_token"), "no access token in the answer")
        ACCESS["token"] = answer["access_token"]
        scopes = set(answer.get("scope", "").split())
        missing = {"sessions:read", "sessions:write", "sessions:connect", "policies:read", "policies:write"} - scopes
        expect(not missing, "the token lacks the scopes: " + " ".join(sorted(missing)))
        return "an access token for %ds" % answer.get("expires_in", 0)
    if not check("the token is exchanged for an access token", exchange):
        return finish()

    def me():
        status, body = api("GET", "/v1/me")
        expect(status == 200 and isinstance(body, dict) and body.get("email"), "GET /v1/me answered %d" % status)
    check("the access token is accepted", me)

    def leftovers():
        status, mine = api("GET", "/v1/sessions")
        expect(status == 200 and isinstance(mine, list), "GET /v1/sessions answered %d" % status)
        old = [s["id"] for s in mine if s.get("name", "").startswith(NAME_PREFIX)]
        for sid in old:
            api("DELETE", "/v1/sessions/" + sid)
        return "%d of the token's owner's sessions; %d left by an earlier run, deleted" % (len(mine), len(old))
    check("sessions are listed", leftovers)

    # --- one session -------------------------------------------------------------
    def create():
        body = {"name": NAME_PREFIX + time.strftime("%Y%m%d-%H%M%S", time.gmtime())}
        if digests:
            body["canary"] = digests
        status, s = api("POST", "/v1/sessions", body)
        expect(status == 201 and isinstance(s, dict), "POST /v1/sessions answered %d: %s" % (status, s))
        state["sid"] = s["id"]
        return s["id"]
    if not check("a session is created", create):
        return finish()
    sid = state["sid"]

    def session_checks():
        def running():
            return "running after %.0fs" % wait_state(sid, "running", START_TIMEOUT)
        if not check("it runs", running):
            return

        hook = os.environ.get("SESSION_HOOK")
        if hook:
            def run_hook():
                done = subprocess.run(hook, shell=True, env=dict(os.environ, SESSION_ID=sid, CANARY_API_TOKEN=""),
                                      capture_output=True, text=True, timeout=300)
                output = (done.stdout + done.stderr).strip()
                expect(done.returncode == 0, "exit %d: %s" % (done.returncode, output[-300:]))
                return output[-300:]
            check("SESSION_HOOK", run_hook)

        mcp = MCP(sid)

        def run_js():
            mcp.connect()
            out = mcp.run_js("console.log('canary', 6 * 7)")
            expect("canary 42" in out, "run_js printed: %s" % out[:200])
        if not check("run_js: console output comes back", run_js):
            return

        def browser_execute():
            operations = [
                # A page of its own, with nothing to fetch: no site out there
                # is part of this check.
                {"type": "navigate", "params": {"url": "data:text/html,<title>canary page</title><p id=p>hello</p>"}},
                {"type": "evaluate", "params": {"script": "window.__canary = %s; document.title + '/' + document.getElementById('p').textContent" % json.dumps(state["marker"])}},
            ]
            out = browser(mcp, operations)
            if "canary page/hello" not in out:
                # Once more: the first call can meet a browser still starting.
                print("      (first try: %s)" % scrub(out[:200]), flush=True)
                out = browser(mcp, operations)
            expect("CANARY-RETURNED" in out and "canary page/hello" in out, "browser_execute: %s" % out[:300])
        check("browser_execute: a page is loaded and read", browser_execute)

        def execute():
            out = exec_echo(mcp, state["marker"])
            expect("CANARY-EXEC completed" in out and state["marker"] in out, "exec: %s" % out[:300])
        check("exec: a program runs, given as {bin, args}", execute)

        # --- the policy binds -----------------------------------------------------
        if not EXPECT_POLICIES:
            skip("the policy browser-only denies exec", "EXPECT_POLICIES=0")
        else:
            def restrict():
                status, presets = api("GET", "/v1/policy-presets")
                expect(status == 200 and isinstance(presets, list), "GET /v1/policy-presets answered %d" % status)
                preset = [p for p in presets if p.get("id") == "browser-only"]
                expect(preset, "there is no preset browser-only")
                # A token writes a policy by taking it over ("iac").
                status, saved = api("PUT", "/v1/sessions/%s/policy" % sid,
                                    {"kind": "rego", "source": preset[0]["source"],
                                     "management": {"mode": "iac", "managed_url": MANAGED_URL}})
                expect(status in (200, 202), "PUT policy answered %d: %s" % (status, saved))
                wait_policy(sid)

                def denied():
                    out = exec_echo(mcp, "denied")
                    return "CANARY-THROWN" in out and "CANARY-EXEC" not in out
                until("exec is denied", denied)
                out = browser(mcp, [{"type": "evaluate", "params": {"script": "document.title"}}])
                expect("CANARY-RETURNED" in out and "canary page" in out, "the browser under browser-only: %s" % out[:300])
            restricted = check("the policy browser-only denies exec and allows the browser", restrict)

            def restore():
                status, saved = api("DELETE", "/v1/sessions/%s/policy" % sid)
                expect(status in (200, 202), "DELETE policy answered %d: %s" % (status, saved))
                wait_policy(sid)
                until("exec is allowed again", lambda: "CANARY-EXEC completed" in exec_echo(mcp, "restored"))
            if restricted:
                check("the policy is put back, and exec runs again", restore)
            else:
                skip("the policy is put back, and exec runs again", "it was not changed")

        # --- sleep and wake ---------------------------------------------------------
        def sleep():
            status, s = api("POST", "/v1/sessions/%s/sleep" % sid, timeout=180)
            expect(status == 200 and isinstance(s, dict), "POST sleep answered %d: %s" % (status, s))
            wait_state(sid, "asleep", 180)
            status, s = api("GET", "/v1/sessions/" + sid)
            saved = bool(s.get("stateSaved"))
            if EXPECT_STATE_SAVED:
                expect(saved, "asleep, but stateSaved is not true: it would wake fresh")
            return "asleep, stateSaved %s" % str(saved).lower()
        asleep = check("sleep: the session is put to sleep" + (", its state saved" if EXPECT_STATE_SAVED else ""), sleep)

        def wake():
            status, s = api("POST", "/v1/sessions/%s/wake" % sid)
            expect(status == 200, "POST wake answered %d: %s" % (status, s))
            took = wait_state(sid, "running", START_TIMEOUT)
            mcp.connect()
            out = mcp.run_js("console.log('awake', 1 + 1)")
            expect("awake 2" in out, "run_js after waking: %s" % out[:200])
            if EXPECT_STATE_SAVED:
                # The page's own memory: there only if the pod came back from
                # its snapshot. A fresh start reloads the tab, and it is gone.
                out = browser(mcp, [{"type": "evaluate", "params": {"script": "String(window.__canary)"}}])
                expect(state["marker"] in out, "the marker left in the page's memory did not survive: %s" % out[:200])
                return "running after %.0fs, with the page's memory as it was" % took
            return "running after %.0fs" % took
        if asleep:
            check("wake: it runs again" + (" as it was" if EXPECT_STATE_SAVED else ""), wake)
        else:
            skip("wake", "it did not go to sleep")

    try:
        session_checks()
    finally:
        def delete():
            status, _ = api("DELETE", "/v1/sessions/" + sid)
            expect(status == 204, "DELETE answered %d" % status)
            started = time.time()
            while time.time() - started < 60:
                status, _ = api("GET", "/v1/sessions/" + sid)
                if status == 404:
                    return
                time.sleep(2)
            raise Failed("the session is still there a minute after its deletion (%d)" % status)
        check("the session is deleted", delete)
    return finish()


def finish():
    failed = [r for r in results if r[0] is False]
    passed = [r for r in results if r[0]]
    print("\n%d passed, %d failed" % (len(passed), len(failed)), flush=True)
    summary = os.environ.get("SUMMARY")
    if summary:
        with open(summary, "a") as f:
            f.write("| | Check | |\n|---|---|---|\n")
            for ok, name, detail in results:
                f.write("| %s | %s | %s |\n" % ({True: "pass", False: "**FAIL**", None: "skipped"}[ok], name,
                                                  detail.replace("|", "\\|").replace("\n", " ")))
            f.write("\n%d passed, %d failed.\n" % (len(passed), len(failed)))
    return 1 if failed or not passed else 0


if __name__ == "__main__":
    sys.exit(main())
