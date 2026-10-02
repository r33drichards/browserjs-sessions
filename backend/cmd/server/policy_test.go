package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"

	"github.com/r33drichards/computer-use/backend/internal/auth"
	"github.com/r33drichards/computer-use/backend/internal/config"
	"github.com/r33drichards/computer-use/backend/internal/idle"
	"github.com/r33drichards/computer-use/backend/internal/policy"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
	"github.com/r33drichards/computer-use/backend/internal/tokens"
)

// The whole server, as run() wires it. Without POLICY_OPERATOR_URL there is
// nothing of policies in it; with it, the routes are on the app's host,
// behind the same sign-in as the rest of the API.
func TestPolicyRoutesNeedTheOperatorConfigured(t *testing.T) {
	off := newServer(t)
	created := off.session(alice)
	for _, path := range []string{"/api/policy-presets", "/api/sessions/" + created.ID + "/policy"} {
		if rec := off.do("GET", appHost, path, alice, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s with policies off: %d %s", path, rec.Code, rec.Body)
		}
	}
	rec := off.do("GET", appHost, "/api/sessions/"+created.ID, alice, "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "policy") {
		t.Errorf("a session with policies off: %d %s", rec.Code, rec.Body)
	}

	operator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" || r.URL.Path != "/v1/validate" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"errors":[],"warnings":[]}`))
	}))
	defer operator.Close()
	on := &server{t: t, key: off.key}
	store, client := sessionstest.NewWithPolicies(t)
	on.client = client
	cfg := config.Config{
		WebDir: t.TempDir(), PublicURL: sessionstest.PublicURL, SessionURLs: sessionstest.URLs(),
		SignOutURL: "/.pomerium/sign_out", ReadyTimeout: time.Second, MaxSessionsPerUser: 5,
		PolicyOperatorURL: operator.URL, OperatorAPIToken: "secret",
	}
	verifier, err := auth.NewAssertionVerifier(func(*jwt.Token) (any, error) { return &off.key.PublicKey, nil }, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	on.handler, _ = newHandler(cfg, verifier, store, idle.New(15*time.Minute, time.Now))

	rec = on.do("POST", appHost, "/api/sessions", alice, `{"name":"work","policy":{"kind":"rego","source":"package browserjs.policy"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with a policy: %d %s", rec.Code, rec.Body)
	}
	var s struct {
		ID     string
		Policy struct{ State string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil || s.Policy.State != "loading" {
		t.Fatalf("created %s (%v)", rec.Body, err)
	}
	if obj := sessionstest.Policy(t, client, s.ID); obj == nil {
		t.Error("the session has no SessionPolicy")
	}
	rec = on.do("GET", appHost, "/api/policy-presets", alice, "")
	var presets []policy.Preset
	if err := json.Unmarshal(rec.Body.Bytes(), &presets); rec.Code != http.StatusOK || err != nil || len(presets) == 0 {
		t.Errorf("presets: %d %s", rec.Code, rec.Body)
	}
	if rec := on.do("GET", appHost, "/api/sessions/"+s.ID+"/policy", alice, ""); rec.Code != http.StatusOK {
		t.Errorf("GET policy: %d %s", rec.Code, rec.Body)
	}
	// Behind the sign-in, and the owner check, like everything under /api.
	if rec := on.do("GET", appHost, "/api/policy-presets", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("presets without signing in: %d", rec.Code)
	}
	if rec := on.do("GET", appHost, "/api/sessions/"+s.ID+"/policy", bob, ""); rec.Code != http.StatusNotFound {
		t.Errorf("another user's policy: %d", rec.Code)
	}
}

// The mode rule and the scopes with real credentials: a token on the API
// host under /v1, the cookie on the app's host under /api, one set of
// handlers.
func TestPoliciesWithAPITokens(t *testing.T) {
	operator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"errors":[],"warnings":[]}`))
	}))
	defer operator.Close()
	base := newServer(t)
	verifier, err := auth.NewAssertionVerifier(func(*jwt.Token) (any, error) { return &base.key.PublicKey, nil }, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	store, client := sessionstest.NewWithPolicies(t)
	cfg := config.Config{
		WebDir: t.TempDir(), PublicURL: sessionstest.PublicURL, SessionURLs: sessionstest.URLs(),
		SignOutURL: "/.pomerium/sign_out", ReadyTimeout: time.Second, MaxSessionsPerUser: 5,
		PolicyOperatorURL: operator.URL, OperatorAPIToken: "secret",
		APIURL: "https://" + apiHost, AllowedEmails: []string{alice, bob},
	}
	app, _ := newHandler(cfg, verifier, store, idle.New(15*time.Minute, time.Now))
	tokenStore := tokens.NewStore(dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{tokens.GVR: "APITokenList"}), sessionstest.Namespace)
	s := &server{t: t, key: base.key, client: client}
	if s.handler, err = withAPITokens(cfg, verifier, tokenStore, app); err != nil {
		t.Fatal(err)
	}

	mine, other := s.session(alice), s.session(alice)
	_, all := s.newToken(alice, auth.Scopes...)
	_, sessionsOnly := s.newToken(alice, auth.ScopeSessionsRead, auth.ScopeSessionsWrite)
	_, bobs := s.newToken(bob, auth.Scopes...)
	const source = `{"kind":"rego","source":"package browserjs.policy"`
	const link = "https://git.example.com/infra"
	path := "/v1/sessions/" + mine.ID + "/policy"
	message := func(rec *httptest.ResponseRecorder) (m struct {
		Error      string
		ManagedURL string `json:"managed_url"`
	}) {
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return m
	}

	// Made in the UI: in editor mode, which a token reads and does not write.
	if rec := s.bearer("GET", apiHost, path, all, ""); rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"1"` {
		t.Fatalf("GET with a token: %d %s", rec.Code, rec.Body)
	}
	if rec := s.bearer("PUT", apiHost, path, all, source+`}`); rec.Code != http.StatusConflict || message(rec).Error != "this policy is managed in the editor" {
		t.Errorf("token, editor mode: %d %s", rec.Code, rec.Body)
	}
	if rec := s.bearer("DELETE", apiHost, path, all, ""); rec.Code != http.StatusConflict {
		t.Errorf("token reset, editor mode: %d %s", rec.Code, rec.Body)
	}
	// Unless it takes the policy over in the same request.
	rec := s.bearer("PUT", apiHost, path, all, source+`,"management":{"mode":"iac","managed_url":"`+link+`"}}`)
	if rec.Code != http.StatusOK && rec.Code != http.StatusAccepted {
		t.Fatalf("token taking over: %d %s", rec.Code, rec.Body)
	}
	if by := sessionstest.Policy(t, client, mine.ID).GetAnnotations()["browserjs.dev/updated-by"]; by != "token:ci" {
		t.Errorf("updated-by = %q", by)
	}
	// Now the cookie may not write, and is told where the policy is managed.
	rec = s.do("PUT", appHost, "/api/sessions/"+mine.ID+"/policy", alice, `{"kind":"rego","source":"package browserjs.policy\n"}`)
	if m := message(rec); rec.Code != http.StatusConflict || m.Error != "this policy is managed externally" || m.ManagedURL != link {
		t.Errorf("cookie, iac mode: %d %s", rec.Code, rec.Body)
	}
	// Either may change the mode. (Each save waits ten seconds for an
	// operator that is not there, so the token's save in iac mode is left
	// to internal/api's tests.)
	if rec := s.do("PUT", appHost, "/api/sessions/"+mine.ID+"/policy/management", alice, `{"mode":"editor"}`); rec.Code != http.StatusOK {
		t.Errorf("cookie taking back: %d %s", rec.Code, rec.Body)
	}
	if rec := s.bearer("PUT", apiHost, path+"/management", all, `{"mode":"iac","managed_url":"`+link+`"}`); rec.Code != http.StatusOK {
		t.Errorf("token changing the mode: %d %s", rec.Code, rec.Body)
	}

	// Scopes: a session's policy needs them, what is about no session does not.
	if rec := s.bearer("GET", apiHost, path, sessionsOnly, ""); rec.Code != http.StatusForbidden {
		t.Errorf("GET without policies:read: %d %s", rec.Code, rec.Body)
	}
	if rec := s.bearer("PUT", apiHost, path+"/management", sessionsOnly, `{"mode":"editor"}`); rec.Code != http.StatusForbidden {
		t.Errorf("PUT management without policies:write: %d %s", rec.Code, rec.Body)
	}
	if rec := s.bearer("POST", apiHost, "/v1/sessions", sessionsOnly, `{"policy":`+source+`}}`); rec.Code != http.StatusForbidden {
		t.Errorf("create with a policy without policies:write: %d %s", rec.Code, rec.Body)
	}
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/policy-presets", ""}, {"POST", "/v1/policies/validate", source + `}`},
	} {
		if rec := s.bearer(c.method, apiHost, c.path, sessionsOnly, c.body); rec.Code != http.StatusOK {
			t.Errorf("%s %s with any token: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	// Somebody else's token: the session is not there.
	if rec := s.bearer("GET", apiHost, path, bobs, ""); rec.Code != http.StatusNotFound {
		t.Errorf("another user's token: %d %s", rec.Code, rec.Body)
	}
	// A token made for one session does not reach another's policy.
	body, _ := json.Marshal(map[string]any{"name": "agent", "scopes": auth.Scopes, "session_id": mine.ID})
	rec = s.do("POST", appHost, "/api/tokens", alice, string(body))
	var bound struct{ Token string }
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &bound) != nil || bound.Token == "" {
		t.Fatalf("a token for one session: %d %s", rec.Code, rec.Body)
	}
	if rec := s.bearer("GET", apiHost, path, bound.Token, ""); rec.Code != http.StatusOK {
		t.Errorf("its own session's policy: %d %s", rec.Code, rec.Body)
	}
	for _, c := range []struct{ method, suffix, body string }{
		{"GET", "", ""}, {"PUT", "", source + `,"management":{"mode":"iac","managed_url":"` + link + `"}}`},
		{"DELETE", "", ""}, {"PUT", "/management", `{"mode":"iac","managed_url":"` + link + `"}`},
	} {
		before := sessionstest.Policy(t, client, other.ID).GetResourceVersion()
		rec := s.bearer(c.method, apiHost, "/v1/sessions/"+other.ID+"/policy"+c.suffix, bound.Token, c.body)
		if rec.Code != http.StatusForbidden || sessionstest.Policy(t, client, other.ID).GetResourceVersion() != before {
			t.Errorf("%s another session's policy%s with a session's token: %d %s", c.method, c.suffix, rec.Code, rec.Body)
		}
	}
}

// The API host lists the policy routes whether or not there are policies.
// With policies off they are still not there: the 404 is the API's own.
func TestAPIHostHasNoPolicyRoutesWithPoliciesOff(t *testing.T) {
	s, _ := tokenServer(t, alice)
	mine := s.session(alice)
	_, token := s.newToken(alice, auth.Scopes...)
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/sessions/" + mine.ID + "/policy"}, {"PUT", "/v1/sessions/" + mine.ID + "/policy"},
		{"DELETE", "/v1/sessions/" + mine.ID + "/policy"}, {"PUT", "/v1/sessions/" + mine.ID + "/policy/management"},
		{"POST", "/v1/policies/validate"}, {"POST", "/v1/policies/evaluate"},
		{"GET", "/v1/policy-presets"},
	} {
		if rec := s.bearer(c.method, apiHost, c.path, token, `{}`); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s with policies off: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	rec := s.bearer("GET", apiHost, "/v1/sessions/"+mine.ID, token, "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "policy") {
		t.Errorf("a session with policies off: %d %s", rec.Code, rec.Body)
	}
}

// Nothing reaches a session's pod before the session's first policy is in
// force: until then OPA denies the session everything, and a call forwarded
// to the pod would be refused for no reason of the caller's. The call is
// held, as for a pod that is still starting, and goes through once the
// policy is loaded. Created cold and taken from the warm pool alike.
func TestNothingReachesAPodBeforeItsFirstPolicyIsInForce(t *testing.T) {
	operator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"errors":[],"warnings":[]}`))
	}))
	defer operator.Close()
	base := newServer(t)
	s := &server{t: t, key: base.key}
	store, client := sessionstest.NewWithPolicies(t)
	s.client = client
	store.EnablePolicies(client, sessionstest.Namespace, policy.Unrestricted())
	store.EnableWarmPool(sessionstest.WarmPoolName, time.Second)
	cfg := config.Config{
		WebDir: t.TempDir(), PublicURL: sessionstest.PublicURL, SessionURLs: sessionstest.URLs(),
		SignOutURL: "/.pomerium/sign_out", ReadyTimeout: 1500 * time.Millisecond, MaxSessionsPerUser: 5,
		PolicyOperatorURL: operator.URL, OperatorAPIToken: "secret",
	}
	verifier, err := auth.NewAssertionVerifier(func(*jwt.Token) (any, error) { return &s.key.PublicKey, nil }, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	pod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.pod.Add(1)
		_, _ = w.Write([]byte("pod"))
	}))
	defer pod.Close()
	handler, px := newHandler(cfg, verifier, store, idle.New(15*time.Minute, time.Now))
	px.Target = func(sessions.Session, int) string { return strings.TrimPrefix(pod.URL, "http://") }
	s.handler = handler

	state := func(id string) string {
		t.Helper()
		var got sessionJSON
		rec := s.do("GET", appHost, "/api/sessions/"+id, alice, "")
		if err := json.Unmarshal(rec.Body.Bytes(), &got); rec.Code != http.StatusOK || err != nil {
			t.Fatalf("GET session: %d %s", rec.Code, rec.Body)
		}
		return got.State
	}
	for _, from := range []string{"cold", "the warm pool"} {
		if from == "the warm pool" {
			sessionstest.PlayPolicyClaimController(t, client, "s-bcdfg")
		}
		rec := s.do("POST", appHost, "/api/sessions", alice, `{"name":"work"}`)
		var created sessionJSON
		if err := json.Unmarshal(rec.Body.Bytes(), &created); rec.Code != http.StatusCreated || err != nil {
			t.Fatalf("%s: create: %d %s", from, rec.Code, rec.Body)
		}
		if from == "the warm pool" && created.ID != "s-bcdfg" {
			t.Fatalf("session %s, want the warm Sandbox", created.ID)
		}
		id, mcp := created.ID, "/"+created.ID+"/mcp"
		// Its pod runs. Its policy exists and no OPA replica has it.
		sessionstest.SetStatus(t, client, id, sessionstest.Ready("10.0.0.7"))
		before := s.pod.Load()
		for _, status := range []map[string]any{nil, sessionstest.PolicyCompiled(1, true)} {
			if status != nil {
				sessionstest.SetPolicyStatus(t, client, id, status)
			}
			if got := state(id); got != "starting" {
				t.Errorf("%s: the API says %s before the policy is in force", from, got)
			}
			if rec := s.do("GET", sessionsHost, mcp, alice, ""); rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s: the event stream before the policy is in force: %d", from, rec.Code)
			}
			if status == nil {
				continue // one wait for the timeout is enough
			}
			rec := s.do("POST", sessionsHost, mcp, alice, `{}`)
			if rec.Code != http.StatusGatewayTimeout || rec.Header().Get("Retry-After") == "" {
				t.Errorf("%s: a call before the policy is in force: %d %s", from, rec.Code, rec.Body)
			}
		}
		if got := s.pod.Load(); got != before {
			t.Fatalf("%s: %d requests reached the pod before its policy was in force", from, got-before)
		}

		// A call that is waiting goes through when the policy is loaded.
		held := make(chan *httptest.ResponseRecorder, 1)
		go func() { held <- s.do("POST", sessionsHost, mcp, alice, `{}`) }()
		time.Sleep(50 * time.Millisecond)
		sessionstest.SetPolicyStatus(t, client, id, sessionstest.PolicyReady(1))
		if rec := <-held; rec.Code != http.StatusOK || rec.Body.String() != "pod" {
			t.Errorf("%s: the held call: %d %s", from, rec.Code, rec.Body)
		}
		if got := state(id); got != "running" {
			t.Errorf("%s: the API says %s with the policy in force", from, got)
		}
		// An edit that is still loading does not hold calls back.
		sessionstest.SetPolicyStatus(t, client, id, sessionstest.PolicyCompiled(2, false))
		if rec := s.do("POST", sessionsHost, mcp, alice, `{}`); rec.Code != http.StatusOK {
			t.Errorf("%s: a call while an edit loads: %d %s", from, rec.Code, rec.Body)
		}
	}
}
