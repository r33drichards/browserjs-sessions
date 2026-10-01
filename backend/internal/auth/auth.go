// Package auth verifies Keycloak access tokens and carries the caller's
// identity on the request context.
package auth

import (
	"context"
	"errors"
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
}

func NewJWTVerifier(kf jwt.Keyfunc, issuer, adminRole string) *JWTVerifier {
	return &JWTVerifier{keyfunc: kf, issuer: issuer, adminRole: adminRole}
}

// NewJWKSVerifier fetches (and keeps refreshing) signing keys from jwksURL.
func NewJWKSVerifier(ctx context.Context, jwksURL, issuer, adminRole string) (*JWTVerifier, error) {
	k, err := keyfunc.NewDefaultCtx(ctx, []string{jwksURL})
	if err != nil {
		return nil, err
	}
	return NewJWTVerifier(k.Keyfunc, issuer, adminRole), nil
}

type claims struct {
	jwt.RegisteredClaims
	Username    string `json:"preferred_username"`
	RealmAccess struct {
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
		return h[7:]
	}
	return ""
}

// Middleware rejects requests without a valid bearer token.
func Middleware(v Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := BearerToken(r)
			if raw == "" {
				http.Error(w, "missing bearer token", http.StatusUnauthorized)
				return
			}
			u, err := v.Verify(r.Context(), raw)
			if err != nil {
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), u)))
		})
	}
}
