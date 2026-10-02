package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/r33drichards/computer-use/terraform-provider-browserjs/internal/client"
	"github.com/r33drichards/computer-use/terraform-provider-browserjs/internal/fakeapi"
)

const (
	policyRes  = "browserjs_session_policy"
	managedURL = "https://github.com/example/infra/tree/main/browserjs"

	// Everything in the browser but script in the page. No desktop, no shell.
	noScripting = `package browserjs.policy

import rego.v1

allow_tool_call if {
	input.server == "browser"
	input.tool == "browser_execute"
	every op in input.arguments.operations {
		not op.type in {"evaluate", "setContent"}
	}
}
`
	observeOnly = `package browserjs.policy

import rego.v1

allow_tool_call if {
	input.server == "browser"
	input.tool == "browser_execute"
	every op in input.arguments.operations {
		op.type in {"screenshot", "url", "wait"}
	}
}
`
	// The whole browser and nothing else.
	browserOnly = `package browserjs.policy

import rego.v1

allow_tool_call if {
	input.server == "browser"
	input.tool == "browser_execute"
}
`
	// noScripting with desktop control and the shell left open: either one
	// walks around its rules.
	bypassable = noScripting + `
allow_tool_call if {
	input.server == "browser"
	input.tool == "desktop_execute"
}

allow_tool_call if input.server == "exec"
`
)

func policyConfig(sessionID, source string) cfg {
	return cfg{"session_id": sessionID, "managed_url": managedURL, "rego": source}
}

// withPolicy makes a session and gives it the no-scripting policy.
func withPolicy(t *testing.T, h *harness) (id string, state tftypes.Value) {
	t.Helper()
	id = h.fake.AddSession("research")
	return id, h.mustApply(policyRes, h.null(policyRes), policyConfig(id, noScripting))
}

func TestPolicyConfigIsValidatedOffline(t *testing.T) {
	h := newHarness(t)
	id := "s-ab2cd"
	before := len(h.fake.Requests())

	wantError(t, h.validate(policyRes, cfg{"session_id": id, "managed_url": managedURL}), "rego", "Missing")
	wantError(t, h.validate(policyRes, policyConfig(id, "")), "rego", "length")
	wantError(t, h.validate(policyRes, policyConfig(id, strings.Repeat("#", 65537))), "rego", "length")
	wantError(t, h.validate(policyRes, cfg{"session_id": id, "managed_url": "http://example.com/x", "rego": noScripting}), "managed_url", "https")
	wantError(t, h.validate(policyRes, cfg{"session_id": id, "managed_url": "not a url", "rego": noScripting}), "managed_url", "https")
	wantError(t, h.validate(policyRes, cfg{"session_id": "research", "managed_url": managedURL, "rego": noScripting}), "session_id", "session ID")
	noErrors(t, "validate", h.validate(policyRes, policyConfig(id, noScripting)))
	// Values not known until apply are not judged.
	noErrors(t, "validate", h.validate(policyRes, cfg{"session_id": unknown, "managed_url": unknown, "rego": unknown}))

	if after := len(h.fake.Requests()); after != before {
		t.Errorf("validation made %d requests", after-before)
	}
}

func TestPolicyCreatePutsThePolicyInIaCMode(t *testing.T) {
	h := newHarness(t)
	id, state := withPolicy(t, h)

	var put fakeapi.Request
	for _, r := range h.fake.Requests() {
		if r.Method == "PUT" && r.Path == "/v1/sessions/"+id+"/policy" {
			put = r
		}
	}
	var body client.PolicyInput
	if err := json.Unmarshal([]byte(put.Body), &body); err != nil {
		t.Fatalf("PUT body %q: %v", put.Body, err)
	}
	if body.Kind != "rego" || body.Source != noScripting || body.Management == nil ||
		body.Management.Mode != "iac" || body.Management.ManagedURL != managedURL {
		t.Errorf("PUT body %s", put.Body)
	}

	server := h.fake.PolicyOf(id)
	if server.Management.Mode != "iac" || server.Management.ManagedURL != managedURL {
		t.Errorf("the server has management %+v", server.Management)
	}
	got := attrs(state)
	if got["id"] != id || got["session_id"] != id || got["state"] != "ready" || got["wait_for_ready"] != true {
		t.Errorf("state %v", got)
	}
	if got["version"] != float64(server.Version) || got["hash"] != server.Hash || got["compiled_rego"] != server.Rego {
		t.Errorf("state %v, server %+v", got, server)
	}
	if got["rego"] != noScripting || got["compiled_rego"] != noScripting {
		t.Errorf("state %v", got)
	}
}

