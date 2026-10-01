package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const issuer = "https://kc.example.com/realms/browserjs"

func sign(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newVerifier(t *testing.T) (*JWTVerifier, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	kf := func(*jwt.Token) (any, error) { return &key.PublicKey, nil }
	v, err := NewJWTVerifier(kf, issuer, "admin", []string{"browserjs-spa", "claude-connector"})
	if err != nil {
		t.Fatal(err)
	}
	return v, key
}

func TestVerify(t *testing.T) {
	v, key := newVerifier(t)
	base := func() jwt.MapClaims {
		return jwt.MapClaims{
			"iss": issuer, "sub": "user-1", "preferred_username": "robert",
			"azp": "browserjs-spa", "typ": "Bearer",
			"exp":          time.Now().Add(time.Hour).Unix(),
			"realm_access": map[string]any{"roles": []any{"offline_access"}},
		}
	}

	u, err := v.Verify(t.Context(), sign(t, key, base()))
	if err != nil {
		t.Fatal(err)
	}
	if u.Subject != "user-1" || u.Username != "robert" || u.Admin {
		t.Errorf("unexpected user: %+v", u)
	}

	admin := base()
	admin["realm_access"] = map[string]any{"roles": []any{"admin"}}
	if u, err := v.Verify(t.Context(), sign(t, key, admin)); err != nil || !u.Admin {
		t.Errorf("admin role not recognised: %+v, %v", u, err)
	}

	// The other allowed client, and a token with no typ claim at all.
	connector := base()
	connector["azp"] = "claude-connector"
	delete(connector, "typ")
	if _, err := v.Verify(t.Context(), sign(t, key, connector)); err != nil {
		t.Errorf("token for an allowed client rejected: %v", err)
	}

	for name, mutate := range map[string]func(jwt.MapClaims){
		"wrong issuer": func(c jwt.MapClaims) { c["iss"] = "https://evil.example" },
		"expired":      func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Minute).Unix() },
		"no subject":   func(c jwt.MapClaims) { delete(c, "sub") },
		"no expiry":    func(c jwt.MapClaims) { delete(c, "exp") },
		"wrong client": func(c jwt.MapClaims) { c["azp"] = "some-other-app" },
		"no client":    func(c jwt.MapClaims) { delete(c, "azp") },
		"ID token":     func(c jwt.MapClaims) { c["typ"] = "ID" },
		"refresh":      func(c jwt.MapClaims) { c["typ"] = "Refresh" },
	} {
		c := base()
		mutate(c)
		if _, err := v.Verify(t.Context(), sign(t, key, c)); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}

	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	if _, err := v.Verify(t.Context(), sign(t, other, base())); err == nil {
		t.Error("token signed by another key was accepted")
	}

	// Only asymmetric algorithms: an HS256 token "signed" with public material
	// must not verify.
	hs, err := jwt.NewWithClaims(jwt.SigningMethodHS256, base()).SignedString([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	hsVerifier, _ := NewJWTVerifier(func(*jwt.Token) (any, error) { return []byte("secret"), nil },
		issuer, "admin", []string{"browserjs-spa"})
	if _, err := hsVerifier.Verify(t.Context(), hs); err == nil {
		t.Error("HS256 token was accepted")
	}
}

// A verifier that would silently skip a check must not be constructible.
func TestNewVerifierRejectsIncompleteSettings(t *testing.T) {
	kf := func(*jwt.Token) (any, error) { return nil, nil }
	clients := []string{"browserjs-spa"}
	for name, build := range map[string]func() (*JWTVerifier, error){
		"empty issuer":     func() (*JWTVerifier, error) { return NewJWTVerifier(kf, "", "admin", clients) },
		"empty admin role": func() (*JWTVerifier, error) { return NewJWTVerifier(kf, issuer, "", clients) },
		"no clients":       func() (*JWTVerifier, error) { return NewJWTVerifier(kf, issuer, "admin", nil) },
		"blank client":     func() (*JWTVerifier, error) { return NewJWTVerifier(kf, issuer, "admin", []string{""}) },
		"jwks, empty issuer": func() (*JWTVerifier, error) {
			return NewJWKSVerifier(t.Context(), "http://127.0.0.1:1/certs", "", "admin", clients)
		},
	} {
		if v, err := build(); err == nil || v != nil {
			t.Errorf("%s: got %v, %v; want an error", name, v, err)
		}
	}
}

func TestBearerToken(t *testing.T) {
	for header, want := range map[string]string{
		"Bearer abc": "abc", "bearer abc": "abc", "Bearer   abc ": "abc",
		"Basic abc": "", "Bearer ": "", "": "",
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", header)
		if got := BearerToken(r); got != want {
			t.Errorf("BearerToken(%q) = %q, want %q", header, got, want)
		}
	}
}

func TestMiddleware(t *testing.T) {
	v, key := newVerifier(t)
	var seen User
	h := Middleware(v)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = UserFrom(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/sessions", nil))
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("no token: got %d (WWW-Authenticate %q), want 401", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	for _, header := range []string{"Bearer not-a-jwt", "Basic dXNlcjpwYXNz"} {
		req := httptest.NewRequest("GET", "/api/sessions", nil)
		req.Header.Set("Authorization", header)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%q: got %d, want 401", header, rec.Code)
		}
	}

	req := httptest.NewRequest("GET", "/api/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+sign(t, key, jwt.MapClaims{
		"iss": issuer, "sub": "user-1", "exp": time.Now().Add(time.Hour).Unix(), "azp": "browserjs-spa",
	}))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || seen.Subject != "user-1" {
		t.Errorf("valid token: code %d, user %+v", rec.Code, seen)
	}
}
