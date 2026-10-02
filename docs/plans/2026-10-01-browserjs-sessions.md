# browserjs sessions Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** A web app where signed-in users create, view (live VNC), stop and delete per-user "browserjs sessions" — a persistent Chromium plus its own mcp-js server — run as Kubernetes Agent Sandbox resources that sleep when idle and wake on demand.

**Architecture:** One Go backend holds the only Kubernetes credentials. Pomerium sits in front of it, signs users in through Dex (Google, GitHub) and tells the backend who is calling in a signed header. The backend checks that the caller owns the session (or is an admin), creates/suspends/deletes one `Sandbox` per session, and reverse-proxies each session's VNC websocket and MCP endpoint, which arrive on the session's own hostname. A React + Cloudscape UI (wireframe theme) talks only to the backend.

**Tech Stack:** Go (stdlib `net/http`, `client-go` dynamic client, `golang-jwt`, `keyfunc`), React 18 + Vite + TypeScript, Cloudscape, `@novnc/novnc`, Pomerium Core v0.33.3, Dex, `kubernetes-sigs/agent-sandbox` (v1.0.4 locally), kind + colima for local, GKE Agent Sandbox + Pod Snapshots for production. Nix dev shell.

Design: `docs/plans/2026-10-01-browserjs-sessions-design.md`. Read it first.

---

> **Changed since this plan was written:** sign-in is Pomerium in front of everything, with Dex federating Google and GitHub; every session has a host of its own; authorization is an owner-or-admin check in the backend. There is no Keycloak and no Topaz. The task texts of Phases 1 to 3 below were written before that and still mention them: for those, the code is the reference. Phases 4 and 5 and the open items are current.

## Read this before starting

**Facts verified while writing this plan (2026-10-01):**

