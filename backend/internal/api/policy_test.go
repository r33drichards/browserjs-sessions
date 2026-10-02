package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"

	"github.com/r33drichards/computer-use/backend/internal/api"
	"github.com/r33drichards/computer-use/backend/internal/auth"
	"github.com/r33drichards/computer-use/backend/internal/authz"
	"github.com/r33drichards/computer-use/backend/internal/policy"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
)

const operatorToken = "operator-api-token-for-tests"

// operator is the policy operator's HTTP API, faked as
// docs/contracts/policy/operator-api.yaml describes it. A source that
// contains "INVALID" does not validate; one that contains "DENY" evaluates
// to a denial.
type operator struct {
	t      *testing.T
	server *httptest.Server

	mu     sync.Mutex
	down   bool
	calls  map[string]int
	bodies map[string][]byte // the last body each path was sent
}

func newOperator(t *testing.T) *operator {
	o := &operator{t: t, calls: map[string]int{}, bodies: map[string][]byte{}}
	o.server = httptest.NewServer(http.HandlerFunc(o.serve))
	t.Cleanup(o.server.Close)
	return o
}

func (o *operator) setDown(down bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.down = down
}

func (o *operator) count(path string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls[path]
}

func (o *operator) sent(path string) map[string]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	var v map[string]any
	if err := json.Unmarshal(o.bodies[path], &v); err != nil {
		o.t.Fatalf("the operator was sent %q on %s: %v", o.bodies[path], path, err)
	}
	return v
}

func (o *operator) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	o.mu.Lock()
	o.calls[r.URL.Path]++
	o.bodies[r.URL.Path] = body
	down := o.down
	o.mu.Unlock()
	if down {
		http.Error(w, "the operator is restarting", http.StatusBadGateway)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+operatorToken {
		w.WriteHeader(http.StatusUnauthorized) // with an empty body
		return
	}
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	var req struct {
		Kind   string          `json:"kind"`
		Source string          `json:"source"`
		Input  json.RawMessage `json:"input"`
	}
	if r.Method == http.MethodPost {
		if err := json.Unmarshal(body, &req); err != nil || req.Kind == "" || req.Source == "" || strings.Contains(req.Source, "MALFORMED") {
			reply(http.StatusBadRequest, map[string]string{"error": "the request is not JSON of the right shape"})
			return
		}
	}
	refusal := []map[string]any{{"row": 2, "col": 5, "code": "rego_parse_error", "message": "unexpected INVALID token"}}
	switch r.Method + " " + r.URL.Path {
	case "POST /v1/validate":
		if strings.Contains(req.Source, "INVALID") {
			reply(http.StatusOK, map[string]any{"ok": false, "errors": refusal, "warnings": []any{
				map[string]any{"code": "unknown_operation", "message": "nothing in version 1 is called that"},
			}})
			return
		}
		reply(http.StatusOK, map[string]any{
			"ok": true, "rego": "package browserjs.policy\n\nallow_tool_call := true\n", "hash": "sha256:0f0f",
			"errors": []any{}, "warnings": []any{},
		})
	case "POST /v1/evaluate":
		switch {
		case len(req.Input) == 0:
			reply(http.StatusBadRequest, map[string]string{"error": "an input is required"})
		case strings.Contains(req.Source, "INVALID"):
			reply(http.StatusOK, map[string]any{"ok": false, "errors": refusal})
		default:
			reply(http.StatusOK, map[string]any{"ok": true, "allow": !strings.Contains(req.Source, "DENY")})
		}
	default:
		http.NotFound(w, r)
	}
}

type policyFixture struct {
	*fixture
	operator *operator
	policies *policy.Handlers
}

// newPolicyFixture is newFixture with policies enabled: a fake operator, and
// a blueprint whose sessions ask OPA unless legacy.
func newPolicyFixture(t *testing.T, legacy bool) *policyFixture {
	t.Helper()
	var store *sessions.Store
	var client dynamic.Interface
	if legacy {
		store, client = sessionstest.New(t)
		store.EnablePolicies(client, sessionstest.Namespace, policy.Unrestricted())
	} else {
		store, client = sessionstest.NewWithPolicies(t)
		store.EnablePolicies(client, sessionstest.Namespace, policy.Unrestricted())
	}
	op := newOperator(t)
	handlers := policy.New(store, policy.NewOperator(op.server.URL+"/", operatorToken))
	if handlers == nil {
		t.Fatal("no handlers for a store with policies enabled")
	}
	handlers.Wait, handlers.Poll, handlers.NewWait = 60*time.Millisecond, 2*time.Millisecond, 5*time.Second
	az := &faulty{Checker: authz.NewOwners(store, 0)}
	mux := http.NewServeMux()
	a := api.New(store, az, sessionstest.URLs(), 5)
	a.EnablePolicies(handlers)
	a.Register(mux)
	return &policyFixture{
		fixture:  &fixture{t: t, handler: mux, api: a, store: store, client: client, faults: az},
		operator: op, policies: handlers,
	}
}

