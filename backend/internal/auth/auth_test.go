package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const host = "app.example.com"

func sign(t *testing.T, key *ecdsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodES256, claims).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newVerifier(t *testing.T, admins ...string) (*AssertionVerifier, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewAssertionVerifier(func(*jwt.Token) (any, error) { return &key.PublicKey, nil }, admins)
	if err != nil {
		t.Fatal(err)
	}
	return v, key
}

// assertion is what Pomerium puts in X-Pomerium-Jwt-Assertion for a request
// to host.
func assertion() jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss": host, "aud": host, "jti": "j-1", "sid": "sid-1",
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"sub": "google-oauth2|1234", "user": "google-oauth2|1234",
		"email": "alice@example.com", "name": "Alice Example", "groups": []any{},
	}
}

func TestVerify(t *testing.T) {
	v, key := newVerifier(t, "root@example.com")

	u, err := v.Verify(t.Context(), sign(t, key, assertion()), host)
	if err != nil {
		t.Fatal(err)
	}
	if u.Subject != "alice@example.com" || u.Name != "Alice Example" || u.Admin {
		t.Errorf("unexpected user: %+v", u)
	}

	for name, mutate := range map[string]func(jwt.MapClaims){
		"wrong audience":      func(c jwt.MapClaims) { c["aud"] = "s-abcdefg234.sessions.example.com" },
		"parent of audience":  func(c jwt.MapClaims) { c["aud"] = "example.com" },
		"no audience":         func(c jwt.MapClaims) { delete(c, "aud") },
		"empty audience":      func(c jwt.MapClaims) { c["aud"] = "" },
		"expired":             func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-5 * time.Minute).Unix() },
		"no expiry":           func(c jwt.MapClaims) { delete(c, "exp") },
		"not valid yet":       func(c jwt.MapClaims) { c["nbf"] = time.Now().Add(time.Hour).Unix() },
		"no email":            func(c jwt.MapClaims) { delete(c, "email") },
		"empty email":         func(c jwt.MapClaims) { c["email"] = "" },
		"blank email":         func(c jwt.MapClaims) { c["email"] = "  " },
		"email not a string":  func(c jwt.MapClaims) { c["email"] = []any{"alice@example.com"} },
		"another port's host": func(c jwt.MapClaims) { c["aud"] = host + ":8443" },
	} {
		c := assertion()
		mutate(c)
		requestHost := host
		if name == "another port's host" {
			requestHost = host + ":443"
		}
		if u, err := v.Verify(t.Context(), sign(t, key, c), requestHost); err == nil {
			t.Errorf("%s: accepted as %+v", name, u)
		}
	}

	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if _, err := v.Verify(t.Context(), sign(t, other, assertion()), host); err == nil {
		t.Error("an assertion signed by another key was accepted")
	}
	if _, err := v.Verify(t.Context(), "not-a-jwt", host); err == nil {
		t.Error("something that is not a JWT was accepted")
	}
	if _, err := v.Verify(t.Context(), sign(t, key, assertion()), ""); err == nil {
		t.Error("an assertion was accepted for a request with no host")
	}
}

// An assertion that is not signed, or "signed" with a shared secret, is not
// Pomerium's, whatever the key function would hand out for it.
func TestVerifyRequiresAnAsymmetricSignature(t *testing.T) {
	unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, assertion()).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	hs, err := jwt.NewWithClaims(jwt.SigningMethodHS256, assertion()).SignedString([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		raw string
		key any
	}{
		"unsigned": {unsigned, jwt.UnsafeAllowNoneSignatureType},
		"HS256":    {hs, []byte("secret")},
	} {
		v, err := NewAssertionVerifier(func(*jwt.Token) (any, error) { return c.key, nil }, nil)
		if err != nil {
			t.Fatal(err)
		}
		if u, err := v.Verify(t.Context(), c.raw, host); err == nil {
			t.Errorf("%s assertion accepted as %+v", name, u)
		}
	}
}

// The audience is the host the request was made to. How Pomerium writes it
// when that host has a port is not something the documentation settles, so
// both spellings are taken, but never another host or another port.
func TestVerifyAudienceIsTheRequestHost(t *testing.T) {
	v, key := newVerifier(t)
	for _, c := range []struct {
		aud  any
		host string
		ok   bool
	}{
		{"app.example.com", "app.example.com", true},
		{"app.example.com", "App.Example.com", true},
		{"APP.example.com", "app.example.com", true},
		{"app.example.com", "app.example.com:443", true},
		{"app.example.com:8080", "app.example.com:8080", true},
		{"app.example.com:8080", "app.example.com", true},
		{"app.example.com", "app.example.com.", true},
		{[]any{"app.example.com"}, "app.example.com", true},
		{[]any{"other.example.com", "app.example.com"}, "app.example.com", true},
		{"app.example.com:8080", "app.example.com:9090", false},
		{"app.example.com", "s-abcdefg234.sessions.example.com", false},
		{"s-abcdefg234.sessions.example.com", "s-abcdefg235.sessions.example.com", false},
		{"https://app.example.com/", "app.example.com", false},
		{[]any{}, "app.example.com", false},
	} {
		claims := assertion()
		claims["aud"] = c.aud
		_, err := v.Verify(t.Context(), sign(t, key, claims), c.host)
		if (err == nil) != c.ok {
			t.Errorf("aud %v on a request to %q: err = %v, want accepted = %v", c.aud, c.host, err, c.ok)
		}
	}
}