- Agent Sandbox core resource: group `agents.x-k8s.io`, version `v1beta1`, kind `Sandbox`, plural `sandboxes`. Spec fields used here: `podTemplate` (`metadata.labels/annotations`, `spec` = PodSpec), `volumeClaimTemplates` (immutable after create), `service` (bool), `operatingMode` (`Running` | `Suspended`; Suspended removes the pod and keeps the volumes). Status: `conditions` (`Ready`, `Suspended`, `Finished`, `PodScheduled`), `podIPs`.
- Install on any cluster: `kubectl apply --server-side -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.5/sandbox-with-extensions.yaml`.
- A `Sandbox` can be created directly with an inline `podTemplate`; no `SandboxTemplate`/`SandboxClaim` is needed. The backend renders the pod from a blueprint file. (Deviation from the design doc's wording: there is no `SandboxTemplate` object; the blueprint file in `deploy/` plays that role.)
- Topaz: image `ghcr.io/aserto-dev/topaz`, directory gRPC on `9292`, manifest format `model: {version: 3}` with `types: {t: {relations: {r: "user | group#member"}, permissions: {p: "a | b"}}}`.
- mcp-js v0.21.0-rc.3: `get_artifact_upload_url`, `--public-url` / `MCP_V8_PUBLIC_URL`, upload route `PUT /api/artifact-uploads/{token}` outside bearer auth, Host-header allowlist on `/mcp` defaulting to `localhost, 127.0.0.1, ::1`.
- The browser image source is `~/railway-browser-mcp` (`Dockerfile.browser`, `browser/entrypoint.sh`, `browser/server.js`, `flake.nix`). It serves noVNC/websockify on `127.0.0.1:6080` behind Caddy basic auth on `$PORT`, and the browser MCP on `:8081`.
- fleet's sign-in pattern (`trycua/cua@39a206e`, `libs/fleet/src/auth/`): a `keycloak-js` singleton configured from `window.__…_CFG__` in a runtime `/config.js`; `init({onLoad: "login-required", flow: "standard", pkceMethod: "S256", checkLoginIframe: false})`; `getToken()` calls `updateToken(30)`; an `AuthProvider` renders children only after init resolves.

**Not verified — each has an explicit "verify first" step in its task:**

- The exact Go method/field names of the Topaz directory client (`github.com/aserto-dev/go-aserto/ds/v3`, `go-directory` v3 reader/writer). Task 12.
- GKE Pod Snapshot resource names and fields. Task 23.
- Whether headed Chromium runs acceptably under gVisor. Task 21.

**Conventions:**

- Run everything inside the dev shell: `nix develop` (Task 1). Never install Go or Node globally.
- Commit after every task. Commit messages have **no** `Co-Authored-By` trailer.
- Session ID = the `Sandbox` name, generated as `s-` + 10 lowercase base32 chars. The user's chosen name is the annotation `browserjs.dev/name`. The owner (Keycloak `sub`) is the label `browserjs.dev/owner`.
- All Kubernetes objects for sessions live in one namespace, `browserjs-sessions`.
- Session states the API returns: `starting`, `running`, `stopping`, `asleep` (idle-suspended, wakes on demand), `stopped` (user-suspended, wakes only on resume), `failed`.

---

## Phase 0 — Setup

### Task 1: Repo scaffold and dev shell

**Files:**
- Create: `flake.nix`
- Create: `.gitignore`
- Create: `backend/go.mod`
- Create: `README.md`

**Step 1: Write `flake.nix`**

```nix
{
  description = "browserjs sessions";
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  outputs = { self, nixpkgs }:
    let
      systems = [ "aarch64-darwin" "x86_64-darwin" "x86_64-linux" "aarch64-linux" ];
      forAll = f: nixpkgs.lib.genAttrs systems (s: f nixpkgs.legacyPackages.${s});
    in {
      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            go gopls nodejs_22 kubectl kind kustomize colima docker-client jq curl
          ];
        };
      });
    };
}
```

**Step 2: Write `.gitignore`**

```
node_modules/
web/dist/
backend/bin/
.direnv/
result
*.log
```

**Step 3: Create the Go module**

Run: `nix develop -c bash -c 'cd backend 2>/dev/null || mkdir backend && cd backend && go mod init github.com/r33drichards/browserjs-sessions/backend && go version'`
Expected: `go.mod` created; prints a Go version ≥ 1.24.

**Step 4: Write `README.md`**

```markdown
# browserjs sessions

Per-user browserjs sessions (Chromium + mcp-js) as Kubernetes Agent Sandbox
resources, with a web UI. Design and plan: `docs/plans/`.

Dev shell: `nix develop`. Backend tests: `cd backend && go test ./...`.
```

**Step 5: Commit**

```bash
git add flake.nix flake.lock .gitignore backend/go.mod README.md
git commit -m "chore: repo scaffold and nix dev shell"
```

---

## Phase 1 — Backend core (all unit-tested, no cluster needed)

Package layout under `backend/`:

```
cmd/server/main.go          wiring
internal/config/            env → Config
internal/auth/              token verification, request user
internal/authz/             Authorizer interface, memory + Topaz implementations
internal/sessions/          Sandbox ↔ Session, Store over the dynamic client
internal/idle/              activity tracker and sweep
internal/api/               REST handlers
internal/proxy/             MCP / upload / VNC reverse proxy, VNC tickets, waker
```

### Task 2: Config

**Files:**
- Create: `backend/internal/config/config.go`
- Test: `backend/internal/config/config_test.go`

**Step 1: Write the failing test**

```go
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
```

**Step 2: Run to verify it fails**

Run: `cd backend && go test ./internal/config/`
Expected: FAIL — `undefined: FromEnv`.

**Step 3: Implement**

```go
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
```

**Step 4: Run to verify it passes**

Run: `go test ./internal/config/`
Expected: `ok`.

**Step 5: Commit**

```bash
git add backend/internal/config
git commit -m "feat(backend): config from environment"
```

### Task 3: Token verification and request user

**Files:**
- Create: `backend/internal/auth/auth.go`
- Test: `backend/internal/auth/auth_test.go`

The verifier takes a `jwt.Keyfunc` so tests can supply a locally generated key instead of a JWKS server.

**Step 1: Add dependencies**

Run: `cd backend && go get github.com/golang-jwt/jwt/v5 github.com/MicahParks/keyfunc/v3`

**Step 2: Write the failing test**

```go
package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const issuer = "https://kc.example.com/realms/browserjs"

func sign(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newVerifier(t *testing.T) (*JWTVerifier, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	kf := func(*jwt.Token) (any, error) { return &key.PublicKey, nil }
	return NewJWTVerifier(kf, issuer, "admin"), key
}

func TestVerify(t *testing.T) {
	v, key := newVerifier(t)
	base := func() jwt.MapClaims {
		return jwt.MapClaims{
			"iss": issuer, "sub": "user-1", "preferred_username": "robert",
			"exp": time.Now().Add(time.Hour).Unix(),
			"realm_access": map[string]any{"roles": []any{"offline_access"}},
		}
	}

	u, err := v.Verify(t.Context(), sign(t, key, base()))
	if err != nil {
		t.Fatal(err)
	}
	if u.Subject != "user-1" || u.Username != "robert" || u.Admin {
		t.Errorf("unexpected user: %+v", u)
	}

	admin := base()
	admin["realm_access"] = map[string]any{"roles": []any{"admin"}}
	if u, _ := v.Verify(t.Context(), sign(t, key, admin)); !u.Admin {
		t.Error("admin role not recognised")
	}

	for name, mutate := range map[string]func(jwt.MapClaims){
		"wrong issuer": func(c jwt.MapClaims) { c["iss"] = "https://evil.example" },
		"expired":      func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Minute).Unix() },
		"no subject":   func(c jwt.MapClaims) { delete(c, "sub") },
	} {
		c := base()
		mutate(c)
		if _, err := v.Verify(t.Context(), sign(t, key, c)); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}

	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	if _, err := v.Verify(t.Context(), sign(t, other, base())); err == nil {
		t.Error("token signed by another key was accepted")
	}
}

func TestMiddleware(t *testing.T) {
	v, key := newVerifier(t)
	var seen User
	h := Middleware(v)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = UserFrom(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/sessions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", rec.Code)
	}

	req := httptest.NewRequest("GET", "/api/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+sign(t, key, jwt.MapClaims{
		"iss": issuer, "sub": "user-1", "exp": time.Now().Add(time.Hour).Unix(),
	}))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || seen.Subject != "user-1" {
		t.Errorf("valid token: code %d, user %+v", rec.Code, seen)
	}
}
```

**Step 3: Run to verify it fails**

Run: `go test ./internal/auth/`
Expected: FAIL — `undefined: NewJWTVerifier`.

**Step 4: Implement**

```go
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

func WithUser(ctx context.Context, u User) context.Context { return context.WithValue(ctx, ctxKey{}, u) }

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
```

**Step 5: Run to verify it passes**

Run: `go test ./internal/auth/`
Expected: `ok`.

**Step 6: Commit**

```bash
git add backend/go.mod backend/go.sum backend/internal/auth
git commit -m "feat(backend): Keycloak token verification and auth middleware"
```

### Task 4: Authorizer interface and in-memory implementation

The in-memory implementation is the test double for every later task and the reference behaviour the Topaz implementation (Task 12) must match.

**Files:**
- Create: `backend/internal/authz/authz.go`
- Create: `backend/internal/authz/memory.go`
- Test: `backend/internal/authz/memory_test.go`

**Step 1: Write the failing test**

```go
package authz

import "testing"

func TestMemoryOwnerAdminStranger(t *testing.T) {
	ctx := t.Context()
	a := NewMemory()
	owner, admin, stranger := "u-owner", "u-admin", "u-stranger"
	if err := a.SetAdmin(ctx, admin, true); err != nil {
		t.Fatal(err)
	}
	if err := a.AddSession(ctx, "s-1", owner); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		user string
		perm Permission
		want bool
	}{
		{owner, View, true}, {owner, Manage, true},
		{admin, View, true}, {admin, Manage, true},
		{stranger, View, false}, {stranger, Manage, false},
	}
	for _, c := range cases {
		got, err := a.Check(ctx, c.user, "s-1", c.perm)
		if err != nil || got != c.want {
			t.Errorf("Check(%s,%s) = %v,%v; want %v", c.user, c.perm, got, err, c.want)
		}
	}

	// Unknown session: nobody, not even an admin, is allowed.
	if ok, _ := a.Check(ctx, admin, "s-missing", View); ok {
		t.Error("check on a missing session must be false")
	}

	// Removing the session revokes access; revoking admin does too.
	_ = a.RemoveSession(ctx, "s-1")
	if ok, _ := a.Check(ctx, owner, "s-1", View); ok {
		t.Error("owner still allowed after session removal")
	}
	_ = a.AddSession(ctx, "s-2", owner)
	_ = a.SetAdmin(ctx, admin, false)
	if ok, _ := a.Check(ctx, admin, "s-2", View); ok {
		t.Error("former admin still allowed")
	}
}
```

**Step 2: Run to verify it fails**

Run: `go test ./internal/authz/`
Expected: FAIL — `undefined: NewMemory`.

**Step 3: Implement `authz.go`**

```go
// Package authz answers "may this user do this to this session?".
package authz

import "context"

type Permission string

const (
	View   Permission = "can_view"   // see the session, open its VNC view
	Manage Permission = "can_manage" // rename, stop, resume, delete, call its MCP endpoint
)

// Authorizer is implemented by Topaz in production and by Memory in tests.
// A Check on a session the Authorizer does not know is false for everyone.
type Authorizer interface {
	Check(ctx context.Context, userID, sessionID string, p Permission) (bool, error)
	// AddSession records sessionID as owned by ownerID.
	AddSession(ctx context.Context, sessionID, ownerID string) error
	RemoveSession(ctx context.Context, sessionID string) error
	// SetAdmin adds or removes userID from the admins group.
	SetAdmin(ctx context.Context, userID string, admin bool) error
}
```

**Step 4: Implement `memory.go`**

```go
package authz

import (
	"context"
	"sync"
)

type Memory struct {
	mu     sync.RWMutex
	owner  map[string]string
	admins map[string]bool
}

func NewMemory() *Memory {
	return &Memory{owner: map[string]string{}, admins: map[string]bool{}}
}

func (m *Memory) Check(_ context.Context, userID, sessionID string, _ Permission) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	owner, ok := m.owner[sessionID]
	if !ok {
		return false, nil
	}
	return owner == userID || m.admins[userID], nil
}

func (m *Memory) AddSession(_ context.Context, sessionID, ownerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.owner[sessionID] = ownerID
	return nil
}

func (m *Memory) RemoveSession(_ context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.owner, sessionID)
	return nil
}

func (m *Memory) SetAdmin(_ context.Context, userID string, admin bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if admin {
		m.admins[userID] = true
	} else {
		delete(m.admins, userID)
	}
	return nil
}
```

**Step 5: Run to verify it passes**

Run: `go test ./internal/authz/`
Expected: `ok`.

**Step 6: Commit**

```bash
git add backend/internal/authz
git commit -m "feat(backend): authorizer interface and in-memory implementation"
```

### Task 5: Session model and state derivation

Turns a `Sandbox` object into the `Session` the API returns. Pure function, table-tested.

**Files:**
- Create: `backend/internal/sessions/session.go`
- Test: `backend/internal/sessions/session_test.go`

**Step 1: Add dependencies**

Run: `cd backend && go get k8s.io/client-go@latest k8s.io/apimachinery@latest sigs.k8s.io/yaml`

**Step 2: Write the failing test**

```go
package sessions

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func sandbox(mode, stoppedBy string, conds ...map[string]any) *unstructured.Unstructured {
	cs := make([]any, len(conds))
	for i, c := range conds {
		cs[i] = c
	}
	ann := map[string]any{AnnName: "my session"}
	if stoppedBy != "" {
		ann[AnnStoppedBy] = stoppedBy
	}
	spec := map[string]any{}
	if mode != "" {
		spec["operatingMode"] = mode
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agents.x-k8s.io/v1beta1", "kind": "Sandbox",
		"metadata": map[string]any{
			"name": "s-abc", "creationTimestamp": "2026-10-01T00:00:00Z",
			"labels": map[string]any{LabelOwner: "user-1"}, "annotations": ann,
		},
		"spec":   spec,
		"status": map[string]any{"conditions": cs, "podIPs": []any{"10.0.0.7"}},
	}}
}

func cond(typ, status, reason, msg string) map[string]any {
	return map[string]any{"type": typ, "status": status, "reason": reason, "message": msg}
}

func TestFromSandboxState(t *testing.T) {
	cases := []struct {
		name string
		obj  *unstructured.Unstructured
		want State
	}{
		{"no conditions yet", sandbox("", ""), Starting},
		{"not ready", sandbox("Running", "", cond("Ready", "False", "DependenciesNotReady", "pod pending")), Starting},
		{"ready", sandbox("Running", "", cond("Ready", "True", "DependenciesReady", "")), Running},
		{"suspending", sandbox("Suspended", StoppedByIdle, cond("Suspended", "False", "PodTerminating", "")), Stopping},
		{"asleep", sandbox("Suspended", StoppedByIdle, cond("Suspended", "True", "PodTerminated", "")), Asleep},
		{"stopped by user", sandbox("Suspended", StoppedByUser, cond("Suspended", "True", "PodTerminated", "")), Stopped},
		// A stale Suspended=True lingers after resume; operatingMode decides.
		{"resumed, stale suspended condition", sandbox("Running", "",
			cond("Suspended", "True", "PodTerminated", ""), cond("Ready", "False", "DependenciesNotReady", "")), Starting},
		{"pod failed", sandbox("Running", "", cond("Finished", "True", "PodFailed", "OOMKilled"),
			cond("Ready", "False", "PodFailed", "OOMKilled")), Failed},
		{"invalid", sandbox("Running", "", cond("Ready", "False", "InvalidConfiguration", "name too long")), Failed},
	}
	for _, c := range cases {
		if got := FromSandbox(c.obj).State; got != c.want {
			t.Errorf("%s: state = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestFromSandboxFields(t *testing.T) {
	s := FromSandbox(sandbox("Running", "", cond("Ready", "False", "DependenciesNotReady", "pod pending")))
	if s.ID != "s-abc" || s.Name != "my session" || s.Owner != "user-1" || s.PodIP != "10.0.0.7" {
		t.Errorf("unexpected fields: %+v", s)
	}
	if s.Message != "pod pending" {
		t.Errorf("message = %q", s.Message)
	}
	if s.Created.IsZero() {
		t.Error("created not parsed")
	}
}
```

**Step 3: Run to verify it fails**

Run: `go test ./internal/sessions/`
Expected: FAIL — `undefined: FromSandbox`.

**Step 4: Implement**

```go
// Package sessions maps browserjs sessions onto Agent Sandbox resources.
package sessions

import (
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var SandboxGVR = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}

const (
	LabelOwner   = "browserjs.dev/owner"
	AnnName      = "browserjs.dev/name"
	AnnStoppedBy = "browserjs.dev/stopped-by"

	StoppedByUser = "user" // stays stopped until resumed
	StoppedByIdle = "idle" // wakes on the next request
)

type State string

const (
	Starting State = "starting"
	Running  State = "running"
	Stopping State = "stopping"
	Asleep   State = "asleep"
	Stopped  State = "stopped"
	Failed   State = "failed"
)

type Session struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Owner   string    `json:"owner"`
	State   State     `json:"state"`
	Message string    `json:"message,omitempty"` // why it is starting or failed
	Created time.Time `json:"created"`
	PodIP   string    `json:"-"`
}

type condition struct{ status, reason, message string }

func conditions(obj *unstructured.Unstructured) map[string]condition {
	out := map[string]condition{}
	list, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		c := condition{}
		c.status, _ = m["status"].(string)
		c.reason, _ = m["reason"].(string)
		c.message, _ = m["message"].(string)
		out[typ] = c
	}
	return out
}

// FromSandbox derives the API view of a session from its Sandbox.
func FromSandbox(obj *unstructured.Unstructured) Session {
	s := Session{
		ID:      obj.GetName(),
		Name:    obj.GetAnnotations()[AnnName],
		Owner:   obj.GetLabels()[LabelOwner],
		Created: obj.GetCreationTimestamp().Time,
	}
	if ips, _, _ := unstructured.NestedStringSlice(obj.Object, "status", "podIPs"); len(ips) > 0 {
		s.PodIP = ips[0]
	}
	mode, _, _ := unstructured.NestedString(obj.Object, "spec", "operatingMode")
	conds := conditions(obj)
	ready := conds["Ready"]

	switch {
	case mode == "Suspended":
		// The Suspended condition is only meaningful while operatingMode is
		// Suspended: the controller leaves a stale one behind after a resume.
		switch {
		case conds["Suspended"].status != "True":
			s.State = Stopping
		case obj.GetAnnotations()[AnnStoppedBy] == StoppedByIdle:
			s.State = Asleep
		default:
			s.State = Stopped
		}
	case ready.status == "True":
		s.State = Running
	case conds["Finished"].reason == "PodFailed" || ready.reason == "InvalidConfiguration":
		s.State = Failed
		s.Message = ready.message
	default:
		s.State = Starting
		s.Message = ready.message
	}
	return s
}
```

**Step 5: Run to verify it passes**

Run: `go test ./internal/sessions/`
Expected: `ok`.

**Step 6: Commit**

```bash
git add backend/go.mod backend/go.sum backend/internal/sessions
git commit -m "feat(backend): session model and state derived from Sandbox conditions"
```

### Task 6: Session store over the Kubernetes API

Creates, reads, lists, renames, suspends/resumes and deletes `Sandbox` objects. The pod spec comes from a blueprint: a YAML fragment with `podTemplate` and `volumeClaimTemplates`, run through Go `text/template` with `.ID` and `.PublicURL`.

**Files:**
- Create: `backend/internal/sessions/store.go`
- Create: `backend/internal/sessions/sessionstest/sessionstest.go`
- Test: `backend/internal/sessions/store_test.go`

**Step 1: Write the test helper `sessionstest/sessionstest.go`**

Shared by this task's tests and by the API, waker and proxy tests later.

```go
// Package sessionstest builds a sessions.Store backed by a fake cluster.
package sessionstest

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"

	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

const Namespace = "browserjs-sessions"

const Blueprint = `
podTemplate:
  metadata:
    labels:
      app: browserjs-session
  spec:
    containers:
      - name: browser
        image: browser:test
      - name: mcp-js
        image: mcp-js:test
        env:
          - name: MCP_V8_PUBLIC_URL
            value: "{{ .PublicURL }}/s/{{ .ID }}"
volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      accessModes: ["ReadWriteOnce"]
      resources:
        requests:
          storage: 5Gi
`

// New returns a store on an empty fake cluster, plus the raw client so tests
// can play the controller's part (set status).
func New(t *testing.T) (*sessions.Store, dynamic.Interface) {
	t.Helper()
	client := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{sessions.SandboxGVR: "SandboxList"})
	store, err := sessions.NewStore(client, Namespace, Blueprint, "https://sessions.example.com")
	if err != nil {
		t.Fatal(err)
	}
	return store, client
}

// SetStatus overwrites a Sandbox's status, as the controller would.
func SetStatus(t *testing.T, client dynamic.Interface, id string, status map[string]any) {
	t.Helper()
	ctx := context.Background()
	res := client.Resource(sessions.SandboxGVR).Namespace(Namespace)
	obj, err := res.Get(ctx, id, v1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedMap(obj.Object, status, "status"); err != nil {
		t.Fatal(err)
	}
	if _, err := res.Update(ctx, obj, v1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// Ready is the status of a running session with a pod IP.
func Ready(podIP string) map[string]any {
	return map[string]any{
		"podIPs":     []any{podIP},
		"conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "DependenciesReady"}},
	}
}

// Suspended is the status of a session whose pod has been removed.
func Suspended() map[string]any {
	return map[string]any{
		"conditions": []any{map[string]any{"type": "Suspended", "status": "True", "reason": "PodTerminated"}},
	}
}
```

**Step 2: Write the failing test `store_test.go`**

```go
package sessions_test

import (
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions/sessionstest"
)

func TestCreateRendersBlueprint(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)

	s, err := store.Create(ctx, "  research  ", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.ID, "s-") || len(s.ID) != 12 {
		t.Errorf("unexpected id %q", s.ID)
	}
	if s.Name != "research" || s.Owner != "user-1" || s.State != sessions.Starting {
		t.Errorf("unexpected session: %+v", s)
	}

	obj, err := client.Resource(sessions.SandboxGVR).Namespace(sessionstest.Namespace).Get(ctx, s.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if mode, _, _ := unstructured.NestedString(obj.Object, "spec", "operatingMode"); mode != "Running" {
		t.Errorf("operatingMode = %q", mode)
	}
	containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "podTemplate", "spec", "containers")
	env := containers[1].(map[string]any)["env"].([]any)[0].(map[string]any)
	if want := "https://sessions.example.com/s/" + s.ID; env["value"] != want {
		t.Errorf("public URL env = %v, want %s", env["value"], want)
	}
	// The owner label is also on the pod, for NetworkPolicy and debugging.
	podLabels, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "podTemplate", "metadata", "labels")
	if podLabels[sessions.LabelOwner] != "user-1" || podLabels["app"] != "browserjs-session" {
		t.Errorf("pod labels = %v", podLabels)
	}
	if vcts, _, _ := unstructured.NestedSlice(obj.Object, "spec", "volumeClaimTemplates"); len(vcts) != 1 {
		t.Errorf("volumeClaimTemplates = %v", vcts)
	}
}

func TestCreateRejectsBadNames(t *testing.T) {
	store, _ := sessionstest.New(t)
	for _, name := range []string{"", "   ", strings.Repeat("x", 64)} {
		if _, err := store.Create(t.Context(), name, "user-1"); !errors.Is(err, sessions.ErrInvalidName) {
			t.Errorf("name %q: err = %v, want ErrInvalidName", name, err)
		}
	}
}

func TestListGetRenameDelete(t *testing.T) {
	ctx := t.Context()
	store, _ := sessionstest.New(t)
	a, _ := store.Create(ctx, "a", "user-1")
	_, _ = store.Create(ctx, "b", "user-2")

	mine, err := store.List(ctx, "user-1")
	if err != nil || len(mine) != 1 || mine[0].ID != a.ID {
		t.Fatalf("List(user-1) = %+v, %v", mine, err)
	}
	if all, _ := store.List(ctx, ""); len(all) != 2 {
		t.Errorf("List(all) returned %d", len(all))
	}

	if err := store.Rename(ctx, a.ID, "renamed"); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Get(ctx, a.ID); got.Name != "renamed" {
		t.Errorf("name after rename = %q", got.Name)
	}

	if err := store.Delete(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, a.ID); !errors.Is(err, sessions.ErrNotFound) {
		t.Errorf("Get after delete: err = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, a.ID); !errors.Is(err, sessions.ErrNotFound) {
		t.Errorf("second delete: err = %v, want ErrNotFound", err)
	}
}

func TestSuspendAndResume(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)
	s, _ := store.Create(ctx, "a", "user-1")

	if err := store.Suspend(ctx, s.ID, sessions.StoppedByIdle); err != nil {
		t.Fatal(err)
	}
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Suspended())
	if got, _ := store.Get(ctx, s.ID); got.State != sessions.Asleep {
		t.Errorf("state after idle suspend = %s", got.State)
	}

	if err := store.Resume(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	obj, _ := client.Resource(sessions.SandboxGVR).Namespace(sessionstest.Namespace).Get(ctx, s.ID, metav1.GetOptions{})
	if mode, _, _ := unstructured.NestedString(obj.Object, "spec", "operatingMode"); mode != "Running" {
		t.Errorf("operatingMode after resume = %q", mode)
	}
	if _, ok := obj.GetAnnotations()[sessions.AnnStoppedBy]; ok {
		t.Error("stopped-by annotation not cleared on resume")
	}
}
```

**Step 3: Run to verify it fails**

Run: `go test ./internal/sessions/...`
Expected: FAIL — `undefined: sessions.NewStore`.

**Step 4: Implement `store.go`**

```go
package sessions

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/template"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"
)

var (
	ErrNotFound    = errors.New("session not found")
	ErrInvalidName = errors.New("name must be 1–63 characters")
)

type Store struct {
	client    dynamic.ResourceInterface
	blueprint *template.Template
	publicURL string
}

// NewStore parses blueprint (see deploy/base/blueprint.yaml for the format).
func NewStore(client dynamic.Interface, namespace, blueprint, publicURL string) (*Store, error) {
	tmpl, err := template.New("blueprint").Option("missingkey=error").Parse(blueprint)
	if err != nil {
		return nil, fmt.Errorf("blueprint: %w", err)
	}
	return &Store{
		client:    client.Resource(SandboxGVR).Namespace(namespace),
		blueprint: tmpl,
		publicURL: publicURL,
	}, nil
}

func newID() string {
	b := make([]byte, 7)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "s-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))[:10]
}

func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 63 {
		return "", ErrInvalidName
	}
	return name, nil
}

func (s *Store) Create(ctx context.Context, name, owner string) (Session, error) {
	name, err := cleanName(name)
	if err != nil {
		return Session{}, err
	}
	id := newID()

	var rendered bytes.Buffer
	if err := s.blueprint.Execute(&rendered, map[string]string{"ID": id, "PublicURL": s.publicURL}); err != nil {
		return Session{}, fmt.Errorf("render blueprint: %w", err)
	}
	spec := map[string]any{}
	if err := yaml.Unmarshal(rendered.Bytes(), &spec); err != nil {
		return Session{}, fmt.Errorf("parse blueprint: %w", err)
	}
	spec["operatingMode"] = "Running"
	// Stamp the owner on the pod as well as the Sandbox.
	if err := unstructured.SetNestedField(spec, owner, "podTemplate", "metadata", "labels", LabelOwner); err != nil {
		return Session{}, fmt.Errorf("blueprint podTemplate.metadata.labels: %w", err)
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": SandboxGVR.GroupVersion().String(),
		"kind":       "Sandbox",
		"metadata": map[string]any{
			"name":        id,
			"labels":      map[string]any{LabelOwner: owner},
			"annotations": map[string]any{AnnName: name},
		},
		"spec": spec,
	}}
	created, err := s.client.Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return Session{}, err
	}
	return FromSandbox(created), nil
}

func (s *Store) Get(ctx context.Context, id string) (Session, error) {
	obj, err := s.client.Get(ctx, id, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	return FromSandbox(obj), nil
}

// List returns owner's sessions, or everyone's when owner is "".
func (s *Store) List(ctx context.Context, owner string) ([]Session, error) {
	opts := metav1.ListOptions{LabelSelector: LabelOwner}
	if owner != "" {
		opts.LabelSelector = LabelOwner + "=" + owner
	}
	list, err := s.client.List(ctx, opts)
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, FromSandbox(&list.Items[i]))
	}
	return out, nil
}

func (s *Store) patch(ctx context.Context, id string, patch map[string]any) error {
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = s.client.Patch(ctx, id, types.MergePatchType, body, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return ErrNotFound
	}
	return err
}

func (s *Store) Rename(ctx context.Context, id, name string) error {
	name, err := cleanName(name)
	if err != nil {
		return err
	}
	return s.patch(ctx, id, map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{AnnName: name}},
	})
}

// Suspend removes the session's pod and keeps its disk. by is StoppedByUser
// or StoppedByIdle.
func (s *Store) Suspend(ctx context.Context, id, by string) error {
	return s.patch(ctx, id, map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{AnnStoppedBy: by}},
		"spec":     map[string]any{"operatingMode": "Suspended"},
	})
}

func (s *Store) Resume(ctx context.Context, id string) error {
	return s.patch(ctx, id, map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{AnnStoppedBy: nil}},
		"spec":     map[string]any{"operatingMode": "Running"},
	})
}

// Delete removes the session. Its disk goes with it (the PVC is owned by the
// Sandbox — confirmed against a real cluster in Task 20).
func (s *Store) Delete(ctx context.Context, id string) error {
	err := s.client.Delete(ctx, id, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return ErrNotFound
	}
	return err
}
```

**Step 5: Run to verify it passes**

Run: `go test ./internal/sessions/...`
Expected: `ok`. If the fake client rejects the merge patch, it is a fake-client limitation, not a design problem: replace `patch` with get → mutate → `Update` and keep the tests unchanged.

**Step 6: Commit**

```bash
git add backend/go.mod backend/go.sum backend/internal/sessions
git commit -m "feat(backend): session store over Agent Sandbox resources"
```

### Task 7: REST API

**Files:**
- Create: `backend/internal/api/api.go`
- Test: `backend/internal/api/api_test.go`

Routes (Go 1.22 method+path patterns). Every route is behind `auth.Middleware`.

| Route | Rule |
|---|---|
| `GET /api/sessions` | own sessions; `?all=1` returns everyone's, admins only (ignored otherwise) |
| `POST /api/sessions` | `{"name": "…"}`; 409 at the per-user cap; 400 on a bad name |
| `GET /api/sessions/{id}` | needs View |
| `PATCH /api/sessions/{id}` | needs Manage; `{"name": "…"}` and/or `{"action": "stop" \| "resume"}` |
| `DELETE /api/sessions/{id}` | needs Manage |

A session the caller may not see is 404, never 403.

**Step 1: Write the failing test**

```go
package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/r33drichards/browserjs-sessions/backend/internal/api"
	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/authz"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions/sessionstest"
)

type fixture struct {
	t       *testing.T
	handler http.Handler
	store   *sessions.Store
	authz   *authz.Memory
}

func newFixture(t *testing.T) *fixture {
	store, _ := sessionstest.New(t)
	az := authz.NewMemory()
	mux := http.NewServeMux()
	api.New(store, az, 2).Register(mux)
	return &fixture{t: t, handler: mux, store: store, authz: az}
}

// do performs a request as user, bypassing token verification.
func (f *fixture) do(user auth.User, method, path, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(auth.WithUser(req.Context(), user))
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("bad JSON %q: %v", rec.Body.String(), err)
	}
	return v
}

var (
	alice = auth.User{Subject: "alice"}
	bob   = auth.User{Subject: "bob"}
	root  = auth.User{Subject: "root", Admin: true}
)

func TestCreateListAndCap(t *testing.T) {
	f := newFixture(t)

	rec := f.do(alice, "POST", "/api/sessions", `{"name":"one"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	created := decode[sessions.Session](t, rec)
	if created.Name != "one" || created.Owner != "alice" {
		t.Errorf("created = %+v", created)
	}
	// Creating registers ownership with the authorizer.
	if ok, _ := f.authz.Check(t.Context(), "alice", created.ID, authz.Manage); !ok {
		t.Error("owner relation not recorded")
	}

	if rec := f.do(alice, "POST", "/api/sessions", `{"name":""}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty name: %d", rec.Code)
	}
	f.do(alice, "POST", "/api/sessions", `{"name":"two"}`)
	if rec := f.do(alice, "POST", "/api/sessions", `{"name":"three"}`); rec.Code != http.StatusConflict {
		t.Errorf("over cap: %d, want 409", rec.Code)
	}

	f.do(bob, "POST", "/api/sessions", `{"name":"bobs"}`)
	if got := decode[[]sessions.Session](t, f.do(alice, "GET", "/api/sessions", "")); len(got) != 2 {
		t.Errorf("alice sees %d sessions, want 2", len(got))
	}
	// all=1 is honoured for admins only.
	if got := decode[[]sessions.Session](t, f.do(alice, "GET", "/api/sessions?all=1", "")); len(got) != 2 {
		t.Errorf("non-admin all=1 returned %d", len(got))
	}
	if got := decode[[]sessions.Session](t, f.do(root, "GET", "/api/sessions?all=1", "")); len(got) != 3 {
		t.Errorf("admin all=1 returned %d, want 3", len(got))
	}
}

func TestOwnershipIsEnforcedAs404(t *testing.T) {
	f := newFixture(t)
	id := decode[sessions.Session](t, f.do(alice, "POST", "/api/sessions", `{"name":"mine"}`)).ID
	path := "/api/sessions/" + id

	for _, c := range []struct{ method, body string }{
		{"GET", ""}, {"PATCH", `{"name":"x"}`}, {"PATCH", `{"action":"stop"}`}, {"DELETE", ""},
	} {
		if rec := f.do(bob, c.method, path, c.body); rec.Code != http.StatusNotFound {
			t.Errorf("stranger %s: %d, want 404", c.method, rec.Code)
		}
	}
	if rec := f.do(root, "GET", path, ""); rec.Code != http.StatusOK {
		t.Errorf("admin GET: %d", rec.Code)
	}
	if rec := f.do(alice, "GET", "/api/sessions/s-missing000", ""); rec.Code != http.StatusNotFound {
		t.Errorf("missing session: %d", rec.Code)
	}
}

func TestPatchAndDelete(t *testing.T) {
	f := newFixture(t)
	id := decode[sessions.Session](t, f.do(alice, "POST", "/api/sessions", `{"name":"a"}`)).ID
	path := "/api/sessions/" + id

	if rec := f.do(alice, "PATCH", path, `{"name":"renamed"}`); rec.Code != http.StatusOK ||
		decode[sessions.Session](t, rec).Name != "renamed" {
		t.Errorf("rename: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(alice, "PATCH", path, `{"action":"stop"}`); rec.Code != http.StatusOK {
		t.Errorf("stop: %d", rec.Code)
	}
	// Without a controller the status never shows "suspended", so the state
	// reads as stopping; what matters is that the user's intent was recorded.
	if s, _ := f.store.Get(t.Context(), id); s.State != sessions.Stopping {
		t.Errorf("state after stop = %s", s.State)
	}
	if rec := f.do(alice, "PATCH", path, `{"action":"resume"}`); rec.Code != http.StatusOK {
		t.Errorf("resume: %d", rec.Code)
	}
	if rec := f.do(alice, "PATCH", path, `{"action":"explode"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown action: %d", rec.Code)
	}

	if rec := f.do(alice, "DELETE", path, ""); rec.Code != http.StatusNoContent {
		t.Errorf("delete: %d", rec.Code)
	}
	if ok, _ := f.authz.Check(t.Context(), "alice", id, authz.View); ok {
		t.Error("owner relation survived delete")
	}
	if rec := f.do(alice, "GET", path, ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET after delete: %d", rec.Code)
	}
}

func TestAdminMembershipIsSynced(t *testing.T) {
	f := newFixture(t)
	id := decode[sessions.Session](t, f.do(alice, "POST", "/api/sessions", `{"name":"a"}`)).ID
	// root has never been seen; its token says admin, so the first request
	// must register that before the check.
	if rec := f.do(root, "DELETE", "/api/sessions/"+id, ""); rec.Code != http.StatusNoContent {
		t.Errorf("admin delete: %d", rec.Code)
	}
}
```

**Step 2: Run to verify it fails**

Run: `go test ./internal/api/`
Expected: FAIL — `undefined: api.New`.

**Step 3: Implement**

```go
// Package api is the REST API the UI calls.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/authz"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

type API struct {
	store *sessions.Store
	authz authz.Authorizer
	cap   int

	mu        sync.Mutex
	adminSeen map[string]bool // last admin flag pushed to the authorizer, per user
}

func New(store *sessions.Store, az authz.Authorizer, maxPerUser int) *API {
	return &API{store: store, authz: az, cap: maxPerUser, adminSeen: map[string]bool{}}
}

func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/sessions", a.user(a.list))
	mux.HandleFunc("POST /api/sessions", a.user(a.create))
	mux.HandleFunc("GET /api/sessions/{id}", a.session(authz.View, a.get))
	mux.HandleFunc("PATCH /api/sessions/{id}", a.session(authz.Manage, a.patch))
	mux.HandleFunc("DELETE /api/sessions/{id}", a.session(authz.Manage, a.delete))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

type userHandler func(w http.ResponseWriter, r *http.Request, u auth.User)

// user resolves the caller and keeps the authorizer's admins group in step
// with the token's admin role.
func (a *API) user(next userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := auth.UserFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "not signed in")
			return
		}
		if err := a.SyncAdmin(r, u); err != nil {
			writeError(w, http.StatusServiceUnavailable, "authorization unavailable")
			return
		}
		next(w, r, u)
	}
}

