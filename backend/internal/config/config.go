// Package config reads the backend's settings from the environment.
package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr      string // listen address
	Namespace string // namespace holding session Sandboxes
	PublicURL string // externally reachable base URL, no trailing slash

	OIDCIssuer  string // expected `iss` claim
	OIDCJWKSURL string // where to fetch signing keys (may differ from the issuer host in-cluster)
	AdminRole   string // Keycloak realm role that makes a user an admin

	TopazAddr     string // Topaz directory gRPC address
	BlueprintPath string // session pod blueprint (YAML template)
	WebDir        string // built UI to serve

	// Passed through to the UI in /config.js.
	KCURL, KCRealm, KCClientID string

	IdleAfter          time.Duration // idle time before a session is put to sleep
	ReadyTimeout       time.Duration // how long a request waits for a waking session
	MaxSessionsPerUser int
}

func FromEnv(get func(string) string) (Config, error) {
	or := func(k, def string) string {
		if v := get(k); v != "" {
			return v
		}
		return def
	}
	c := Config{
		Addr:          or("ADDR", ":8080"),
		Namespace:     or("NAMESPACE", "browserjs-sessions"),
		PublicURL:     strings.TrimRight(get("PUBLIC_URL"), "/"),
		OIDCIssuer:    get("OIDC_ISSUER"),
		OIDCJWKSURL:   get("OIDC_JWKS_URL"),
		AdminRole:     or("ADMIN_ROLE", "admin"),
		TopazAddr:     get("TOPAZ_ADDR"),
		BlueprintPath: or("BLUEPRINT_PATH", "/etc/browserjs/blueprint.yaml"),
		WebDir:        or("WEB_DIR", "/srv/web"),
		KCURL:         get("KC_URL"),
		KCRealm:       or("KC_REALM", "browserjs"),
		KCClientID:    or("KC_CLIENT_ID", "browserjs-spa"),
	}
	for k, v := range map[string]string{
		"PUBLIC_URL": c.PublicURL, "OIDC_ISSUER": c.OIDCIssuer, "OIDC_JWKS_URL": c.OIDCJWKSURL, "TOPAZ_ADDR": c.TopazAddr,
	} {
		if v == "" {
			return Config{}, fmt.Errorf("%s is required", k)
		}
	}
	var err error
	if c.IdleAfter, err = time.ParseDuration(or("IDLE_AFTER", "15m")); err != nil {
		return Config{}, fmt.Errorf("IDLE_AFTER: %w", err)
	}
	if c.ReadyTimeout, err = time.ParseDuration(or("READY_TIMEOUT", "3m")); err != nil {
		return Config{}, fmt.Errorf("READY_TIMEOUT: %w", err)
	}
	if c.MaxSessionsPerUser, err = strconv.Atoi(or("MAX_SESSIONS_PER_USER", "5")); err != nil {
		return Config{}, fmt.Errorf("MAX_SESSIONS_PER_USER: %w", err)
	}
	return c, nil
}
