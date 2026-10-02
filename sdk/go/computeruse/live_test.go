package computeruse_test

// The SDK against the real API. Off unless COMPUTERUSE_LIVE=1; see
// sdk/scripts/live.sh, which reads the token and runs this.
//
// It creates one session and deletes it, whatever happens in between.

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/r33drichards/browserjs-sessions/sdk/go/computeruse"
)

const execJS = `
const call = async (tool, args) => JSON.parse((await mcp.callTool("exec", tool, args)).content[0].text);
const { id } = await call("exec", { bin: "echo", args: ["hello-from-exec"], timeout: 30 });
let logs = "", offset = 0, status = "running";
for (let i = 0; i < 200 && (status === "running" || status === "started"); i++) {
  const r = await call("stream_logs", { id, offset });
  logs += r.logs; offset = r.next_offset; status = r.status;
}
console.log(status, logs);
`

const browserJS = `
const r = await mcp.callTool("browser", "browser_execute", { operations: [
  { type: "navigate", params: { url: "https://example.com" } },
  { type: "evaluate", params: { script: "document.title" } },
] });
console.log(r.content[0].text);
`

func putAndWait(t *testing.T, session *computeruse.Session, source string) {
	t.Helper()
	// A token writes a policy only as its manager.
	managedURL := "https://github.com/r33drichards/computer-use"
	saved, err := session.PutPolicy(computeruse.PolicyInput{
		Source:     source,
		Management: &computeruse.Management{Mode: computeruse.ManagementModeIac, ManagedUrl: &managedURL},
	})
	if err != nil {
		t.Fatalf("put policy: %v", err)
	}
	state := saved.State
	for i := 0; i < 90 && state != computeruse.PolicyStateReady; i++ {
		time.Sleep(time.Second)
		p, err := session.Policy()
		if err != nil {
			t.Fatalf("get policy: %v", err)
		}
		state = p.State
	}
	if state != computeruse.PolicyStateReady {
		t.Fatalf("the policy is %v after 90s", state)
	}
}

func runJS(t *testing.T, session *computeruse.Session, code, want string) {
	t.Helper()
	result, err := session.RunJs(code)
	if err != nil {
		t.Fatalf("run_js: %v", err)
	}
	if result.Error != nil || !strings.Contains(result.Output, want) {
		t.Fatalf("run_js: want %q in the output, got %s", want, result.RawJson)
	}
}

func TestLive(t *testing.T) {
	if os.Getenv("COMPUTERUSE_LIVE") != "1" {
		t.Skip("set COMPUTERUSE_LIVE=1 (see sdk/scripts/live.sh)")
	}
	token := os.Getenv("COMPUTERUSE_API_TOKEN")
	options := computeruse.ClientOptions{ApiToken: token}
	if base := os.Getenv("COMPUTERUSE_BASE_URL"); base != "" {
		options.BaseUrl = &base
	}
	client, err := computeruse.NewClient(options)
	if err != nil {
		t.Fatal(err)
	}

	t.Log("token exchange")
	access, err := client.AccessToken(false)
	if err != nil || access == "" || access == token {
		t.Fatalf("want an access token that is not the API token (err: %v)", err)
	}

	t.Log("me")
	me, err := client.Me()
	if err != nil || me.Email == "" || me.Admin {
		t.Fatalf("me: admin=%v err=%v", me.Admin, err)
	}

	t.Log("create")
	session, err := client.CreateSession(computeruse.CreateSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// Whatever happens: what was created is deleted.
	defer func() {
		t.Log("delete")
		if err := session.Delete(); err != nil {
			t.Errorf("delete: %v", err)
		}
		if _, err := session.Refresh(); !errors.Is(err, computeruse.ErrComputerUseErrorNotFound) {
			t.Errorf("after delete, a read answered %v", err)
		}
	}()
	if info := session.LastInfo(); info == nil || info.Name == "" {
		t.Fatal("the service names a session")
	}

	t.Log("wait until running")
	limit := uint64(300_000)
	if info, err := session.WaitUntilRunning(&limit); err != nil || info.State != computeruse.SessionStateRunning {
		t.Fatalf("wait: %v %v", info.State, err)
	}

	t.Log("run_js: console output, browser_execute, exec then stream_logs")
	runJS(t, session, "console.log('answer', 6 * 7)", "answer 42")
	runJS(t, session, browserJS, "Example Domain")
	runJS(t, session, execJS, "hello-from-exec")

	t.Log("list_tools and call_tool")
	tools, err := session.ListTools()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range tools {
		found = found || tool.Name == "run_js"
	}
	if !found {
		t.Fatalf("no run_js among %d tools", len(tools))
	}
	if listed, err := session.CallTool("list_artifacts", nil); err != nil || len(listed.Content) == 0 {
		t.Fatalf("call_tool: %v", err)
	}

	t.Log("policy: get")
	policy, err := session.Policy()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("policy is %v", policy.State)

	t.Log("policy: browser-only, and exec is denied")
	presets, err := client.PolicyPresets()
	if err != nil {
		t.Fatal(err)
	}
	source := map[string]string{}
	for _, preset := range presets {
		source[preset.Id] = preset.Source
	}
	if source["browser-only"] == "" || source["unrestricted"] == "" {
		t.Fatalf("presets: %d, without browser-only or unrestricted", len(presets))
	}
	putAndWait(t, session, source["browser-only"])
	if result, err := session.RunJs(execJS); err != nil {
		t.Logf("denied exec failed the call: %v", err)
	} else if strings.Contains(result.Output, "hello-from-exec") || result.Error == nil {
		t.Fatalf("exec was not denied under browser-only: %s", result.RawJson)
	}

	t.Log("policy: unrestricted again, and exec works")
	putAndWait(t, session, source["unrestricted"])
	runJS(t, session, execJS, "hello-from-exec")

	t.Log("sleep")
	asleep, err := session.Sleep()
	if err != nil {
		t.Fatal(err)
	}
	if asleep.State != computeruse.SessionStateStopping && asleep.State != computeruse.SessionStateAsleep {
		t.Fatalf("after sleep the session is %v", asleep.State)
	}
	if asleep.StateSaved == nil || !*asleep.StateSaved {
		t.Fatal("the sleep took no snapshot")
	}

	t.Log("wake")
	if _, err := session.Wake(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.WaitUntilRunning(&limit); err != nil {
		t.Fatal(err)
	}

	t.Log("rename")
	if renamed, err := session.Rename("sdk-live-renamed"); err != nil || renamed.Name != "sdk-live-renamed" {
		t.Fatalf("rename: %v", err)
	}
}