// SyncAdmin pushes the user's admin flag to the authorizer when it changes.
func (a *API) SyncAdmin(r *http.Request, u auth.User) error {
	a.mu.Lock()
	seen, known := a.adminSeen[u.Subject]
	a.mu.Unlock()
	if known && seen == u.Admin {
		return nil
	}
	if err := a.authz.SetAdmin(r.Context(), u.Subject, u.Admin); err != nil {
		return err
	}
	a.mu.Lock()
	a.adminSeen[u.Subject] = u.Admin
	a.mu.Unlock()
	return nil
}

type sessionHandler func(w http.ResponseWriter, r *http.Request, id string)

// session additionally requires permission p on {id}. Denied and missing
// both answer 404 so session IDs don't leak.
func (a *API) session(p authz.Permission, next sessionHandler) http.HandlerFunc {
	return a.user(func(w http.ResponseWriter, r *http.Request, u auth.User) {
		id := r.PathValue("id")
		allowed, err := a.authz.Check(r.Context(), u.Subject, id, p)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "authorization unavailable")
			return
		}
		if !allowed {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		next(w, r, id)
	})
}

func (a *API) storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sessions.ErrNotFound):
		writeError(w, http.StatusNotFound, "session not found")
	case errors.Is(err, sessions.ErrInvalidName):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "cluster request failed")
	}
}

func (a *API) list(w http.ResponseWriter, r *http.Request, u auth.User) {
	owner := u.Subject
	if u.Admin && r.URL.Query().Get("all") == "1" {
		owner = ""
	}
	list, err := a.store.List(r.Context(), owner)
	if err != nil {
		a.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *API) create(w http.ResponseWriter, r *http.Request, u auth.User) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON with a name")
		return
	}
	mine, err := a.store.List(r.Context(), u.Subject)
	if err != nil {
		a.storeError(w, err)
		return
	}
	if len(mine) >= a.cap {
		writeError(w, http.StatusConflict, "session limit reached; delete one first")
		return
	}
	s, err := a.store.Create(r.Context(), body.Name, u.Subject)
	if err != nil {
		a.storeError(w, err)
		return
	}
	if err := a.authz.AddSession(r.Context(), s.ID, u.Subject); err != nil {
		// A session nobody is allowed to reach is worse than none.
		_ = a.store.Delete(r.Context(), s.ID)
		writeError(w, http.StatusServiceUnavailable, "authorization unavailable")
		return
	}
	writeJSON(w, http.StatusCreated, s)
}