func TestPolicyEditIsInPlaceAndNeverReplaces(t *testing.T) {
	h := newHarness(t)
	session := h.mustApply(sessionRes, h.null(sessionRes), cfg{"name": "research"})
	id := str(session, "id")
	state := h.mustApply(policyRes, h.null(policyRes), policyConfig(id, noScripting))
	before := attrs(state)

	p := h.plan(policyRes, state, policyConfig(id, observeOnly))
	noErrors(t, "plan", p.diags)
	if len(p.replace) > 0 {
		t.Fatalf("a policy edit plans a replacement: %v", p.replace)
	}
	planned := attrs(p.state)
	if planned["id"] != id {
		t.Errorf("id planned as %v", planned["id"])
	}
	for _, name := range []string{"version", "hash", "compiled_rego", "state"} {
		if planned[name] != unknown {
			t.Errorf("%s is planned as %v, want unknown", name, planned[name])
		}
	}
	next, diags := h.apply(p)
	noErrors(t, "apply", diags)
	consistent(t, p.state, next)
	after := attrs(next)
	if after["version"].(float64) <= before["version"].(float64) || after["hash"] == before["hash"] {
		t.Errorf("version %v -> %v, hash %v -> %v", before["version"], after["version"], before["hash"], after["hash"])
	}
	if h.fake.PolicyOf(id).Source != observeOnly {
		t.Errorf("the server has %s", h.fake.PolicyOf(id).Source)
	}

	// The session saw none of it.
	if sp := h.plan(sessionRes, h.mustRead(sessionRes, session), cfg{"name": "research"}); sp.changes() || len(sp.replace) > 0 {
		t.Errorf("the session plans a change after a policy edit: %v", attrs(sp.state))
	}
	if h.fake.Count("POST", "/v1/sessions") != 1 || h.fake.Count("DELETE", "/v1/sessions/"+id) != 0 {
		t.Error("the session was recreated or deleted")
	}
}

func TestPolicyMovingToAnotherSessionReplaces(t *testing.T) {
	h := newHarness(t)
	_, state := withPolicy(t, h)
	other := h.fake.AddSession("other")
	p := h.plan(policyRes, state, policyConfig(other, noScripting))
	noErrors(t, "plan", p.diags)
	if len(p.replace) != 1 || !strings.Contains(p.replace[0], "session_id") {
		t.Fatalf("requires replace: %v", p.replace)
	}
}

// The policy's schema has rego and no json: the JSON format is gone.
func TestPolicySchemaIsRegoOnly(t *testing.T) {
	h := newHarness(t)
	required := map[string]bool{}
	for _, a := range h.resourceSchema(policyRes).Block.Attributes {
		required[a.Name] = a.Required
	}
	if _, ok := required["json"]; ok {
		t.Error("the policy still has a json attribute")
	}
	if !required["rego"] {
		t.Error("rego is not required")
	}
}

// A refresh that changes nothing plans nothing, and Rego is compared as text.
func TestPolicyIsComparedAsText(t *testing.T) {
	h := newHarness(t)
	id, state := withPolicy(t, h)
	state = h.mustRead(policyRes, state)
	if p := h.plan(policyRes, state, policyConfig(id, noScripting)); p.changes() {
		t.Errorf("a second plan changes %v", attrs(p.state))
	}
	p := h.plan(policyRes, state, policyConfig(id, noScripting+"\n# a comment\n"))
	noErrors(t, "plan", p.diags)
	if !p.changes() || len(p.replace) > 0 {
		t.Errorf("a comment added: changes %v, replace %v", p.changes(), p.replace)
	}
}

// Rego is the only kind there is. An API that reports another is not read as
// if it were Rego.
func TestPolicyOfAnUnknownKindIsAnError(t *testing.T) {
	h := newHarness(t)
	id, state := withPolicy(t, h)
	h.fake.Configure(func(s *fakeapi.Server) { s.ReportKind = "json" })
	_, diags := h.read(policyRes, state)
	wantError(t, diags, "of a kind this provider does not know", id, `"json"`, "Rego only")
	_, diags = h.importState(policyRes, id)
	wantError(t, diags, "of a kind this provider does not know")
}

