package fakeapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// examples is where the contract's example policies are.
var examples = filepath.Join("..", "..", "..", "docs", "contracts", "policy", "examples")

func TestUnrestrictedIsTheContracts(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(examples, "unrestricted.rego"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != Unrestricted {
		t.Errorf("Unrestricted is not docs/contracts/policy/examples/unrestricted.rego:\n%s", raw)
	}
}

// The seven presets validate, and none of them leaves a way around its own
// rules.
func TestValidateAcceptsTheContractExamples(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(examples, "*.rego"))
	if err != nil || len(files) != 7 {
		t.Fatalf("%d files, %v", len(files), err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if v := Validate(string(raw)); !v.OK || v.Hash == "" || v.Rego != string(raw) || len(v.Warnings) != 0 {
			t.Errorf("%s: ok %v, errors %+v, warnings %+v", filepath.Base(f), v.OK, v.Errors, v.Warnings)
		}
	}
}

func TestValidatePositions(t *testing.T) {
	const head = "package browserjs.policy\n"
	for name, tc := range map[string]struct {
		source, code string
		row, col     int
	}{
		"empty":            {"", "size_error", 0, 0},
		"too large":        {head + "allow_tool_call := true\n#" + strings.Repeat("x", 65536), "size_error", 0, 0},
		"no package":       {"allow_tool_call := true\n", "rego_parse_error", 1, 1},
		"commented out":    {"# package browserjs.policy\nallow_tool_call := true\n", "rego_parse_error", 1, 1},
		"wrong package":    {"\npackage  other\n", "policy_guard_error", 2, 10},
		"unbalanced":       {head + "allow_tool_call if {\n", "rego_parse_error", 3, 1},
		"closing too soon": {head + "}\n", "rego_parse_error", 2, 1},
		"no entry rule":    {head + "allow if input.server == \"browser\"\n", "policy_guard_error", 0, 0},
		"entry in comment": {head + "# allow_tool_call := true\n", "policy_guard_error", 0, 0},
	} {
		v := Validate(tc.source)
		if v.OK || len(v.Errors) != 1 || v.Hash != "" || v.Rego != "" {
			t.Errorf("%s: %+v", name, v)
			continue
		}
		if e := v.Errors[0]; e.Code != tc.code || e.Row != tc.row || e.Col != tc.col {
			t.Errorf("%s: %+v, want %s at %d:%d", name, e, tc.code, tc.row, tc.col)
		}
	}
}

// Braces in a comment or a string are not the module's.
func TestValidateIgnoresCommentsAndStrings(t *testing.T) {
	source := "package browserjs.policy\n\n# a brace: {\nallow_tool_call if {\n" +
		"\tregex.match(`^a{2}}$`, input.tool)\n\tinput.server != \"}\\\"}\" # }\n}\n"
	if v := Validate(source); !v.OK {
		t.Errorf("%+v", v.Errors)
	}
}

func TestValidateWarnings(t *testing.T) {
	const restricted = `package browserjs.policy

import rego.v1

allow_tool_call if {
	input.server == "browser"
	input.tool == "browser_execute"
	every op in input.arguments.operations {
		op.type != "evaluate"
	}
}
`
	const desktop = `
allow_tool_call if {
	input.server == "browser"
	input.tool == "desktop_execute"
}
`
	const shell = `
allow_tool_call if input.server == "exec"
`
	const listedCommands = `
allow_tool_call if {
	input.server == "exec"
	input.tool == "exec"
	input.arguments.bin in {"pwd", "ls"}
}
`
	for name, tc := range map[string]struct {
		source string
		codes  []string
	}{
		"the browser alone":        {restricted, nil},
		"desktop named in a note":  {restricted + "# no desktop_execute: \"desktop_execute\" would undo this\n", nil},
		"browser and desktop":      {restricted + desktop, []string{"browser_bypass_desktop"}},
		"browser and shell":        {restricted + shell, []string{"browser_bypass_shell"}},
		"browser, desktop, shell":  {restricted + desktop + shell, []string{"browser_bypass_desktop", "browser_bypass_shell"}},
		"listed commands":          {"package browserjs.policy\n" + listedCommands, nil},
		"listed commands, desktop": {"package browserjs.policy\n" + listedCommands + desktop, []string{"shell_bypass_desktop"}},
		"everything":               {Unrestricted, nil},
	} {
		v := Validate(tc.source)
		if !v.OK {
			t.Errorf("%s: %+v", name, v.Errors)
			continue
		}
		var codes []string
		for _, w := range v.Warnings {
			codes = append(codes, w.Code)
			// These are about the policy as a whole.
			if w.Row != 0 || w.Col != 0 || w.Message == "" {
				t.Errorf("%s: %+v", name, w)
			}
		}
		if strings.Join(codes, ",") != strings.Join(tc.codes, ",") {
			t.Errorf("%s: warnings %v, want %v", name, codes, tc.codes)
		}
	}
}

func do(t *testing.T, s *Server, method, path, auth, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// The rules of backend-api.yaml that the provider leans on.
func TestTheContractsRules(t *testing.T) {
	s := New("bjs_a_b")
	id := s.AddSession("a")
	auth := "Bearer bjs_a_b"
	policy := "/v1/sessions/" + id + "/policy"
	const source = `{"kind":"rego","source":"package browserjs.policy\nallow_tool_call if input.server == \"browser\"\n"`

	for _, bad := range []string{"", "Bearer nope", "bjs_a_b"} {
		if code, body := do(t, s, "GET", "/v1/sessions", bad, ""); code != http.StatusUnauthorized || !strings.Contains(body, "invalid token") {
			t.Errorf("auth %q: %d %s", bad, code, body)
		}
	}
	// A token may not write a policy in editor mode...
	if code, body := do(t, s, "PUT", policy, auth, source+"}"); code != http.StatusConflict || !strings.Contains(body, "managed in the editor") {
		t.Errorf("PUT in editor mode: %d %s", code, body)
	}
	if code, _ := do(t, s, "DELETE", policy, auth, ""); code != http.StatusConflict {
		t.Errorf("DELETE in editor mode: %d", code)
	}
	// ...unless the request takes the policy over.
	iac := source + `,"management":{"mode":"iac","managed_url":"https://example.com/x"}}`
	if code, body := do(t, s, "PUT", policy, auth, iac); code != http.StatusOK || !strings.Contains(body, `"version":2`) {
		t.Errorf("PUT taking over: %d %s", code, body)
	}
	// A request that changes nothing does not raise the version.
	if code, body := do(t, s, "PUT", policy, auth, iac); code != http.StatusOK || !strings.Contains(body, `"version":2`) {
		t.Errorf("PUT again: %d %s", code, body)
	}
	if code, _ := do(t, s, "PUT", policy, auth, strings.Replace(iac, "https://", "http://", 1)); code != http.StatusBadRequest {
		t.Errorf("http managed_url: %d", code)
	}
	req := httptest.NewRequest("PUT", policy, strings.NewReader(source+"}"))
	req.Header.Set("Authorization", auth)
	req.Header.Set("If-Match", `"1"`)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("stale If-Match: %d", w.Code)
	}
	if code, body := do(t, s, "PUT", policy, auth, `{"kind":"rego","source":"package other"}`); code != http.StatusUnprocessableEntity || !strings.Contains(body, `"errors":[{`) {
		t.Errorf("invalid: %d %s", code, body)
	}
	// Rego is the only kind, and may be left out.
	for _, path := range []string{policy, "/v1/policies/validate"} {
		method := map[bool]string{true: "PUT", false: "POST"}[path == policy]
		if code, body := do(t, s, method, path, auth, `{"kind":"json","source":"{\"version\":1}"}`); code != http.StatusBadRequest || !strings.Contains(body, "kind must be rego") {
			t.Errorf("%s %s with kind json: %d %s", method, path, code, body)
		}
	}
	if code, body := do(t, s, "POST", "/v1/policies/validate", auth, `{"source":"package browserjs.policy\nallow_tool_call := true\n"}`); code != http.StatusOK || !strings.Contains(body, `"ok":true`) {
		t.Errorf("validate without a kind: %d %s", code, body)
	}
	if code, body := do(t, s, "DELETE", policy, auth, ""); code != http.StatusOK || !strings.Contains(body, `"mode":"editor"`) || !strings.Contains(body, `"kind":"rego"`) {
		t.Errorf("DELETE: %d %s", code, body)
	}
	if got := s.PolicyOf(id); got.Source != Unrestricted {
		t.Errorf("after a reset the policy is %q", got.Source)
	}
	// Nothing serves the JSON format's schema any more.
	if code, _ := do(t, s, "GET", "/v1/policy-schema.json", auth, ""); code != http.StatusNotFound {
		t.Errorf("/v1/policy-schema.json: %d", code)
	}
	if code, body := do(t, s, "PUT", policy+"/management", auth, `{"mode":"iac"}`); code != http.StatusBadRequest {
		t.Errorf("management without a URL: %d %s", code, body)
	}
	if code, _ := do(t, s, "GET", "/v1/sessions/s-zzzzz/policy", auth, ""); code != http.StatusNotFound {
		t.Errorf("unknown session: %d", code)
	}
	legacy := s.AddLegacySession("old")
	if code, _ := do(t, s, "GET", "/v1/sessions/"+legacy+"/policy", auth, ""); code != http.StatusConflict {
		t.Errorf("legacy GET: %d", code)
	}
	if code, body := do(t, s, "GET", "/v1/sessions/"+legacy, auth, ""); code != http.StatusOK || !strings.Contains(body, `"state":"unsupported"`) {
		t.Errorf("legacy session: %d %s", code, body)
	}
	// Nothing is served outside /v1.
	if code, _ := do(t, s, "GET", "/api/sessions", auth, ""); code != http.StatusNotFound {
		t.Errorf("/api: %d", code)
	}
}