func TestVerifyEmailAndAdmins(t *testing.T) {
	v, key := newVerifier(t, "Root@Example.com", " ops@example.com ")
	for email, want := range map[string]User{
		"alice@example.com":  {Subject: "alice@example.com", Name: "Alice Example"},
		"Alice@Example.COM":  {Subject: "alice@example.com", Name: "Alice Example"},
		"root@example.com":   {Subject: "root@example.com", Name: "Alice Example", Admin: true},
		"ROOT@EXAMPLE.COM":   {Subject: "root@example.com", Name: "Alice Example", Admin: true},
		"ops@example.com":    {Subject: "ops@example.com", Name: "Alice Example", Admin: true},
		"root@example.com.x": {Subject: "root@example.com.x", Name: "Alice Example"},
	} {
		c := assertion()
		c["email"] = email
		u, err := v.Verify(t.Context(), sign(t, key, c), host)
		if err != nil || u != want {
			t.Errorf("%s: got %+v, %v; want %+v", email, u, err, want)
		}
	}

	// The name is optional.
	c := assertion()
	delete(c, "name")
	if u, err := v.Verify(t.Context(), sign(t, key, c), host); err != nil || u.Name != "" {
		t.Errorf("no name: %+v, %v", u, err)
	}
}

// A verifier that would silently skip a check must not be constructible.
func TestNewVerifierRejectsIncompleteSettings(t *testing.T) {
	if v, err := NewAssertionVerifier(nil, nil); err == nil || v != nil {
		t.Errorf("no key function: got %v, %v; want an error", v, err)
	}
	if v, err := NewJWKSVerifier(t.Context(), "", nil); err == nil || v != nil {
		t.Errorf("no JWKS URL: got %v, %v; want an error", v, err)
	}
}

func TestAuthenticate(t *testing.T) {
	v, key := newVerifier(t)
	req := httptest.NewRequest("GET", "https://"+host+"/api/me", nil)
	if _, err := Authenticate(v, req); !errors.Is(err, ErrNoAssertion) {
		t.Errorf("no header: err = %v, want ErrNoAssertion", err)
	}
	req.Header.Set(AssertionHeader, sign(t, key, assertion()))
	if u, err := Authenticate(v, req); err != nil || u.Subject != "alice@example.com" {
		t.Errorf("valid assertion: %+v, %v", u, err)
	}
	// The same assertion, replayed at another host.
	req.Host = "s-abcdefg234.sessions.example.com"
	if u, err := Authenticate(v, req); err == nil {
		t.Errorf("an assertion for %s was accepted at %s as %+v", host, req.Host, u)
	}
	// A bearer token is not an identity here.
	req = httptest.NewRequest("GET", "https://"+host+"/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+sign(t, key, assertion()))
	if _, err := Authenticate(v, req); !errors.Is(err, ErrNoAssertion) {
		t.Errorf("bearer token: err = %v, want ErrNoAssertion", err)
	}
}

func TestMiddleware(t *testing.T) {
	v, key := newVerifier(t, "alice@example.com")
	var seen User
	calls := 0
	h := Middleware(v)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		seen, _ = UserFrom(r.Context())
	}))
	do := func(assertion string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "https://"+host+"/api/sessions", nil)
		if assertion != "" {
			req.Header.Set(AssertionHeader, assertion)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	expired := assertion()
	expired["exp"] = time.Now().Add(-5 * time.Minute).Unix()
	elsewhere := assertion()
	elsewhere["aud"] = "other.example.com"
	for name, raw := range map[string]string{
		"no assertion": "", "not a JWT": "not-a-jwt",
		"expired": sign(t, key, expired), "another host's": sign(t, key, elsewhere),
	} {
		rec := do(raw)
		var body struct{ Error string }
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("Content-Type") != "application/json" ||
			json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Error == "" {
			t.Errorf("%s: %d %q (%s), want 401 with a JSON error", name, rec.Code, rec.Body, rec.Header().Get("Content-Type"))
		}
	}
	if calls != 0 {
		t.Fatalf("the handler ran %d times for requests with no valid assertion", calls)
	}

	if rec := do(sign(t, key, assertion())); rec.Code != http.StatusOK || seen.Subject != "alice@example.com" || !seen.Admin {
		t.Errorf("valid assertion: code %d, user %+v", rec.Code, seen)
	}
}