func TestPolicy202ThenReady(t *testing.T) {
	h := newHarness(t)
	h.fake.Configure(func(s *fakeapi.Server) { s.PolicyLoadingReads = 3 })
	id := h.fake.AddSession("research")
	state := h.mustApply(policyRes, h.null(policyRes), policyConfig(id, noScripting))
	if str(state, "state") != "ready" {
		t.Errorf("state %q after waiting", str(state, "state"))
	}
	// Three reads say loading, the fourth says ready.
	if n := h.fake.Count("GET", "/v1/sessions/"+id+"/policy"); n != 4 {
		t.Errorf("%d reads while waiting, want 4", n)
	}
}

func TestPolicy202WithoutWaiting(t *testing.T) {
	h := newHarness(t)
	h.fake.Configure(func(s *fakeapi.Server) { s.PolicyLoadingReads = 2 })
	id := h.fake.AddSession("research")
	c := policyConfig(id, noScripting)
	c["wait_for_ready"] = false
	state := h.mustApply(policyRes, h.null(policyRes), c)
	if str(state, "state") != "loading" {
		t.Errorf("state %q", str(state, "state"))
	}
	if n := h.fake.Count("GET", "/v1/sessions/"+id+"/policy"); n != 0 {
		t.Errorf("%d reads without wait_for_ready", n)
	}
	// Refreshes catch up, and none of them plans a change.
	for range 3 {
		state = h.mustRead(policyRes, state)
	}
	if str(state, "state") != "ready" {
		t.Errorf("state %q after refreshing", str(state, "state"))
	}
	if p := h.plan(policyRes, state, c); p.changes() {
		t.Errorf("a plan after loading changes %v", attrs(p.state))
	}
}

func TestPolicy202ThenInvalid(t *testing.T) {
	h := newHarness(t)
	h.fake.Configure(func(s *fakeapi.Server) {
		s.PolicyLoadingReads = 2
		s.CompileErrors = []client.Diagnostic{{Row: 7, Col: 3, Code: "rego_unsafe_var_error", Message: "var x is unsafe"}}
	})
	id := h.fake.AddSession("research")
	p := h.plan(policyRes, h.null(policyRes), policyConfig(id, browserOnly))
	noErrors(t, "plan", p.diags)
	state, diags := h.apply(p)
	wantError(t, diags, "rego", "Invalid policy", "rego line 7, column 3: var x is unsafe (rego_unsafe_var_error)")
	wantError(t, diags, "did not compile")
	if !state.IsNull() {
		t.Errorf("a failed create left state %v", attrs(state))
	}
}

func TestPolicyWaitTimesOut(t *testing.T) {
	h := newHarness(t)
	h.fake.Configure(func(s *fakeapi.Server) { s.PolicyLoadingReads = 1 << 30 })
	id := h.fake.AddSession("research")
	c := policyConfig(id, noScripting)
	c["timeouts"] = cfg{"create": "30ms"}
	p := h.plan(policyRes, h.null(policyRes), c)
	noErrors(t, "plan", p.diags)
	_, diags := h.apply(p)
	wantError(t, diags, "not in force in time", "still loading", "30ms")
}

func TestPolicyInvalidAtPlanTime(t *testing.T) {
	h := newHarness(t)
	id := h.fake.AddSession("research")

	// A rule that is not closed: the error is at the end, on line 4.
	unclosed := "package browserjs.policy\n\nallow_tool_call if {\n"
	p := h.plan(policyRes, h.null(policyRes), policyConfig(id, unclosed))
	wantError(t, p.diags, `AttributeName("rego")`, "Invalid policy", "rego line 4, column 1: unexpected end of file: } expected (rego_parse_error)")

	wrongPackage := "# a policy\npackage wrong.name\n\nallow_tool_call := true\n"
	p = h.plan(policyRes, h.null(policyRes), policyConfig(id, wrongPackage))
	wantError(t, p.diags, `AttributeName("rego")`, "Invalid policy", "rego line 2, column 9: the package must be browserjs.policy, not wrong.name (policy_guard_error)")

	// An error about the module as a whole has no position to give.
	noEntry := "package browserjs.policy\n\nallow := true\n"
	p = h.plan(policyRes, h.null(policyRes), policyConfig(id, noEntry))
	wantError(t, p.diags, `AttributeName("rego")`, "Invalid policy", "the policy must define allow_tool_call (policy_guard_error)")
	for _, text := range errorsOf(p.diags) {
		if strings.Contains(text, "line") {
			t.Errorf("an error without a position names a line: %s", text)
		}
	}

	if n := h.fake.Count("PUT", "/v1/sessions/"+id+"/policy"); n != 0 {
		t.Errorf("a plan wrote the policy %d times", n)
	}
	if n := h.fake.Count("POST", "/v1/policies/validate"); n != 3 {
		t.Errorf("%d validate calls, want 3", n)
	}
}

