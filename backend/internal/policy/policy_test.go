package policy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

// The mapping of backend-api.yaml's PolicyState, for the generation the
// object is at.
func TestStateOf(t *testing.T) {
	cond := func(typ, status string, generation int64) map[string]any {
		return map[string]any{"type": typ, "status": status, "observedGeneration": generation}
	}
	for name, c := range map[string]struct {
		generation int64
		conditions []any
		want       State
	}{
		"no status yet":                            {1, nil, Loading},
		"ready":                                    {3, []any{cond("Compiled", "True", 3), cond("Loaded", "True", 3), cond("Ready", "True", 3)}, Ready},
		"ready, for the version before":            {4, []any{cond("Compiled", "True", 3), cond("Loaded", "True", 3), cond("Ready", "True", 3)}, Loading},
		"compiled, not on every replica":           {3, []any{cond("Compiled", "True", 3), cond("Loaded", "False", 3), cond("Ready", "False", 3)}, Loading},
		"does not compile":                         {3, []any{cond("Compiled", "False", 3), cond("Ready", "False", 3)}, Invalid},
		"did not compile, for the version before":  {4, []any{cond("Compiled", "False", 3), cond("Ready", "False", 3)}, Loading},
		"ready is unknown":                         {3, []any{cond("Compiled", "True", 3), cond("Ready", "Unknown", 3)}, Loading},
		"a condition without observedGeneration":   {3, []any{map[string]any{"type": "Ready", "status": "True"}}, Loading},
		"conditions that are not what they should": {3, []any{"Ready", map[string]any{"type": int64(7)}}, Loading},
	} {
		obj := &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"generation": c.generation},
			"status":   map[string]any{"conditions": c.conditions},
		}}
		if got := stateOf(obj); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

// Who may write a policy: every cell of the table.
func TestMayWrite(t *testing.T) {
	cookie := auth.User{Subject: "alice@example.com"}
	token := auth.User{Subject: "alice@example.com", Token: &auth.TokenInfo{Name: "ci", Scopes: []string{ScopeWrite}}}
	editor := Management{Mode: "editor"}
	iac := Management{Mode: "iac", ManagedURL: "https://git.example.com/infra"}
	for name, c := range map[string]struct {
		current   Management
		user      auth.User
		requested *Management
		refusal   string
	}{
		"editor, cookie":                     {editor, cookie, nil, ""},
		"editor, cookie, handing over":       {editor, cookie, &iac, ""},
		"editor, token":                      {editor, token, nil, "this policy is managed in the editor"},
		"editor, token, staying in editor":   {editor, token, &editor, "this policy is managed in the editor"},
		"editor, token, taking over":         {editor, token, &iac, ""},
		"iac, cookie":                        {iac, cookie, nil, "this policy is managed externally"},
		"iac, cookie, taking back in a save": {iac, cookie, &editor, "this policy is managed externally"},
		"iac, token":                         {iac, token, nil, ""},
		"iac, token, handing back":           {iac, token, &editor, ""},
		"no mode on record is editor":        {Management{}, token, nil, "this policy is managed in the editor"},
	} {
		got := mayWrite(c.current, c.user, c.requested)
		switch {
		case c.refusal == "" && got != nil:
			t.Errorf("%s: refused with %+v", name, got)
		case c.refusal != "" && (got == nil || got.Error != c.refusal):
			t.Errorf("%s: %+v, want %q", name, got, c.refusal)
		case got != nil && (c.current.Mode == "iac") != (got.ManagedURL == iac.ManagedURL):
			t.Errorf("%s: managed_url = %q", name, got.ManagedURL)
		}
	}
	if updatedBy(cookie) != "ui" || updatedBy(token) != "token:ci" {
		t.Errorf("updated-by: %q, %q", updatedBy(cookie), updatedBy(token))
	}
	if !hasScope(cookie, ScopeWrite) || !hasScope(token, ScopeWrite) || hasScope(token, ScopeRead) {
		t.Error("scopes: a cookie has all, a token what it was given")
	}
}

func TestIfMatchHeader(t *testing.T) {
	for _, c := range []struct {
		header  string
		version int64
		exists  bool
		want    bool
	}{
		{``, 3, true, true}, {``, 0, false, true}, {`"3"`, 3, true, true}, {`"2"`, 3, true, false},
		{`3`, 3, true, true}, {`W/"3"`, 3, true, true}, {`"1", "3"`, 3, true, true}, {`"1","2"`, 3, true, false},
		{`*`, 3, true, true}, {`*`, 0, false, false}, {`"0"`, 0, false, false}, {`"33"`, 3, true, false},
	} {
		if got := ifMatch(c.header, c.version, c.exists); got != c.want {
			t.Errorf("If-Match %s at version %d (exists %v) = %v", c.header, c.version, c.exists, got)
		}
	}
}

