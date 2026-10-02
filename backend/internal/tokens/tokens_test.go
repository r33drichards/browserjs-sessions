package tokens

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
)

const (
	namespace = "browserjs-sessions"
	alice     = "alice@example.com"
	bob       = "bob@example.com"
	root      = "root@example.com"
)

type fixture struct {
	t      *testing.T
	store  *Store
	client *dynfake.FakeDynamicClient
	clock  time.Time
	mux    *http.ServeMux
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, clock: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	f.client = dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{GVR: "APITokenList"})
	f.store = NewStore(f.client, namespace)
	f.store.now = func() time.Time { return f.clock }
	f.mux = http.NewServeMux()
	NewHandlers(f.store, auth.NewAllowList([]string{alice, bob, root}), "https://api.example.com").Register(f.mux)
	return f
}

func (f *fixture) resource() dynamic.ResourceInterface {
	return f.client.Resource(GVR).Namespace(namespace)
}

func (f *fixture) object(id string) *unstructured.Unstructured {
	f.t.Helper()
	obj, err := f.resource().Get(context.Background(), objectName(id), metav1.GetOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
	return obj
}

func (f *fixture) create(owner string, scopes []string, life time.Duration) (Token, string) {
	f.t.Helper()
	tok, secret, err := f.store.Create(context.Background(), owner, "ci", scopes, "", life)
	if err != nil {
		f.t.Fatal(err)
	}
	return tok, secret
}

// as makes a request the way it arrives behind auth.Middleware.
func (f *fixture) as(u auth.User, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(auth.WithUser(req.Context(), u))
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

func user(email string) auth.User { return auth.User{Subject: email, Admin: email == root} }

var tokenForm = regexp.MustCompile(`^bjs_[a-z2-7]{12}_[A-Za-z0-9_-]{43}$`)

func TestGenerate(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		id, token, err := generate(NewStore(dynfake.NewSimpleDynamicClient(runtime.NewScheme()), namespace).random)
		if err != nil {
			t.Fatal(err)
		}
		if !tokenForm.MatchString(token) || len(token) != tokenLen {
			t.Fatalf("token of the wrong form (length %d)", len(token))
		}
		if got, ok := parse(token); !ok || got != id {
			t.Fatalf("parse gave %q %v for id %q", got, ok, id)
		}
		if seen[token] || seen[id] {
			t.Fatal("a repeat")
		}
		seen[token], seen[id] = true, true
	}
	if _, _, err := generate(strings.NewReader("too short")); err == nil {
		t.Error("made a token without enough random bytes")
	}
}

func TestParseRefusesOtherForms(t *testing.T) {
	_, good, _ := generate(bytes.NewReader(bytes.Repeat([]byte{7}, 40)))
	if _, ok := parse(good); !ok {
		t.Fatal("a good token was refused")
	}
	for _, bad := range []string{
		"", "bjs_", good[:len(good)-1], good + "A", "BJS" + good[3:], "xjs" + good[3:],
		good[:4] + "ABCDEFGHIJKL" + good[16:],        // id in upper case
		good[:4] + "abcdefghij01" + good[16:],        // id outside base32
		good[:16] + "-" + good[17:],                  // no separator
		good[:17] + strings.Repeat("=", 43),          // secret outside base64url
		good[:17] + strings.Repeat("a", 42) + "\n",   // a line end
		good[:17] + strings.Repeat("a", 42) + "\x00", // a NUL
	} {
		if _, ok := parse(bad); ok {
			t.Errorf("parse accepted %q", bad)
		}
	}
}

func TestCreateStoresOnlyTheHash(t *testing.T) {
	f := newFixture(t)
	tok, secret := f.create("  Alice@Example.com ", []string{auth.ScopeSessionsRead}, 90*24*time.Hour)
	if !tokenForm.MatchString(secret) || !strings.HasPrefix(secret, "bjs_"+tok.ID+"_") {
		t.Fatal("token of the wrong form")
	}
	obj := f.object(tok.ID)
	stored, _ := json.Marshal(obj.Object)
	if strings.Contains(string(stored), secret) || strings.Contains(string(stored), secret[len("bjs_")+idLen+1:]) {
		t.Fatal("the token is in its record")
	}
	spec := obj.Object["spec"].(map[string]any)
	if spec["sha256"] != digest(secret) || spec["owner"] != alice || spec["id"] != tok.ID ||
		spec["expiresAt"] != "2026-12-31T12:00:00Z" || obj.GetName() != "tok-"+tok.ID {
		t.Errorf("stored %v", spec)
	}
	if tok.Owner != alice || !tok.Expires.Equal(f.clock.Add(90*24*time.Hour)) || tok.Created.IsZero() {
		t.Errorf("returned %+v", tok)
	}
}

func TestVerify(t *testing.T) {
	f := newFixture(t)
	tok, secret := f.create(alice, []string{auth.ScopeSessionsRead, auth.ScopePoliciesWrite}, 24*time.Hour)
	ctx := context.Background()

	owner, info, err := f.store.VerifyToken(ctx, secret)
	if err != nil || owner != alice || info.Name != "ci" || !info.Has(auth.ScopePoliciesWrite) || info.Has(auth.ScopeSessionsWrite) {
		t.Fatalf("a good token: %q %+v %v", owner, info, err)
	}

	// The right id with another secret, another token's secret, a token
	// that was never made, and things that are not tokens.
	_, other := f.create(bob, auth.Scopes, time.Hour)
	last := byte('A')
	if secret[len(secret)-1] == 'A' {
		last = 'B'
	}
	_, never, _ := generate(bytes.NewReader(bytes.Repeat([]byte{9}, 40)))
	for name, bad := range map[string]string{
		"one character off":      secret[:len(secret)-1] + string(last),
		"another token's secret": secret[:17] + other[17:],
		"never made":             never,
		"empty":                  "",
		"the hash itself":        digest(secret),
		"the id":                 tok.ID,
	} {
		if _, _, err := f.store.VerifyToken(ctx, bad); !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// Expiry is to the second.
	f.clock = tok.Expires.Add(-time.Second)
	if _, _, err := f.store.VerifyToken(ctx, secret); err != nil {
		t.Errorf("a second before expiry: %v", err)
	}
	f.clock = tok.Expires
	if _, _, err := f.store.VerifyToken(ctx, secret); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("at expiry: %v", err)
	}
	f.clock = tok.Expires.Add(-time.Hour)

	// Revoked: refused by the very next request.
	if err := f.store.Delete(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.VerifyToken(ctx, secret); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("after revocation: %v", err)
	}
}

func TestVerifyDistinguishesAnOutageFromABadToken(t *testing.T) {
	f := newFixture(t)
	_, secret := f.create(alice, auth.Scopes, time.Hour)
	f.client.PrependReactor("get", "apitokens", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the API server is away")
	})
	_, _, err := f.store.VerifyToken(context.Background(), secret)
	if err == nil || errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("got %v", err)
	}
	// Something that is not a token is refused without asking the cluster.
	if _, _, err := f.store.VerifyToken(context.Background(), "nonsense"); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("got %v", err)
	}
}