// A policy that restricts the browser and leaves the desktop and the shell
// open is saved, with a warning for each way around it. The API gives these
// no line and no column.
func TestPolicyWarnings(t *testing.T) {
	h := newHarness(t)
	id := h.fake.AddSession("research")
	check := func(when string, diags []*tfprotov6.Diagnostic) {
		t.Helper()
		noErrors(t, when, diags)
		wantWarning(t, diags, `AttributeName("rego")`, "Policy warning", "the policy refuses some browser_execute calls but allows desktop_execute", "(browser_bypass_desktop)")
		wantWarning(t, diags, `AttributeName("rego")`, "Policy warning", "the policy refuses some browser_execute calls but allows exec", "(browser_bypass_shell)")
		for _, text := range warningsOf(diags) {
			if strings.Contains(text, "line") || strings.Contains(text, "column") {
				t.Errorf("%s: a warning without a position names one: %s", when, text)
			}
		}
	}
	p := h.plan(policyRes, h.null(policyRes), policyConfig(id, bypassable))
	check("plan", p.diags)
	state, diags := h.apply(p)
	check("apply", diags)
	if str(state, "state") != "ready" || h.fake.PolicyOf(id).Source != bypassable {
		t.Errorf("a policy with warnings was not saved: %v", attrs(state))
	}

	// The presets that restrict the browser deny both, and are not warned about.
	p = h.plan(policyRes, state, policyConfig(id, noScripting))
	noErrors(t, "plan", p.diags)
	if w := warningsOf(p.diags); len(w) != 0 {
		t.Errorf("warnings %q", w)
	}
}

func TestPolicyNotValidatedWhileUnknown(t *testing.T) {
	h := newHarness(t)
	p := h.plan(policyRes, h.null(policyRes), cfg{"session_id": unknown, "managed_url": managedURL, "rego": unknown})
	noErrors(t, "plan", p.diags)
	if n := h.fake.Count("POST", "/v1/policies/validate"); n != 0 {
		t.Errorf("%d validate calls for an unknown source", n)
	}
}

func TestPolicyValidationUnavailableIsAWarning(t *testing.T) {
	h := newHarness(t)
	id := h.fake.AddSession("research")
	h.fake.Configure(func(s *fakeapi.Server) { s.ValidateUnavailable = true })
	p := h.plan(policyRes, h.null(policyRes), policyConfig(id, noScripting))
	noErrors(t, "plan", p.diags)
	wantWarning(t, p.diags, "not checked at plan time", "503")
	// The apply does not save what could not be checked.
	_, diags := h.apply(p)
	wantError(t, diags, "save the policy", "503")
	if h.fake.PolicyOf(id).Management.Mode != "editor" {
		t.Error("something was saved")
	}
}

// An apply whose plan did not catch the error (the source was unknown then).
func TestPolicyInvalidAtApplyTime(t *testing.T) {
	h := newHarness(t)
	id := h.fake.AddSession("research")
	p := h.plan(policyRes, h.null(policyRes), policyConfig(id, noScripting))
	noErrors(t, "plan", p.diags)
	bad := policyConfig(id, "package browserjs.policy\n\nallow_tool_call if {\n\tinput.server == \"browser\"\n}}\n")
	typ := h.resourceSchema(policyRes).ValueType()
	p.config = toValue(t, typ, bad)
	c := cfg{"id": unknown, "version": unknown, "hash": unknown, "compiled_rego": unknown, "state": unknown, "wait_for_ready": true}
	for k, v := range bad {
		c[k] = v
	}
	p.state = toValue(t, typ, c)
	state, diags := h.apply(p)
	wantError(t, diags, `AttributeName("rego")`, "Invalid policy", "rego line 5, column 2: unexpected } (rego_parse_error)")
	if !state.IsNull() {
		t.Errorf("a failed create left state %v", attrs(state))
	}
}

