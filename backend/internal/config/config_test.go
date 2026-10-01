package config

import (
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestFromEnvDefaultsAndRequired(t *testing.T) {
	if _, err := FromEnv(env(map[string]string{})); err == nil {
		t.Fatal("expected error when required vars are missing")
	}
	c, err := FromEnv(env(map[string]string{
		"PUBLIC_URL":    "https://sessions.example.com/",
		"OIDC_ISSUER":   "https://kc.example.com/realms/browserjs",
		"OIDC_JWKS_URL": "http://keycloak:8080/realms/browserjs/protocol/openid-connect/certs",
		"TOPAZ_ADDR":    "topaz:9292",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "https://sessions.example.com" {
		t.Errorf("PublicURL trailing slash not trimmed: %q", c.PublicURL)
	}
	if c.Addr != ":8080" || c.Namespace != "browserjs-sessions" || c.AdminRole != "admin" {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if c.IdleAfter != 15*time.Minute || c.MaxSessionsPerUser != 5 || c.ReadyTimeout != 3*time.Minute {
		t.Errorf("unexpected defaults: %+v", c)
	}
}

func TestFromEnvOverrides(t *testing.T) {
	c, err := FromEnv(env(map[string]string{
		"PUBLIC_URL": "http://localhost:8080", "OIDC_ISSUER": "i", "OIDC_JWKS_URL": "j", "TOPAZ_ADDR": "t",
		"IDLE_AFTER": "5m", "MAX_SESSIONS_PER_USER": "2", "ADDR": ":9000",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.IdleAfter != 5*time.Minute || c.MaxSessionsPerUser != 2 || c.Addr != ":9000" {
		t.Errorf("overrides not applied: %+v", c)
	}
	if _, err := FromEnv(env(map[string]string{
		"PUBLIC_URL": "x", "OIDC_ISSUER": "i", "OIDC_JWKS_URL": "j", "TOPAZ_ADDR": "t", "IDLE_AFTER": "soon",
	})); err == nil {
		t.Fatal("expected error for a bad duration")
	}
}