func (a *API) get(w http.ResponseWriter, r *http.Request, id string) {
	s, err := a.store.Get(r.Context(), id)
	if err != nil {
		a.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (a *API) patch(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Name   *string `json:"name"`
		Action string  `json:"action"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON")
		return
	}
	var err error
	if body.Name != nil {
		err = a.store.Rename(r.Context(), id, *body.Name)
	}
	if err == nil {
		switch body.Action {
		case "":
		case "stop":
			err = a.store.Suspend(r.Context(), id, sessions.StoppedByUser)
		case "resume":
			err = a.store.Resume(r.Context(), id)
		default:
			writeError(w, http.StatusBadRequest, `action must be "stop" or "resume"`)
			return
		}
	}
	if err != nil {
		a.storeError(w, err)
		return
	}
	a.get(w, r, id)
}

func (a *API) delete(w http.ResponseWriter, r *http.Request, id string) {
	if err := a.store.Delete(r.Context(), id); err != nil && !errors.Is(err, sessions.ErrNotFound) {
		a.storeError(w, err)
		return
	}
	if err := a.authz.RemoveSession(r.Context(), id); err != nil {
		// The session is gone; the reconcile loop (Task 12) clears the leftover.
		w.Header().Set("X-Authz-Cleanup", "deferred")
	}
	w.WriteHeader(http.StatusNoContent)
}
```

**Step 4: Run to verify it passes**

Run: `go test ./internal/api/`
Expected: `ok`.

**Step 5: Commit**

```bash
git add backend/internal/api
git commit -m "feat(backend): sessions REST API with ownership checks"
```

### Task 8: Idle tracker

Tracks last activity and open connections per session, and reports which sessions have been idle too long.

**Files:**
- Create: `backend/internal/idle/idle.go`
- Test: `backend/internal/idle/idle_test.go`

**Step 1: Write the failing test**

```go
package idle

import (
	"slices"
	"testing"
	"time"
)

func TestIdle(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	tr := New(15*time.Minute, func() time.Time { return now })

	tr.Touch("a")
	tr.Touch("b")
	now = now.Add(10 * time.Minute)
	tr.Touch("b")
	now = now.Add(6 * time.Minute) // a idle 16m, b idle 6m

	// c has never been seen (e.g. the backend restarted): it gets a full
	// idle period from first sight, not an immediate suspend.
	if got := tr.Idle([]string{"a", "b", "c"}); !slices.Equal(got, []string{"a"}) {
		t.Errorf("Idle = %v, want [a]", got)
	}
	now = now.Add(15 * time.Minute)
	if got := tr.Idle([]string{"b", "c"}); !slices.Equal(got, []string{"b", "c"}) {
		t.Errorf("Idle = %v, want [b c]", got)
	}
}

func TestOpenConnectionKeepsSessionAwake(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	tr := New(15*time.Minute, func() time.Time { return now })

	done := tr.Open("a") // e.g. a VNC viewer
	now = now.Add(time.Hour)
	if got := tr.Idle([]string{"a"}); len(got) != 0 {
		t.Errorf("session with an open connection reported idle: %v", got)
	}
	done()
	done() // closing twice must not go negative
	if got := tr.Idle([]string{"a"}); len(got) != 0 {
		t.Errorf("idle immediately after close: %v (closing counts as activity)", got)
	}
	now = now.Add(16 * time.Minute)
	if got := tr.Idle([]string{"a"}); !slices.Equal(got, []string{"a"}) {
		t.Errorf("Idle = %v, want [a]", got)
	}
}

func TestForget(t *testing.T) {
	now := time.Now()
	tr := New(time.Minute, func() time.Time { return now })
	tr.Touch("a")
	tr.Forget("a")
	now = now.Add(2 * time.Minute)
	if got := tr.Idle([]string{"a"}); len(got) != 0 {
		t.Errorf("forgotten session treated as known: %v", got)
	}
}
```

**Step 2: Run to verify it fails**

Run: `go test ./internal/idle/`
Expected: FAIL — `undefined: New`.

**Step 3: Implement**

```go
// Package idle decides which sessions have gone unused long enough to sleep.
package idle

import (
	"sync"
	"time"
)

type Tracker struct {
	after time.Duration
	now   func() time.Time

	mu   sync.Mutex
	last map[string]time.Time
	open map[string]int
}

func New(after time.Duration, now func() time.Time) *Tracker {
	return &Tracker{after: after, now: now, last: map[string]time.Time{}, open: map[string]int{}}
}

// Touch records activity on a session (an MCP call, an upload).
func (t *Tracker) Touch(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last[id] = t.now()
}

// Open marks a long-lived connection (a VNC viewer). The session cannot go
// idle until the returned func is called.
func (t *Tracker) Open(id string) (done func()) {
	t.mu.Lock()
	t.open[id]++
	t.last[id] = t.now()
	t.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			if t.open[id]--; t.open[id] <= 0 {
				delete(t.open, id)
			}
			t.last[id] = t.now()
		})
	}
}

// Forget drops a deleted session.
func (t *Tracker) Forget(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.last, id)
	delete(t.open, id)
}

// Idle returns which of the given running sessions should be put to sleep.
// A session seen for the first time starts its idle period now.
func (t *Tracker) Idle(running []string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	var out []string
	for _, id := range running {
		last, known := t.last[id]
		if !known {
			t.last[id] = now
			continue
		}
		if t.open[id] == 0 && now.Sub(last) >= t.after {
			out = append(out, id)
		}
	}
	return out
}
```

**Step 4: Run to verify it passes**

Run: `go test ./internal/idle/`
Expected: `ok`.

**Step 5: Commit**

```bash
git add backend/internal/idle
git commit -m "feat(backend): idle tracker for scale-to-zero"
```

### Task 9: Waker

`EnsureAwake` is called before proxying. It resumes a sleeping session and waits until it is running.

**Files:**
- Create: `backend/internal/proxy/waker.go`
- Test: `backend/internal/proxy/waker_test.go`

**Step 1: Write the failing test**

```go
package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions/sessionstest"
)

func TestEnsureAwake(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)
	w := &Waker{Store: store, Timeout: 2 * time.Second, Poll: 10 * time.Millisecond}

	s, _ := store.Create(ctx, "a", "user-1")

	// Already running: returned as is.
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Ready("10.0.0.7"))
	got, err := w.EnsureAwake(ctx, s.ID)
	if err != nil || got.PodIP != "10.0.0.7" {
		t.Fatalf("running: %+v, %v", got, err)
	}

	// Asleep: resumed, and the call returns once the controller reports ready.
	_ = store.Suspend(ctx, s.ID, sessions.StoppedByIdle)
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Suspended())
	go func() {
		// Play the controller: wait for the resume, then report ready.
		for {
			if cur, _ := store.Get(context.Background(), s.ID); cur.State == sessions.Starting {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		sessionstest.SetStatus(t, client, s.ID, sessionstest.Ready("10.0.0.8"))
	}()
	got, err = w.EnsureAwake(ctx, s.ID)
	if err != nil || got.PodIP != "10.0.0.8" {
		t.Fatalf("asleep: %+v, %v", got, err)
	}

	// Stopped by the user: not woken.
	_ = store.Suspend(ctx, s.ID, sessions.StoppedByUser)
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Suspended())
	if _, err := w.EnsureAwake(ctx, s.ID); !errors.Is(err, ErrStopped) {
		t.Errorf("stopped: err = %v, want ErrStopped", err)
	}

	if _, err := w.EnsureAwake(ctx, "s-missing000"); !errors.Is(err, sessions.ErrNotFound) {
		t.Errorf("missing: err = %v", err)
	}
}

func TestEnsureAwakeTimesOut(t *testing.T) {
	ctx := t.Context()
	store, _ := sessionstest.New(t)
	w := &Waker{Store: store, Timeout: 50 * time.Millisecond, Poll: 10 * time.Millisecond}
	s, _ := store.Create(ctx, "a", "user-1") // never becomes ready
	if _, err := w.EnsureAwake(ctx, s.ID); !errors.Is(err, ErrNotReady) {
		t.Errorf("err = %v, want ErrNotReady", err)
	}
}
```

**Step 2: Run to verify it fails**

Run: `go test ./internal/proxy/`
Expected: FAIL — `undefined: Waker`.

**Step 3: Implement**

```go
// Package proxy forwards per-session traffic (MCP, uploads, VNC) to the
// session's pod, waking it first if it is asleep.
package proxy

import (
	"context"
	"errors"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

var (
	ErrStopped  = errors.New("session is stopped; resume it first")
	ErrFailed   = errors.New("session failed to start")
	ErrNotReady = errors.New("session did not become ready in time")
)

type Waker struct {
	Store   *sessions.Store
	Timeout time.Duration
	Poll    time.Duration
}

// EnsureAwake returns the session once it is running, resuming it if it was
// put to sleep for being idle. A session the user stopped is left stopped.
func (w *Waker) EnsureAwake(ctx context.Context, id string) (sessions.Session, error) {
	ctx, cancel := context.WithTimeout(ctx, w.Timeout)
	defer cancel()
	resumed := false
	for {
		s, err := w.Store.Get(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return sessions.Session{}, ErrNotReady
			}
			return sessions.Session{}, err
		}
		switch s.State {
		case sessions.Running:
			if s.PodIP != "" {
				return s, nil
			}
		case sessions.Stopped:
			return sessions.Session{}, ErrStopped
		case sessions.Failed:
			return sessions.Session{}, ErrFailed
		case sessions.Asleep:
			if !resumed {
				if err := w.Store.Resume(ctx, id); err != nil {
					return sessions.Session{}, err
				}
				resumed = true
			}
		}
		// starting, stopping, or just resumed: wait.
		select {
		case <-ctx.Done():
			return sessions.Session{}, ErrNotReady
		case <-time.After(w.Poll):
		}
	}
}
```

Note on `stopping` with an idle stop in progress: the loop waits for it to reach `asleep`, then resumes. That is deliberate — resuming mid-termination races the controller.

**Step 4: Run to verify it passes**

Run: `go test ./internal/proxy/`
Expected: `ok`.

**Step 5: Commit**

```bash
git add backend/internal/proxy
git commit -m "feat(backend): wake sleeping sessions on demand"
```

### Task 10: Per-session proxy (MCP, uploads, VNC)

**Files:**
- Create: `backend/internal/proxy/tickets.go`
- Create: `backend/internal/proxy/proxy.go`
- Test: `backend/internal/proxy/proxy_test.go`

Routes:

| Route | Auth | Forwarded to (in the session pod) |
|---|---|---|
| `GET /.well-known/oauth-protected-resource/s/{id}/mcp` | none | — (served here; tells Claude's connector where to sign in) |
| `/s/{id}/mcp` and `/s/{id}/mcp/…`, any method | bearer token + Manage | `:8080/mcp…` (mcp-js) |
| `PUT /s/{id}/api/artifact-uploads/{token}` | none; the token is the credential | `:8080/api/artifact-uploads/{token}` |
| `POST /api/sessions/{id}/vnc-ticket` | bearer token + View | — (returns a one-time ticket) |
| `GET /s/{id}/vnc?ticket=…` (websocket) | the ticket | `:6080/websockify` |

Why a ticket for VNC: browsers cannot set an `Authorization` header on a websocket, and a long-lived access token in a URL ends up in logs. The ticket is single-use, bound to one session, and expires in 30 seconds.

Details that matter:

- The upstream request's `Host` must be `localhost:<port>`: mcp-js only accepts `localhost`, `127.0.0.1` and `::1` on `/mcp` by default.
- The `Authorization` header is not forwarded. The pod runs agent-controlled code and has no use for the user's token.
- `FlushInterval: -1` so MCP's streamed responses are not buffered.
- Errors: not found or not allowed → 404; stopped by the user → 409; failed → 502; still waking at the timeout → 504 with `Retry-After`.

**Step 1: Write `tickets.go`**

```go
package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

const ticketTTL = 30 * time.Second

type tickets struct {
	now func() time.Time
	mu  sync.Mutex
	m   map[string]ticket
}

type ticket struct {
	session string
	expires time.Time
}

func newTickets(now func() time.Time) *tickets { return &tickets{now: now, m: map[string]ticket{}} }

func (t *tickets) Issue(session string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	id := hex.EncodeToString(b)
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for k, v := range t.m { // tickets are short-lived; sweep on issue
		if !v.expires.After(now) {
			delete(t.m, k)
		}
	}
	t.m[id] = ticket{session: session, expires: now.Add(ticketTTL)}
	return id
}

// Redeem consumes a ticket; it is valid once, for the session it was issued for.
func (t *tickets) Redeem(id, session string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	tk, ok := t.m[id]
	delete(t.m, id)
	return ok && tk.session == session && tk.expires.After(t.now())
}
```

**Step 2: Write the failing test `proxy_test.go`**

```go
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/authz"
	"github.com/r33drichards/browserjs-sessions/backend/internal/idle"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions/sessionstest"
)

// tokenVerifier treats the token text as the user ID.
type tokenVerifier struct{}

func (tokenVerifier) Verify(_ context.Context, raw string) (auth.User, error) {
	if raw == "bad" {
		return auth.User{}, errors.New("bad token")
	}
	return auth.User{Subject: raw}, nil
}

type upstreamCall struct{ method, path, host, authorization, body string }

type env struct {
	mux      *http.ServeMux
	id       string
	calls    *[]upstreamCall
	store    *sessions.Store
	tracker  *idle.Tracker
	upstream *httptest.Server
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := t.Context()
	store, client := sessionstest.New(t)
	az := authz.NewMemory()
	s, _ := store.Create(ctx, "a", "alice")
	_ = az.AddSession(ctx, s.ID, "alice")
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Ready("10.0.0.7"))

	var calls []upstreamCall
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, upstreamCall{r.Method, r.URL.Path, r.Host, r.Header.Get("Authorization"), string(body)})
		_, _ = io.WriteString(w, "upstream-ok")
	}))
	t.Cleanup(upstream.Close)

	tracker := idle.New(15*time.Minute, time.Now)
	p := &Proxy{
		Verifier:  tokenVerifier{},
		Authz:     az,
		Waker:     &Waker{Store: store, Timeout: time.Second, Poll: 5 * time.Millisecond},
		Idle:      tracker,
		PublicURL: "https://sessions.example.com",
		Issuer:    "https://kc.example.com/realms/browserjs",
		// Every pod port maps to the one test upstream.
		Target: func(sessions.Session, int) string { return strings.TrimPrefix(upstream.URL, "http://") },
	}
	mux := http.NewServeMux()
	p.Register(mux)
	return &env{mux: mux, id: s.ID, calls: &calls, store: store, tracker: tracker, upstream: upstream}
}

func (e *env) do(method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func TestMCPProxy(t *testing.T) {
	e := newEnv(t)
	path := "/s/" + e.id + "/mcp"

	rec := e.do("POST", path, "alice", `{"jsonrpc":"2.0"}`)
	if rec.Code != http.StatusOK || rec.Body.String() != "upstream-ok" {
		t.Fatalf("owner call: %d %s", rec.Code, rec.Body)
	}
	c := (*e.calls)[0]
	if c.method != "POST" || c.path != "/mcp" || c.host != "localhost:8080" || c.body != `{"jsonrpc":"2.0"}` {
		t.Errorf("upstream saw %+v", c)
	}
	if c.authorization != "" {
		t.Error("the user's token was forwarded into the session pod")
	}

	// No token: 401 that points the client at the sign-in metadata.
	rec = e.do("POST", path, "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	want := `Bearer resource_metadata="https://sessions.example.com/.well-known/oauth-protected-resource/s/` + e.id + `/mcp"`
	if got := rec.Header().Get("WWW-Authenticate"); got != want {
		t.Errorf("WWW-Authenticate = %q", got)
	}
	if rec := e.do("POST", path, "bad", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad token: %d", rec.Code)
	}
	// Someone else's session, and a missing one, look the same.
	if rec := e.do("POST", path, "bob", ""); rec.Code != http.StatusNotFound {
		t.Errorf("stranger: %d, want 404", rec.Code)
	}
	if rec := e.do("POST", "/s/s-missing000/mcp", "alice", ""); rec.Code != http.StatusNotFound {
		t.Errorf("missing: %d", rec.Code)
	}
	if len(*e.calls) != 1 {
		t.Errorf("rejected requests reached the pod: %d upstream calls", len(*e.calls))
	}
}

func TestStoppedSessionIsNotWoken(t *testing.T) {
	e := newEnv(t)
	_ = e.store.Suspend(t.Context(), e.id, sessions.StoppedByUser)
	// (status still says ready in the fake cluster; operatingMode decides)
	if rec := e.do("POST", "/s/"+e.id+"/mcp", "alice", ""); rec.Code != http.StatusConflict && rec.Code != http.StatusGatewayTimeout {
		t.Errorf("stopped session: %d", rec.Code)
	}
	if len(*e.calls) != 0 {
		t.Error("request reached a stopped session")
	}
}

func TestMetadata(t *testing.T) {
	e := newEnv(t)
	rec := e.do("GET", "/.well-known/oauth-protected-resource/s/"+e.id+"/mcp", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("metadata: %d", rec.Code)
	}
	var m struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m.Resource != "https://sessions.example.com/s/"+e.id+"/mcp" ||
		len(m.AuthorizationServers) != 1 || m.AuthorizationServers[0] != "https://kc.example.com/realms/browserjs" {
		t.Errorf("metadata = %+v", m)
	}
}

func TestUploadNeedsNoLogin(t *testing.T) {
	e := newEnv(t)
	rec := e.do("PUT", "/s/"+e.id+"/api/artifact-uploads/abc123", "", "file-bytes")
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d", rec.Code)
	}
	c := (*e.calls)[0]
	if c.method != "PUT" || c.path != "/api/artifact-uploads/abc123" || c.body != "file-bytes" {
		t.Errorf("upstream saw %+v", c)
	}
	// Only that route is open: the rest of the pod's API is not reachable.
	if rec := e.do("GET", "/s/"+e.id+"/api/artifacts", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("other pod API paths must not be exposed: %d", rec.Code)
	}
}

func TestVNCTicket(t *testing.T) {
	e := newEnv(t)
	ticketPath := "/api/sessions/" + e.id + "/vnc-ticket"

	if rec := e.do("POST", ticketPath, "bob", ""); rec.Code != http.StatusNotFound {
		t.Errorf("stranger ticket: %d", rec.Code)
	}
	rec := e.do("POST", ticketPath, "alice", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ticket: %d", rec.Code)
	}
	var body struct{ Ticket string }
	_ = json.Unmarshal(rec.Body.Bytes(), &body)

	vnc := "/s/" + e.id + "/vnc?ticket=" + body.Ticket
	if rec := e.do("GET", vnc, "", ""); rec.Code != http.StatusOK {
		t.Fatalf("vnc with ticket: %d", rec.Code)
	}
	if c := (*e.calls)[0]; c.path != "/websockify" || c.host != "localhost:6080" {
		t.Errorf("upstream saw %+v", c)
	}
	if rec := e.do("GET", vnc, "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("ticket reuse: %d, want 401", rec.Code)
	}
	if rec := e.do("GET", "/s/"+e.id+"/vnc", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no ticket: %d", rec.Code)
	}
}
```

**Step 3: Run to verify it fails**

Run: `go test ./internal/proxy/`
Expected: FAIL — `undefined: Proxy`.

**Step 4: Implement `proxy.go`**

```go
package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/authz"
	"github.com/r33drichards/browserjs-sessions/backend/internal/idle"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

// Ports inside a session pod.
const (
	mcpPort = 8080 // mcp-js
	vncPort = 6080 // websockify in front of x11vnc
)

type Proxy struct {
	Verifier  auth.Verifier
	Authz     authz.Authorizer
	Waker     *Waker
	Idle      *idle.Tracker
	PublicURL string
	Issuer    string
	// Target returns host:port for a port of a session's pod. Defaults to
	// the pod IP; tests override it.
	Target func(s sessions.Session, port int) string
	// SyncAdmin, when set, keeps the authorizer's admins group in step with
	// the caller's token before a check (see api.API.SyncAdmin).
	SyncAdmin func(r *http.Request, u auth.User) error

	tickets *tickets
}

func (p *Proxy) Register(mux *http.ServeMux) {
	if p.Target == nil {
		p.Target = func(s sessions.Session, port int) string { return net.JoinHostPort(s.PodIP, strconv.Itoa(port)) }
	}
	p.tickets = newTickets(time.Now)

	mux.HandleFunc("GET /.well-known/oauth-protected-resource/s/{id}/mcp", p.metadata)
	mux.HandleFunc("/s/{id}/mcp", p.mcp)
	mux.HandleFunc("/s/{id}/mcp/{rest...}", p.mcp)
	mux.HandleFunc("PUT /s/{id}/api/artifact-uploads/{token}", p.upload)
	mux.HandleFunc("POST /api/sessions/{id}/vnc-ticket", p.vncTicket)
	mux.HandleFunc("GET /s/{id}/vnc", p.vnc)
}

func (p *Proxy) metadataURL(id string) string {
	return p.PublicURL + "/.well-known/oauth-protected-resource/s/" + id + "/mcp"
}

// metadata is the OAuth protected-resource document (RFC 9728) that tells an
// MCP client which authorization server to use for this session's endpoint.
func (p *Proxy) metadata(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resource":                 p.PublicURL + "/s/" + r.PathValue("id") + "/mcp",
		"authorization_servers":    []string{p.Issuer},
		"scopes_supported":         []string{"openid", "profile", "email", "offline_access"},
		"bearer_methods_supported": []string{"header"},
	})
}

// authorize verifies the bearer token and the caller's permission on the
// session. It writes the response itself when it returns false.
func (p *Proxy) authorize(w http.ResponseWriter, r *http.Request, id string, perm authz.Permission) bool {
	raw := auth.BearerToken(r)
	var u auth.User
	var err error
	if raw != "" {
		u, err = p.Verifier.Verify(r.Context(), raw)
	}
	if raw == "" || err != nil {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata=%q`, p.metadataURL(id)))
		http.Error(w, "sign in required", http.StatusUnauthorized)
		return false
	}
	if p.SyncAdmin != nil {
		if err := p.SyncAdmin(r, u); err != nil {
			http.Error(w, "authorization unavailable", http.StatusServiceUnavailable)
			return false
		}
	}
	allowed, err := p.Authz.Check(r.Context(), u.Subject, id, perm)
	if err != nil {
		http.Error(w, "authorization unavailable", http.StatusServiceUnavailable)
		return false
	}
	if !allowed {
		http.Error(w, "session not found", http.StatusNotFound)
		return false
	}
	return true
}