func TestPolicyOnASessionThatPredatesPolicies(t *testing.T) {
	h := newHarness(t)
	id := h.fake.AddLegacySession("old")
	p := h.plan(policyRes, h.null(policyRes), policyConfig(id, noScripting))
	noErrors(t, "plan", p.diags)
	state, diags := h.apply(p)
	wantError(t, diags, "session_id", "The session predates policies", id, "Recreate the session")
	if !state.IsNull() {
		t.Errorf("a failed create left state %v", attrs(state))
	}
	_, diags = h.importState(policyRes, id)
	wantError(t, diags, "The session predates policies")
}

func TestPolicyOnASessionThatDoesNotExist(t *testing.T) {
	h := newHarness(t)
	p := h.plan(policyRes, h.null(policyRes), policyConfig("s-ab2cd", noScripting))
	noErrors(t, "plan", p.diags)
	_, diags := h.apply(p)
	wantError(t, diags, "session_id", "No such session", "s-ab2cd")
}

func TestPolicyDriftWhenTheUITakesItBack(t *testing.T) {
	h := newHarness(t)
	id, state := withPolicy(t, h)
	c := policyConfig(id, noScripting)

	h.fake.UIManageHere(id)
	state = h.mustRead(policyRes, state)
	if got := attrs(state)["managed_url"]; got != "" {
		t.Fatalf("managed_url reads as %q after the UI took the policy, want empty", got)
	}
	p := h.plan(policyRes, state, c)
	noErrors(t, "plan", p.diags)
	if !p.changes() || len(p.replace) > 0 {
		t.Fatalf("plan: changes %v, replace %v", p.changes(), p.replace)
	}
	if attrs(p.state)["managed_url"] != managedURL {
		t.Errorf("managed_url planned as %v", attrs(p.state)["managed_url"])
	}
	state, diags := h.apply(p)
	noErrors(t, "apply", diags)
	if m := h.fake.PolicyOf(id).Management; m.Mode != "iac" || m.ManagedURL != managedURL {
		t.Errorf("the apply did not take the policy back: %+v", m)
	}
	if p := h.plan(policyRes, h.mustRead(policyRes, state), c); p.changes() {
		t.Errorf("still drifting: %v", attrs(p.state))
	}
}

func TestPolicyDriftWhenTheSourceIsEdited(t *testing.T) {
	h := newHarness(t)
	id, state := withPolicy(t, h)
	hash := str(state, "hash")

	if err := h.fake.UIEdit(id, observeOnly); err != nil {
		t.Fatal(err)
	}
	state = h.mustRead(policyRes, state)
	got := attrs(state)
	if got["rego"] != observeOnly || got["hash"] == hash || got["managed_url"] != "" {
		t.Fatalf("the refresh did not see the edit: %v", got)
	}
	p := h.plan(policyRes, state, policyConfig(id, noScripting))
	noErrors(t, "plan", p.diags)
	if !p.changes() || len(p.replace) > 0 {
		t.Fatalf("plan: changes %v, replace %v", p.changes(), p.replace)
	}
	state, diags := h.apply(p)
	noErrors(t, "apply", diags)
	if server := h.fake.PolicyOf(id); server.Source != noScripting || server.Hash != hash || str(state, "hash") != hash {
		t.Errorf("the apply did not put the policy back: %+v", server)
	}
}

func TestPolicyOfADeletedSessionIsRemovedFromState(t *testing.T) {
	h := newHarness(t)
	session := h.mustApply(sessionRes, h.null(sessionRes), cfg{"name": "research"})
	id := str(session, "id")
	state := h.mustApply(policyRes, h.null(policyRes), policyConfig(id, noScripting))
	h.mustApply(sessionRes, session, nil)
	if got := h.mustRead(policyRes, state); !got.IsNull() {
		t.Errorf("the policy of a deleted session is still in the state: %v", attrs(got))
	}
	h.mustApply(policyRes, state, nil)
}