// send is do with request headers.
func (f *fixture) send(user auth.User, method, path, body string, header map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(auth.WithUser(f.t.Context(), user))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// The shapes of docs/contracts/policy/backend-api.yaml. strict refuses a
// field they do not have, so a handler cannot answer with more than the
// contract says, and the tests check that it answers with no less.
type (
	management struct {
		Mode       string `json:"mode"`
		ManagedURL string `json:"managed_url"`
	}
	diagnostic struct {
		Row     int    `json:"row"`
		Col     int    `json:"col"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	policySummary struct {
		Kind       string      `json:"kind"`
		Version    int64       `json:"version"`
		Hash       string      `json:"hash"`
		State      string      `json:"state"`
		Management *management `json:"management"`
	}
	fullPolicy struct {
		policySummary
		Source   string       `json:"source"`
		Rego     string       `json:"rego"`
		Errors   []diagnostic `json:"errors"`
		Warnings []diagnostic `json:"warnings"`
		Loaded   *struct {
			Replicas int `json:"replicas"`
			Total    int `json:"total"`
		} `json:"loaded"`
		Updated   *time.Time `json:"updated"`
		UpdatedBy string     `json:"updated_by"`
	}
	sessionWithPolicy struct {
		ID      string         `json:"id"`
		Name    string         `json:"name"`
		Owner   string         `json:"owner"`
		State   string         `json:"state"`
		Message string         `json:"message"`
		Created time.Time      `json:"created"`
		MCPURL  string         `json:"mcp_url"`
		Policy  *policySummary `json:"policy"`
	}
	validation struct {
		OK       *bool         `json:"ok"`
		Rego     string        `json:"rego"`
		Hash     string        `json:"hash"`
		Errors   *[]diagnostic `json:"errors"`
		Warnings *[]diagnostic `json:"warnings"`
	}
	evaluation struct {
		OK     *bool        `json:"ok"`
		Allow  *bool        `json:"allow"`
		Errors []diagnostic `json:"errors"`
	}
	policyError struct {
		Error    string        `json:"error"`
		Errors   *[]diagnostic `json:"errors"`
		Warnings []diagnostic  `json:"warnings"`
	}
	modeError struct {
		Error      string `json:"error"`
		ManagedURL string `json:"managed_url"`
	}
	plainError struct {
		Error string `json:"error"`
	}
	preset struct {
		ID          string `json:"id"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Kind        string `json:"kind"`
		Source      string `json:"source"`
	}
)

func strict[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%d %s does not have the contract's shape: %v", rec.Code, rec.Body, err)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	return v
}

const (
	unrestrictedSource = `{"version": 1, "allow": {"operations": ["*"]}}`
	noScripting        = "package browserjs.policy\n\nimport rego.v1\n\nallow_tool_call if input.tool == \"browser_execute\"\n"
	observeOnly        = "package browserjs.policy\n\nimport rego.v1\n\nallow_tool_call if input.arguments.operations == [{\"type\": \"screenshot\"}]\n"
	regoSource         = "package browserjs.policy\n\nallow_tool_call := false\n"
	managedURL         = "https://git.example.com/infra/policies"
)

func policyBody(t *testing.T, kind, source string, m *management) string {
	t.Helper()
	body := map[string]any{"kind": kind, "source": source}
	if m != nil {
		mm := map[string]any{"mode": m.Mode}
		if m.ManagedURL != "" {
			mm["managed_url"] = m.ManagedURL
		}
		body["management"] = mm
	}
	out, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// token is alice with an API token of the given scopes.
func token(scopes ...string) auth.User {
	return auth.User{Subject: alice.Subject, Name: alice.Name, Token: &auth.TokenInfo{Name: "ci", Scopes: scopes}}
}

var allScopes = []string{"sessions:read", "sessions:write", "policies:read", "policies:write"}

// newSession creates a session for alice with the given body and returns
// its ID.
func (f *policyFixture) newSession(body string) string {
	f.t.Helper()
	rec := f.do(alice, "POST", "/api/sessions", body)
	if rec.Code != http.StatusCreated {
		f.t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	return strict[sessionWithPolicy](f.t, rec).ID
}

// reconcile plays the policy operator for one session until the test ends:
// each new generation of its policy is reported ready on every replica.
func (f *policyFixture) reconcile(id string) {
	f.t.Helper()
	tracker := f.client.(*dynfake.FakeDynamicClient).Tracker()
	done := make(chan struct{})
	stopped := make(chan struct{})
	f.t.Cleanup(func() { close(done); <-stopped })
	go func() {
		defer close(stopped)
		for {
			select {
			case <-done:
				return
			case <-time.After(time.Millisecond):
			}
			cur, err := tracker.Get(sessions.PolicyGVR, sessionstest.Namespace, id)
			if err != nil {
				continue
			}
			obj := cur.(*unstructured.Unstructured)
			if observed, _, _ := unstructured.NestedInt64(obj.Object, "status", "observedGeneration"); observed != obj.GetGeneration() {
				_ = sessionstest.TrySetPolicyStatus(f.client, id, sessionstest.PolicyReady(obj.GetGeneration()))
			}
		}
	}()
}

func (f *policyFixture) spec(id string) (kind, source, mode, url, by string) {
	f.t.Helper()
	obj := sessionstest.Policy(f.t, f.client, id)
	if obj == nil {
		f.t.Fatalf("session %s has no SessionPolicy", id)
	}
	kind, _, _ = unstructured.NestedString(obj.Object, "spec", "kind")
	source, _, _ = unstructured.NestedString(obj.Object, "spec", "source")
	mode, _, _ = unstructured.NestedString(obj.Object, "spec", "management", "mode")
	url, _, _ = unstructured.NestedString(obj.Object, "spec", "management", "managedURL")
	return kind, source, mode, url, obj.GetAnnotations()[sessions.AnnUpdatedBy]
}

func (f *policyFixture) get(id string) fullPolicy {
	f.t.Helper()
	rec := f.do(alice, "GET", "/api/sessions/"+id+"/policy", "")
	if rec.Code != http.StatusOK {
		f.t.Fatalf("GET policy: %d %s", rec.Code, rec.Body)
	}
	return strict[fullPolicy](f.t, rec)
}

// With POLICY_OPERATOR_URL unset nothing of this exists.
func TestPoliciesAreOffByDefault(t *testing.T) {
	f := newFixture(t)
	rec := f.do(alice, "POST", "/api/sessions", `{"name":"one"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	created := decode[map[string]any](t, rec)
	if _, has := created["policy"]; has {
		t.Errorf("a session carries a policy: %s", rec.Body)
	}
	id := created["id"].(string)
	if sessionstest.Policy(t, f.client, id) != nil {
		t.Error("a SessionPolicy was made")
	}
	for _, body := range []string{
		f.do(alice, "GET", "/api/sessions/"+id, "").Body.String(),
		f.do(alice, "GET", "/api/sessions", "").Body.String(),
	} {
		if strings.Contains(body, "policy") {
			t.Errorf("a session carries a policy: %s", body)
		}
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/sessions/" + id + "/policy"},
		{"PUT", "/api/sessions/" + id + "/policy"},
		{"DELETE", "/api/sessions/" + id + "/policy"},
		{"PUT", "/api/sessions/" + id + "/policy/management"},
		{"POST", "/api/policies/validate"},
		{"POST", "/api/policies/evaluate"},
		{"GET", "/api/policy-presets"},
	} {
		if rec := f.do(alice, c.method, c.path, `{}`); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: %d, want no such route", c.method, c.path, rec.Code)
		}
	}
	// A policy that is asked for is not dropped in silence.
	rec = f.do(alice, "POST", "/api/sessions", `{"name":"two","policy":`+policyBody(t, "rego", noScripting, nil)+`}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("create with a policy: %d %s, want 409", rec.Code, rec.Body)
	}
	if mine, _ := f.store.List(t.Context(), alice.Subject); len(mine) != 1 {
		t.Errorf("%d sessions, want 1", len(mine))
	}
}

func TestCreateWithoutAPolicyIsUnrestricted(t *testing.T) {
	f := newPolicyFixture(t, false)
	rec := f.do(alice, "POST", "/api/sessions", `{"name":"one"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	s := strict[sessionWithPolicy](t, rec)
	want := policySummary{Kind: "rego", Version: 1, State: "loading", Management: &management{Mode: "editor"}}
	if s.Policy == nil || *s.Policy.Management != *want.Management || s.Policy.Kind != want.Kind || s.Policy.Version != 1 || s.Policy.State != "loading" || s.Policy.Hash != "" {
		t.Errorf("policy = %+v, want %+v", s.Policy, want)
	}
	contract, err := os.ReadFile("../../../docs/contracts/policy/examples/unrestricted.rego")
	if err != nil {
		t.Fatal(err)
	}
	if kind, source, mode, _, by := f.spec(s.ID); kind != "rego" || source != string(contract) || mode != "editor" || by != "ui" {
		t.Errorf("SessionPolicy: %s %q %s by %s", kind, source, mode, by)
	}
	// The contract's own example is not sent to be checked: a session can be
	// created while the operator is away.
	if n := f.operator.count("/v1/validate"); n != 0 {
		t.Errorf("the operator was asked %d times", n)
	}
}

func TestCreateWithAPolicy(t *testing.T) {
	f := newPolicyFixture(t, false)
	rec := f.do(alice, "POST", "/api/sessions", `{"name":"one","policy":`+policyBody(t, "rego", noScripting, nil)+`}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	s := strict[sessionWithPolicy](t, rec)
	if kind, source, mode, _, by := f.spec(s.ID); kind != "rego" || source != noScripting || mode != "editor" || by != "ui" {
		t.Errorf("SessionPolicy: %s %q %s by %s", kind, source, mode, by)
	}
	if sent := f.operator.sent("/v1/validate"); sent["kind"] != "rego" || sent["source"] != noScripting || len(sent) != 2 {
		t.Errorf("the operator was asked about %v", sent)
	}

	// Managed as code from the start, by a token.
	rec = f.do(token(allScopes...), "POST", "/api/sessions",
		`{"name":"two","policy":`+policyBody(t, "rego", regoSource, &management{Mode: "iac", ManagedURL: managedURL})+`}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	s = strict[sessionWithPolicy](t, rec)
	if *s.Policy.Management != (management{Mode: "iac", ManagedURL: managedURL}) || s.Policy.Kind != "rego" {
		t.Errorf("policy = %+v", s.Policy)
	}
	if kind, source, mode, url, by := f.spec(s.ID); kind != "rego" || source != regoSource || mode != "iac" || url != managedURL || by != "token:ci" {
		t.Errorf("SessionPolicy: %s %q %s %s by %s", kind, source, mode, url, by)
	}
}

// An invalid policy is 422 and creates nothing; so does one that could not
// be checked.
func TestCreateWithAPolicyThatIsRefused(t *testing.T) {
	big := strings.Repeat("x", policy.MaxSource+1)
	for name, c := range map[string]struct {
		user  auth.User
		body  string
		down  bool
		code  int
		asked int
	}{
		"does not validate":       {alice, policyBody(t, "rego", "package INVALID", nil), false, 422, 1},
		"operator away":           {alice, policyBody(t, "rego", noScripting, nil), true, 503, 1},
		"not a kind":              {alice, policyBody(t, "yaml", noScripting, nil), false, 400, 0},
		"empty":                   {alice, `{"kind":"rego","source":""}`, false, 422, 0},
		"too long":                {alice, policyBody(t, "rego", big, nil), false, 422, 0},
		"as code without a link":  {alice, policyBody(t, "rego", noScripting, &management{Mode: "iac"}), false, 400, 0},
		"as code, not https":      {alice, policyBody(t, "rego", noScripting, &management{Mode: "iac", ManagedURL: "http://git.example.com"}), false, 400, 0},
		"not a mode":              {alice, policyBody(t, "rego", noScripting, &management{Mode: "terraform"}), false, 400, 0},
		"token without the scope": {token("sessions:write", "policies:read"), policyBody(t, "rego", noScripting, nil), false, 403, 0},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPolicyFixture(t, false)
			f.operator.setDown(c.down)
			rec := f.do(c.user, "POST", "/api/sessions", `{"name":"one","policy":`+c.body+`}`)
			if rec.Code != c.code {
				t.Fatalf("create: %d %s, want %d", rec.Code, rec.Body, c.code)
			}
			if c.code == 422 {
				refusal := strict[policyError](t, rec)
				if refusal.Error == "" || refusal.Errors == nil || len(*refusal.Errors) == 0 || (*refusal.Errors)[0].Code == "" || (*refusal.Errors)[0].Message == "" {
					t.Errorf("refusal = %s", rec.Body)
				}
			} else if strict[plainError](t, rec).Error == "" {
				t.Errorf("refusal = %s", rec.Body)
			}
			if all, _ := f.store.ListAll(t.Context()); len(all) != 0 {
				t.Errorf("%d sessions were created", len(all))
			}
			list, err := f.client.Resource(sessions.PolicyGVR).Namespace(sessionstest.Namespace).List(t.Context(), metav1.ListOptions{})
			if err != nil || len(list.Items) != 0 {
				t.Errorf("%d policies were created (%v)", len(list.Items), err)
			}
			if n := f.operator.count("/v1/validate"); n != c.asked {
				t.Errorf("the operator was asked %d times, want %d", n, c.asked)
			}
		})
	}
}

// The operator's diagnostics reach the caller as they are.
func TestRefusalCarriesTheOperatorsDiagnostics(t *testing.T) {
	f := newPolicyFixture(t, false)
	rec := f.do(alice, "POST", "/api/sessions", `{"policy":`+policyBody(t, "rego", "package INVALID", nil)+`}`)
	refusal := strict[policyError](t, rec)
	want := diagnostic{Row: 2, Col: 5, Code: "rego_parse_error", Message: "unexpected INVALID token"}
	if rec.Code != 422 || len(*refusal.Errors) != 1 || (*refusal.Errors)[0] != want || len(refusal.Warnings) != 1 || refusal.Warnings[0].Code != "unknown_operation" {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

// A new session is "starting" until its policy is in force: until then OPA
// denies it everything, whatever its pod says.
func TestANewSessionStartsWhenItsPolicyIsReady(t *testing.T) {
	f := newPolicyFixture(t, false)
	id := f.newSession(`{"name":"one"}`)
	sessionstest.SetStatus(t, f.client, id, sessionstest.Ready("10.0.0.7"))

	views := func() []sessionWithPolicy {
		one := strict[sessionWithPolicy](t, f.do(alice, "GET", "/api/sessions/"+id, ""))
		list := strict[[]sessionWithPolicy](t, f.do(alice, "GET", "/api/sessions", ""))
		all := strict[[]sessionWithPolicy](t, f.do(root, "GET", "/api/sessions?all=1", ""))
		if len(list) != 1 || len(all) != 1 {
			t.Fatalf("lists of %d and %d sessions", len(list), len(all))
		}
		return []sessionWithPolicy{one, list[0], all[0]}
	}
	expect := func(when, state, policyState string) {
		t.Helper()
		for i, s := range views() {
			if s.State != state || s.Policy == nil || s.Policy.State != policyState {
				t.Errorf("%s (view %d): state %q, policy %+v; want %q with policy %q", when, i, s.State, s.Policy, state, policyState)
			}
			if (s.State == "starting") != (s.Message != "") {
				t.Errorf("%s (view %d): message %q in state %q", when, i, s.Message, s.State)
			}
		}
	}
	expect("not yet observed", "starting", "loading")
	sessionstest.SetPolicyStatus(t, f.client, id, sessionstest.PolicyCompiled(1, true))
	expect("compiled, on one replica of two", "starting", "loading")
	sessionstest.SetPolicyStatus(t, f.client, id, sessionstest.PolicyReady(1))
	expect("ready", "running", "ready")
	if s := views()[0]; s.Policy.Hash != "sha256:0f0f" || s.Policy.Version != 1 {
		t.Errorf("policy = %+v", s.Policy)
	}

	// A later edit does not stop the session: the policy before it stays in
	// force until the new one is.
	if rec := f.do(alice, "PUT", "/api/sessions/"+id+"/policy", policyBody(t, "rego", noScripting, nil)); rec.Code != http.StatusAccepted {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	expect("an edit not yet observed", "running", "loading")
	sessionstest.SetPolicyStatus(t, f.client, id, sessionstest.PolicyRejected(2, false))
	expect("an edit that does not compile", "running", "invalid")
}

// The operator has the last word. If it refuses the policy of a session
// just made (after saying it was valid), the session is deleted.
func TestASessionWhoseFirstPolicyIsRefusedIsDeleted(t *testing.T) {
	f := newPolicyFixture(t, false)
	kept := f.newSession(`{"name":"kept"}`)
	sessionstest.SetPolicyStatus(t, f.client, kept, sessionstest.PolicyReady(1))
	refused := f.newSession(`{"name":"refused","policy":` + policyBody(t, "rego", noScripting, nil) + `}`)
	sessionstest.SetStatus(t, f.client, refused, sessionstest.Ready("10.0.0.7"))
	sessionstest.SetPolicyStatus(t, f.client, refused, sessionstest.PolicyRejected(1, true))

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := f.store.Get(t.Context(), refused)
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the session whose policy was refused is still there")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if sessionstest.Policy(t, f.client, refused) != nil {
		t.Error("its policy is still there")
	}
	if _, err := f.store.Get(t.Context(), kept); err != nil {
		t.Errorf("the other session: %v", err)
	}
}

func TestGetPolicy(t *testing.T) {
	f := newPolicyFixture(t, false)
	id := f.newSession(`{"name":"one","policy":` + policyBody(t, "rego", noScripting, nil) + `}`)

	rec := f.do(alice, "GET", "/api/sessions/"+id+"/policy", "")
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"1"` {
		t.Fatalf("GET: %d, ETag %q, %s", rec.Code, rec.Header().Get("ETag"), rec.Body)
	}
	p := strict[fullPolicy](t, rec)
	if p.Kind != "rego" || p.Version != 1 || p.State != "loading" || p.Source != noScripting || p.UpdatedBy != "ui" ||
		*p.Management != (management{Mode: "editor"}) || p.Rego != "" || p.Loaded != nil || p.Updated != nil || p.Errors != nil {
		t.Errorf("before the operator has seen it: %+v", p)
	}

	sessionstest.SetPolicyStatus(t, f.client, id, sessionstest.PolicyReady(1))
	p = f.get(id)
	applied := time.Date(2026, 10, 2, 12, 1, 7, 0, time.UTC)
	if p.State != "ready" || p.Hash != "sha256:0f0f" || !strings.HasPrefix(p.Rego, "package browserjs.policy") ||
		p.Loaded == nil || p.Loaded.Replicas != 2 || p.Loaded.Total != 2 || p.Updated == nil || !p.Updated.Equal(applied) ||
		len(p.Warnings) != 1 || p.Warnings[0].Code != "unknown_operation" || len(p.Errors) != 0 {
		t.Errorf("ready: %+v", p)
	}

	// Ready, but for the version before this one: not ready.
	if rec := f.do(alice, "PUT", "/api/sessions/"+id+"/policy", policyBody(t, "rego", observeOnly, nil)); rec.Code != http.StatusAccepted {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	if p = f.get(id); p.State != "loading" || p.Version != 2 || p.Hash != "sha256:0f0f" {
		t.Errorf("after an edit: %+v", p)
	}
	sessionstest.SetPolicyStatus(t, f.client, id, sessionstest.PolicyRejected(2, false))
	p = f.get(id)
	want := diagnostic{Row: 3, Col: 1, Code: "rego_parse_error", Message: "unexpected } token"}
	if p.State != "invalid" || len(p.Errors) != 1 || p.Errors[0] != want || p.Rego == "" || p.Source != observeOnly {
		t.Errorf("invalid: %+v", p)
	}
}

// Saved and in force is 200; saved and not yet loaded everywhere is 202.
func TestPutPolicy(t *testing.T) {
	f := newPolicyFixture(t, false)
	id := f.newSession(`{"name":"one"}`)
	path := "/api/sessions/" + id + "/policy"

	// Nobody reports on it: 202 after the wait, and the policy is saved.
	rec := f.do(alice, "PUT", path, policyBody(t, "rego", noScripting, nil))
	if rec.Code != http.StatusAccepted || rec.Header().Get("ETag") != `"2"` {
		t.Fatalf("PUT: %d, ETag %q, %s", rec.Code, rec.Header().Get("ETag"), rec.Body)
	}
	if p := strict[fullPolicy](t, rec); p.State != "loading" || p.Version != 2 || p.Source != noScripting {
		t.Errorf("202 with %+v", p)
	}
	if _, source, _, _, _ := f.spec(id); source != noScripting {
		t.Errorf("saved %q", source)
	}

	// The operator reports it ready within the wait: 200.
	f.policies.Wait = 5 * time.Second
	f.reconcile(id)
	rec = f.do(alice, "PUT", path, policyBody(t, "rego", regoSource, nil))
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"3"` {
		t.Fatalf("PUT: %d, ETag %q, %s", rec.Code, rec.Header().Get("ETag"), rec.Body)
	}
	p := strict[fullPolicy](t, rec)
	if p.State != "ready" || p.Version != 3 || p.Kind != "rego" || p.Source != regoSource || p.Loaded == nil || p.Loaded.Replicas != 2 {
		t.Errorf("200 with %+v", p)
	}

	// A request that changes nothing is 200 and does not raise the version,
	// nor is the operator asked.
	asked := f.operator.count("/v1/validate")
	rec = f.do(alice, "PUT", path, policyBody(t, "rego", regoSource, &management{Mode: "editor"}))
	if p := strict[fullPolicy](t, rec); rec.Code != http.StatusOK || p.Version != 3 || rec.Header().Get("ETag") != `"3"` {
		t.Errorf("unchanged: %d %+v", rec.Code, p)
	}
	if f.operator.count("/v1/validate") != asked {
		t.Error("the operator was asked about a policy that is in force")
	}

	// Not valid: 422, nothing saved.
	rec = f.do(alice, "PUT", path, policyBody(t, "rego", "package INVALID", nil))
	if refusal := strict[policyError](t, rec); rec.Code != 422 || len(*refusal.Errors) != 1 {
		t.Errorf("invalid: %d %s", rec.Code, rec.Body)
	}
	// The operator cannot be asked: 503, nothing saved.
	f.operator.setDown(true)
	rec = f.do(alice, "PUT", path, policyBody(t, "rego", observeOnly, nil))
	if strict[plainError](t, rec); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("operator away: %d %s", rec.Code, rec.Body)
	}
	f.operator.setDown(false)
	for _, body := range []string{`{`, policyBody(t, "yaml", regoSource, nil), policyBody(t, "rego", regoSource, &management{Mode: "iac"})} {
		if rec := f.do(alice, "PUT", path, body); rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %s: %d %s", body, rec.Code, rec.Body)
		}
	}
	if rec := f.do(alice, "PUT", path, `{"kind":"rego","source":""}`); rec.Code != 422 {
		t.Errorf("empty source: %d %s", rec.Code, rec.Body)
	}
	if p := f.get(id); p.Version != 3 || p.Source != regoSource {
		t.Errorf("a refused write was saved: %+v", p)
	}

	// The operator refuses at the reconcile what it passed when asked: the
	// answer is not "in force".
	f2 := newPolicyFixture(t, false)
	f2.policies.Wait = 5 * time.Second
	id2 := f2.newSession(`{"name":"two"}`)
	go func() {
		for range 2000 {
			if obj := sessionstest.Policy(t, f2.client, id2); obj != nil && obj.GetGeneration() == 2 {
				_ = sessionstest.TrySetPolicyStatus(f2.client, id2, sessionstest.PolicyRejected(2, true))
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	rec = f2.do(alice, "PUT", "/api/sessions/"+id2+"/policy", policyBody(t, "rego", noScripting, nil))
	if p := strict[fullPolicy](t, rec); rec.Code != http.StatusAccepted || p.State != "invalid" || len(p.Errors) != 1 {
		t.Errorf("refused at the reconcile: %d %+v", rec.Code, p)
	}
}

func TestIfMatch(t *testing.T) {
	f := newPolicyFixture(t, false)
	id := f.newSession(`{"name":"one"}`)
	path := "/api/sessions/" + id + "/policy"
	put := func(source, ifMatch string) int {
		t.Helper()
		rec := f.send(alice, "PUT", path, policyBody(t, "rego", source, nil), map[string]string{"If-Match": ifMatch})
		if rec.Code == http.StatusPreconditionFailed {
			if strict[plainError](t, rec).Error == "" {
				t.Errorf("412 with %s", rec.Body)
			}
		}
		return rec.Code
	}
	if code := put(noScripting, `"7"`); code != http.StatusPreconditionFailed {
		t.Errorf(`If-Match "7" at version 1: %d`, code)
	}
	if _, source, _, _, _ := f.spec(id); source == noScripting {
		t.Error("a write that failed its condition was saved")
	}
	if code := put(noScripting, `"1"`); code != http.StatusAccepted {
		t.Errorf(`If-Match "1" at version 1: %d`, code)
	}
	// What the editor read is no longer what is there.
	if code := put(observeOnly, `"1"`); code != http.StatusPreconditionFailed {
		t.Errorf(`If-Match "1" at version 2: %d`, code)
	}
	for _, header := range []string{`"2"`, `W/"3"`, `"9", "4"`, `*`, ``} {
		source := `{"version": 1, "description": ` + string(mustJSON(t, header)) + `}`
		if code := put(source, header); code != http.StatusAccepted {
			t.Errorf("If-Match %s: %d", header, code)
		}
	}
	// The same rule for a reset.
	if rec := f.send(alice, "DELETE", path, "", map[string]string{"If-Match": `"1"`}); rec.Code != http.StatusPreconditionFailed {
		t.Errorf("DELETE with a stale If-Match: %d", rec.Code)
	}
	if rec := f.send(alice, "DELETE", path, "", map[string]string{"If-Match": `"7"`}); rec.Code != http.StatusAccepted {
		t.Errorf("DELETE with the current If-Match: %d %s", rec.Code, rec.Body)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Every cell of the table in backend-api.yaml: who may write a policy in
// which mode, and that either may change the mode in either.
func TestManagementModes(t *testing.T) {
	ci := token(allScopes...)
	asCode := &management{Mode: "iac", ManagedURL: managedURL}
	type attempt struct {
		name       string
		method     string
		management *management // sent with a PUT
		code       int
		refusal    string // of a 409
		// Of the policy afterwards.
		source, mode, by string
	}
	changed := "saved"
	for _, c := range []struct {
		name  string
		setup string // the mode the policy is in
		user  auth.User
		tries []attempt
	}{
		{"editor mode, cookie", "editor", alice, []attempt{
			{"save", "PUT", nil, 202, "", changed, "editor", "ui"},
			{"save and hand over", "PUT", asCode, 202, "", changed, "iac", "ui"},
			{"reset", "DELETE", nil, 202, "", unrestrictedSource, "editor", "ui"},
		}},
		{"editor mode, token", "editor", ci, []attempt{
			{"save", "PUT", nil, 409, "this policy is managed in the editor", "", "editor", "ui"},
			{"save, staying in the editor", "PUT", &management{Mode: "editor"}, 409, "this policy is managed in the editor", "", "editor", "ui"},
			{"save and take over", "PUT", asCode, 202, "", changed, "iac", "token:ci"},
			{"reset", "DELETE", nil, 409, "this policy is managed in the editor", "", "editor", "ui"},
		}},
		{"iac mode, cookie", "iac", alice, []attempt{
			{"save", "PUT", nil, 409, "this policy is managed externally", "", "iac", "token:ci"},
			{"save and take back", "PUT", &management{Mode: "editor"}, 409, "this policy is managed externally", "", "iac", "token:ci"},
			{"reset", "DELETE", nil, 409, "this policy is managed externally", "", "iac", "token:ci"},
		}},
		{"iac mode, token", "iac", ci, []attempt{
			{"save", "PUT", nil, 202, "", changed, "iac", "token:ci"},
			{"save and hand back", "PUT", &management{Mode: "editor"}, 202, "", changed, "editor", "token:ci"},
			{"reset", "DELETE", nil, 202, "", unrestrictedSource, "editor", "token:ci"},
		}},
	} {
		for _, try := range c.tries {
			t.Run(c.name+", "+try.name, func(t *testing.T) {
				f := newPolicyFixture(t, false)
				body := `{"name":"one","policy":` + policyBody(t, "rego", noScripting, nil) + `}`
				if c.setup == "iac" {
					body = `{"name":"one","policy":` + policyBody(t, "rego", noScripting, asCode) + `}`
				}
				creator := alice
				if c.setup == "iac" {
					creator = ci
				}
				rec := f.do(creator, "POST", "/api/sessions", body)
				if rec.Code != http.StatusCreated {
					t.Fatalf("create: %d %s", rec.Code, rec.Body)
				}
				id := strict[sessionWithPolicy](t, rec).ID
				path := "/api/sessions/" + id + "/policy"

				sent := ""
				if try.method == "PUT" {
					sent = policyBody(t, "rego", observeOnly, try.management)
				}
				rec = f.do(c.user, try.method, path, sent)
				if rec.Code != try.code {
					t.Fatalf("%s: %d %s, want %d", try.method, rec.Code, rec.Body, try.code)
				}
				wantSource := noScripting
				switch try.source {
				case changed:
					wantSource = observeOnly
				case unrestrictedSource:
					wantSource = policy.Unrestricted().Source
				}
				if try.code == http.StatusConflict {
					refusal := strict[modeError](t, rec)
					wantURL := ""
					if c.setup == "iac" {
						wantURL = managedURL
					}
					if refusal.Error != try.refusal || refusal.ManagedURL != wantURL {
						t.Errorf("refusal = %+v, want %q with link %q", refusal, try.refusal, wantURL)
					}
				} else {
					p := strict[fullPolicy](t, rec)
					if p.Source != wantSource || p.Management.Mode != try.mode || p.UpdatedBy != try.by {
						t.Errorf("answer = %+v", p)
					}
				}
				_, source, mode, url, by := f.spec(id)
				if source != wantSource || mode != try.mode || by != try.by || (mode == "iac") != (url == managedURL) {
					t.Errorf("afterwards: %q, mode %s (%s), by %s; want %q, mode %s, by %s", source, mode, url, by, wantSource, try.mode, try.by)
				}
			})
		}

		// Either credential may change the mode, in either mode.
		for _, to := range []management{{Mode: "editor"}, {Mode: "iac", ManagedURL: "https://example.com/other"}} {
			t.Run(c.name+", management to "+to.Mode, func(t *testing.T) {
				f := newPolicyFixture(t, false)
				first := (*management)(nil)
				creator := alice
				if c.setup == "iac" {
					first, creator = asCode, ci
				}
				rec := f.do(creator, "POST", "/api/sessions", `{"name":"one","policy":`+policyBody(t, "rego", noScripting, first)+`}`)
				if rec.Code != http.StatusCreated {
					t.Fatalf("create: %d %s", rec.Code, rec.Body)
				}
				id := strict[sessionWithPolicy](t, rec).ID
				rec = f.do(c.user, "PUT", "/api/sessions/"+id+"/policy/management", string(mustJSON(t, map[string]string{"mode": to.Mode, "managed_url": to.ManagedURL})))
				if rec.Code != http.StatusOK {
					t.Fatalf("PUT management: %d %s", rec.Code, rec.Body)
				}
				if p := strict[fullPolicy](t, rec); *p.Management != to || p.Source != noScripting {
					t.Errorf("answer = %+v", p)
				}
				_, source, mode, url, _ := f.spec(id)
				if source != noScripting || mode != to.Mode || url != to.ManagedURL {
					t.Errorf("afterwards: %q, mode %s (%s)", source, mode, url)
				}
			})
		}
	}
}

func TestPutManagement(t *testing.T) {
	f := newPolicyFixture(t, false)
	id := f.newSession(`{"name":"one"}`)
	path := "/api/sessions/" + id + "/policy/management"
	for _, body := range []string{
		`{`, `{}`, `{"mode":"terraform"}`, `{"mode":"iac"}`, `{"mode":"iac","managed_url":""}`,
		`{"mode":"iac","managed_url":"http://git.example.com/infra"}`, `{"mode":"iac","managed_url":"git.example.com/infra"}`,
		`{"mode":"iac","managed_url":"https://"}`, `{"mode":"iac","managed_url":"https://example.com/` + strings.Repeat("a", 2048) + `"}`,
	} {
		rec := f.do(alice, "PUT", path, body)
		if strict[plainError](t, rec); rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %.60s: %d %s", body, rec.Code, rec.Body)
		}
	}
	if _, _, mode, _, _ := f.spec(id); mode != "editor" {
		t.Fatalf("mode = %s after refused changes", mode)
	}

	rec := f.do(alice, "PUT", path, `{"mode":"iac","managed_url":"`+managedURL+`"}`)
	p := strict[fullPolicy](t, rec)
	if rec.Code != http.StatusOK || *p.Management != (management{Mode: "iac", ManagedURL: managedURL}) || p.UpdatedBy != "ui" || rec.Header().Get("ETag") != `"2"` {
		t.Errorf("to iac: %d %+v", rec.Code, p)
	}
	// The session carries it, for the tag on the list.
	if s := strict[sessionWithPolicy](t, f.do(alice, "GET", "/api/sessions/"+id, "")); *s.Policy.Management != (management{Mode: "iac", ManagedURL: managedURL}) {
		t.Errorf("session policy = %+v", s.Policy)
	}
	// Saying it again changes nothing.
	rec = f.do(token(allScopes...), "PUT", path, `{"mode":"iac","managed_url":"`+managedURL+`"}`)
	if p := strict[fullPolicy](t, rec); rec.Code != http.StatusOK || p.Version != 2 || p.UpdatedBy != "ui" {
		t.Errorf("again: %d %+v", rec.Code, p)
	}
	// Back to the editor: the link goes.
	rec = f.do(alice, "PUT", path, `{"mode":"editor","managed_url":"`+managedURL+`"}`)
	if p := strict[fullPolicy](t, rec); rec.Code != http.StatusOK || *p.Management != (management{Mode: "editor"}) || p.Version != 3 {
		t.Errorf("to editor: %d %+v", rec.Code, p)
	}
	if _, _, mode, url, _ := f.spec(id); mode != "editor" || url != "" {
		t.Errorf("afterwards: mode %s, link %q", mode, url)
	}
}

type route struct {
	method, path, body string
	code               int    // for someone who may
	scope              string // what a token needs
}

func policyRoutes(t *testing.T, id string) []route {
	return []route{
		{"GET", "/api/sessions/" + id + "/policy", "", 200, "policies:read"},
		{"PUT", "/api/sessions/" + id + "/policy", policyBody(t, "rego", noScripting, &management{Mode: "iac", ManagedURL: managedURL}), 202, "policies:write"},
		{"PUT", "/api/sessions/" + id + "/policy/management", `{"mode":"editor"}`, 200, "policies:write"},
		// Of a session that has the unrestricted policy already: no change.
		{"DELETE", "/api/sessions/" + id + "/policy", "", 200, "policies:write"},
		// Not about a session: any token.
		{"POST", "/api/policies/validate", policyBody(t, "rego", noScripting, nil), 200, ""},
		{"POST", "/api/policies/evaluate", `{"kind":"rego","source":"package browserjs.policy","input":{"operation":"mcp_call_tool"}}`, 200, ""},
		{"GET", "/api/policy-presets", "", 200, ""},
	}
}

// The owner and an admin may; anyone else is told the session is not there,
// as for the session itself. What is not about a session is anyone's.
func TestWhoMayUseThePolicyRoutes(t *testing.T) {
	for i, r := range policyRoutes(t, "{id}") {
		ofSession := strings.Contains(r.path, "/sessions/")
		for name, c := range map[string]struct {
			user auth.User
			may  bool
		}{"owner": {alice, true}, "admin": {root, true}, "stranger": {bob, !ofSession}} {
			t.Run(r.method+" "+r.path+" as "+name, func(t *testing.T) {
				f := newPolicyFixture(t, false)
				id := f.newSession(`{"name":"one"}`)
				r := policyRoutes(t, id)[i]
				before := sessionstest.Policy(t, f.client, id)
				rec := f.do(c.user, r.method, r.path, r.body)
				switch {
				case c.may && rec.Code != r.code:
					t.Errorf("%d %s, want %d", rec.Code, rec.Body, r.code)
				case !c.may:
					if rec.Code != http.StatusNotFound || strict[plainError](t, rec).Error != "session not found" {
						t.Errorf("%d %s, want 404 as for a session that is not there", rec.Code, rec.Body)
					}
					if after := sessionstest.Policy(t, f.client, id); after.GetResourceVersion() != before.GetResourceVersion() {
						t.Error("a stranger changed the policy")
					}
				}
				// Nobody at all: not signed in.
				req := httptest.NewRequest(r.method, r.path, strings.NewReader(r.body))
				anon := httptest.NewRecorder()
				f.handler.ServeHTTP(anon, req)
				if anon.Code != http.StatusUnauthorized {
					t.Errorf("without a user: %d", anon.Code)
				}
			})
		}
	}
	// A session that does not exist, and an ID that could not be one.
	f := newPolicyFixture(t, false)
	for _, id := range []string{"s-aaaaaaaaaa", "nonsense"} {
		for _, r := range policyRoutes(t, id)[:4] {
			if rec := f.do(root, r.method, r.path, r.body); rec.Code != http.StatusNotFound {
				t.Errorf("%s %s: %d", r.method, r.path, rec.Code)
			}
		}
	}
}

// A token does what its scopes say: reading needs policies:read, writing
// policies:write, and neither implies the other.
func TestTokenScopes(t *testing.T) {
	for _, scope := range []string{"policies:read", "policies:write"} {
		other := map[string]string{"policies:read": "policies:write", "policies:write": "policies:read"}[scope]
		for i, r := range policyRoutes(t, "{id}") {
			t.Run(r.method+" "+r.path+" with "+scope, func(t *testing.T) {
				f := newPolicyFixture(t, false)
				id := f.newSession(`{"name":"one"}`)
				r := policyRoutes(t, id)[i]
				if r.method == "DELETE" {
					// A token resets only what is managed as code.
					if rec := f.do(alice, "PUT", "/api/sessions/"+id+"/policy/management", `{"mode":"iac","managed_url":"`+managedURL+`"}`); rec.Code != 200 {
						t.Fatal(rec.Code, rec.Body)
					}
					r.code = http.StatusAccepted
				}
				rec := f.do(token("sessions:read", "sessions:write", scope), r.method, r.path, r.body)
				if r.scope == scope || r.scope == "" {
					if rec.Code != r.code {
						t.Errorf("with %s: %d %s, want %d", scope, rec.Code, rec.Body, r.code)
					}
					return
				}
				if rec.Code != http.StatusForbidden || !strings.Contains(strict[plainError](t, rec).Error, other) {
					t.Errorf("with %s only: %d %s, want 403 naming %s", scope, rec.Code, rec.Body, other)
				}
			})
		}
	}
	// A token is never an admin's, whoever's it is: the user it stands for
	// is what the session check is asked about.
	f := newPolicyFixture(t, false)
	id := f.newSession(`{"name":"one"}`)
	other := f.newSession(`{"name":"two"}`)
	// A token made for one session reaches that session's policy and no
	// other of its owner's, and cannot create a session with a policy.
	bound := auth.User{Subject: alice.Subject, Token: &auth.TokenInfo{Name: "agent", Scopes: auth.Scopes, Session: id}}
	for i, r := range policyRoutes(t, id) {
		want := r.code
		if r.method == "DELETE" {
			// By now in editor mode again, which a token does not reset.
			want = http.StatusConflict
		}
		if rec := f.do(bound, r.method, r.path, r.body); rec.Code != want {
			t.Errorf("%s %s with the session's token: %d %s, want %d", r.method, r.path, rec.Code, rec.Body, want)
		}
		if i >= 4 {
			continue
		}
		r = policyRoutes(t, other)[i]
		before := sessionstest.Policy(t, f.client, other).GetResourceVersion()
		rec := f.do(bound, r.method, r.path, r.body)
		if rec.Code != http.StatusForbidden || !strings.Contains(strict[plainError](t, rec).Error, "another session") {
			t.Errorf("%s %s with another session's token: %d %s, want 403", r.method, r.path, rec.Code, rec.Body)
		}
		if sessionstest.Policy(t, f.client, other).GetResourceVersion() != before {
			t.Errorf("%s %s with another session's token changed the policy", r.method, r.path)
		}
	}
	if rec := f.do(bound, "POST", "/api/sessions", `{"policy":`+policyBody(t, "rego", noScripting, nil)+`}`); rec.Code != http.StatusForbidden {
		t.Errorf("create with a policy, with a session's token: %d %s", rec.Code, rec.Body)
	}
	bobs := auth.User{Subject: bob.Subject, Token: &auth.TokenInfo{Name: "ci", Scopes: allScopes}}
	if rec := f.do(bobs, "GET", "/api/sessions/"+id+"/policy", ""); rec.Code != http.StatusNotFound {
		t.Errorf("another user's token: %d", rec.Code)
	}
}

// A session from before policies has no policy and cannot be given one.
func TestSessionsThatPredatePolicies(t *testing.T) {
	f := newPolicyFixture(t, true)
	rec := f.do(alice, "POST", "/api/sessions", `{"name":"old"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	s := strict[sessionWithPolicy](t, rec)
	sessionstest.SetStatus(t, f.client, s.ID, sessionstest.Ready("10.0.0.7"))
	for _, body := range []string{
		f.do(alice, "GET", "/api/sessions/"+s.ID, "").Body.String(),
		strings.Trim(strings.TrimSpace(f.do(alice, "GET", "/api/sessions", "").Body.String()), "[]"),
	} {
		var got sessionWithPolicy
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err, body)
		}
		// It runs as it always did: there is no policy to wait for.
		if got.State != "running" || got.Policy == nil || *got.Policy != (policySummary{State: "unsupported"}) {
			t.Errorf("session = %s", body)
		}
		if !strings.Contains(body, `"policy":{"state":"unsupported"}`) {
			t.Errorf("policy of an unsupported session = %s", body)
		}
	}
	if sessionstest.Policy(t, f.client, s.ID) != nil {
		t.Error("a SessionPolicy was made for a session that never asks OPA")
	}
	for _, r := range policyRoutes(t, s.ID)[:4] {
		rec := f.do(alice, r.method, r.path, r.body)
		if rec.Code != http.StatusConflict || !strings.Contains(strict[modeError](t, rec).Error, "before policies") {
			t.Errorf("%s %s: %d %s, want 409", r.method, r.path, rec.Code, rec.Body)
		}
	}
	if sessionstest.Policy(t, f.client, s.ID) != nil {
		t.Error("a SessionPolicy was made by a refused write")
	}
	// One that is asked to be restricted is not made at all.
	rec = f.do(alice, "POST", "/api/sessions", `{"name":"new","policy":`+policyBody(t, "rego", noScripting, nil)+`}`)
	if strict[plainError](t, rec); rec.Code != http.StatusConflict {
		t.Errorf("create with a policy: %d %s", rec.Code, rec.Body)
	}
	if mine, _ := f.store.List(t.Context(), alice.Subject); len(mine) != 1 {
		t.Errorf("%d sessions", len(mine))
	}
}

// A policy removed behind the backend (kubectl): nothing is in force, and
// saving one puts it back.
func TestAPolicyThatWasRemoved(t *testing.T) {
	f := newPolicyFixture(t, false)
	id := f.newSession(`{"name":"one"}`)
	sessionstest.SetStatus(t, f.client, id, sessionstest.Ready("10.0.0.7"))
	if err := f.client.Resource(sessions.PolicyGVR).Namespace(sessionstest.Namespace).Delete(t.Context(), id, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	s := strict[sessionWithPolicy](t, f.do(alice, "GET", "/api/sessions/"+id, ""))
	if s.State != "starting" || *s.Policy != (policySummary{State: "loading"}) {
		t.Errorf("session = %+v, policy %+v", s, s.Policy)
	}
	if rec := f.do(alice, "GET", "/api/sessions/"+id+"/policy", ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(alice, "PUT", "/api/sessions/"+id+"/policy/management", `{"mode":"editor"}`); rec.Code != http.StatusNotFound {
		t.Errorf("PUT management: %d %s", rec.Code, rec.Body)
	}
	if rec := f.send(alice, "PUT", "/api/sessions/"+id+"/policy", policyBody(t, "rego", noScripting, nil), map[string]string{"If-Match": `"1"`}); rec.Code != http.StatusPreconditionFailed {
		t.Errorf("PUT with If-Match: %d %s", rec.Code, rec.Body)
	}
	rec := f.do(alice, "PUT", "/api/sessions/"+id+"/policy", policyBody(t, "rego", noScripting, nil))
	if p := strict[fullPolicy](t, rec); rec.Code != http.StatusAccepted || p.Source != noScripting || p.Version != 1 {
		t.Errorf("PUT: %d %+v", rec.Code, p)
	}
	obj := sessionstest.Policy(t, f.client, id)
	if refs := obj.GetOwnerReferences(); len(refs) != 1 || refs[0].Name != id || obj.GetLabels()[sessions.LabelOwner] != sessions.OwnerLabel(alice.Subject) {
		t.Errorf("the new SessionPolicy: owners %v, labels %v", refs, obj.GetLabels())
	}
}

func TestValidateEvaluateSchemaAndPresets(t *testing.T) {
	f := newPolicyFixture(t, false)

	rec := f.do(alice, "POST", "/api/policies/validate", policyBody(t, "rego", noScripting, nil))
	v := strict[validation](t, rec)
	if rec.Code != 200 || v.OK == nil || !*v.OK || v.Errors == nil || v.Warnings == nil || v.Hash != "sha256:0f0f" || !strings.HasPrefix(v.Rego, "package browserjs.policy") {
		t.Errorf("valid: %d %s", rec.Code, rec.Body)
	}
	// An invalid policy is a 200 with ok false.
	rec = f.do(alice, "POST", "/api/policies/validate", policyBody(t, "rego", "package INVALID", nil))
	v = strict[validation](t, rec)
	if rec.Code != 200 || v.OK == nil || *v.OK || v.Errors == nil || len(*v.Errors) != 1 || (*v.Errors)[0].Row != 2 || v.Warnings == nil || len(*v.Warnings) != 1 {
		t.Errorf("invalid: %d %s", rec.Code, rec.Body)
	}
	// Only the policy goes to the operator, whatever else was sent.
	f.do(alice, "POST", "/api/policies/validate", `{"kind":"rego","source":"package browserjs.policy","management":{"mode":"iac"},"session_id":"s-aaaaa"}`)
	if sent := f.operator.sent("/v1/validate"); len(sent) != 2 || sent["kind"] != "rego" || sent["source"] != "package browserjs.policy" {
		t.Errorf("the operator was sent %v", sent)
	}

	input := `{"operation":"mcp_call_tool","server":"browser","tool":"browser_execute","arguments":{"operations":[{"type":"evaluate"}]}}`
	for source, allow := range map[string]bool{noScripting: true, "package browserjs.policy # DENY": false} {
		kind := "rego"
		if !allow {
			kind = "rego"
		}
		rec = f.do(alice, "POST", "/api/policies/evaluate", `{"kind":"`+kind+`","source":`+string(mustJSON(t, source))+`,"input":`+input+`}`)
		e := strict[evaluation](t, rec)
		if rec.Code != 200 || e.OK == nil || !*e.OK || e.Allow == nil || *e.Allow != allow {
			t.Errorf("evaluate %q: %d %s", source, rec.Code, rec.Body)
		}
	}
	if sent := f.operator.sent("/v1/evaluate"); len(sent) != 3 || sent["input"].(map[string]any)["tool"] != "browser_execute" {
		t.Errorf("the operator was sent %v", sent)
	}
	rec = f.do(alice, "POST", "/api/policies/evaluate", `{"kind":"rego","source":"package INVALID","input":`+input+`}`)
	if e := strict[evaluation](t, rec); rec.Code != 200 || e.OK == nil || *e.OK || e.Allow != nil || len(e.Errors) != 1 {
		t.Errorf("evaluate an invalid policy: %d %s", rec.Code, rec.Body)
	}

	for _, c := range []struct {
		path, body string
		code       int
	}{
		{"/api/policies/validate", `{`, 400},
		{"/api/policies/validate", `{"kind":"yaml","source":"x"}`, 400},
		{"/api/policies/validate", `{"kind":"rego","source":"MALFORMED"}`, 400}, // the operator's own 400
		{"/api/policies/validate", `{"kind":"rego","source":"` + strings.Repeat("x", 130<<10) + `"}`, 413},
		{"/api/policies/evaluate", `{`, 400},
		{"/api/policies/evaluate", `{"kind":"rego","source":"package browserjs.policy"}`, 400},
		{"/api/policies/evaluate", `{"kind":"toml","source":"{}","input":{}}`, 400},
	} {
		rec := f.do(alice, "POST", c.path, c.body)
		if strict[plainError](t, rec); rec.Code != c.code {
			t.Errorf("POST %s %.40s: %d %s, want %d", c.path, c.body, rec.Code, rec.Body, c.code)
		}
	}

	// There is no schema to serve: Rego is the only form.
	if rec = f.do(alice, "GET", "/api/policy-schema.json", ""); rec.Code != http.StatusNotFound {
		t.Errorf("schema: %d %s", rec.Code, rec.Body)
	}
	for _, body := range []string{`{"kind":"json","source":"{\"version\": 1}"}`, `{"kind":"yaml","source":"x"}`} {
		for _, path := range []string{"/api/policies/validate", "/api/policies/evaluate"} {
			if rec = f.do(alice, "POST", path, body); rec.Code != http.StatusBadRequest {
				t.Errorf("%s %s: %d %s", path, body, rec.Code, rec.Body)
			}
		}
	}
	// A policy that names no kind is Rego.
	if rec = f.do(alice, "POST", "/api/policies/validate", `{"source":"package browserjs.policy"}`); rec.Code != http.StatusOK {
		t.Errorf("validate without a kind: %d %s", rec.Code, rec.Body)
	}

	rec = f.do(alice, "GET", "/api/policy-presets", "")
	presets := strict[[]preset](t, rec)
	if rec.Code != 200 || len(presets) != 7 || presets[0].ID != "unrestricted" {
		t.Fatalf("presets: %d %s", rec.Code, rec.Body)
	}
	for _, p := range presets {
		if p.ID == "" || p.Title == "" || p.Description == "" || p.Kind != "rego" || !strings.Contains(p.Source, "package browserjs.policy\n") {
			t.Errorf("preset %+v", p)
		}
	}

	// Without the operator: 503 for what it answers; the presets are the
	// backend's own.
	f.operator.setDown(true)
	for _, r := range policyRoutes(t, "")[4:6] {
		rec := f.do(alice, r.method, r.path, r.body)
		if strict[plainError](t, rec); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without the operator: %d %s", r.method, r.path, rec.Code, rec.Body)
		}
	}
	if rec := f.do(alice, "GET", "/api/policy-presets", ""); rec.Code != 200 {
		t.Errorf("presets without the operator: %d", rec.Code)
	}
}

// From the warm pool, through the API: the session is the pool's Sandbox and
// has the policy it was asked to have.
func TestCreateFromTheWarmPoolWithAPolicy(t *testing.T) {
	f := newPolicyFixture(t, false)
	f.store.EnableWarmPool(sessionstest.WarmPoolName, time.Second)
	sessionstest.PlayPolicyClaimController(t, f.client, "s-bcdfg")
	id := f.newSession(`{"name":"one","policy":` + policyBody(t, "rego", noScripting, nil) + `}`)
	if id != "s-bcdfg" {
		t.Fatalf("session %s, want the warm Sandbox", id)
	}
	if p := f.get(id); p.Source != noScripting || p.Version != 1 || p.State != "loading" {
		t.Errorf("policy = %+v", p)
	}
}