// forward wakes the session if needed and proxies the request to path on
// port of its pod.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, id string, port int, path string) {
	s, err := p.Waker.EnsureAwake(r.Context(), id)
	switch {
	case errors.Is(err, sessions.ErrNotFound):
		http.Error(w, "session not found", http.StatusNotFound)
		return
	case errors.Is(err, ErrStopped):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case errors.Is(err, ErrFailed):
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	case err != nil:
		w.Header().Set("Retry-After", "10")
		http.Error(w, "session is waking up; retry shortly", http.StatusGatewayTimeout)
		return
	}
	target := p.Target(s, port)
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = target
			pr.Out.URL.Path = path
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = ""
			pr.Out.Host = "localhost:" + strconv.Itoa(port)
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "session is not responding", http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

func (p *Proxy) mcp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !p.authorize(w, r, id, authz.Manage) {
		return
	}
	p.Idle.Touch(id)
	path := "/mcp"
	if rest := r.PathValue("rest"); rest != "" {
		path += "/" + rest
	}
	p.forward(w, r, id, mcpPort, path)
	p.Idle.Touch(id)
}

// upload forwards mcp-js's one-time upload URL. There is no login: the
// token in the path is the credential, and mcp-js checks it.
func (p *Proxy) upload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p.Idle.Touch(id)
	p.forward(w, r, id, mcpPort, "/api/artifact-uploads/"+r.PathValue("token"))
}

func (p *Proxy) vncTicket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !p.authorize(w, r, id, authz.View) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"ticket": p.tickets.Issue(id)})
}

func (p *Proxy) vnc(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !p.tickets.Redeem(r.URL.Query().Get("ticket"), id) {
		http.Error(w, "invalid or expired ticket", http.StatusUnauthorized)
		return
	}
	done := p.Idle.Open(id) // a viewer keeps the session awake
	defer done()
	p.forward(w, r, id, vncPort, "/websockify")
}
```

**Step 5: Run to verify it passes**

Run: `go test ./internal/proxy/`
Expected: `ok`. In `TestStoppedSessionIsNotWoken` the fake cluster never reports the pod terminated, so the session reads `stopping` and the call ends as 504 rather than 409; the test accepts both and asserts the request never reached the pod.

**Step 6: Commit**

```bash
git add backend/internal/proxy
git commit -m "feat(backend): per-session MCP, upload and VNC proxy"
```

### Task 11: Idle sweeper and server wiring

**Files:**
- Create: `backend/internal/idle/sweeper.go`
- Test: `backend/internal/idle/sweeper_test.go`
- Create: `backend/cmd/server/main.go`
- Create: `backend/cmd/server/web.go`
- Test: `backend/cmd/server/web_test.go`

**Step 1: Write the failing sweeper test**

```go
package idle_test

import (
	"testing"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/idle"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions/sessionstest"
)

func TestSweepSuspendsOnlyIdleRunningSessions(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	tracker := idle.New(15*time.Minute, func() time.Time { return now })

	busy, _ := store.Create(ctx, "busy", "u")
	quiet, _ := store.Create(ctx, "quiet", "u")
	starting, _ := store.Create(ctx, "starting", "u") // never ready
	for _, id := range []string{busy.ID, quiet.ID} {
		sessionstest.SetStatus(t, client, id, sessionstest.Ready("10.0.0.1"))
	}

	if err := idle.Sweep(ctx, store, tracker); err != nil { // first sight starts the clock
		t.Fatal(err)
	}
	now = now.Add(16 * time.Minute)
	tracker.Touch(busy.ID)
	if err := idle.Sweep(ctx, store, tracker); err != nil {
		t.Fatal(err)
	}

	state := func(id string) sessions.State { s, _ := store.Get(ctx, id); return s.State }
	if state(busy.ID) != sessions.Running {
		t.Errorf("busy session was suspended")
	}
	if state(quiet.ID) != sessions.Stopping { // fake cluster never finishes the suspend
		t.Errorf("quiet session state = %s", state(quiet.ID))
	}
	if state(starting.ID) != sessions.Starting {
		t.Errorf("a session that is still starting was suspended")
	}
}
```

**Step 2: Run to verify it fails**

Run: `go test ./internal/idle/`
Expected: FAIL — `undefined: idle.Sweep`.

**Step 3: Implement `sweeper.go`**

```go
package idle

import (
	"context"
	"log/slog"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

// Sweep puts every running session that has been idle too long to sleep.
func Sweep(ctx context.Context, store *sessions.Store, t *Tracker) error {
	all, err := store.List(ctx, "")
	if err != nil {
		return err
	}
	var running []string
	for _, s := range all {
		if s.State == sessions.Running {
			running = append(running, s.ID)
		}
	}
	for _, id := range t.Idle(running) {
		if err := store.Suspend(ctx, id, sessions.StoppedByIdle); err != nil {
			slog.Error("idle suspend failed", "session", id, "err", err)
			continue
		}
		slog.Info("session put to sleep", "session", id)
	}
	return nil
}

// Run sweeps every interval until ctx is done.
func Run(ctx context.Context, store *sessions.Store, t *Tracker, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := Sweep(ctx, store, t); err != nil {
				slog.Error("idle sweep failed", "err", err)
			}
		}
	}
}
```

**Step 4: Run to verify it passes**

Run: `go test ./internal/idle/`
Expected: `ok`.

**Step 5: Write the failing web test `cmd/server/web_test.go`**

```go
package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/r33drichards/browserjs-sessions/backend/internal/config"
)

func TestWebHandler(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>app</html>"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("console.log(1)"), 0o644)

	h := webHandler(config.Config{WebDir: dir, KCURL: "https://kc.example.com", KCRealm: "browserjs", KCClientID: "browserjs-spa"})
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}

	if rec := get("/assets/app.js"); rec.Body.String() != "console.log(1)" {
		t.Errorf("asset: %q", rec.Body)
	}
	// Client-side routes fall back to the app shell.
	if rec := get("/sessions/s-abc"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "app") {
		t.Errorf("SPA fallback: %d %q", rec.Code, rec.Body)
	}
	cfg := get("/config.js").Body.String()
	for _, want := range []string{"window.__BROWSERJS_CFG__", `"kcUrl":"https://kc.example.com"`, `"kcRealm":"browserjs"`, `"kcClientId":"browserjs-spa"`} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config.js missing %s: %s", want, cfg)
		}
	}
}
```

**Step 6: Implement `cmd/server/web.go`**

```go
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/r33drichards/browserjs-sessions/backend/internal/config"
)

// webHandler serves the built UI, falling back to index.html for client-side
// routes, plus /config.js: runtime settings for the UI, so one image works in
// every environment (the fleet pattern).
func webHandler(cfg config.Config) http.Handler {
	files := http.FileServer(http.Dir(cfg.WebDir))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /config.js", func(w http.ResponseWriter, _ *http.Request) {
		body, _ := json.Marshal(map[string]string{
			"kcUrl": cfg.KCURL, "kcRealm": cfg.KCRealm, "kcClientId": cfg.KCClientID,
		})
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(append([]byte("window.__BROWSERJS_CFG__ = "), append(body, ';')...))
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(cfg.WebDir, filepath.Clean("/"+r.URL.Path))
		if info, err := os.Stat(path); err != nil || info.IsDir() || !strings.HasPrefix(path, cfg.WebDir) {
			http.ServeFile(w, r, filepath.Join(cfg.WebDir, "index.html"))
			return
		}
		files.ServeHTTP(w, r)
	})
	return mux
}
```

**Step 7: Implement `cmd/server/main.go`**

`authz.NewTopaz` arrives in Task 12; until then this file uses `authz.NewMemory()` behind the same variable so the server builds and runs.

```go
// Command server is the browserjs sessions backend.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/r33drichards/browserjs-sessions/backend/internal/api"
	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/authz"
	"github.com/r33drichards/browserjs-sessions/backend/internal/config"
	"github.com/r33drichards/browserjs-sessions/backend/internal/idle"
	"github.com/r33drichards/browserjs-sessions/backend/internal/proxy"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

func kubeConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.FromEnv(os.Getenv)
	if err != nil {
		return err
	}
	kube, err := kubeConfig()
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(kube)
	if err != nil {
		return err
	}
	blueprint, err := os.ReadFile(cfg.BlueprintPath)
	if err != nil {
		return err
	}
	store, err := sessions.NewStore(dyn, cfg.Namespace, string(blueprint), cfg.PublicURL)
	if err != nil {
		return err
	}
	verifier, err := auth.NewJWKSVerifier(ctx, cfg.OIDCJWKSURL, cfg.OIDCIssuer, cfg.AdminRole)
	if err != nil {
		return err
	}
	var az authz.Authorizer = authz.NewMemory() // replaced by Topaz in Task 12

	tracker := idle.New(cfg.IdleAfter, time.Now)
	go idle.Run(ctx, store, tracker, time.Minute)

	rest := api.New(store, az, cfg.MaxSessionsPerUser)
	apiMux := http.NewServeMux()
	rest.Register(apiMux)

	px := &proxy.Proxy{
		Verifier:  verifier,
		Authz:     az,
		Waker:     &proxy.Waker{Store: store, Timeout: cfg.ReadyTimeout, Poll: time.Second},
		Idle:      tracker,
		PublicURL: cfg.PublicURL,
		Issuer:    cfg.OIDCIssuer,
		SyncAdmin: rest.SyncAdmin,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	px.Register(mux) // registers the more specific /api/sessions/{id}/vnc-ticket itself
	mux.Handle("/api/", auth.Middleware(verifier)(apiMux))
	mux.Handle("/", webHandler(cfg))

	srv := &http.Server{Addr: cfg.Addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info("listening", "addr", cfg.Addr, "namespace", cfg.Namespace)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}
```

**Step 8: Run everything**

Run: `go vet ./... && go test ./... && go build -o bin/server ./cmd/server`
Expected: vet clean, all packages `ok`, binary built. If `ServeMux` panics at startup about conflicting patterns, the conflict is between `POST /api/sessions/{id}/vnc-ticket` and `/api/`; it should not be (the longer literal pattern is more specific), but if it does, move the ticket route under the `apiMux`.

**Step 9: Commit**

```bash
git add backend
git commit -m "feat(backend): idle sweeper, static UI serving and server wiring"
```

### Task 12: (removed) Topaz authorizer

Dropped. Authorization is the owner-or-admin check in `backend/internal/authz`
(`Owners`): the session's owner is the email recorded on its Sandbox, admins
are the `ADMIN_EMAILS` list. There is no Topaz and no directory to keep in step.

---

## Phase 2 — Session images

A session pod has two containers that share a network namespace and one disk:

| Container | Listens on | Disk mounts (subPaths of the one volume) |
|---|---|---|
| `browser` | `6080` websockify (VNC), `8081` browser MCP | `chrome` → `/data/chrome` |
| `mcp-js` | `8080` MCP + upload route | `memory` → `/data/memory`, `mcp` → `/data/mcp` |

### Task 13: Browser image — session mode and tab restore

Start from the working image in `~/railway-browser-mcp` (commit `5ba359c`).

**Files:**
- Create: `images/browser/` — copy of `Dockerfile.browser` (as `Dockerfile`), `flake.nix`, `flake.lock`, and `browser/{entrypoint.sh,server.js,package.json,package-lock.json,Caddyfile}` from `~/railway-browser-mcp`, keeping relative paths the flake expects
- Modify: `images/browser/browser/entrypoint.sh`
- Modify: `images/browser/browser/server.js`
- Create: `images/browser/test/tabs.test.sh`

**Step 1: Copy the sources and confirm the copy builds unchanged**

```bash
mkdir -p images/browser
cp ~/railway-browser-mcp/{flake.nix,flake.lock,.dockerignore} images/browser/
cp ~/railway-browser-mcp/Dockerfile.browser images/browser/Dockerfile
cp -R ~/railway-browser-mcp/browser images/browser/browser
git add images/browser && git commit -m "chore(images): import browser image from railway-browser-mcp@5ba359c"
```

**Step 2: Session mode in `entrypoint.sh`**

Add a mode switch near the top, after the existing variable block. When `SESSION_MODE=1`:

- `VNC_PASSWORD` is not required and Caddy is not started (the backend is the only way in, enforced by NetworkPolicy);
- websockify listens on `0.0.0.0:6080` instead of `127.0.0.1:6080`;
- Chromium gets `--restore-last-session`.

```bash
SESSION_MODE="${SESSION_MODE:-0}"
if [ "$SESSION_MODE" != 1 ]; then
  : "${VNC_PASSWORD:?set VNC_PASSWORD (basic-auth password for the /vnc viewer)}"
fi
WEBSOCKIFY_BIND=127.0.0.1
RESTORE_FLAG=""
if [ "$SESSION_MODE" = 1 ]; then
  WEBSOCKIFY_BIND=0.0.0.0
  RESTORE_FLAG="--restore-last-session"
fi
```

Remove the original unconditional `: "${VNC_PASSWORD:?…}"` line. Change the websockify line to `websockify --web "$NOVNC_WEB" "$WEBSOCKIFY_BIND:6080" 127.0.0.1:5900 &`, add `$RESTORE_FLAG` to the `chromium` invocation, and wrap the three Caddy lines (`VNC_HASH=…`, `export …`, `caddy run … &` and its `pids+=`) in `if [ "$SESSION_MODE" != 1 ]; then … fi`.

**Step 3: Clean shutdown so the tab list is saved**

Chromium only writes a complete session file on a clean exit, and after an unclean one it shows a "restore pages?" bubble instead of restoring. Two changes:

1. Before each Chromium start, inside the `while true` loop, mark the previous exit as clean:

```bash
    prefs="$PROFILE_DIR/Default/Preferences"
    if [ -f "$prefs" ]; then
      sed -i 's/"exit_type":"[A-Za-z]*"/"exit_type":"Normal"/; s/"exited_cleanly":false/"exited_cleanly":true/' "$prefs"
    fi
```

2. Replace the `cleanup` function so SIGTERM (pod shutdown, suspend) asks Chromium to quit and waits for it before killing the rest:

```bash
cleanup() {
  pkill -TERM -x chromium 2>/dev/null || true
  for _ in $(seq 1 50); do pgrep -x chromium >/dev/null || break; sleep 0.2; done
  kill "${pids[@]}" 2>/dev/null || true
}
trap cleanup EXIT TERM INT
```

Check the process name with `pgrep -l chrom` inside a running container first: the Nix package's binary may be `chromium` or `.chromium-wrapped`; use what you see.

**Step 4: Rebind named tabs after a restart (`server.js`)**

`server.js` maps tab names to pages in memory. After a restart that map is empty while Chromium has restored the pages, so the next call for tab `"default"` would open a new blank tab beside the restored one. Persist name → URL and re-adopt a restored page with the same URL.

Add near the `tabs` map:

```js
import fs from 'node:fs';

const TAB_STATE = process.env.TAB_STATE_FILE || '';
let savedTabs = {};
if (TAB_STATE) {
  try {
    savedTabs = JSON.parse(fs.readFileSync(TAB_STATE, 'utf8'));
  } catch {}
}

function saveTabs() {
  if (!TAB_STATE) return;
  const state = {};
  for (const [name, page] of tabs) if (!page.isClosed()) state[name] = page.url();
  try {
    fs.writeFileSync(TAB_STATE, JSON.stringify(state));
  } catch {}
}
```

In `getTab`, before the `about:blank` adoption, prefer a restored page whose URL matches what this name last had:

```js
  const pages = await browser.pages();
  const wanted = savedTabs[name];
  const restored = wanted && pages.find((p) => !owned.has(p) && p.url() === wanted);
  const blank = pages.find((p) => !owned.has(p) && p.url() === 'about:blank');
  const page = restored || blank || (await browser.newPage());
```

(replacing the existing `const blank = (await browser.pages()).find(…)` and `const page = …` lines), and call `saveTabs()` at the end of `executePipeline`'s `finally` block and in the page `close` handler. Set `TAB_STATE_FILE=/data/chrome/browserjs-tabs.json` in the blueprint (Task 18).

**Step 5: Test tab rebinding against a local Chrome**

`images/browser/test/tabs.test.sh` — start a headless Chrome with a throwaway profile, run `server.js` against it with `TAB_STATE_FILE` set, navigate tab `default` to `https://example.com`, kill **only `server.js`**, start it again, and call `url` on tab `default`:

```bash
#!/usr/bin/env bash
# Usage: tabs.test.sh  (needs node and a Chrome/Chromium binary in $CHROME)
set -euo pipefail
cd "$(dirname "$0")/../browser"
[ -d node_modules ] || npm ci --silent
tmp="$(mktemp -d)"; trap 'kill $CPID $SPID 2>/dev/null; rm -rf "$tmp"' EXIT
"${CHROME:?set CHROME to a Chrome/Chromium binary}" --headless=new --remote-debugging-port=9334 \
  --user-data-dir="$tmp/prof" --no-first-run about:blank >/dev/null 2>&1 & CPID=$!
sleep 3
start() { CDP_URL=http://127.0.0.1:9334 BROWSER_MCP_PORT=8792 TAB_STATE_FILE="$tmp/tabs.json" node server.js >/dev/null 2>&1 & SPID=$!; sleep 2; }
call() { curl -s -X POST localhost:8792/mcp -H 'content-type: application/json' \
  -H 'accept: application/json, text/event-stream' \
  -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"browser_execute\",\"arguments\":$1}}"; }
pages() { curl -s localhost:9334/json/list | grep -c '"type": "page"'; }

start
call '{"operations":[{"type":"navigate","params":{"url":"https://example.com"}}]}' >/dev/null
before="$(pages)"
kill $SPID; wait $SPID 2>/dev/null || true
start
out="$(call '{"operations":[{"type":"url"}]}')"
echo "$out" | grep -q 'example.com' || { echo "FAIL: default tab was not rebound: $out"; exit 1; }
[ "$(pages)" = "$before" ] || { echo "FAIL: a new tab was opened ($(pages) vs $before)"; exit 1; }
echo PASS
```

Run it first **without** the Step 4 change: `CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" bash images/browser/test/tabs.test.sh`
Expected: `FAIL: default tab was not rebound`. Apply Step 4, run again. Expected: `PASS`.

**Step 6: Commit**

```bash
git add images/browser
git commit -m "feat(images): browser session mode, clean shutdown and tab restore"
```

The Chromium-level restore (`--restore-last-session` after a clean SIGTERM) can only be tested in the container; it is covered by the integration test in Task 20.

### Task 14: mcp-js session image

**Files:**
- Create: `images/mcp-js/Dockerfile`
- Create: `images/mcp-js/{mcp-servers.json,mcp_tools.rego,filesystem.rego,run_js.md,instructions.md}`

**Step 1: Copy the config from the Railway deployment**

```bash
mkdir -p images/mcp-js
cp ~/railway-browser-mcp/mcp-js/{mcp_tools.rego,filesystem.rego,run_js.md,instructions.md} images/mcp-js/
```

Write `images/mcp-js/mcp-servers.json` so the browser MCP is reached on localhost (same pod). Open `~/railway-browser-mcp/mcp-js/mcp-servers.json`, copy it, and change only the URL from `http://browser.railway.internal:8081/mcp` to `http://127.0.0.1:8081/mcp`.

In `run_js.md`, delete the sentence "Uploaded artifacts are not kept across server restarts — use `/data/memory/` for anything long-lived." — in a session, artifacts are on the disk.

**Step 2: Find the session-database setting**

Run: `docker run --rm --entrypoint mcp-v8 wholelottahoopla/mcp-js:0.21.0-rc.3 --help | grep -i -A3 "session-db"`
Expected: a `--session-db-path` flag with its `MCP_V8_…` environment variable. Use the variable name it prints in the Dockerfile below (written here as `MCP_V8_SESSION_DB_PATH`).

**Step 3: Write `images/mcp-js/Dockerfile`**

```dockerfile
# mcp-js for one browserjs session: no JWT check (the backend authenticates
# and is the only client that can reach the pod), browser MCP on localhost,
# artifacts and upload grants on the session disk.
# v0.21.0-rc.3
FROM wholelottahoopla/mcp-js@sha256:1615350e509c06e23bb79ec5d26a839e43c2fbed4675b6987b49088a77dd9bbc
COPY mcp_tools.rego filesystem.rego mcp-servers.json run_js.md instructions.md /etc/mcp/
ENV MCP_V8_HTTP_PORT=8080 \
    MCP_V8_HEAP_STORE=none \
    MCP_V8_MCP_CONFIG=/etc/mcp/mcp-servers.json \
    MCP_V8_POLICIES_JSON='{"mcp_tools":{"policies":[{"url":"file:///etc/mcp/mcp_tools.rego"}]},"filesystem":{"policies":[{"url":"file:///etc/mcp/filesystem.rego"}]}}' \
    MCP_V8_RUN_JS_DESCRIPTION=@/etc/mcp/run_js.md \
    MCP_V8_INSTRUCTIONS=@/etc/mcp/instructions.md \
    MCP_V8_SESSION_DB_PATH=/data/mcp/sessions
EXPOSE 8080
```

`MCP_V8_PUBLIC_URL` is set per session by the blueprint. Check the base image's entrypoint and user with `docker inspect wholelottahoopla/mcp-js:0.21.0-rc.3 --format '{{.Config.Entrypoint}} {{.Config.User}}'`; the pod's `fsGroup` (Task 18) must give that user write access to `/data`.

**Step 4: Build and smoke-test**

```bash
docker build -t browserjs/mcp-js:dev images/mcp-js
docker run --rm -d --name mcpjs-smoke -p 18080:8080 -e MCP_V8_PUBLIC_URL=http://localhost:18080/s/s-test browserjs/mcp-js:dev
sleep 3; docker logs mcpjs-smoke 2>&1 | tail -5
curl -s -o /dev/null -w '%{http_code}\n' localhost:18080/api/artifacts
docker rm -f mcpjs-smoke
```

Expected: the log shows the server listening on 8080 (it may also log that the browser upstream on 8081 is unreachable — fine here, there is no browser container), and the curl prints `200` (no JWT enforcement).

**Step 5: Commit**

```bash
git add images/mcp-js
git commit -m "feat(images): mcp-js session image"
```

---

## Phase 3 — Web UI

React 18 + Vite + TypeScript + Cloudscape. Three screens: sign-in (Keycloak redirect), sessions list with a create dialog, session detail with the VNC view. Use the `cloudscape-design` skill when writing Cloudscape code.

### Task 15: Scaffold, sign-in and API client

**Files:**
- Create: `web/package.json`, `web/tsconfig.json`, `web/vite.config.ts`, `web/index.html`
- Create: `web/src/main.tsx`, `web/src/App.tsx`
- Create: `web/src/auth/keycloak.ts`, `web/src/auth/AuthProvider.tsx`
- Create: `web/src/api.ts`
- Test: `web/src/api.test.ts`

**Step 1: `web/package.json`**

```json
{
  "name": "browserjs-sessions-web",
  "private": true,
  "type": "module",
  "scripts": {
    "dev": "vite",
    "build": "tsc --noEmit && vite build",
    "test": "vitest run"
  },
  "dependencies": {
    "@cloudscape-design/components": "^3.0.825",
    "@cloudscape-design/global-styles": "^1.0.36",
    "@novnc/novnc": "^1.7.0",
    "keycloak-js": "^25.0.6",
    "react": "^18.3.1",
    "react-dom": "^18.3.1",
    "react-router-dom": "^6.28.0"
  },
  "devDependencies": {
    "@types/react": "^18.3.12",
    "@types/react-dom": "^18.3.1",
    "@vitejs/plugin-react": "^4.3.4",
    "typescript": "^5.7.2",
    "vite": "^5.4.11",
    "vitest": "^2.1.8"
  }
}
```

`web/tsconfig.json`:

```json
{
  "compilerOptions": {
    "target": "ES2022", "module": "ESNext", "moduleResolution": "Bundler",
    "jsx": "react-jsx", "strict": true, "skipLibCheck": true, "noEmit": true,
    "lib": ["ES2022", "DOM", "DOM.Iterable"], "types": ["vite/client"]
  },
  "include": ["src"]
}
```

`web/vite.config.ts` (the dev server proxies API, session and config traffic to a backend on `:8080`):

```ts
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

const backend = "http://localhost:8080"

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/api": backend,
      "/config.js": backend,
      "/s": { target: backend, ws: true },
    },
  },
})
```

`web/index.html`:

```html
<!doctype html>
<html lang="en">
  <head>
    <meta charset="UTF-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1.0" />
    <title>browserjs sessions</title>
    <script src="/config.js"></script>
  </head>
  <body>
    <div id="root"></div>
    <script type="module" src="/src/main.tsx"></script>
  </body>
</html>
```

Run: `cd web && npm install`

**Step 2: Write the failing API client test `web/src/api.test.ts`**

```ts
import { afterEach, describe, expect, it, vi } from "vitest"
import { ApiError, createApi } from "./api"

function fakeFetch(status: number, body: unknown) {
  return vi.fn(async () => new Response(body === undefined ? null : JSON.stringify(body), { status }))
}

afterEach(() => vi.restoreAllMocks())

describe("api", () => {
  it("sends the bearer token and parses sessions", async () => {
    const fetch = fakeFetch(200, [{ id: "s-1", name: "a", owner: "u", state: "running", created: "2026-10-01T00:00:00Z" }])
    const api = createApi(async () => "tok", fetch)
    const list = await api.listSessions()
    expect(list[0].id).toBe("s-1")
    const [url, init] = fetch.mock.calls[0] as [string, RequestInit]
    expect(url).toBe("/api/sessions")
    expect(new Headers(init.headers).get("Authorization")).toBe("Bearer tok")
  })

  it("posts a name to create and a stop action to patch", async () => {
    const fetch = fakeFetch(201, { id: "s-2", name: "b", owner: "u", state: "starting", created: "" })
    const api = createApi(async () => "tok", fetch)
    await api.createSession("b")
    await api.setRunning("s-2", false)
    const [, create] = fetch.mock.calls[0] as [string, RequestInit]
    expect(create.method).toBe("POST")
    expect(create.body).toBe(JSON.stringify({ name: "b" }))
    const [url, patch] = fetch.mock.calls[1] as [string, RequestInit]
    expect(url).toBe("/api/sessions/s-2")
    expect(patch.body).toBe(JSON.stringify({ action: "stop" }))
  })

  it("throws ApiError carrying the server's message", async () => {
    const api = createApi(async () => "tok", fakeFetch(409, { error: "session limit reached; delete one first" }))
    await expect(api.createSession("x")).rejects.toMatchObject({
      status: 409,
      message: "session limit reached; delete one first",
    })
    await expect(api.createSession("x")).rejects.toBeInstanceOf(ApiError)
  })

  it("accepts an empty 204 on delete", async () => {
    const api = createApi(async () => "tok", fakeFetch(204, undefined))
    await expect(api.deleteSession("s-1")).resolves.toBeUndefined()
  })
})
```

Run: `npm test` — Expected: FAIL, cannot find `./api`.

**Step 3: Implement `web/src/api.ts`**

```ts
export type SessionState = "starting" | "running" | "stopping" | "asleep" | "stopped" | "failed"

export interface Session {
  id: string
  name: string
  owner: string
  state: SessionState
  message?: string
  created: string
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message)
  }
}

type Fetch = typeof fetch

export function createApi(getToken: () => Promise<string | undefined>, fetchImpl: Fetch = fetch) {
  async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
    const headers = new Headers()
    const token = await getToken()
    if (token) headers.set("Authorization", `Bearer ${token}`)
    if (body !== undefined) headers.set("Content-Type", "application/json")
    const res = await fetchImpl(path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
    })
    if (!res.ok) {
      let message = `${res.status} ${res.statusText}`
      try {
        message = (await res.json()).error ?? message
      } catch {}
      throw new ApiError(res.status, message)
    }
    return (res.status === 204 ? undefined : await res.json()) as T
  }

  return {
    listSessions: (all = false) => call<Session[]>("GET", all ? "/api/sessions?all=1" : "/api/sessions"),
    getSession: (id: string) => call<Session>("GET", `/api/sessions/${id}`),
    createSession: (name: string) => call<Session>("POST", "/api/sessions", { name }),
    renameSession: (id: string, name: string) => call<Session>("PATCH", `/api/sessions/${id}`, { name }),
    setRunning: (id: string, running: boolean) =>
      call<Session>("PATCH", `/api/sessions/${id}`, { action: running ? "resume" : "stop" }),
    deleteSession: (id: string) => call<void>("DELETE", `/api/sessions/${id}`),
    vncTicket: async (id: string) =>
      (await call<{ ticket: string }>("POST", `/api/sessions/${id}/vnc-ticket`)).ticket,
  }
}

export type Api = ReturnType<typeof createApi>
```

Run: `npm test` — Expected: 4 passed.

**Step 4: Sign-in, following fleet's pattern**

`web/src/auth/keycloak.ts`:

```ts
// Keycloak singleton, initialised once by AuthProvider. Settings come from
// window.__BROWSERJS_CFG__, served by the backend at /config.js, so one build
// works in every environment.
import Keycloak from "keycloak-js"

declare global {
  interface Window {
    __BROWSERJS_CFG__?: { kcUrl?: string; kcRealm?: string; kcClientId?: string }
  }
}

const cfg = window.__BROWSERJS_CFG__ ?? {}

export const kc = new Keycloak({
  url: cfg.kcUrl ?? "http://localhost:8081",
  realm: cfg.kcRealm ?? "browserjs",
  clientId: cfg.kcClientId ?? "browserjs-spa",
})

let initialisation: Promise<boolean> | null = null

export function initKc(): Promise<boolean> {
  if (initialisation) return initialisation
  initialisation = kc
    .init({ onLoad: "login-required", flow: "standard", pkceMethod: "S256", checkLoginIframe: false })
    .then(authed => {
      // onTokenExpired fires after expiry; refresh ahead of it instead.
      kc.onTokenExpired = () => {
        kc.updateToken(30).catch(() => kc.login())
      }
      return authed
    })
    .catch(error => {
      initialisation = null
      throw error
    })
  return initialisation
}

// A fresh access token, refreshed if it is within 30s of expiry.
export async function getToken(): Promise<string | undefined> {
  if (!kc.authenticated) return undefined
  try {
    await kc.updateToken(30)
  } catch {
    await kc.login()
    return undefined
  }
  return kc.token
}

export const isAdmin = () => kc.hasRealmRole("admin")
export const username = () => (kc.tokenParsed?.preferred_username as string | undefined) ?? ""
```

`web/src/auth/AuthProvider.tsx`:

```tsx
// Gates rendering until Keycloak login resolves. With onLoad: "login-required"
// the user is redirected to Keycloak before the app mounts.
import { useEffect, useState } from "react"
import { initKc } from "./keycloak"

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const [ready, setReady] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    initKc()
      .then(authed => (authed ? setReady(true) : setError("Not authenticated")))
      .catch(e => setError(String(e)))
  }, [])

  if (error) {
    return (
      <main className="auth-error">
        <h1>We couldn&apos;t sign you in</h1>
        <code>{error}</code>
        <button type="button" onClick={() => window.location.reload()}>
          Try again
        </button>
      </main>
    )
  }
  if (!ready) return null
  return <>{children}</>
}
```

**Step 5: `web/src/main.tsx` and a placeholder `App.tsx`**

```tsx
import "@cloudscape-design/global-styles/index.css"
import React from "react"
import ReactDOM from "react-dom/client"
import { BrowserRouter } from "react-router-dom"
import { App } from "./App"
import { AuthProvider } from "./auth/AuthProvider"
import { applyWireframeTheme } from "./theme"
import "./wireframe.css"

applyWireframeTheme()

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <AuthProvider>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </AuthProvider>
  </React.StrictMode>,
)
```

```tsx
// web/src/App.tsx
import { Route, Routes } from "react-router-dom"
import { SessionDetail } from "./pages/SessionDetail"
import { SessionsList } from "./pages/SessionsList"

export function App() {
  return (
    <Routes>
      <Route path="/" element={<SessionsList />} />
      <Route path="/sessions/:id" element={<SessionDetail />} />
    </Routes>
  )
}
```

`theme.ts`, `wireframe.css` and the two pages arrive in Tasks 16–17; the build will not pass until then. Commit now anyway so the API client and its tests are saved:

```bash
git add web
git commit -m "feat(web): scaffold, Keycloak sign-in and API client"
```

### Task 16: Wireframe theme and the sessions list

**Files:**
- Create: `web/src/theme.ts`, `web/src/wireframe.css`
- Create: `web/src/shell.tsx`
- Create: `web/src/pages/SessionsList.tsx`
- Create: `web/src/pages/SessionDetail.tsx` (stub; filled in Task 17)

**Step 1: `web/src/theme.ts`** — strip colour out of Cloudscape: black ink on white paper, no fills.

```ts
import { applyTheme } from "@cloudscape-design/components/theming"

const ink = "#111111"
const paper = "#ffffff"
const pencil = "#6b6b6b"

export function applyWireframeTheme() {
  applyTheme({
    theme: {
      tokens: {
        fontFamilyBase: '"Comic Neue", "Chalkboard SE", "Comic Sans MS", "Segoe Print", cursive',
        colorBackgroundLayoutMain: paper,
        colorBackgroundContainerContent: paper,
        colorBackgroundContainerHeader: paper,
        colorTextBodyDefault: ink,
        colorTextBodySecondary: pencil,
        colorTextHeadingDefault: ink,
        colorTextLinkDefault: ink,
        colorTextLinkHover: ink,
        colorBorderDividerDefault: ink,
        colorBorderDividerSecondary: pencil,
        colorBackgroundButtonPrimaryDefault: paper,
        colorBackgroundButtonPrimaryHover: "#eeeeee",
        colorBackgroundButtonPrimaryActive: "#dddddd",
        colorTextButtonPrimaryDefault: ink,
        colorTextButtonPrimaryHover: ink,
        colorTextButtonPrimaryActive: ink,
        colorBorderButtonPrimaryDefault: ink,
        colorBorderButtonNormalDefault: ink,
        colorTextButtonNormalDefault: ink,
        colorBorderInputDefault: ink,
        colorBorderItemFocused: ink,
        borderRadiusButton: "2px",
        borderRadiusContainer: "2px",
        borderRadiusInput: "2px",
      },
    },
  })
}
```

**Step 2: `web/src/wireframe.css`** — the sketch: uneven hand-drawn outlines, dashed rules, hatching for empty areas, underlined links, no shadows.

```css
:root {
  --ink: #111;
  --pencil: #6b6b6b;
  /* An uneven radius reads as hand-drawn. */
  --wobble: 255px 15px 225px 15px / 15px 225px 15px 255px;
}

body {
  background: #fff;
  color: var(--ink);
}

/* Boxes: outlined, never filled or shadowed. */
.wf-box,
[class*="awsui_container_"],
[class*="awsui_dialog_"] {
  border: 2px solid var(--ink) !important;
  border-radius: var(--wobble) !important;
  box-shadow: none !important;
  background: #fff !important;
}
[class*="awsui_button_"] {
  border-radius: var(--wobble) !important;
  border-width: 2px !important;
  box-shadow: none !important;
}
[class*="awsui_container_"]::before,
[class*="awsui_container_"]::after {
  box-shadow: none !important;
  border: 0 !important;
}

a {
  color: var(--ink);
  text-decoration: underline;
}

.wf-header {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  padding: 12px 20px;
  border-bottom: 2px dashed var(--ink);
}
.wf-header h1 {
  margin: 0;
  font-size: 22px;
}
.wf-header button {
  background: none;
  border: 0;
  font: inherit;
  text-decoration: underline;
  cursor: pointer;
}
.wf-main {
  max-width: 1200px;
  margin: 0 auto;
  padding: 20px;
}

/* State as text in a box, not a coloured badge. */
.wf-state {
  display: inline-block;
  padding: 0 8px;
  border: 1.5px solid var(--ink);
  border-radius: var(--wobble);
  font-size: 13px;
}
.wf-state[data-state="failed"] {
  border-style: double;
  border-width: 4px;
  font-weight: bold;
}
.wf-state[data-state="asleep"],
.wf-state[data-state="stopped"] {
  border-style: dashed;
  color: var(--pencil);
}

/* Placeholder area: diagonal hatching, like an unfilled wireframe region. */
.wf-placeholder {
  display: grid;
  place-items: center;
  min-height: 420px;
  border: 2px dashed var(--ink);
  border-radius: var(--wobble);
  background: repeating-linear-gradient(45deg, #fff, #fff 10px, #f1f1f1 10px, #f1f1f1 11px);
  text-align: center;
}

.wf-screen {
  border: 2px solid var(--ink);
  border-radius: 4px;
  background: #fff;
  aspect-ratio: 1280 / 800;
  width: 100%;
  overflow: hidden;
}
.wf-mono {
  font-family: ui-monospace, Menlo, monospace;
  font-size: 13px;
  word-break: break-all;
}

.auth-error {
  max-width: 480px;
  margin: 80px auto;
  padding: 24px;
  border: 2px solid var(--ink);
  border-radius: var(--wobble);
}
```

The `[class*="awsui_…"]` selectors lean on Cloudscape's generated class names, which is fragile across Cloudscape upgrades. After any upgrade, look at the list page and confirm boxes are still outlined and unshadowed.

**Step 3: `web/src/shell.tsx`**

```tsx
import { Link } from "react-router-dom"
import { createApi } from "./api"
import { getToken, kc, username } from "./auth/keycloak"

export const api = createApi(getToken)

export function Shell({ children }: { children: React.ReactNode }) {
  return (
    <>
      <header className="wf-header">
        <h1>
          <Link to="/">browserjs sessions</Link>
        </h1>
        <span>
          {username()} · <button onClick={() => kc.logout({ redirectUri: window.location.origin })}>sign out</button>
        </span>
      </header>
      <main className="wf-main">{children}</main>
    </>
  )
}

export function StateTag({ state }: { state: string }) {
  return (
    <span className="wf-state" data-state={state}>
      {state}
    </span>
  )
}
```

**Step 4: `web/src/pages/SessionsList.tsx`**

Polls every 3 seconds so state changes (starting → running, running → asleep) show without a reload.

```tsx
import Box from "@cloudscape-design/components/box"
import Button from "@cloudscape-design/components/button"
import FormField from "@cloudscape-design/components/form-field"
import Header from "@cloudscape-design/components/header"
import Input from "@cloudscape-design/components/input"
import Modal from "@cloudscape-design/components/modal"
import SpaceBetween from "@cloudscape-design/components/space-between"
import Table from "@cloudscape-design/components/table"
import Toggle from "@cloudscape-design/components/toggle"
import { useCallback, useEffect, useState } from "react"
import { Link, useNavigate } from "react-router-dom"
import type { Session } from "../api"
import { isAdmin } from "../auth/keycloak"
import { Shell, StateTag, api } from "../shell"

export function SessionsList() {
  const navigate = useNavigate()
  const [sessions, setSessions] = useState<Session[] | null>(null)
  const [error, setError] = useState("")
  const [showAll, setShowAll] = useState(false)
  const [creating, setCreating] = useState(false)
  const [name, setName] = useState("")
  const [createError, setCreateError] = useState("")
  const [busy, setBusy] = useState(false)

  const load = useCallback(() => {
    api
      .listSessions(showAll)
      .then(list => {
        setSessions(list.sort((a, b) => b.created.localeCompare(a.created)))
        setError("")
      })
      .catch(e => setError(String(e.message ?? e)))
  }, [showAll])

  useEffect(() => {
    load()
    const timer = setInterval(load, 3000)
    return () => clearInterval(timer)
  }, [load])

  async function create() {
    setBusy(true)
    setCreateError("")
    try {
      const session = await api.createSession(name)
      navigate(`/sessions/${session.id}`)
    } catch (e) {
      setCreateError(String((e as Error).message))
    } finally {
      setBusy(false)
    }
  }

  async function act(fn: () => Promise<unknown>) {
    try {
      await fn()
    } catch (e) {
      setError(String((e as Error).message))
    }
    load()
  }

  return (
    <Shell>
      <Table
        loading={sessions === null}
        loadingText="Loading sessions"
        items={sessions ?? []}
        trackBy="id"
        header={
          <Header
            counter={sessions ? `(${sessions.length})` : undefined}
            actions={
              <SpaceBetween direction="horizontal" size="s" alignItems="center">
                {isAdmin() && (
                  <Toggle checked={showAll} onChange={e => setShowAll(e.detail.checked)}>
                    everyone&apos;s
                  </Toggle>
                )}
                <Button variant="primary" onClick={() => setCreating(true)}>
                  New session
                </Button>
              </SpaceBetween>
            }
          >
            Sessions
          </Header>
        }
        columnDefinitions={[
          { id: "name", header: "Name", cell: s => <Link to={`/sessions/${s.id}`}>{s.name}</Link> },
          { id: "state", header: "State", cell: s => <StateTag state={s.state} /> },
          ...(showAll ? [{ id: "owner", header: "Owner", cell: (s: Session) => <span className="wf-mono">{s.owner}</span> }] : []),
          { id: "created", header: "Created", cell: s => new Date(s.created).toLocaleString() },
          {
            id: "actions",
            header: "",
            cell: s => (
              <SpaceBetween direction="horizontal" size="xs">
                {s.state === "running" || s.state === "starting" ? (
                  <Button onClick={() => act(() => api.setRunning(s.id, false))}>Stop</Button>
                ) : (
                  <Button onClick={() => act(() => api.setRunning(s.id, true))}>Resume</Button>
                )}
                <Button
                  onClick={() => {
                    if (window.confirm(`Delete "${s.name}" and its disk? This cannot be undone.`))
                      act(() => api.deleteSession(s.id))
                  }}
                >
                  Delete
                </Button>
              </SpaceBetween>
            ),
          },
        ]}
        empty={
          <Box textAlign="center" padding="l">
            {error ? `Couldn't load sessions: ${error}` : "No sessions yet. Create one to get a browser."}
          </Box>
        }
      />
      {error && sessions && sessions.length > 0 && <Box padding={{ top: "s" }}>⚠ {error}</Box>}

      <Modal
        visible={creating}
        onDismiss={() => setCreating(false)}
        header="New session"
        footer={
          <Box float="right">
            <SpaceBetween direction="horizontal" size="xs">
              <Button onClick={() => setCreating(false)}>Cancel</Button>
              <Button variant="primary" loading={busy} disabled={!name.trim()} onClick={create}>
                Create
              </Button>
            </SpaceBetween>
          </Box>
        }
      >
        <FormField label="Name" errorText={createError}>
          <Input value={name} onChange={e => setName(e.detail.value)} autoFocus />
        </FormField>
      </Modal>
    </Shell>
  )
}
```

**Step 5: Stub `web/src/pages/SessionDetail.tsx`** so the build passes:

```tsx
import { Shell } from "../shell"

