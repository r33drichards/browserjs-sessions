// The SDK against the real API. Off unless COMPUTERUSE_LIVE=1; see
// sdk/scripts/live.sh, which reads the token and runs this.
//
// It creates one session and deletes it, whatever happens in between.

import assert from "node:assert/strict";
import { test } from "node:test";

import { Client, ComputerUseError, ManagementMode, PolicyState, SessionState } from "../dist/index.js";

const EXEC_JS = `
const call = async (tool, args) => JSON.parse((await mcp.callTool("exec", tool, args)).content[0].text);
const { id } = await call("exec", { bin: "echo", args: ["hello-from-exec"], timeout: 30 });
let logs = "", offset = 0, status = "running";
for (let i = 0; i < 200 && (status === "running" || status === "started"); i++) {
  const r = await call("stream_logs", { id, offset });
  logs += r.logs; offset = r.next_offset; status = r.status;
}
console.log(status, logs);
`;

const BROWSER_JS = `
const r = await mcp.callTool("browser", "browser_execute", { operations: [
  { type: "navigate", params: { url: "https://example.com" } },
  { type: "evaluate", params: { script: "document.title" } },
] });
console.log(r.content[0].text);
`;

const step = (name) => console.log("live:", name);
const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

async function putAndWait(session, source) {
  // A token writes a policy only as its manager.
  const management = { mode: ManagementMode.Iac, managedUrl: "https://github.com/r33drichards/computer-use" };
  let state = (await session.putPolicy({ source, management })).state;
  for (let i = 0; i < 90 && state !== PolicyState.Ready; i++) {
    await pause(1000);
    state = (await session.policy()).state;
  }
  assert.equal(state, PolicyState.Ready, "the policy is not ready after 90s");
}

async function scenario(client, session) {
  assert.ok(session.lastInfo().name, "the service names a session");

  step("wait until running");
  assert.equal((await session.waitUntilRunning(300_000n)).state, SessionState.Running);

  step("run_js: console output");
  let result = await session.runJs("console.log('answer', 6 * 7)");
  assert.equal(result.error, undefined, result.rawJson);
  assert.match(result.output, /answer 42/);

  step("run_js: browser_execute");
  result = await session.runJs(BROWSER_JS);
  assert.equal(result.error, undefined, result.rawJson);
  assert.match(result.output, /Example Domain/);

  step("run_js: exec, then stream_logs");
  result = await session.runJs(EXEC_JS);
  assert.equal(result.error, undefined, result.rawJson);
  assert.match(result.output, /hello-from-exec/);

  step("list_tools and call_tool");
  assert.ok((await session.listTools()).some((tool) => tool.name === "run_js"));
  assert.ok((await session.callTool("list_artifacts", undefined)).content.length > 0);

  step("policy: get");
  const policy = await session.policy();
  console.log("live: policy is", policy.state, "version", policy.version);

  step("policy: browser-only, and exec is denied");
  const presets = new Map((await client.policyPresets()).map((preset) => [preset.id, preset.source]));
  await putAndWait(session, presets.get("browser-only"));
  try {
    result = await session.runJs(EXEC_JS);
    assert.doesNotMatch(result.output, /hello-from-exec/, "exec ran under browser-only");
    assert.notEqual(result.error, undefined, "a denied call is an error of the program");
  } catch (error) {
    if (!ComputerUseError.instanceOf(error)) throw error;
    console.log("live: denied exec failed the call:", String(error));
  }

  step("policy: unrestricted again, and exec works");
  await putAndWait(session, presets.get("unrestricted"));
  assert.match((await session.runJs(EXEC_JS)).output, /hello-from-exec/);

  step("sleep");
  const asleep = await session.sleep();
  assert.ok([SessionState.Stopping, SessionState.Asleep].includes(asleep.state), String(asleep.state));
  assert.equal(asleep.stateSaved, true, "the sleep took a snapshot");

  step("wake");
  const waking = await session.wake();
  assert.ok([SessionState.Starting, SessionState.Running].includes(waking.state), String(waking.state));
  await session.waitUntilRunning(300_000n);

  step("rename");
  assert.equal((await session.rename("sdk-live-renamed")).name, "sdk-live-renamed");
}

test("live", { skip: process.env.COMPUTERUSE_LIVE !== "1" && "set COMPUTERUSE_LIVE=1 (see sdk/scripts/live.sh)", timeout: 1_200_000 }, async () => {
  const token = process.env.COMPUTERUSE_API_TOKEN;
  const client = new Client({ apiToken: token, baseUrl: process.env.COMPUTERUSE_BASE_URL });

  step("token exchange");
  const access = await client.accessToken(false);
  assert.ok(access && access !== token, "an access token, not the API token");

  step("me");
  const me = await client.me();
  assert.ok(me.email);
  assert.equal(me.admin, false);

  step("create");
  const session = await client.createSession({});
  try {
    await scenario(client, session);
  } finally {
    // Whatever happened: what was created is deleted.
    step("delete");
    await session.delete();
  }
  await assert.rejects(session.refresh(), (error) => ComputerUseError.NotFound.instanceOf(error));
});