func TestPolicyDestroyResetsToUnrestricted(t *testing.T) {
	h := newHarness(t)
	id, state := withPolicy(t, h)
	h.mustApply(policyRes, state, nil)
	server := h.fake.PolicyOf(id)
	if server.Management.Mode != "editor" || server.Source != fakeapi.Unrestricted {
		t.Errorf("after destroy the server has %+v", server)
	}
	if !h.fake.Has(id) {
		t.Error("destroying the policy deleted the session")
	}
}

func TestPolicyDestroyLeavesAPolicyTheUIManages(t *testing.T) {
	h := newHarness(t)
	id, state := withPolicy(t, h)
	if err := h.fake.UIEdit(id, observeOnly); err != nil {
		t.Fatal(err)
	}
	p := h.plan(policyRes, state, nil)
	noErrors(t, "plan", p.diags)
	_, diags := h.apply(p)
	noErrors(t, "destroy", diags)
	wantWarning(t, diags, "left as it is", id)
	if h.fake.PolicyOf(id).Source != observeOnly {
		t.Error("the destroy overwrote the policy edited in the UI")
	}
}

func TestPolicyImport(t *testing.T) {
	h := newHarness(t)
	id, _ := withPolicy(t, h)

	state, diags := h.importState(policyRes, id)
	noErrors(t, "import", diags)
	got := attrs(state)
	if got["id"] != id || got["session_id"] != id || got["rego"] != noScripting || got["managed_url"] != managedURL || got["wait_for_ready"] != true {
		t.Errorf("imported %v", got)
	}
	if p := h.plan(policyRes, state, policyConfig(id, noScripting)); p.changes() {
		t.Errorf("a plan after import changes %v", attrs(p.state))
	}

	_, diags = h.importState(policyRes, "research")
	wantError(t, diags, "Not a session ID")
}

// Importing the policy of a session made in the UI: the first apply puts it
// in iac mode.
func TestPolicyImportFromEditorMode(t *testing.T) {
	h := newHarness(t)
	id := h.fake.AddSession("made-in-the-ui")
	if err := h.fake.UIEdit(id, browserOnly); err != nil {
		t.Fatal(err)
	}
	state, diags := h.importState(policyRes, id)
	noErrors(t, "import", diags)
	if got := attrs(state); got["rego"] != browserOnly || got["managed_url"] != "" {
		t.Errorf("imported %v", got)
	}
	state = h.mustApply(policyRes, state, policyConfig(id, browserOnly))
	if m := h.fake.PolicyOf(id).Management; m.Mode != "iac" || m.ManagedURL != managedURL {
		t.Errorf("management after the first apply: %+v", m)
	}
	if str(state, "managed_url") != managedURL {
		t.Errorf("state %v", attrs(state))
	}
}

func TestPolicyChangingOnlyTheWaitSendsNothing(t *testing.T) {
	h := newHarness(t)
	id, state := withPolicy(t, h)
	puts := h.fake.Count("PUT", "/v1/sessions/"+id+"/policy")
	c := policyConfig(id, noScripting)
	c["wait_for_ready"] = false
	p := h.plan(policyRes, state, c)
	noErrors(t, "plan", p.diags)
	planned := attrs(p.state)
	for _, name := range []string{"version", "hash", "compiled_rego", "state"} {
		if planned[name] != attrs(state)[name] {
			t.Errorf("%s is planned as %v, want the value in the state", name, planned[name])
		}
	}
	next, diags := h.apply(p)
	noErrors(t, "apply", diags)
	consistent(t, p.state, next)
	if attrs(next)["wait_for_ready"] != false {
		t.Errorf("state %v", attrs(next))
	}
	if n := h.fake.Count("PUT", "/v1/sessions/"+id+"/policy"); n != puts {
		t.Errorf("%d more writes of an unchanged policy", n-puts)
	}
}

func TestPolicyManagedURLChangeIsInPlace(t *testing.T) {
	h := newHarness(t)
	id, state := withPolicy(t, h)
	c := policyConfig(id, noScripting)
	c["managed_url"] = "https://gitlab.example.com/infra/browserjs"
	p := h.plan(policyRes, state, c)
	noErrors(t, "plan", p.diags)
	if len(p.replace) > 0 {
		t.Fatalf("requires replace: %v", p.replace)
	}
	_, diags := h.apply(p)
	noErrors(t, "apply", diags)
	if got := h.fake.PolicyOf(id).Management.ManagedURL; got != c["managed_url"] {
		t.Errorf("managed_url on the server is %q", got)
	}
}
