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
	return NewJWTVerifier(kf, issuer, "admin"), key
}

func TestVerify(t *testing.T) {
	v, key := newVerifier(t)
	base := func() jwt.MapClaims {
		return jwt.MapClaims{
			"iss": issuer, "sub": "user-1", "preferred_username": "robert",
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
	if u, _ := v.Verify(t.Context(), sign(t, key, admin)); !u.Admin {
		t.Error("admin role not recognised")
	}

	for name, mutate := range map[string]func(jwt.MapClaims){
		"wrong issuer": func(c jwt.MapClaims) { c["iss"] = "https://evil.example" },
		"expired":      func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Minute).Unix() },
		"no subject":   func(c jwt.MapClaims) { delete(c, "sub") },
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
}

func TestMiddleware(t *testing.T) {
	v, key := newVerifier(t)
	var seen User
	h := Middleware(v)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = UserFrom(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/sessions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", rec.Code)
	}

	req := httptest.NewRequest("GET", "/api/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+sign(t, key, jwt.MapClaims{
		"iss": issuer, "sub": "user-1", "exp": time.Now().Add(time.Hour).Unix(),
	}))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || seen.Subject != "user-1" {
		t.Errorf("valid token: code %d, user %+v", rec.Code, seen)
	}
}
