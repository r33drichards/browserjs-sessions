// Loads the built library and makes real calls through it, against a fake of
// the API host that this file starts. Nothing here talks to the real
// service, and the token is not a real one.
//
//   npm run build && node --test test/smoke.test.mjs

import assert from "node:assert/strict";
import http from "node:http";
import { after, before, test } from "node:test";

import {
  BuildError,
  Client,
  ClientOptionsBuilder,
  ComputerUseError,
  CreateSessionRequestBuilder,
  SessionState,
  sdkVersion,
} from "../dist/index.js";

const TOKEN = "bjs_aaaaaaaaaaaa_c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0";
const session = (name, state) => ({
  id: "s-abcde",
  name,
  owner: "you@example.com",
  state,
  created: "2026-10-01T12:00:00Z",
  mcp_url: "https://sessions.example.test/s-abcde/mcp",
});

const seen = [];
let server;
let url;

before(async () => {
  server = http.createServer(async (request, response) => {
    let body = "";
    for await (const chunk of request) body += chunk;
    seen.push({ method: request.method, path: request.url, authorization: request.headers.authorization, body });
    const answer = (status, value, headers = {}) => {
      response.writeHead(status, { "Content-Type": "application/json", ...headers });
      response.end(typeof value === "string" ? value : JSON.stringify(value));
    };
    const route = `${request.method} ${request.url}`;
    if (route === "POST /oauth/token") {
      return answer(200, { access_token: "access-1", token_type: "Bearer", expires_in: 3600, scope: "" });
    }
    if (request.headers.authorization !== "Bearer access-1") return answer(401, { error: "invalid token" });
    if (route === "GET /v1/me") return answer(200, { email: "you@example.com", name: "You", admin: false });
    if (route === "POST /v1/sessions") return answer(201, session(JSON.parse(body).name ?? "brave-otter", "starting"));
    if (route === "POST /v1/sessions/s-abcde/sleep") return answer(200, session("smoke", "asleep"));
    if (route === "POST /s-abcde/mcp") {
      const message = JSON.parse(body);
      if (message.method === "notifications/initialized") return answer(202, "");
      const result =
        message.method === "initialize"
          ? { protocolVersion: "2025-06-18", capabilities: {} }
          : { content: [{ type: "text", text: JSON.stringify({ output: "ran: " + message.params.arguments.code }) }] };
      const reply = JSON.stringify({ jsonrpc: "2.0", id: message.id, result });
      return answer(200, `event: message\ndata: ${reply}\n\n`, { "Content-Type": "text/event-stream", "Mcp-Session-Id": "m-1" });
    }
    return answer(404, { error: "session not found" });
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  url = `http://127.0.0.1:${server.address().port}`;
});

after(() => server.close());

const client = () => new Client(new ClientOptionsBuilder().apiToken(TOKEN).baseUrl(url).maxRetries(0).build());

test("create a session, run JavaScript in it, sleep it", async () => {
  const c = client();
  const me = await c.me();
  assert.equal(me.email, "you@example.com");

  const created = await c.createSession(new CreateSessionRequestBuilder().name("smoke").build());
  assert.equal(created.id(), "s-abcde");
  assert.equal(created.mcpUrl(), `${url}/s-abcde/mcp`);
  assert.equal(created.lastInfo().state, SessionState.Starting);

  const result = await created.runJs("console.log(6 * 7)");
  assert.equal(result.output, "ran: console.log(6 * 7)");
  assert.equal(result.error, undefined);

  const info = await created.sleep();
  assert.equal(info.state, SessionState.Asleep);

  // The API token went to the token endpoint only.
  assert.equal(seen[0].path, "/oauth/token");
  for (const call of seen.slice(1)) assert.equal(call.authorization, "Bearer access-1");
  const last = seen[seen.length - 1];
  assert.equal(`${last.method} ${last.path}`, "POST /v1/sessions/s-abcde/sleep");
});

test("records are plain objects with optional fields", async () => {
  const created = await client().createSession({ name: "direct" });
  assert.equal(created.lastInfo().name, "direct");
});

test("errors are typed", async () => {
  await assert.rejects(client().getSession("s-zzzzz"), (error) => {
    assert.ok(ComputerUseError.NotFound.instanceOf(error), String(error));
    assert.equal(error.inner.message, "session not found");
    return true;
  });
  assert.throws(
    () => Client.withToken(""),
    (error) => ComputerUseError.Configuration.instanceOf(error),
  );
  assert.throws(
    () => new ClientOptionsBuilder().build(),
    (error) => BuildError.MissingRequiredField.instanceOf(error),
  );
});

test("builders do not mutate, and the version is the crate's", () => {
  const base = new CreateSessionRequestBuilder();
  const named = base.name("a");
  assert.equal(base.build().name, undefined);
  assert.equal(named.build().name, "a");
  assert.match(sdkVersion(), /^\d+\.\d+\.\d+/);
});
