package config

import (
	"slices"
	"strings"
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
	if !slices.Equal(c.AllowedClients, []string{"browserjs-spa", "claude-connector"}) {
		t.Errorf("AllowedClients default = %q", c.AllowedClients)
	}
}

func valid() map[string]string {
	return map[string]string{
		"PUBLIC_URL": "http://localhost:8080", "OIDC_ISSUER": "i", "OIDC_JWKS_URL": "j", "TOPAZ_ADDR": "t",
	}
}

func TestFromEnvAllowedClients(t *testing.T) {
	m := valid()
	m["OIDC_ALLOWED_CLIENTS"] = " my-spa , ,other "
	c, err := FromEnv(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.AllowedClients, []string{"my-spa", "other"}) {
		t.Errorf("AllowedClients = %q", c.AllowedClients)
	}
	m["OIDC_ALLOWED_CLIENTS"] = " , "
	if _, err := FromEnv(env(m)); err == nil {
		t.Error("expected an error for a client list with no clients in it")
	}
}

func TestFromEnvReportsEachMissingVariable(t *testing.T) {
	for _, k := range []string{"PUBLIC_URL", "OIDC_ISSUER", "OIDC_JWKS_URL", "TOPAZ_ADDR"} {
		m := valid()
		delete(m, k)
		if _, err := FromEnv(env(m)); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("without %s: err = %v", k, err)
		}
	}
	// The first missing one is named, the same one every time.
	for range 20 {
		if _, err := FromEnv(env(map[string]string{})); err == nil || !strings.Contains(err.Error(), "PUBLIC_URL") {
			t.Fatalf("err = %v, want PUBLIC_URL", err)
		}
	}
}

func TestFromEnvRejectsNonsenseValues(t *testing.T) {
	for k, v := range map[string]string{
		"MAX_SESSIONS_PER_USER": "0", "IDLE_AFTER": "0s", "READY_TIMEOUT": "-1m", "PUBLIC_URL": "sessions.example.com",
	} {
		m := valid()
		m[k] = v
		if _, err := FromEnv(env(m)); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=%s: err = %v", k, v, err)
		}
	}
	for _, k := range []string{"MAX_SESSIONS_PER_USER", "READY_TIMEOUT"} {
		m := valid()
		m[k] = "lots"
		if _, err := FromEnv(env(m)); err == nil {
			t.Errorf("%s=lots: expected an error", k)
		}
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
		"PUBLIC_URL": "http://x", "OIDC_ISSUER": "i", "OIDC_JWKS_URL": "j", "TOPAZ_ADDR": "t", "IDLE_AFTER": "soon",
	})); err == nil {
		t.Fatal("expected error for a bad duration")
	}
}
