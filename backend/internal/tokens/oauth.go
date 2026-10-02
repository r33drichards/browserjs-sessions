package tokens

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/r33drichards/computer-use/backend/internal/auth"
)

// AccessTokenLife is how long an access token lasts. The API token it came
// from is checked on every request all the same, so revoking that ends its
// access tokens too; the hour bounds what a leaked access token is worth.
const AccessTokenLife = time.Hour

// MinSigningKeyBytes is the least a signing key may be: HS256's block.
const MinSigningKeyBytes = 32

// Signer makes and checks access tokens: JWTs signed with a key only the
// backend has (HS256), naming the API host as both issuer and audience.
type Signer struct {
	key    []byte
	issuer string
}

// NewSigner signs with key for the API host at issuer (API_URL). An empty
// key gets a random one: access tokens then stop working when the process
// does, and their holders ask for new ones.
func NewSigner(key []byte, issuer string) (*Signer, error) {
	if len(key) == 0 {
		key = make([]byte, MinSigningKeyBytes)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
	}
	if len(key) < MinSigningKeyBytes {
		return nil, errors.New("tokens: the signing key must be at least 32 bytes")
	}
	if issuer == "" {
		return nil, errors.New("tokens: access tokens need an issuer")
	}
	return &Signer{key: key, issuer: issuer}, nil
}

// EnableExchange lets API tokens be exchanged for access tokens signed by
// signer, and accepts those.
func (s *Store) EnableExchange(signer *Signer) { s.signer = signer }

// accessClaims is an access token's payload. sub is the owner, client_id
// the API token it came from, scope what it was narrowed to.
type accessClaims struct {
	jwt.RegisteredClaims
	ClientID string `json:"client_id"`
	Scope    string `json:"scope"`
}

// ExchangeToken is auth.TokenVerifier's: OAuth's client-credentials grant.
// The client ID is an API token's id and the secret is the token itself, so
// what is checked is exactly what VerifyToken checks.
func (s *Store) ExchangeToken(ctx context.Context, clientID, clientSecret string, scopes []string) (string, auth.Grant, error) {
	if s.signer == nil {
		return "", auth.Grant{}, auth.ErrInvalidToken
	}
	t, err := s.check(ctx, clientSecret)
	if err != nil {
		return "", auth.Grant{}, err
	}
	if clientID != t.ID {
		return "", auth.Grant{}, auth.ErrInvalidToken
	}
	// The grant may be narrower than the API token, never wider.
	granted := t.Scopes
	if len(scopes) > 0 {
		granted = nil
		for _, scope := range scopes {
			if !slices.Contains(t.Scopes, scope) {
				return "", auth.Grant{}, auth.ErrInvalidScope
			}
			if !slices.Contains(granted, scope) {
				granted = append(granted, scope)
			}
		}
	}
	now := s.now()
	expires := now.Add(AccessTokenLife)
	if t.Expires.Before(expires) {
		expires = t.Expires
	}
	var jti [16]byte
	if _, err := s.random.Read(jti[:]); err != nil {
		return "", auth.Grant{}, err
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.signer.issuer,
			Audience:  jwt.ClaimStrings{s.signer.issuer},
			Subject:   t.Owner,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expires),
			ID:        hex.EncodeToString(jti[:]),
		},
		ClientID: t.ID,
		Scope:    strings.Join(granted, " "),
	}).SignedString(s.signer.key)
	if err != nil {
		return "", auth.Grant{}, err
	}
	s.touch(ctx, t)
	return t.Owner, auth.Grant{AccessToken: signed, Scopes: granted, ExpiresIn: expires.Sub(now)}, nil
}

// verifyAccessToken checks an access token: the signature, that it was made
// by and for this API host, that it has not expired, and then the API token
// it came from, as it is now. Revoking that one ends this one.
func (s *Store) verifyAccessToken(ctx context.Context, raw string) (string, auth.TokenInfo, error) {
	invalid := func() (string, auth.TokenInfo, error) { return "", auth.TokenInfo{}, auth.ErrInvalidToken }
	if s.signer == nil {
		return invalid()
	}
	var c accessClaims
	_, err := jwt.ParseWithClaims(raw, &c, func(*jwt.Token) (any, error) { return s.signer.key, nil },
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithIssuer(s.signer.issuer),
		jwt.WithAudience(s.signer.issuer),
		jwt.WithTimeFunc(s.now),
	)
	if err != nil || !ValidID(c.ClientID) {
		return invalid()
	}
	t, err := s.record(ctx, c.ClientID)
	if err != nil {
		return "", auth.TokenInfo{}, err
	}
	if t.Owner != c.Subject {
		return invalid()
	}
	// What the API token has now bounds what was granted then.
	var scopes []string
	for _, scope := range strings.Fields(c.Scope) {
		if slices.Contains(t.Scopes, scope) {
			scopes = append(scopes, scope)
		}
	}
	s.touch(ctx, t)
	return t.Owner, auth.TokenInfo{Name: t.Name, Scopes: scopes, Session: t.Session}, nil
}