func TestVerifyRefusesATamperedRecord(t *testing.T) {
	f := newFixture(t)
	tok, secret := f.create(alice, auth.Scopes, time.Hour)
	for name, change := range map[string]func(spec map[string]any){
		"no expiry":         func(spec map[string]any) { delete(spec, "expiresAt") },
		"empty hash":        func(spec map[string]any) { spec["sha256"] = "" },
		"no owner":          func(spec map[string]any) { spec["owner"] = "" },
		"another's id":      func(spec map[string]any) { spec["id"] = "aaaaaaaaaaaa" },
		"unreadable expiry": func(spec map[string]any) { spec["expiresAt"] = "never" },
	} {
		obj := f.object(tok.ID)
		original := obj.DeepCopy()
		change(obj.Object["spec"].(map[string]any))
		if _, err := f.resource().Update(context.Background(), obj, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.store.VerifyToken(context.Background(), secret); !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := f.resource().Update(context.Background(), original, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLastUsedIsWrittenAtMostHourly(t *testing.T) {
	f := newFixture(t)
	tok, secret := f.create(alice, auth.Scopes, 30*24*time.Hour)
	patches := func() (n int) {
		for _, a := range f.client.Actions() {
			if a.GetVerb() == "patch" {
				if a.GetSubresource() != "status" {
					t.Errorf("a patch of %q, not of the status", a.GetSubresource())
				}
				n++
			}
		}
		return n
	}
	verify := func() {
		t.Helper()
		if _, _, err := f.store.VerifyToken(context.Background(), secret); err != nil {
			t.Fatal(err)
		}
	}
	lastUsed := func() string {
		used, _, _ := unstructured.NestedString(f.object(tok.ID).Object, "status", "lastUsedTime")
		return used
	}
	for range 5 {
		verify()
		f.clock = f.clock.Add(10 * time.Minute)
	}
	if patches() != 1 || lastUsed() != "2026-10-02T12:00:00Z" {
		t.Fatalf("after 50 minutes: %d writes, last used %q", patches(), lastUsed())
	}
	f.clock = f.clock.Add(15 * time.Minute)
	verify()
	if patches() != 2 || lastUsed() != "2026-10-02T13:05:00Z" {
		t.Fatalf("after 65 minutes: %d writes, last used %q", patches(), lastUsed())
	}
	// A restart forgets what it wrote; the record says it.
	f.store = NewStore(f.client, namespace)
	f.store.now = func() time.Time { return f.clock }
	verify()
	if patches() != 2 {
		t.Errorf("a new process wrote again within the hour")
	}
	if got, _ := f.store.List(context.Background(), alice); len(got) != 1 || got[0].LastUsed == nil || !got[0].LastUsed.Equal(f.clock) {
		t.Errorf("listed %+v", got)
	}
}

func TestLastUsedFailureDoesNotFailTheRequest(t *testing.T) {
	f := newFixture(t)
	_, secret := f.create(alice, auth.Scopes, time.Hour)
	tries := 0
	f.client.PrependReactor("patch", "apitokens", func(k8stesting.Action) (bool, runtime.Object, error) {
		tries++
		return true, nil, errors.New("forbidden")
	})
	for range 3 {
		if _, _, err := f.store.VerifyToken(context.Background(), secret); err != nil {
			t.Fatal(err)
		}
	}
	if tries != 1 {
		t.Errorf("tried to write %d times within the hour", tries)
	}
}

type tokenJSON struct {
	ID, Name, Owner, Token string
	TokenURL               string `json:"token_url"`
	Session                string `json:"session_id"`
	Scopes                 []string
	Created, Expires       time.Time
	LastUsed               *time.Time `json:"last_used"`
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	dec := json.NewDecoder(rec.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%d %v", rec.Code, err)
	}
	return v
}

func TestHandlersCreateListRevoke(t *testing.T) {
	f := newFixture(t)
	rec := f.as(user(alice), "POST", "/api/tokens", `{"name":" terraform ","scopes":["policies:write","sessions:read","sessions:read"]}`)
	if rec.Code != http.StatusCreated || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	created := decode[tokenJSON](t, rec)
	if !tokenForm.MatchString(created.Token) || created.Name != "terraform" || created.Owner != alice ||
		created.TokenURL != "https://api.example.com/oauth/token" || created.Session != "" ||
		fmt.Sprint(created.Scopes) != "[sessions:read policies:write]" ||
		!created.Expires.Equal(f.clock.Add(90*24*time.Hour)) || created.Created.IsZero() || created.LastUsed != nil {
		t.Fatalf("created %+v", created)
	}
	if owner, _, err := f.store.VerifyToken(context.Background(), created.Token); err != nil || owner != alice {
		t.Fatalf("the new token does not verify: %v", err)
	}

	f.clock = f.clock.Add(time.Minute)
	if rec := f.as(user(bob), "POST", "/api/tokens", `{"name":"bob's","scopes":["sessions:read"],"expires_in_days":365}`); rec.Code != http.StatusCreated {
		t.Fatalf("bob: %d %s", rec.Code, rec.Body)
	}

	// The list never has the token, and has only the caller's.
	rec = f.as(user(alice), "GET", "/api/tokens", "")
	list := decode[[]tokenJSON](t, rec)
	if rec.Code != http.StatusOK || len(list) != 1 || list[0].ID != created.ID || list[0].Token != "" {
		t.Fatalf("list: %d %+v", rec.Code, list)
	}
	if strings.Contains(rec.Body.String(), "sha256") || strings.Contains(rec.Body.String(), digest(created.Token)) {
		t.Error("the hash is in the list")
	}
	// ?all=1 is for admins.
	if list := decode[[]tokenJSON](t, f.as(user(alice), "GET", "/api/tokens?all=1", "")); len(list) != 1 {
		t.Errorf("alice saw %d tokens with all=1", len(list))
	}
	if list := decode[[]tokenJSON](t, f.as(user(root), "GET", "/api/tokens?all=1", "")); len(list) != 2 {
		t.Errorf("an admin saw %d tokens with all=1", len(list))
	}
	if list := decode[[]tokenJSON](t, f.as(user(root), "GET", "/api/tokens", "")); len(list) != 0 {
		t.Errorf("an admin's own list has %d tokens", len(list))
	}

	// Bob cannot revoke Alice's, and cannot tell that it exists.
	for _, id := range []string{created.ID, "aaaaaaaaaaaa", "not-an-id"} {
		if rec := f.as(user(bob), "DELETE", "/api/tokens/"+id, ""); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
			t.Errorf("bob revoking %s: %d %q", id, rec.Code, rec.Body)
		}
	}
	if _, _, err := f.store.VerifyToken(context.Background(), created.Token); err != nil {
		t.Fatalf("bob revoked alice's token: %v", err)
	}
	if rec := f.as(user(alice), "DELETE", "/api/tokens/"+created.ID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if _, _, err := f.store.VerifyToken(context.Background(), created.Token); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("after revocation: %v", err)
	}
	// An admin revokes anyone's.
	bobs, _ := f.store.List(context.Background(), bob)
	if rec := f.as(user(root), "DELETE", "/api/tokens/"+bobs[0].ID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("admin revoke: %d", rec.Code)
	}
	if left, _ := f.store.ListAll(context.Background()); len(left) != 0 {
		t.Errorf("%d tokens left", len(left))
	}
}

func TestHandlersRefuseAToken(t *testing.T) {
	f := newFixture(t)
	tok, _ := f.create(alice, auth.Scopes, time.Hour)
	withToken := auth.User{Subject: alice, Token: &auth.TokenInfo{Name: "ci", Scopes: auth.Scopes}}
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/tokens", ""},
		{"POST", "/api/tokens", `{"name":"successor","scopes":["sessions:read"]}`},
		{"DELETE", "/api/tokens/" + tok.ID, ""},
	} {
		if rec := f.as(withToken, c.method, c.path, c.body); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with a token: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if list, _ := f.store.List(context.Background(), alice); len(list) != 1 {
		t.Errorf("%d tokens after the refusals", len(list))
	}
	// And nobody at all.
	req := httptest.NewRequest("GET", "/api/tokens", nil)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no caller: %d", rec.Code)
	}
}

func TestHandlersValidate(t *testing.T) {
	f := newFixture(t)
	for name, body := range map[string]string{
		"no name":           `{"scopes":["sessions:read"]}`,
		"blank name":        `{"name":"   ","scopes":["sessions:read"]}`,
		"long name":         `{"name":"` + strings.Repeat("n", 65) + `","scopes":["sessions:read"]}`,
		"control character": `{"name":"a\nb","scopes":["sessions:read"]}`,
		"no scopes":         `{"name":"x"}`,
		"empty scopes":      `{"name":"x","scopes":[]}`,
		"unknown scope":     `{"name":"x","scopes":["sessions:read","admin"]}`,
		"token scope":       `{"name":"x","scopes":["tokens:write"]}`,
		"no expiry":         `{"name":"x","scopes":["sessions:read"],"expires_in_days":0}`,
		"negative expiry":   `{"name":"x","scopes":["sessions:read"],"expires_in_days":-1}`,
		"over a year":       `{"name":"x","scopes":["sessions:read"],"expires_in_days":366}`,
		"not a session":     `{"name":"x","scopes":["sessions:connect"],"session_id":"../s-abcdefghij"}`,
		"not JSON":          `name=x`,
		"empty":             ``,
	} {
		if rec := f.as(user(alice), "POST", "/api/tokens", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if list, _ := f.store.ListAll(context.Background()); len(list) != 0 {
		t.Errorf("%d tokens were made", len(list))
	}
	rec := f.as(user(alice), "POST", "/api/tokens", `{"name":"`+strings.Repeat("n", 64)+`","scopes":["sessions:write"],"expires_in_days":365}`)
	if created := decode[tokenJSON](t, rec); rec.Code != http.StatusCreated || !created.Expires.Equal(f.clock.Add(365*24*time.Hour)) {
		t.Errorf("a year: %d %+v", rec.Code, created)
	}
}

func TestHandlersOnlyTheAllowedMakeTokens(t *testing.T) {
	f := newFixture(t)
	rec := f.as(user("mallory@example.com"), "POST", "/api/tokens", `{"name":"x","scopes":["sessions:read"]}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("got %d %s", rec.Code, rec.Body)
	}
	if list, _ := f.store.ListAll(context.Background()); len(list) != 0 {
		t.Errorf("%d tokens were made", len(list))
	}
}

func TestHandlersLimitPerUser(t *testing.T) {
	f := newFixture(t)
	body := `{"name":"x","scopes":["sessions:read"],"expires_in_days":1}`
	for i := range MaxPerUser {
		if rec := f.as(user(alice), "POST", "/api/tokens", body); rec.Code != http.StatusCreated {
			t.Fatalf("token %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	if rec := f.as(user(alice), "POST", "/api/tokens", body); rec.Code != http.StatusConflict {
		t.Fatalf("token 21: %d %s", rec.Code, rec.Body)
	}
	// The limit is each user's own.
	if rec := f.as(user(bob), "POST", "/api/tokens", body); rec.Code != http.StatusCreated {
		t.Fatalf("bob: %d %s", rec.Code, rec.Body)
	}
	// Expired tokens do not count, and go when their owner makes another.
	f.clock = f.clock.Add(25 * time.Hour)
	if rec := f.as(user(alice), "POST", "/api/tokens", body); rec.Code != http.StatusCreated {
		t.Fatalf("after the others expired: %d %s", rec.Code, rec.Body)
	}
	if list, _ := f.store.List(context.Background(), alice); len(list) != 1 {
		t.Errorf("alice has %d tokens", len(list))
	}
	if list, _ := f.store.List(context.Background(), bob); len(list) != 1 {
		t.Errorf("bob has %d tokens", len(list))
	}
}

// Nothing the package logs may hold a token.
func TestNoTokenIsLogged(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)

	f := newFixture(t)
	rec := f.as(user(alice), "POST", "/api/tokens", `{"name":"x","scopes":["sessions:read"]}`)
	created := decode[tokenJSON](t, rec)
	f.client.PrependReactor("patch", "apitokens", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	_, _, _ = f.store.VerifyToken(context.Background(), created.Token)
	_, _, _ = f.store.VerifyToken(context.Background(), created.Token[:len(created.Token)-1]+"!")
	f.as(user(alice), "DELETE", "/api/tokens/"+created.ID, "")
	_, _, _ = f.store.VerifyToken(context.Background(), created.Token)

	secret := created.Token[len("bjs_")+idLen+1:]
	if logs.Len() == 0 {
		t.Fatal("nothing was logged; the test checks nothing")
	}
	if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), digest(created.Token)) {
		t.Errorf("a token or its hash is in the logs:\n%s", logs.String())
	}
}

func (f *fixture) enableExchange(key string) {
	f.t.Helper()
	signer, err := NewSigner([]byte(key), "https://api.example.com")
	if err != nil {
		f.t.Fatal(err)
	}
	f.store.EnableExchange(signer)
}

const testKey = "0123456789abcdef0123456789abcdef"

func TestSignerKey(t *testing.T) {
	if _, err := NewSigner([]byte("short"), "https://api.example.com"); err == nil {
		t.Error("a short key was accepted")
	}
	if _, err := NewSigner(nil, ""); err == nil {
		t.Error("no issuer was accepted")
	}
	a, err := NewSigner(nil, "https://api.example.com")
	b, _ := NewSigner(nil, "https://api.example.com")
	if err != nil || len(a.key) != 32 || bytes.Equal(a.key, b.key) {
		t.Errorf("a made-up key: %v", err)
	}
}

func TestExchangeAndAccessToken(t *testing.T) {
	f := newFixture(t)
	f.enableExchange(testKey)
	ctx := context.Background()
	tok, secret, err := f.store.Create(ctx, alice, "ci", []string{auth.ScopeSessionsRead, auth.ScopeSessionsConnect}, "s-abcdefghij", 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	owner, grant, err := f.store.ExchangeToken(ctx, tok.ID, secret, nil)
	if err != nil || owner != alice || grant.ExpiresIn != time.Hour || fmt.Sprint(grant.Scopes) != "[sessions:read sessions:connect]" {
		t.Fatalf("exchange: %q %+v %v", owner, grant, err)
	}
	if strings.Contains(grant.AccessToken, secret[17:]) || strings.Contains(grant.AccessToken, digest(secret)) {
		t.Fatal("the access token holds the API token")
	}
	owner, info, err := f.store.VerifyToken(ctx, grant.AccessToken)
	if err != nil || owner != alice || info.Name != "ci" || info.Session != "s-abcdefghij" ||
		!info.Has(auth.ScopeSessionsConnect) || info.Has(auth.ScopeSessionsWrite) {
		t.Fatalf("the access token: %q %+v %v", owner, info, err)
	}

	// Narrowed.
	_, narrow, err := f.store.ExchangeToken(ctx, tok.ID, secret, []string{auth.ScopeSessionsRead, auth.ScopeSessionsRead})
	if err != nil || fmt.Sprint(narrow.Scopes) != "[sessions:read]" {
		t.Fatalf("narrowed: %+v %v", narrow, err)
	}
	if _, info, _ := f.store.VerifyToken(ctx, narrow.AccessToken); info.Has(auth.ScopeSessionsConnect) || !info.Has(auth.ScopeSessionsRead) {
		t.Errorf("narrowed access token has %v", info.Scopes)
	}
	if _, _, err := f.store.ExchangeToken(ctx, tok.ID, secret, []string{auth.ScopePoliciesWrite}); !errors.Is(err, auth.ErrInvalidScope) {
		t.Errorf("a wider scope: %v", err)
	}

	// The client must be the token's own id, and the secret the token.
	other, otherSecret := f.create(bob, auth.Scopes, time.Hour)
	for name, c := range map[string][2]string{
		"another's id":     {other.ID, secret},
		"another's secret": {tok.ID, otherSecret},
		"an access token":  {tok.ID, grant.AccessToken},
		"the hash":         {tok.ID, digest(secret)},
		"unknown":          {"aaaaaaaaaaaa", "bjs_aaaaaaaaaaaa_" + strings.Repeat("a", 43)},
		"empty":            {"", ""},
	} {
		if _, _, err := f.store.ExchangeToken(ctx, c[0], c[1], nil); !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// It lasts an hour, to the second.
	f.clock = f.clock.Add(time.Hour - time.Second)
	if _, _, err := f.store.VerifyToken(ctx, grant.AccessToken); err != nil {
		t.Errorf("a second before the hour: %v", err)
	}
	f.clock = f.clock.Add(2 * time.Second)
	if _, _, err := f.store.VerifyToken(ctx, grant.AccessToken); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("after the hour: %v", err)
	}
	// And never longer than the API token.
	short, shortSecret := f.create(alice, auth.Scopes, 10*time.Minute)
	if _, g, err := f.store.ExchangeToken(ctx, short.ID, shortSecret, nil); err != nil || g.ExpiresIn != 10*time.Minute {
		t.Errorf("an access token of a token that expires in ten minutes: %v %v", g.ExpiresIn, err)
	}
}

func TestAccessTokenForgeries(t *testing.T) {
	f := newFixture(t)
	f.enableExchange(testKey)
	ctx := context.Background()
	tok, secret := f.create(alice, auth.Scopes, 24*time.Hour)
	_, grant, err := f.store.ExchangeToken(ctx, tok.ID, secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	sign := func(method jwt.SigningMethod, key any, change func(*accessClaims)) string {
		c := accessClaims{
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer: "https://api.example.com", Audience: jwt.ClaimStrings{"https://api.example.com"}, Subject: alice,
				IssuedAt: jwt.NewNumericDate(f.clock), ExpiresAt: jwt.NewNumericDate(f.clock.Add(time.Hour)),
			},
			ClientID: tok.ID, Scope: strings.Join(auth.Scopes, " "),
		}
		if change != nil {
			change(&c)
		}
		raw, err := jwt.NewWithClaims(method, c).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	if _, _, err := f.store.VerifyToken(ctx, sign(jwt.SigningMethodHS256, []byte(testKey), nil)); err != nil {
		t.Fatalf("the test's own signing is off: %v", err)
	}
	parts := strings.Split(grant.AccessToken, ".")
	for name, forged := range map[string]string{
		"another key":      sign(jwt.SigningMethodHS256, []byte("another key of thirty-two bytes!!"), nil),
		"no signature":     sign(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, nil),
		"signature cut":    parts[0] + "." + parts[1] + ".",
		"HS384":            sign(jwt.SigningMethodHS384, []byte(testKey), nil),
		"another audience": sign(jwt.SigningMethodHS256, []byte(testKey), func(c *accessClaims) { c.Audience = jwt.ClaimStrings{"https://sessions.example.com"} }),
		"another issuer":   sign(jwt.SigningMethodHS256, []byte(testKey), func(c *accessClaims) { c.Issuer = "https://app.example.com" }),
		"no expiry":        sign(jwt.SigningMethodHS256, []byte(testKey), func(c *accessClaims) { c.ExpiresAt = nil }),
		"expired":          sign(jwt.SigningMethodHS256, []byte(testKey), func(c *accessClaims) { c.ExpiresAt = jwt.NewNumericDate(f.clock.Add(-time.Second)) }),
		"another owner":    sign(jwt.SigningMethodHS256, []byte(testKey), func(c *accessClaims) { c.Subject = bob }),
		"no client":        sign(jwt.SigningMethodHS256, []byte(testKey), func(c *accessClaims) { c.ClientID = "" }),
		"unknown client":   sign(jwt.SigningMethodHS256, []byte(testKey), func(c *accessClaims) { c.ClientID = "aaaaaaaaaaaa" }),
		"not a JWT":        "nonsense",
	} {
		if _, _, err := f.store.VerifyToken(ctx, forged); !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Scopes the API token never had are not to be had by claiming them.
	limited, _ := f.create(alice, []string{auth.ScopeSessionsRead}, time.Hour)
	wide := sign(jwt.SigningMethodHS256, []byte(testKey), func(c *accessClaims) { c.ClientID = limited.ID })
	if _, info, err := f.store.VerifyToken(ctx, wide); err != nil || fmt.Sprint(info.Scopes) != "[sessions:read]" {
		t.Errorf("claimed scopes: %v %v", info.Scopes, err)
	}

	// A restart with another key, or with none configured, ends them.
	f.enableExchange("ANOTHER-KEY-0123456789abcdef0123")
	if _, _, err := f.store.VerifyToken(ctx, grant.AccessToken); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("after the key changed: %v", err)
	}
	f.enableExchange(testKey)
	// Revoking the API token ends its access tokens.
	if err := f.store.Delete(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.VerifyToken(ctx, grant.AccessToken); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("after revocation: %v", err)
	}
	// Without a signer there is no exchange and no access token.
	f.store.signer = nil
	if _, _, err := f.store.VerifyToken(ctx, grant.AccessToken); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("no signer: %v", err)
	}
}

func TestSessionBoundToken(t *testing.T) {
	f := newFixture(t)
	rec := f.as(user(alice), "POST", "/api/tokens", `{"name":"agent","scopes":["sessions:connect"],"session_id":"s-abcdefghij"}`)
	created := decode[tokenJSON](t, rec)
	if rec.Code != http.StatusCreated || created.Session != "s-abcdefghij" {
		t.Fatalf("create: %d %+v", rec.Code, created)
	}
	if spec := f.object(created.ID).Object["spec"].(map[string]any); spec["session"] != "s-abcdefghij" {
		t.Errorf("stored %v", spec)
	}
	_, info, err := f.store.VerifyToken(context.Background(), created.Token)
	if err != nil || info.Session != "s-abcdefghij" {
		t.Errorf("verified %+v %v", info, err)
	}
	if list := decode[[]tokenJSON](t, f.as(user(alice), "GET", "/api/tokens", "")); len(list) != 1 || list[0].Session != "s-abcdefghij" {
		t.Errorf("list %+v", list)
	}
}
