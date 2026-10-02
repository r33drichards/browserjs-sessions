// Package config reads the backend's settings from the environment.
package config

import (
	"encoding/base64"
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

	// SessionURLs is where sessions are reached: one host for all of them,
	// each under its ID.
	SessionURLs *sessions.URLTemplate
	// LegacySessionURLs is where sessions were reached before, a host each,
	// and still are for the URLs already handed out. nil for nowhere.
	LegacySessionURLs *sessions.URLTemplate

	PomeriumJWKSURL string   // where to fetch the keys Pomerium signs identities with
	AdminEmails     []string // users who may see and manage every session

	BlueprintPath string // session pod blueprint (YAML template)
	WebDir        string // built UI to serve

	// Passed through to the UI in /config.js.
	SignOutURL string

	IdleAfter          time.Duration // idle time before a session is put to sleep
	ReadyTimeout       time.Duration // how long a request waits for a waking session
	MaxSessionsPerUser int
	// MaxFileBytes is the largest file the session page may send to a
	// session's browser. The browser image has the same limit of its own
	// (FILES_MAX_BYTES).
	MaxFileBytes int64

	// Snapshots makes an idle session sleep to a GKE Pod Snapshot and wake
	// from it. Off unless SNAPSHOTS is set: a cluster without Pod Snapshots
	// (kind) has none of the resources.
	Snapshots       bool
	SnapshotTimeout time.Duration // how long one snapshot may take before the session sleeps without it
	RestoreTimeout  time.Duration // how long a restore may take before the session is started cold

	// WarmPool is the SandboxWarmPool new sessions are taken from, "" for
	// none: every session then starts cold. WarmPoolWait is how long a new
	// session waits for the pool before starting cold after all.
	WarmPool     string
	WarmPoolWait time.Duration

	// APIURL is the base URL of the API host, where API tokens are the
	// credential (https://api.<domain>; the API is under /v1). "" for none:
	// there are then no API tokens at all.
	APIURL string
	// AllowedEmails is who may use the product: the same addresses as the
	// policy of Pomerium's routes, which is not consulted on the API host.
	// Empty: nobody may make a token, and the API host refuses every one.
	AllowedEmails []string
	// APISigningKey signs the access tokens API tokens are exchanged for.
	// Empty: a key made at start, so access tokens end with the process.
	APISigningKey []byte
	// PolicyOperatorURL is the policy operator's base URL, "" for no session
	// policies: the API is then what it was before them. OperatorAPIToken is
	// what the backend calls the operator with.
	PolicyOperatorURL string
	OperatorAPIToken  string
}

// APITokens reports whether users can make API tokens and use them.
func (c Config) APITokens() bool { return c.APIURL != "" && len(c.AllowedEmails) > 0 }

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
		WarmPool:        get("WARM_POOL"),
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
	if legacy := get("LEGACY_SESSION_URL_TEMPLATE"); legacy != "" {
		if c.LegacySessionURLs, err = sessions.ParseURLTemplate(legacy); err != nil {
			return Config{}, fmt.Errorf("LEGACY_SESSION_URL_TEMPLATE: %w", err)
		}
	}
	// Requests are told apart by their host: the app must not live where the
	// sessions do. Nor should it: what a session's pod answers must not be
	// of the app's origin, and the browser's sign-in with the app must not
	// reach a session's MCP endpoint.
	for _, urls := range []struct {
		name, template string
		parsed         *sessions.URLTemplate
	}{
		{"SESSION_URL_TEMPLATE", template, c.SessionURLs},
		{"LEGACY_SESSION_URL_TEMPLATE", get("LEGACY_SESSION_URL_TEMPLATE"), c.LegacySessionURLs},
	} {
		if urls.parsed == nil {
			continue
		}
		if _, session := urls.parsed.Match(public.Host, "/"); session {
			return Config{}, fmt.Errorf("PUBLIC_URL %q is a session host of %s %q", c.PublicURL, urls.name, urls.template)
		}
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
	if c.MaxFileBytes, err = strconv.ParseInt(or("MAX_FILE_BYTES", "104857600"), 10, 64); err != nil || c.MaxFileBytes < 1 {
		return Config{}, fmt.Errorf("MAX_FILE_BYTES must be a positive number of bytes, got %q", get("MAX_FILE_BYTES"))
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
	// The pool names its Sandboxes "<pool>-<5 characters>", and a session's
	// ID is its Sandbox's name.
	if c.WarmPool != "" && !sessions.ValidID(c.WarmPool+"-bcdfg") {
		return Config{}, fmt.Errorf("WARM_POOL must be \"s\" (its Sandboxes' names are session IDs), got %q", c.WarmPool)
	}
	if c.WarmPoolWait, err = positiveDuration(or("WARM_POOL_WAIT", "5s")); err != nil {
		return Config{}, fmt.Errorf("WARM_POOL_WAIT: %w", err)
	}
	if c.APIURL = strings.TrimRight(get("API_URL"), "/"); c.APIURL != "" {
		api, err := url.Parse(c.APIURL)
		if err != nil || (api.Scheme != "http" && api.Scheme != "https") || api.Host == "" || api.Path != "" || api.RawQuery != "" {
			return Config{}, fmt.Errorf("API_URL must be an absolute http(s) URL with no path, got %q", c.APIURL)
		}
		// Requests are told apart by their host, and on the API host nobody
		// has signed in.
		if strings.EqualFold(api.Host, public.Host) {
			return Config{}, fmt.Errorf("API_URL %q must not be the host of PUBLIC_URL", c.APIURL)
		}
		for _, urls := range []*sessions.URLTemplate{c.SessionURLs, c.LegacySessionURLs} {
			if urls == nil {
				continue
			}
			if _, session := urls.Match(api.Host, "/"); session {
				return Config{}, fmt.Errorf("API_URL %q is a session host", c.APIURL)
			}
		}
	}
	for _, email := range strings.Split(get("ALLOWED_EMAILS"), ",") {
		if email = strings.ToLower(strings.TrimSpace(email)); email != "" {
			c.AllowedEmails = append(c.AllowedEmails, email)
		}
	}
	if raw := strings.TrimSpace(get("API_SIGNING_KEY")); raw != "" {
		key, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			key, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(raw, "="))
		}
		// The value is not put in the error.
		if err != nil || len(key) < 32 {
			return Config{}, fmt.Errorf("API_SIGNING_KEY must be at least 32 bytes, in base64")
		}
		c.APISigningKey = key
	}
	if c.PolicyOperatorURL = strings.TrimRight(get("POLICY_OPERATOR_URL"), "/"); c.PolicyOperatorURL != "" {
		operator, err := url.Parse(c.PolicyOperatorURL)
		if err != nil || (operator.Scheme != "http" && operator.Scheme != "https") || operator.Host == "" {
			return Config{}, fmt.Errorf("POLICY_OPERATOR_URL must be an absolute http(s) URL, got %q", c.PolicyOperatorURL)
		}
		if c.OperatorAPIToken = get("OPERATOR_API_TOKEN"); c.OperatorAPIToken == "" {
			return Config{}, fmt.Errorf("OPERATOR_API_TOKEN is required with POLICY_OPERATOR_URL")
		}
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
