// Package auth verifies Keycloak access tokens and carries the caller's
// identity on the request context.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

type User struct {
	Subject  string // Keycloak `sub`; the stable user ID everywhere else
	Username string
	Admin    bool
}

type Verifier interface {
	Verify(ctx context.Context, rawToken string) (User, error)
}

type JWTVerifier struct {
	keyfunc   jwt.Keyfunc
	issuer    string
	adminRole string
	clients   map[string]bool
}

// settings checks what every verifier needs. Each of these, left empty,
// would quietly switch a check off rather than fail.
func settings(issuer, adminRole string, allowedClients []string) (map[string]bool, error) {
	if issuer == "" {
		return nil, errors.New("auth: issuer is required")
	}
	if adminRole == "" {
		return nil, errors.New("auth: admin role is required")
	}
	clients := map[string]bool{}
	for _, c := range allowedClients {
		if c == "" {
			return nil, errors.New("auth: allowed client names must not be empty")
		}
		clients[c] = true
	}
	if len(clients) == 0 {
		return nil, errors.New("auth: at least one allowed client is required")
	}
	return clients, nil
}

// NewJWTVerifier accepts access tokens from issuer that were issued to one
// of allowedClients (the token's `azp`).
func NewJWTVerifier(kf jwt.Keyfunc, issuer, adminRole string, allowedClients []string) (*JWTVerifier, error) {
	clients, err := settings(issuer, adminRole, allowedClients)
	if err != nil {
		return nil, err
	}
	return &JWTVerifier{keyfunc: kf, issuer: issuer, adminRole: adminRole, clients: clients}, nil
}

// NewJWKSVerifier fetches (and keeps refreshing) signing keys from jwksURL.
func NewJWKSVerifier(ctx context.Context, jwksURL, issuer, adminRole string, allowedClients []string) (*JWTVerifier, error) {
	if _, err := settings(issuer, adminRole, allowedClients); err != nil {
		return nil, err
	}
	k, err := keyfunc.NewDefaultCtx(ctx, []string{jwksURL})
	if err != nil {
		return nil, err
	}
	return NewJWTVerifier(k.Keyfunc, issuer, adminRole, allowedClients)
}

type claims struct {
	jwt.RegisteredClaims
	Username        string `json:"preferred_username"`
	AuthorizedParty string `json:"azp"` // the client the token was issued to
	Type            string `json:"typ"` // Keycloak: Bearer, ID, Refresh, ...
	RealmAccess     struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

func (v *JWTVerifier) Verify(_ context.Context, raw string) (User, error) {
	var c claims
	_, err := jwt.ParseWithClaims(raw, &c, v.keyfunc,
		jwt.WithIssuer(v.issuer),
		jwt.WithExpirationRequired(),
		jwt.WithValidMethods([]string{"RS256", "ES256"}),
	)
	if err != nil {
		return User{}, err
	}
	if c.Subject == "" {
		return User{}, errors.New("token has no subject")
	}
	// Any client in the realm can obtain a validly signed token for a user;
	// only tokens issued to our own clients may act on their sessions.
	if !v.clients[c.AuthorizedParty] {
		return User{}, fmt.Errorf("token was issued to client %q, which is not allowed", c.AuthorizedParty)
	}
	if c.Type != "" && c.Type != "Bearer" {
		return User{}, fmt.Errorf("token type %q is not an access token", c.Type)
	}
	u := User{Subject: c.Subject, Username: c.Username}
	for _, r := range c.RealmAccess.Roles {
		if r == v.adminRole {
			u.Admin = true
		}
	}
	return u, nil
}

type ctxKey struct{}

func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

func UserFrom(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(ctxKey{}).(User)
	return u, ok
}

// BearerToken extracts the token from an Authorization header.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// Middleware rejects requests without a valid bearer token.
func Middleware(v Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := BearerToken(r)
			if raw == "" {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "missing bearer token", http.StatusUnauthorized)
				return
			}
			u, err := v.Verify(r.Context(), raw)
			if err != nil {
				// The caller only learns "invalid"; the reason (expired, wrong
				// client, keys unavailable) is for whoever runs the server.
				slog.Debug("token rejected", "err", err)
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), u)))
		})
	}
}
