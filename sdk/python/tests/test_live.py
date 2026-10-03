"""The SDK against the real API. Off unless COMPUTERUSE_LIVE=1; see
sdk/scripts/live.sh, which reads the token and runs this.

It creates one session and deletes it, whatever happens in between.
"""

import asyncio
import os
import unittest

from computeruse import (
    Client,
    ClientOptions,
    ComputerUseError,
    CreateSessionRequest,
    Management,
    ManagementMode,
    PolicyInput,
    PolicyState,
    SessionState,
)

EXEC_JS = """
const call = async (tool, args) => JSON.parse((await mcp.callTool("exec", tool, args)).content[0].text);
const { id } = await call("exec", { bin: "echo", args: ["hello-from-exec"], timeout: 30 });
let logs = "", offset = 0, status = "running";
for (let i = 0; i < 200 && (status === "running" || status === "started"); i++) {
  const r = await call("stream_logs", { id, offset });
  logs += r.logs; offset = r.next_offset; status = r.status;
}
console.log(status, logs);
"""

BROWSER_JS = """
const r = await mcp.callTool("browser", "browser_execute", { operations: [
  { type: "navigate", params: { url: "https://example.com" } },
  { type: "evaluate", params: { script: "document.title" } },
] });
console.log(r.content[0].text);
"""


def step(name):
    print("live:", name, flush=True)


async def put_and_wait(session, source):
    # A token writes a policy only as its manager.
    management = Management(mode=ManagementMode.IAC, managed_url="https://github.com/r33drichards/computer-use")
    state = (await session.put_policy(PolicyInput(source=source, management=management))).state
    for _ in range(90):
        if state == PolicyState.READY:
            return
        await asyncio.sleep(1)
        state = (await session.policy()).state
    raise AssertionError("the policy is %s after 90s" % state)


@unittest.skipUnless(os.environ.get("COMPUTERUSE_LIVE") == "1", "set COMPUTERUSE_LIVE=1 (see sdk/scripts/live.sh)")
class Live(unittest.IsolatedAsyncioTestCase):
    async def test_live(self):
        token = os.environ["COMPUTERUSE_API_TOKEN"]
        client = Client(ClientOptions(api_token=token, base_url=os.environ.get("COMPUTERUSE_BASE_URL")))

        step("token exchange")
        access = await client.access_token(False)
        self.assertTrue(access and access != token, "an access token, not the API token")

        step("me")
        me = await client.me()
        self.assertTrue(me.email)
        self.assertFalse(me.admin)

        step("create")
        session = await client.create_session(CreateSessionRequest())
        try:
            await self.scenario(client, session)
        finally:
            # Whatever happened: what was created is deleted.
            step("delete")
            await session.delete()
        with self.assertRaises(ComputerUseError.NotFound):
            await session.refresh()

    async def scenario(self, client, session):
        created = session.last_info()
        self.assertTrue(created.name, "the service names a session")

        step("wait until running")
        info = await session.wait_until_running(300_000)
        self.assertEqual(info.state, SessionState.RUNNING)

        step("run_js: console output")
        result = await session.run_js("console.log('answer', 6 * 7)")
        self.assertIsNone(result.error, result.raw_json)
        self.assertIn("answer 42", result.output)

        step("run_js: browser_execute")
        result = await session.run_js(BROWSER_JS)
        self.assertIsNone(result.error, result.raw_json)
        self.assertIn("Example Domain", result.output)

        step("run_js: exec, then stream_logs")
        result = await session.run_js(EXEC_JS)
        self.assertIsNone(result.error, result.raw_json)
        self.assertIn("hello-from-exec", result.output)

        step("list_tools and call_tool")
        self.assertIn("run_js", [tool.name for tool in await session.list_tools()])
        self.assertTrue((await session.call_tool("list_artifacts", None)).content)

        step("policy: get")
        policy = await session.policy()
        print("live: policy is", policy.state, "version", policy.version)

        step("policy: browser-only, and exec is denied")
        presets = {preset.id: preset.source for preset in await client.policy_presets()}
        await put_and_wait(session, presets["browser-only"])
        try:
            result = await session.run_js(EXEC_JS)
            self.assertNotIn("hello-from-exec", result.output, "exec ran under browser-only")
            self.assertIsNotNone(result.error, "a denied call is an error of the program")
        except ComputerUseError as error:
            print("live: denied exec failed the call:", error)

        step("policy: unrestricted again, and exec works")
        await put_and_wait(session, presets["unrestricted"])
        self.assertIn("hello-from-exec", (await session.run_js(EXEC_JS)).output)

        step("sleep")
        asleep = await session.sleep()
        self.assertIn(asleep.state, (SessionState.STOPPING, SessionState.ASLEEP))
        self.assertTrue(asleep.state_saved, "the sleep took a snapshot")

        step("wake")
        waking = await session.wake()
        self.assertIn(waking.state, (SessionState.STARTING, SessionState.RUNNING))
        await session.wait_until_running(300_000)

        step("rename")
        self.assertEqual((await session.rename("sdk-live-renamed")).name, "sdk-live-renamed")


if __name__ == "__main__":
    unittest.main()
