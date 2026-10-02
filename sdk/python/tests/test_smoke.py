"""Loads the built library and makes real calls through it, against a fake
of the API host that runs in this process. Nothing here talks to the real
service, and the token is not a real one.

    python -m unittest discover -s tests -v
"""

import asyncio
import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import computeruse
from computeruse import (
    BuildError,
    Client,
    ClientOptionsBuilder,
    ComputerUseError,
    CreateSessionRequest,
    CreateSessionRequestBuilder,
    SessionState,
)

TOKEN = "bjs_aaaaaaaaaaaa_c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0"
SESSION = {
    "id": "s-abcde",
    "name": "smoke",
    "owner": "you@example.com",
    "state": "starting",
    "created": "2026-10-01T12:00:00Z",
    "mcp_url": "https://sessions.example.test/s-abcde/mcp",
}


class FakeApi(BaseHTTPRequestHandler):
    seen = []

    def log_message(self, *args):
        pass

    def answer(self, status, body, content_type="application/json", headers=()):
        raw = body if isinstance(body, bytes) else json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(raw)))
        for name, value in headers:
            self.send_header(name, value)
        self.end_headers()
        self.wfile.write(raw)

    def handle_any(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length).decode() if length else ""
        FakeApi.seen.append((self.command, self.path, self.headers.get("Authorization"), body))
        route = (self.command, self.path)

        if route == ("POST", "/oauth/token"):
            return self.answer(200, {"access_token": "access-1", "token_type": "Bearer", "expires_in": 3600, "scope": ""})
        if self.headers.get("Authorization") != "Bearer access-1":
            return self.answer(401, {"error": "invalid token"})
        if route == ("GET", "/v1/me"):
            return self.answer(200, {"email": "you@example.com", "name": "You", "admin": False})
        if route == ("POST", "/v1/sessions"):
            return self.answer(201, dict(SESSION, name=json.loads(body).get("name", "brave-otter")))
        if route == ("PATCH", "/v1/sessions/s-abcde"):
            return self.answer(200, dict(SESSION, state="stopping"))
        if route == ("GET", "/v1/sessions/s-zzzzz"):
            return self.answer(404, {"error": "session not found"})
        if route == ("POST", "/s-abcde/mcp"):
            message = json.loads(body)
            if message["method"] == "notifications/initialized":
                return self.answer(202, b"")
            if message["method"] == "initialize":
                result = {"protocolVersion": "2025-06-18", "capabilities": {}, "serverInfo": {"name": "fake", "version": "0"}}
            else:
                code = message["params"]["arguments"]["code"]
                result = {"content": [{"type": "text", "text": json.dumps({"output": "ran: " + code})}]}
            event = "event: message\ndata: %s\n\n" % json.dumps({"jsonrpc": "2.0", "id": message["id"], "result": result})
            return self.answer(200, event.encode(), "text/event-stream", [("Mcp-Session-Id", "m-1")])
        return self.answer(404, {"error": "no such route"})

    do_GET = do_POST = do_PATCH = do_DELETE = do_PUT = handle_any


class Smoke(unittest.IsolatedAsyncioTestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), FakeApi)
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()
        cls.url = "http://127.0.0.1:%d" % cls.server.server_address[1]

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()

    def client(self):
        options = ClientOptionsBuilder().api_token(TOKEN).base_url(self.url).max_retries(0).build()
        return Client(options)

    async def test_create_run_js_sleep(self):
        client = self.client()
        me = await client.me()
        self.assertEqual(me.email, "you@example.com")

        session = await client.create_session(CreateSessionRequestBuilder().name("smoke").build())
        self.assertEqual(session.id(), "s-abcde")
        self.assertEqual(session.mcp_url(), self.url + "/s-abcde/mcp")
        self.assertEqual(session.last_info().state, SessionState.STARTING)

        result = await session.run_js("console.log(6 * 7)")
        self.assertEqual(result.output, "ran: console.log(6 * 7)")
        self.assertIsNone(result.error)

        info = await session.sleep()
        self.assertEqual(info.state, SessionState.STOPPING)

        # The API token went to the token endpoint only; the calls carried
        # the access token.
        calls = [(method, path) for method, path, _, _ in FakeApi.seen]
        self.assertEqual(calls[0], ("POST", "/oauth/token"))
        self.assertIn(("POST", "/v1/sessions"), calls)
        patch = [body for method, path, _, body in FakeApi.seen if method == "PATCH"][0]
        self.assertEqual(json.loads(patch), {"action": "sleep"})
        self.assertTrue(all(auth == "Bearer access-1" for _, path, auth, _ in FakeApi.seen if path != "/oauth/token"))

    async def test_records_have_defaults(self):
        session = await self.client().create_session(CreateSessionRequest(name="direct"))
        self.assertEqual(session.last_info().name, "direct")

    async def test_errors_are_typed(self):
        client = self.client()
        with self.assertRaises(ComputerUseError.NotFound) as raised:
            await client.get_session("s-zzzzz")
        self.assertEqual(raised.exception.message, "session not found")

        with self.assertRaises(ComputerUseError.Configuration):
            Client.with_token("")
        with self.assertRaises(BuildError.MissingRequiredField):
            ClientOptionsBuilder().build()

    async def test_calls_run_concurrently(self):
        client = self.client()
        answers = await asyncio.gather(*(client.me() for _ in range(5)))
        self.assertEqual(len(answers), 5)

    def test_builders_do_not_mutate(self):
        base = CreateSessionRequestBuilder()
        named = base.name("a")
        self.assertIsNone(base.build().name)
        self.assertEqual(named.build().name, "a")

    def test_version_and_secrets(self):
        self.assertRegex(computeruse.__version__, r"^\d+\.\d+\.\d+")
        self.assertNotIn(TOKEN, repr(self.client()))


if __name__ == "__main__":
    unittest.main()
