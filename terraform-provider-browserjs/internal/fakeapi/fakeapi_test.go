package fakeapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateAcceptsTheContractExamples(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "docs", "contracts", "policy", "examples")
	for _, pattern := range []string{"*.policy.json", "*.rego"} {
		files, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil || len(files) != 5 {
			t.Fatalf("%s: %d files, %v", pattern, len(files), err)
		}
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			kind := "json"
			if strings.HasSuffix(f, ".rego") {
				kind = "rego"
			}
			if v := Validate(kind, string(raw)); !v.OK || v.Hash == "" || v.Rego == "" {
				t.Errorf("%s: %+v", filepath.Base(f), v)
			}
		}
	}
}

func TestValidatePositions(t *testing.T) {
	for name, tc := range map[string]struct {
		kind, source, code string
		row, col           int
	}{
		"syntax":           {"json", "{\n  \"version\": 1,\n}", "json_syntax", 3, 1},
		"version":          {"json", "{\n \"version\": 2}", "schema", 2, 2},
		"unknown key":      {"json", "{\"version\": 1, \"alow\": {}}", "schema", 1, 16},
		"operation":        {"json", "{\"version\": 1,\n\"deny\": {\"operations\": [\"fly\"]}}", "schema", 2, 25},
		"no package":       {"rego", "allow_tool_call := true\n", "rego_parse_error", 1, 1},
		"wrong package":    {"rego", "\npackage  other\n", "package", 2, 10},
		"unbalanced":       {"rego", "package browserjs.policy\nallow_tool_call if {\n", "rego_parse_error", 3, 1},
		"closing too soon": {"rego", "package browserjs.policy\n}\n", "rego_parse_error", 2, 1},
	} {
		v := Validate(tc.kind, tc.source)
		if v.OK || len(v.Errors) != 1 {
			t.Errorf("%s: %+v", name, v)
			continue
		}
		if e := v.Errors[0]; e.Code != tc.code || e.Row != tc.row || e.Col != tc.col {
			t.Errorf("%s: %+v, want %s at %d:%d", name, e, tc.code, tc.row, tc.col)
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
	const source = `{"kind":"json","source":"{\"version\":1}"`

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
	if code, body := do(t, s, "PUT", policy, auth, `{"kind":"json","source":"{}"}`); code != http.StatusUnprocessableEntity || !strings.Contains(body, `"errors":[{`) {
		t.Errorf("invalid: %d %s", code, body)
	}
	if code, body := do(t, s, "DELETE", policy, auth, ""); code != http.StatusOK || !strings.Contains(body, `"mode":"editor"`) {
		t.Errorf("DELETE: %d %s", code, body)
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