export function SessionDetail() {
  return <Shell>detail</Shell>
}
```

**Step 6: Verify**

Run: `npm test && npm run build`
Expected: tests pass; `tsc` reports no errors; `dist/` is produced.

**Step 7: Commit**

```bash
git add web
git commit -m "feat(web): wireframe theme and sessions list"
```

### Task 17: Session detail and VNC view

**Files:**
- Create: `web/src/components/VncPane.tsx`
- Create: `web/src/types/novnc.d.ts`
- Modify: `web/src/pages/SessionDetail.tsx`

**Step 1: `web/src/types/novnc.d.ts`** (noVNC ships no types)

```ts
declare module "@novnc/novnc" {
  export default class RFB extends EventTarget {
    constructor(target: HTMLElement, url: string, options?: Record<string, unknown>)
    scaleViewport: boolean
    resizeSession: boolean
    viewOnly: boolean
    disconnect(): void
  }
}
```

**Step 2: `web/src/components/VncPane.tsx`**

Fetches a one-time ticket, connects noVNC to `/s/{id}/vnc`, and reconnects by itself (with a fresh ticket each time) after a drop.

```tsx
import RFB from "@novnc/novnc"
import { useEffect, useRef, useState } from "react"
import { api } from "../shell"

type Status = "connecting" | "connected" | "reconnecting"

export function VncPane({ sessionId }: { sessionId: string }) {
  const screenRef = useRef<HTMLDivElement>(null)
  const [status, setStatus] = useState<Status>("connecting")

  useEffect(() => {
    let rfb: RFB | null = null
    let stopped = false
    let retry: ReturnType<typeof setTimeout> | undefined

    async function connect() {
      if (stopped || !screenRef.current) return
      try {
        const ticket = await api.vncTicket(sessionId)
        if (stopped || !screenRef.current) return
        const proto = window.location.protocol === "https:" ? "wss:" : "ws:"
        const url = `${proto}//${window.location.host}/s/${sessionId}/vnc?ticket=${ticket}`
        rfb = new RFB(screenRef.current, url, {})
        rfb.scaleViewport = true
        rfb.resizeSession = false
        rfb.addEventListener("connect", () => setStatus("connected"))
        rfb.addEventListener("disconnect", () => {
          rfb = null
          if (stopped) return
          setStatus("reconnecting")
          retry = setTimeout(connect, 2000)
        })
      } catch {
        if (stopped) return
        setStatus("reconnecting")
        retry = setTimeout(connect, 2000)
      }
    }

    connect()
    return () => {
      stopped = true
      clearTimeout(retry)
      rfb?.disconnect()
    }
  }, [sessionId])

  return (
    <div>
      <div ref={screenRef} className="wf-screen" />
      {status !== "connected" && (
        <p>{status === "connecting" ? "Connecting to the browser…" : "Connection lost — reconnecting…"}</p>
      )}
    </div>
  )
}
```

**Step 3: `web/src/pages/SessionDetail.tsx`**

The VNC pane is only mounted while the session is running: opening a ticket wakes a sleeping session, and that should be the user's choice (the "Wake" button), not a side effect of looking at the page.

```tsx
import Box from "@cloudscape-design/components/box"
import Button from "@cloudscape-design/components/button"
import ColumnLayout from "@cloudscape-design/components/column-layout"
import Container from "@cloudscape-design/components/container"
import Header from "@cloudscape-design/components/header"
import Input from "@cloudscape-design/components/input"
import SpaceBetween from "@cloudscape-design/components/space-between"
import { useCallback, useEffect, useState } from "react"
import { useNavigate, useParams } from "react-router-dom"
import type { Session } from "../api"
import { ApiError } from "../api"
import { VncPane } from "../components/VncPane"
import { Shell, StateTag, api } from "../shell"

