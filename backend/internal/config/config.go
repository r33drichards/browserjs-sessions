// Package config reads the backend's settings from the environment.
package config

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

type Config struct {
	Addr      string // listen address
	Namespace string // namespace holding session Sandboxes
	PublicURL string // the app's (UI and API) base URL, no trailing slash

	// SessionURLs is where sessions are reached: one host per session.
	SessionURLs *sessions.URLTemplate

	PomeriumJWKSURL string   // where to fetch the keys Pomerium signs identities with
	AdminEmails     []string // users who may see and manage every session

	BlueprintPath string // session pod blueprint (YAML template)
	WebDir        string // built UI to serve

	// Passed through to the UI in /config.js.
	SignOutURL string

	IdleAfter          time.Duration // idle time before a session is put to sleep
	ReadyTimeout       time.Duration // how long a request waits for a waking session
	MaxSessionsPerUser int

	// Snapshots makes an idle session sleep to a GKE Pod Snapshot and wake
	// from it. Off unless SNAPSHOTS is set: a cluster without Pod Snapshots
	// (kind) has none of the resources.
	Snapshots       bool
	SnapshotTimeout time.Duration // how long one snapshot may take before the session sleeps without it
	RestoreTimeout  time.Duration // how long a restore may take before the session is started cold
}

func FromEnv(get func(string) string) (Config, error) {
	or := func(k, def string) string {
		if v := get(k); v != "" {
			return v
		}
		return def
	}
	c := Config{
		Addr:            or("ADDR", ":8080"),
		Namespace:       or("NAMESPACE", "browserjs-sessions"),
		PublicURL:       strings.TrimRight(get("PUBLIC_URL"), "/"),
		PomeriumJWKSURL: get("POMERIUM_JWKS_URL"),
		BlueprintPath:   or("BLUEPRINT_PATH", "/etc/browserjs/blueprint.yaml"),
		WebDir:          or("WEB_DIR", "/srv/web"),
		SignOutURL:      or("SIGN_OUT_URL", "/.pomerium/sign_out"),
	}
	template := get("SESSION_URL_TEMPLATE")
	for _, req := range []struct{ name, value string }{
		{"PUBLIC_URL", c.PublicURL}, {"SESSION_URL_TEMPLATE", template}, {"POMERIUM_JWKS_URL", c.PomeriumJWKSURL},
	} {
		if req.value == "" {
			return Config{}, fmt.Errorf("%s is required", req.name)
		}
	}
	public, err := url.Parse(c.PublicURL)
	if err != nil || (public.Scheme != "http" && public.Scheme != "https") || public.Host == "" {
		return Config{}, fmt.Errorf("PUBLIC_URL must be an absolute http(s) URL, got %q", c.PublicURL)
	}
	if c.SessionURLs, err = sessions.ParseURLTemplate(template); err != nil {
		return Config{}, fmt.Errorf("SESSION_URL_TEMPLATE: %w", err)
	}
	// Requests are told apart by their host: the app must not live where the
	// sessions do.
	if _, session := c.SessionURLs.Match(public.Host); session {
		return Config{}, fmt.Errorf("PUBLIC_URL %q is under the session domain of SESSION_URL_TEMPLATE %q", c.PublicURL, template)
	}
	for _, email := range strings.Split(get("ADMIN_EMAILS"), ",") {
		if email = strings.ToLower(strings.TrimSpace(email)); email != "" {
			c.AdminEmails = append(c.AdminEmails, email)
		}
	}
	if c.IdleAfter, err = positiveDuration(or("IDLE_AFTER", "15m")); err != nil {
		return Config{}, fmt.Errorf("IDLE_AFTER: %w", err)
	}
	if c.ReadyTimeout, err = positiveDuration(or("READY_TIMEOUT", "3m")); err != nil {
		return Config{}, fmt.Errorf("READY_TIMEOUT: %w", err)
	}
	if c.MaxSessionsPerUser, err = strconv.Atoi(or("MAX_SESSIONS_PER_USER", "5")); err != nil {
		return Config{}, fmt.Errorf("MAX_SESSIONS_PER_USER: %w", err)
	}
	if c.Snapshots, err = strconv.ParseBool(or("SNAPSHOTS", "false")); err != nil {
		return Config{}, fmt.Errorf("SNAPSHOTS: %w", err)
	}
	if c.SnapshotTimeout, err = positiveDuration(or("SNAPSHOT_TIMEOUT", "2m")); err != nil {
		return Config{}, fmt.Errorf("SNAPSHOT_TIMEOUT: %w", err)
	}
	if c.RestoreTimeout, err = positiveDuration(or("SNAPSHOT_RESTORE_TIMEOUT", "2m")); err != nil {
		return Config{}, fmt.Errorf("SNAPSHOT_RESTORE_TIMEOUT: %w", err)
	}
	if c.MaxSessionsPerUser < 1 {
		return Config{}, fmt.Errorf("MAX_SESSIONS_PER_USER must be at least 1, got %d", c.MaxSessionsPerUser)
	}
	return c, nil
}

func positiveDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be positive, got %s", s)
	}
	return d, nil
}