// The presets are built into the binary, so they are copies; they must be
// the contract's examples, byte for byte, all of them and no others.
func TestPresetsAreTheContractsExamples(t *testing.T) {
	const examples = "../../../docs/contracts/policy/examples"
	files, err := filepath.Glob(examples + "/*" + presetSuffix)
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples in %s: %v", examples, err)
	}
	got := Presets()
	if len(got) != len(files) {
		t.Errorf("%d presets for %d examples", len(got), len(files))
	}
	byID := map[string]Preset{}
	for _, p := range got {
		byID[p.ID] = p
	}
	for _, file := range files {
		want, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		id := filepath.Base(file)
		id = id[:len(id)-len(presetSuffix)]
		p, ok := byID[id]
		if !ok || p.Source != string(want) {
			t.Errorf("preset %q is not %s; copy the file to internal/policy/presets/", id, file)
			continue
		}
		// The description is the comment the file begins with.
		first, _, _ := strings.Cut(string(want), "\npackage ")
		words := strings.Join(strings.Fields(strings.ReplaceAll(first, "#", " ")), " ")
		if p.Description != words || p.Description == "" || p.Kind != KindRego {
			t.Errorf("preset %+v, want the description %q", p, words)
		}
	}
	if len(got) != 7 || byID["read-only-shell"].Title != "Read-only shell" || byID["browser-only"].Title != "Browser only" ||
		byID["one-site"].Description != "The agent may be sent only to example.com and its subdomains, and may type only short text. No desktop control and no shell." {
		t.Errorf("%d presets: %+v", len(got), byID["one-site"])
	}
	if got[0].ID != "unrestricted" || got[0].Title != "Unrestricted" || byID["no-scripting"].Title != "No scripting" {
		t.Errorf("first %q (%q); no-scripting is titled %q", got[0].ID, got[0].Title, byID["no-scripting"].Title)
	}
	for i := 2; i < len(got); i++ {
		if got[i-1].ID >= got[i].ID {
			t.Errorf("presets after the first are not in order: %q before %q", got[i-1].ID, got[i].ID)
		}
	}
	if u := Unrestricted(); u.Kind != KindRego || u.Source != got[0].Source || u.Mode != sessions.PolicyModeEditor {
		t.Errorf("Unrestricted() = %+v", u)
	}
	// Presets hands out a copy.
	got[0].Source = "changed"
	if Presets()[0].Source == "changed" {
		t.Error("a caller can change the presets")
	}
}

func TestOperatorClient(t *testing.T) {
	var answer func(w http.ResponseWriter, r *http.Request)
	var seen *http.Request
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Clone(context.Background())
		body = nil
		_ = json.NewDecoder(r.Body).Decode(&body)
		answer(w, r)
	}))
	defer server.Close()
	op := NewOperator(server.URL+"/", "secret")

	answer = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"rego":"package browserjs.policy\n","hash":"sha256:ab","errors":[],"warnings":[{"code":"unknown_operation","message":"m"}]}`))
	}
	v, err := op.Validate(t.Context(), "rego", "package browserjs.policy")
	if err != nil || !v.OK || v.Hash != "sha256:ab" || v.Rego == "" || len(v.Warnings) != 1 {
		t.Fatalf("Validate = %+v, %v", v, err)
	}
	if seen.Method != "POST" || seen.URL.Path != "/v1/validate" || seen.Header.Get("Authorization") != "Bearer secret" ||
		seen.Header.Get("Content-Type") != "application/json" || len(body) != 2 || body["kind"] != "rego" || body["source"] != "package browserjs.policy" {
		t.Errorf("request: %s %s, %v, body %v", seen.Method, seen.URL.Path, seen.Header, body)
	}

	answer = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"errors":[{"row":1,"col":2,"code":"schema_error","message":"m"}],"warnings":[]}`))
	}
	v, err = op.Validate(t.Context(), "rego", "package browserjs.policy")
	if err != nil || v.OK || len(v.Errors) != 1 || v.Errors[0] != (Diagnostic{Row: 1, Col: 2, Code: "schema_error", Message: "m"}) {
		t.Errorf("an invalid policy: %+v, %v", v, err)
	}

	// Anything that is not a verdict means there is none: never "valid".
	for name, a := range map[string]func(w http.ResponseWriter, r *http.Request){
		"wrong token":   func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
		"not ready":     func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "starting", http.StatusServiceUnavailable) },
		"its own 400":   func(w http.ResponseWriter, _ *http.Request) { http.Error(w, `{"error":"bad"}`, http.StatusBadRequest) },
		"not JSON":      func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) },
		"another shape": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"status":"fine"}`)) },
		"refused, no reason": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"ok":false,"errors":[],"warnings":[]}`))
		},
	} {
		answer = a
		if v, err := op.Validate(t.Context(), "rego", "package browserjs.policy"); !errors.Is(err, ErrOperatorUnavailable) || v.OK {
			t.Errorf("%s: %+v, %v", name, v, err)
		}
	}
	server.Close()
	if _, err := op.Validate(t.Context(), "rego", "package browserjs.policy"); !errors.Is(err, ErrOperatorUnavailable) {
		t.Errorf("no operator: %v", err)
	}
}

func TestGate(t *testing.T) {
	running := sessions.Session{ID: "s-aaaaa", State: sessions.Running}
	for name, c := range map[string]struct {
		summary Summary
		session sessions.Session
		want    sessions.State
	}{
		"first policy loading":             {Summary{State: Loading}, running, sessions.Starting},
		"first policy refused":             {Summary{State: Invalid}, running, sessions.Starting},
		"ready":                            {Summary{State: Ready, inForce: true}, running, sessions.Running},
		"an edit loading":                  {Summary{State: Loading, inForce: true}, running, sessions.Running},
		"an edit refused":                  {Summary{State: Invalid, inForce: true}, running, sessions.Running},
		"predates policies":                {Summary{State: Unsupported}, running, sessions.Running},
		"asleep, whatever its policy says": {Summary{State: Loading}, sessions.Session{State: sessions.Asleep}, sessions.Asleep},
		"failed":                           {Summary{State: Loading}, sessions.Session{State: sessions.Failed, Message: "boom"}, sessions.Failed},
	} {
		got := c.summary.Gate(c.session)
		if got.State != c.want || (got.State == sessions.Starting) != (got.Message != "" && got.Message != "boom") {
			t.Errorf("%s: %q (%q), want %q", name, got.State, got.Message, c.want)
		}
	}
}

func TestNewNeedsPoliciesEnabled(t *testing.T) {
	if New(&sessions.Store{}, NewOperator("http://operator", "t")) != nil {
		t.Error("New made handlers for a store without policies")
	}
}
