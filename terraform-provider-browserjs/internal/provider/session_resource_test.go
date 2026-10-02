package provider

import (
	"testing"

	"github.com/r33drichards/computer-use/terraform-provider-browserjs/internal/fakeapi"
)

const sessionRes = "browserjs_session"

func TestSessionCreateWaitsForItsPolicy(t *testing.T) {
	h := newHarness(t)
	h.fake.Configure(func(s *fakeapi.Server) { s.SessionStartingReads = 3 })

	p := h.plan(sessionRes, h.null(sessionRes), cfg{"name": "research"})
	noErrors(t, "plan", p.diags)
	planned := attrs(p.state)
	for _, name := range []string{"id", "mcp_url", "state", "owner"} {
		if planned[name] != unknown {
			t.Errorf("%s is planned as %v, want unknown", name, planned[name])
		}
	}
	state, diags := h.apply(p)
	noErrors(t, "apply", diags)

	got := attrs(state)
	id := got["id"].(string)
	if !sessionID.MatchString(id) {
		t.Errorf("id %q", id)
	}
	if got["name"] != "research" || got["state"] != "running" || got["owner"] != "dev@example.com" {
		t.Errorf("state %v", got)
	}
	if want := "https://sessions.example.test/" + id + "/mcp"; got["mcp_url"] != want {
		t.Errorf("mcp_url %v, want %s", got["mcp_url"], want)
	}
	// Three reads say starting, the fourth says ready.
	if n := h.fake.Count("GET", "/v1/sessions/"+id); n != 4 {
		t.Errorf("%d reads while waiting, want 4", n)
	}
}

func TestSessionNamedByTheServer(t *testing.T) {
	h := newHarness(t)
	p := h.plan(sessionRes, h.null(sessionRes), cfg{})
	if attrs(p.state)["name"] != unknown {
		t.Fatalf("name is planned as %v", attrs(p.state)["name"])
	}
	state, diags := h.apply(p)
	noErrors(t, "apply", diags)
	if str(state, "name") != "session-1" {
		t.Errorf("name %q", str(state, "name"))
	}
	// Still unset in the configuration: the server's name stays, no change.
	if p := h.plan(sessionRes, state, cfg{}); p.changes() {
		t.Errorf("a second plan changes %v", attrs(p.state))
	}
}

func TestSessionRenameIsInPlace(t *testing.T) {
	h := newHarness(t)
	state := h.mustApply(sessionRes, h.null(sessionRes), cfg{"name": "before"})
	id := str(state, "id")

	p := h.plan(sessionRes, state, cfg{"name": "after"})
	noErrors(t, "plan", p.diags)
	if len(p.replace) > 0 {
		t.Fatalf("a rename plans a replacement: %v", p.replace)
	}
	if got := attrs(p.state); got["id"] != id || got["mcp_url"] != str(state, "mcp_url") {
		t.Errorf("id or mcp_url not kept in the plan: %v", got)
	}
	next, diags := h.apply(p)
	noErrors(t, "apply", diags)
	if str(next, "id") != id || str(next, "name") != "after" {
		t.Errorf("state %v", attrs(next))
	}
	if h.fake.Count("PATCH", "/v1/sessions/"+id) != 1 || h.fake.Count("POST", "/v1/sessions") != 1 {
		t.Errorf("requests %v", h.fake.Requests())
	}
}

func TestSessionGoneIsRemovedFromState(t *testing.T) {
	h := newHarness(t)
	state := h.mustApply(sessionRes, h.null(sessionRes), cfg{"name": "a"})
	h.mustApply(sessionRes, state, nil)
	if h.fake.Has(str(state, "id")) {
		t.Fatal("the session was not deleted")
	}
	if got := h.mustRead(sessionRes, state); !got.IsNull() {
		t.Errorf("a deleted session is still in the state: %v", attrs(got))
	}
	// Destroying what is already gone is not an error.
	h.mustApply(sessionRes, state, nil)
}

func TestSessionImport(t *testing.T) {
	h := newHarness(t)
	id := h.fake.AddSession("made-in-the-ui")
	state, diags := h.importState(sessionRes, id)
	noErrors(t, "import", diags)
	if str(state, "id") != id || str(state, "name") != "made-in-the-ui" || str(state, "state") != "running" {
		t.Errorf("state %v", attrs(state))
	}
	if p := h.plan(sessionRes, state, cfg{"name": "made-in-the-ui"}); p.changes() {
		t.Errorf("a plan after import changes %v", attrs(p.state))
	}
	_, diags = h.importState(sessionRes, "not-an-id")
	wantError(t, diags, "Not a session ID")
}

func TestSessionCreateTimeoutKeepsTheSession(t *testing.T) {
	h := newHarness(t)
	h.fake.Configure(func(s *fakeapi.Server) { s.SessionStartingReads = 1 << 30 })
	p := h.plan(sessionRes, h.null(sessionRes), cfg{"name": "slow", "timeouts": cfg{"create": "30ms"}})
	noErrors(t, "plan", p.diags)
	state, diags := h.apply(p)
	wantError(t, diags, "not loaded in time", `"loading"`)
	// The session exists, so it must be in the state for Terraform to
	// replace or destroy.
	if id := str(state, "id"); !h.fake.Has(id) {
		t.Errorf("the state has id %q, which is not the session created", id)
	}
}

func TestSessionLimit(t *testing.T) {
	h := newHarness(t)
	h.fake.Configure(func(s *fakeapi.Server) { s.Limit = 0 })
	p := h.plan(sessionRes, h.null(sessionRes), cfg{"name": "one-too-many"})
	state, diags := h.apply(p)
	wantError(t, diags, "create the session", "409", "session limit reached")
	if !state.IsNull() {
		t.Errorf("a failed create left state %v", attrs(state))
	}
}