const PLACEHOLDER: Record<string, string> = {
  starting: "Starting the browser…",
  stopping: "Stopping…",
  asleep: "Asleep. It wakes when you or an agent uses it.",
  stopped: "Stopped.",
  failed: "The session failed to start.",
}

export function SessionDetail() {
  const { id = "" } = useParams()
  const navigate = useNavigate()
  const [session, setSession] = useState<Session | null>(null)
  const [missing, setMissing] = useState(false)
  const [error, setError] = useState("")
  const [name, setName] = useState<string | null>(null) // non-null while editing
  const [copied, setCopied] = useState(false)

  const load = useCallback(() => {
    api
      .getSession(id)
      .then(s => {
        setSession(s)
        setError("")
      })
      .catch(e => (e instanceof ApiError && e.status === 404 ? setMissing(true) : setError(String(e.message))))
  }, [id])

  useEffect(() => {
    load()
    const timer = setInterval(load, 3000)
    return () => clearInterval(timer)
  }, [load])

  if (missing) {
    return (
      <Shell>
        <Box padding="l">This session doesn&apos;t exist, or isn&apos;t yours.</Box>
      </Shell>
    )
  }
  if (!session) return <Shell>{error || "Loading…"}</Shell>

  const mcpUrl = `${window.location.origin}/s/${session.id}/mcp`
  const awake = session.state === "running" || session.state === "starting"

  async function act(fn: () => Promise<unknown>) {
    try {
      await fn()
      setError("")
    } catch (e) {
      setError(String((e as Error).message))
    }
    load()
  }

  return (
    <Shell>
      <SpaceBetween size="l">
        <Header
          variant="h1"
          actions={
            <SpaceBetween direction="horizontal" size="xs">
              {awake ? (
                <Button onClick={() => act(() => api.setRunning(session.id, false))}>Stop</Button>
              ) : (
                <Button variant="primary" onClick={() => act(() => api.setRunning(session.id, true))}>
                  {session.state === "asleep" ? "Wake" : "Resume"}
                </Button>
              )}
              <Button
                onClick={async () => {
                  if (!window.confirm(`Delete "${session.name}" and its disk? This cannot be undone.`)) return
                  await api.deleteSession(session.id)
                  navigate("/")
                }}
              >
                Delete
              </Button>
            </SpaceBetween>
          }
        >
          {name === null ? (
            <>
              {session.name}{" "}
              <Button variant="inline-link" onClick={() => setName(session.name)}>
                rename
              </Button>
            </>
          ) : (
            <SpaceBetween direction="horizontal" size="xs">
              <Input value={name} onChange={e => setName(e.detail.value)} autoFocus />
              <Button
                disabled={!name.trim()}
                onClick={() => act(() => api.renameSession(session.id, name)).then(() => setName(null))}
              >
                Save
              </Button>
              <Button variant="link" onClick={() => setName(null)}>
                Cancel
              </Button>
            </SpaceBetween>
          )}
        </Header>

        {error && <Box>⚠ {error}</Box>}

        {session.state === "running" ? (
          <VncPane sessionId={session.id} />
        ) : (
          <div className="wf-placeholder">
            <div>
              <p>{PLACEHOLDER[session.state]}</p>
              {session.message && <p className="wf-mono">{session.message}</p>}
            </div>
          </div>
        )}

        <Container header={<Header variant="h2">Details</Header>}>
          <ColumnLayout columns={3} variant="text-grid">
            <div>
              <Box variant="awsui-key-label">State</Box>
              <StateTag state={session.state} />
            </div>
            <div>
              <Box variant="awsui-key-label">Created</Box>
              {new Date(session.created).toLocaleString()}
            </div>
            <div>
              <Box variant="awsui-key-label">Owner</Box>
              <span className="wf-mono">{session.owner}</span>
            </div>
          </ColumnLayout>
          <Box margin={{ top: "m" }}>
            <Box variant="awsui-key-label">MCP URL — add this to Claude as a connector</Box>
            <SpaceBetween direction="horizontal" size="xs" alignItems="center">
              <span className="wf-mono">{mcpUrl}</span>
              <Button
                onClick={() => {
                  void navigator.clipboard.writeText(mcpUrl)
                  setCopied(true)
                  setTimeout(() => setCopied(false), 1500)
                }}
              >
                {copied ? "Copied" : "Copy"}
              </Button>
            </SpaceBetween>
          </Box>
        </Container>
      </SpaceBetween>
    </Shell>
  )
}
```

**Step 4: Verify**

Run: `npm test && npm run build`
Expected: tests pass, no type errors, `dist/` produced. If `tsc` rejects a Cloudscape prop (for example `alignItems` on `SpaceBetween` in the installed version), check the component's props with the `cloudscape-design` skill and adjust; do not add `any` casts.

**Step 5: Commit**

```bash
git add web
git commit -m "feat(web): session detail with live VNC view"
```

The UI is exercised for real in Task 20.

---

## Phase 4 — Deployment and local integration (done)

What was built; the commands are in `docs/local-development.md`.

### Task 18: Backend image and Kubernetes manifests

- `Dockerfile` (repo root): builds `web/`, builds `backend/cmd/server` with the Go version of `backend/go.mod`, ships both in distroless static, non-root.
- `deploy/base` (kustomize), namespace `browserjs-sessions`:
  - `backend.yaml`: ServiceAccount; Role limited to `sandboxes` get/list/create/update/patch/delete (the store never watches, and needs nothing on PVCs: a session's PVC is deleted with its Sandbox); Deployment with one replica and `Recreate`; Service.
  - `blueprint.yaml` (a ConfigMap file): the session's Sandbox spec, a template over `.ID`, `.SessionURL`, `.PublicURL`. Both containers with HTTP probes, `fsGroup: 1000` for mcp-js, a memory-backed `/dev/shm`, 30 s termination grace, one volume claim mounted at `chrome`, `memory` and `mcp` sub-paths, no service account token.
  - `networkpolicy.yaml`: session pods accept only the backend (6080, 8080) and reach only DNS and the internet (private ranges, CGNAT and link-local excluded); the backend accepts only Pomerium.
  - `pomerium.yaml`, `pomerium-config.yaml`: Pomerium Core v0.33.3 as a one-replica StatefulSet configured from a file, databroker on a PVC, `mcp_allowed_client_id_domains: [claude.ai]`. The app route and the sessions' MCP route admit only a list of email addresses (one policy, written in each overlay's config); the upload, VNC and discovery routes are public. Core with a config file rather than the ingress controller, because the routes are static, wildcard hosts are supported there (only "unofficially" through Ingress), and route order is explicit.
  - `dex.yaml`, `dex-config.yaml`: Dex v2.45.1, Google and GitHub connectors, state in Kubernetes custom resources.
  - `secrets.example.yaml`: the three Secrets the deployment expects (`dex-oauth`, `pomerium`, `pomerium-tls`), placeholders only, not part of the kustomization.
- `deploy/local`: its own copy of the blueprint (see "What the local run found", item 2), local hostnames, NodePorts, the throwaway CA for the backend, test users in Dex (`alice@`, `bob@`, `admin@example.com`, and `mallory@example.com` who is not on Pomerium's list; password `test`), the list of who may sign in, and a sidecar that gives the Pomerium pod a `localhost:5556` leading to Dex.
- `deploy/local-test`: `deploy/local` with the backend trusting the integration test's own signing keys. Applied and removed by the test.

### Task 19: Local cluster

`hack/local-up.sh` (idempotent) and `hack/local-down.sh`: kind cluster `browserjs` with its own kubeconfig in `.local/`, Agent Sandbox, the three images, a throwaway CA, the Secrets (OAuth credentials read from the macOS Keychain at run time), `deploy/local`.

Agent Sandbox is **v1.0.4**: v1.0.5 was released on 2026-10-01 without its controller image. `SANDBOX_VERSION=v1.0.5 hack/local-up.sh` once it is published.

### Task 20: End-to-end tests

- `test/integration.py` (Python standard library): the backend and session pods without Pomerium, with assertions the test signs. 20 checks, all passing on 2026-10-01: identity, create, ownership, MCP (initialize, tools, `run_js`, the browser), refusals (403, never 401), the GET stream rule, upload, VNC (ticket, upgrade, RFB banner), stop, resume with tab, memory file and artifact intact, idle sleep and wake, delete with Sandbox, pod and PVC gone.
- `test/browser-e2e.mjs`: the UI through Pomerium and Dex in a headless Chrome, a user who is not on the list (403 from Pomerium), the public routes, and `test/mcp-client.mjs` for the owner and for another user. 12 checks, all passing.
- `test/mcp-client.mjs`: the MCP SDK's client, with its own OAuth support, against a session through Pomerium. Passes for the owner; a second user is refused with 404.
- `test/mcp-oauth.mjs`: the same sign-in walked request by request, for looking at each step.

Measured locally (kind on colima, 6 CPU / 12 GB): session start 6 to 9 s; wake from idle 3 to 4 s; resume after stop 3 s; stop 1 s; delete, until the PVC is gone, 3 s.

### What the local run found

1. **Pomerium does not serve MCP discovery on wildcard hosts** (worked around in the backend). Pomerium v0.33.3 answers 401 with a `resource_metadata` pointer on `<id>.sessions…/mcp`, but serves `/.well-known/oauth-protected-resource` and `/.well-known/oauth-authorization-server` only on hosts that have an exact (non-wildcard) route: `config/envoyconfig/route_configurations.go` leaves hosts containing `*` out of the per-host virtual hosts. Everything the documents point to (`/.pomerium/mcp/authorize`, `/.pomerium/mcp/token`, MCP calls with the token) works on a wildcard host. So the backend serves the two documents on session hosts (`backend/internal/proxy/metadata.go`), with the content Pomerium serves on an exact-route host, behind a fourth, public wildcard route for `/.well-known/oauth-`. Verified with the MCP SDK's own client (`test/mcp-client.mjs`): discovery, authorization, token, `initialize`, `tools/list`, `run_js`; a second user gets a token and then 404. No upstream issue was found for this; the workaround should go when Pomerium serves the documents itself. The alternative, per-session exact-host routes, was also shown to work by hand but needs three routes per session created through Pomerium's config API, and was not built.
2. **x11vnc never answered a viewer in the cluster** (fixed). Under kind's containerd a container's open-file limit is about a billion; x11vnc walks all of them when a viewer connects. `images/browser/browser/entrypoint.sh` now lowers the limit itself. The local `browserjs/browser:dev` image was built before that and was not rebuilt (a build takes about 14 GB of disk), so `deploy/local/blueprint.yaml` is a copy of the base blueprint that sets the limit in a `command`. Delete that file after the next image build.
3. **Stopping a session took 30 s** (fixed). mcp-v8 has no SIGTERM handler and was PID 1 in its container, where an unhandled signal does nothing, so every stop and delete waited for the kill at the end of the grace period. `images/mcp-js/start.sh` now runs it as a child and passes the signal on. This exposed a small gap, not fixed: for up to 2 s after a stop the proxy still takes the pod to be there, and an MCP call in that window gets 502 instead of 409.
4. Verified facts about Pomerium that the research had left open: the assertion reaches the backend on normal and MCP routes; its `aud` is the bare hostname; an API `fetch` with `Accept: application/json` and no session gets 401 JSON, with `*/*` a 302 (the UI handles both); the public upload and VNC routes need no session, and a websocket works through the VNC route; a 15 MiB upload passes and a 20 MiB one gets the backend's 413; an MCP token is not bound to a host (the backend's owner check is what protects a session); cookies are per host.

---

## Phase 5 — GKE (not started here)

The cluster exists (project `browserjs-sessions`, cluster `browserjs` in us-west1-a, Kubernetes 1.35.8; registry `us-west1-docker.pkg.dev/browserjs-sessions/browserjs`; a reserved IP; DNS for `app.`, `authenticate.`, `dex.` and `*.sessions.browserjs.com` delegated to Cloud DNS). Its infrastructure code is in `infra/` on `main`. Nothing of this application has been deployed to it, and nothing below is verified.

**Images are built in CI, not on this Mac.** GKE's nodes are amd64 and the local images are arm64. The next browser image build picks up the entrypoint's file-limit fix.

### Task 21: Chromium under gVisor

1. Confirm `kubectl get crd sandboxes.agents.x-k8s.io` serves `v1beta1`; if not, change `SandboxGVR` in `backend/internal/sessions/session.go`.
2. Add `deploy/gke/` as an overlay on `deploy/base`: the real hostnames in the backend's environment, in `pomerium-config.yaml` and in `dex-config.yaml`; registry image names (the blueprint names its images itself, so the overlay replaces `blueprint.yaml`); in the blueprint, `runtimeClassName: gvisor` and what the GKE Agent Sandbox documentation's template shows.
3. The browser image runs as root and needs a writable root filesystem. Either make it run as a non-root user or relax the hardening policy for the namespace and record why.
4. Create one session and judge it: does Chromium start under gVisor, is the screen usable, memory, page load time. Check the container's open-file limit there too. If Chromium does not run acceptably under gVisor, stop and report: the alternatives change the security design.

### Task 22: Hostnames, TLS, sign-in

1. Hostnames: the app, `authenticate.`, `dex.` and `*.sessions.` on a domain. Sessions should be on a different registrable domain from the app, so a session's content cannot set cookies the app receives.
2. A certificate covering all four (the wildcard needs a DNS challenge), as the `pomerium-tls` Secret; the `pomerium` Service exposed by a load balancer that passes websockets and long-lived responses without a short timeout.
3. `POMERIUM_JWKS_URL`: the base points it at the app's public hostname. Check the backend can reach that from inside the cluster, or serve the keys another way.
4. Dex: a Google web client and a GitHub OAuth app with callback `https://dex.<domain>/callback`, in the `dex-oauth` Secret. Dex is reached through a public Pomerium route (already in the base config); Pomerium itself talks to that public URL.
5. **Who may sign in.** Put the real list of email addresses in the overlay's Pomerium config (the base has a placeholder). A GitHub user is known by their primary verified email.
6. Add a session's MCP URL to Claude as a connector and run a `run_js` call. The research notes an open Pomerium issue (#6675) about connectors authorized this way showing no tools in Claude Code cloud sessions.
7. Verify NetworkPolicy is enforced: from a session pod, the backend, the API server and `169.254.169.254` must be unreachable and `https://example.com` reachable.

### Task 23: Pod Snapshots for exact tab preservation

Unchanged in intent: read the GKE Pod Snapshots documentation, add a `Snapshotter` to `backend/internal/sessions` (no-op locally, where Chromium session restore does the job), snapshot before suspend, fall back to a plain suspend on failure, bound retention. If a headed, multi-process browser cannot be snapshotted reliably, say so and keep session restore as the only mechanism.

### Task 24: Scale to zero at the node level

Session node pool with a minimum of zero; the backend, Pomerium and Dex on a pool that does not scale to zero. Measure a cold wake (first MCP call to response) and compare it with what MCP clients tolerate; `READY_TIMEOUT` is 3 minutes.

---

## Open items for the user

- **Claude as the MCP client is untested.** The MCP SDK's client signs in and works against a local session; Claude's hosted apps cannot reach a local cluster, and Claude Code was not tried.
- **Who may sign in** on GKE: the list of emails (Task 22 step 5). Changing it means editing the Pomerium config.
- **Idle period.** 15 minutes (`IDLE_AFTER`), assumed, not confirmed.
- **Hostnames** for the GKE deployment, and whether sessions get their own registrable domain.
- **Root or non-root browser image** on GKE (Task 21 step 3).
- **A 502 for 2 s after a stop.** The proxy remembers a running pod for 2 s; an MCP call just after a stop gets 502 rather than 409.
- **Admins are a static list** read at startup; owners are emails, so an email change at the identity provider orphans a user's sessions.
- **Events on the detail page.** The page shows the sandbox's status message, not cluster events.
- Real Google and GitHub sign-in has only been checked as far as the redirect to each provider; nobody has completed one.
