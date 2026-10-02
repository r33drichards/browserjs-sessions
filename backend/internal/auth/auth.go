// Package auth establishes who is calling. Every request reaches the backend
// through Pomerium, which signs in the user and states who they are in a
// signed header; this package verifies that statement and carries the
// caller's identity on the request context.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"

	"github.com/r33drichards/browserjs-sessions/backend/internal/hosts"
)

// AssertionHeader carries Pomerium's signed statement of who the user is: a
// JWT minted for each request, valid for a few minutes, whose audience is
// the host the request was made to.
const AssertionHeader = "X-Pomerium-Jwt-Assertion"

// How far the backend's clock and Pomerium's may disagree.
const clockSkew = 30 * time.Second

type User struct {
	// Subject is the user's email address, in lower case. It is the user ID
	// everywhere else: sessions are owned by it, admins are listed by it.
	Subject string
	Name    string
	Admin   bool

	// Nil when the caller signed in through Pomerium (the UI).
	Token *TokenInfo
}

// TokenInfo is the API token a request was made with.
type TokenInfo struct {
	Name   string   // what its owner called it
	Scopes []string // "sessions:read", "sessions:write", "policies:read", "policies:write"
}

type Verifier interface {
	// Verify checks an assertion that arrived on a request to host (the
	// request's Host header).
	Verify(ctx context.Context, assertion, host string) (User, error)
}

type AssertionVerifier struct {
	keyfunc jwt.Keyfunc
	admins  map[string]bool
}

func normalEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// NewAssertionVerifier accepts assertions signed by a key kf returns. The
// users in adminEmails (compared without regard to case) are admins.
func NewAssertionVerifier(kf jwt.Keyfunc, adminEmails []string) (*AssertionVerifier, error) {
	if kf == nil {
		return nil, errors.New("auth: a key function is required")
	}
	admins := map[string]bool{}
	for _, email := range adminEmails {
		if email = normalEmail(email); email != "" {
			admins[email] = true
		}
	}
	return &AssertionVerifier{keyfunc: kf, admins: admins}, nil
}

// NewJWKSVerifier fetches (and keeps refreshing) Pomerium's signing keys
// from jwksURL.
func NewJWKSVerifier(ctx context.Context, jwksURL string, adminEmails []string) (*AssertionVerifier, error) {
	if jwksURL == "" {
		return nil, errors.New("auth: a JWKS URL is required")
	}
	k, err := keyfunc.NewDefaultCtx(ctx, []string{jwksURL})
	if err != nil {
		return nil, err
	}
	return NewAssertionVerifier(k.Keyfunc, adminEmails)
}

type claims struct {
	jwt.RegisteredClaims
	Email string `json:"email"`
	Name  string `json:"name"`
}

// forHost reports whether audience names the host a request was made to.
// Pomerium writes the request's host name there; an audience and a Host that
// both carry a port must agree on it too.
func forHost(audience jwt.ClaimStrings, host string) bool {
	name, port := hosts.Split(host)
	for _, aud := range audience {
		audName, audPort := hosts.Split(aud)
		if audName == name && (audPort == port || audPort == "" || port == "") {
			return true
		}
	}
	return false
}

func (v *AssertionVerifier) Verify(_ context.Context, raw, host string) (User, error) {
	if name, _ := hosts.Split(host); name == "" {
		return User{}, errors.New("request has no host")
	}
	var c claims
	_, err := jwt.ParseWithClaims(raw, &c, v.keyfunc,
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(clockSkew),
		jwt.WithValidMethods([]string{"ES256", "RS256"}),
	)
	if err != nil {
		return User{}, err
	}
	// The same key signs the assertions for every host behind Pomerium, so
	// one made for another host (another session's, say) must not pass here.
	if !forHost(c.Audience, host) {
		return User{}, fmt.Errorf("assertion is for %q, not for %q", []string(c.Audience), host)
	}
	email := normalEmail(c.Email)
	if email == "" {
		return User{}, errors.New("assertion has no email")
	}
	return User{Subject: email, Name: c.Name, Admin: v.admins[email]}, nil
}

type ctxKey struct{}

func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

func UserFrom(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(ctxKey{}).(User)
	return u, ok
}

// ErrNoAssertion is Authenticate's answer for a request that carries no
// assertion: it did not come through Pomerium, or came by a public route.
var ErrNoAssertion = errors.New("request carries no " + AssertionHeader)

// Authenticate establishes who made a request.
func Authenticate(v Verifier, r *http.Request) (User, error) {
	raw := r.Header.Get(AssertionHeader)
	if raw == "" {
		return User{}, ErrNoAssertion
	}
	return v.Verify(r.Context(), raw, r.Host)
}

// Middleware rejects requests that do not carry a valid assertion.
func Middleware(v Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// A request the API host let in (apihost.go) already has its
			// caller, from its token. Only the server puts one there.
			if u, ok := UserFrom(r.Context()); ok && u.Token != nil {
				next.ServeHTTP(w, r)
				return
			}
			u, err := Authenticate(v, r)
			if err != nil {
				// The caller only learns "not signed in"; the reason (expired,
				// another host's, keys unavailable) is for whoever runs the server.
				slog.Debug("assertion rejected", "err", err)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "not signed in"})
				return
			}
			next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), u)))
		})
	}
}
