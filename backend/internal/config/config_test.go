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
		"PUBLIC_URL":           "https://app.example.com/",
		"SESSION_URL_TEMPLATE": "https://{id}.sessions.example.com",
		"POMERIUM_JWKS_URL":    "http://pomerium-proxy.pomerium.svc/.well-known/pomerium/jwks.json",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "https://app.example.com" {
		t.Errorf("PublicURL trailing slash not trimmed: %q", c.PublicURL)
	}
	if c.Addr != ":8080" || c.Namespace != "browserjs-sessions" || c.SignOutURL != "/.pomerium/sign_out" {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if c.IdleAfter != 15*time.Minute || c.MaxSessionsPerUser != 5 || c.ReadyTimeout != 3*time.Minute {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if len(c.AdminEmails) != 0 {
		t.Errorf("AdminEmails default = %q, want none", c.AdminEmails)
	}
	if got := c.SessionURLs.MCP("s-abcdefg234"); got != "https://s-abcdefg234.sessions.example.com/mcp" {
		t.Errorf("session MCP URL = %q", got)
	}
	if c.PomeriumJWKSURL != "http://pomerium-proxy.pomerium.svc/.well-known/pomerium/jwks.json" {
		t.Errorf("PomeriumJWKSURL = %q", c.PomeriumJWKSURL)
	}
}

func valid() map[string]string {
	return map[string]string{
		"PUBLIC_URL": "http://app.localtest.me:8080", "SESSION_URL_TEMPLATE": "http://{id}.sessions.localtest.me:8080",
		"POMERIUM_JWKS_URL": "j",
	}
}

func TestFromEnvAdminEmails(t *testing.T) {
	m := valid()
	m["ADMIN_EMAILS"] = " Root@Example.com , ,ops@example.com,"
	c, err := FromEnv(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.AdminEmails, []string{"root@example.com", "ops@example.com"}) {
		t.Errorf("AdminEmails = %q", c.AdminEmails)
	}
	m["ADMIN_EMAILS"] = " , "
	if c, err := FromEnv(env(m)); err != nil || len(c.AdminEmails) != 0 {
		t.Errorf("a list with no emails in it: %q, %v; want no admins", c.AdminEmails, err)
	}
}

func TestFromEnvReportsEachMissingVariable(t *testing.T) {
	for _, k := range []string{"PUBLIC_URL", "SESSION_URL_TEMPLATE", "POMERIUM_JWKS_URL"} {
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
	for _, c := range []struct{ k, v string }{
		{"MAX_SESSIONS_PER_USER", "0"}, {"MAX_FILE_BYTES", "0"}, {"MAX_FILE_BYTES", "100MB"}, {"IDLE_AFTER", "0s"}, {"READY_TIMEOUT", "-1m"},
		{"PUBLIC_URL", "app.example.com"},
		{"SESSION_URL_TEMPLATE", "http://sessions.localtest.me:8080"},
		{"SESSION_URL_TEMPLATE", "http://sessions.localtest.me:8080/s/{id}"},
		{"SESSION_URL_TEMPLATE", "http://s-{id}.sessions.localtest.me:8080"},
		// The app's own host must not read as a session's.
		{"PUBLIC_URL", "http://app.sessions.localtest.me:8080"},
		{"PUBLIC_URL", "http://s-abcdefg234.sessions.localtest.me"},
	} {
		m := valid()
		m[c.k] = c.v
		if _, err := FromEnv(env(m)); err == nil || !strings.Contains(err.Error(), c.k) {
			t.Errorf("%s=%s: err = %v", c.k, c.v, err)
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
	m := valid()
	for k, v := range map[string]string{
		"IDLE_AFTER": "5m", "MAX_SESSIONS_PER_USER": "2", "MAX_FILE_BYTES": "2048", "ADDR": ":9000", "SIGN_OUT_URL": "https://app.example.com/bye",
	} {
		m[k] = v
	}
	c, err := FromEnv(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if c.IdleAfter != 5*time.Minute || c.MaxSessionsPerUser != 2 || c.MaxFileBytes != 2048 || c.Addr != ":9000" || c.SignOutURL != "https://app.example.com/bye" {
		t.Errorf("overrides not applied: %+v", c)
	}
	m["IDLE_AFTER"] = "soon"
	if _, err := FromEnv(env(m)); err == nil {
		t.Fatal("expected error for a bad duration")
	}
}

// Snapshots are for GKE: off unless asked for.
func TestFromEnvSnapshots(t *testing.T) {
	c, err := FromEnv(env(valid()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Snapshots || c.SnapshotTimeout != 2*time.Minute || c.RestoreTimeout != 2*time.Minute {
		t.Errorf("defaults: %v, %s, %s", c.Snapshots, c.SnapshotTimeout, c.RestoreTimeout)
	}
	m := valid()
	m["SNAPSHOTS"], m["SNAPSHOT_TIMEOUT"], m["SNAPSHOT_RESTORE_TIMEOUT"] = "true", "90s", "45s"
	if c, err = FromEnv(env(m)); err != nil {
		t.Fatal(err)
	}
	if !c.Snapshots || c.SnapshotTimeout != 90*time.Second || c.RestoreTimeout != 45*time.Second {
		t.Errorf("set: %v, %s, %s", c.Snapshots, c.SnapshotTimeout, c.RestoreTimeout)
	}
	for k, v := range map[string]string{"SNAPSHOTS": "maybe", "SNAPSHOT_TIMEOUT": "0s", "SNAPSHOT_RESTORE_TIMEOUT": "soon"} {
		m := valid()
		m[k] = v
		if _, err := FromEnv(env(m)); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=%s: err = %v", k, v, err)
		}
	}
}

func TestWarmPool(t *testing.T) {
	base := map[string]string{
		"PUBLIC_URL":           "https://app.example.com",
		"SESSION_URL_TEMPLATE": "https://{id}.sessions.example.com",
		"POMERIUM_JWKS_URL":    "https://app.example.com/.well-known/pomerium/jwks.json",
	}
	with := func(k, v string) func(string) string {
		m := map[string]string{k: v}
		for bk, bv := range base {
			m[bk] = bv
		}
		return env(m)
	}
	c, err := FromEnv(env(base))
	if err != nil || c.WarmPool != "" || c.WarmPoolWait != 5*time.Second {
		t.Errorf("defaults: pool %q, wait %s, err %v; want no pool", c.WarmPool, c.WarmPoolWait, err)
	}
	if c, err := FromEnv(with("WARM_POOL", "s")); err != nil || c.WarmPool != "s" {
		t.Errorf("WARM_POOL=s: pool %q, err %v", c.WarmPool, err)
	}
	// Any other pool's Sandboxes would not be named like sessions.
	if _, err := FromEnv(with("WARM_POOL", "sessions")); err == nil || !strings.Contains(err.Error(), "WARM_POOL") {
		t.Errorf("WARM_POOL=sessions: err = %v", err)
	}
	if _, err := FromEnv(with("WARM_POOL_WAIT", "0s")); err == nil {
		t.Error("WARM_POOL_WAIT=0s was accepted")
	}
}
