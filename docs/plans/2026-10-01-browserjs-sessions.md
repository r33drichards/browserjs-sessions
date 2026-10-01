# browserjs sessions Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** A web app where signed-in users create, view (live VNC), stop and delete per-user "browserjs sessions" — a persistent Chromium plus its own mcp-js server — run as Kubernetes Agent Sandbox resources that sleep when idle and wake on demand.

**Architecture:** One Go backend holds the only Kubernetes credentials. It verifies Keycloak tokens, asks Topaz whether the caller may act on a session, creates/suspends/deletes one `Sandbox` per session, and reverse-proxies each session's VNC websocket and MCP endpoint. A React + Cloudscape UI (wireframe theme) talks only to the backend.

**Tech Stack:** Go (stdlib `net/http`, `client-go` dynamic client, `golang-jwt`, `keyfunc`, `go-aserto`), React 18 + Vite + TypeScript, Cloudscape, `keycloak-js`, `@novnc/novnc`, Keycloak, Topaz, `kubernetes-sigs/agent-sandbox` v1.0.5, kind + colima for local, GKE Agent Sandbox + Pod Snapshots for production. Nix dev shell.

Design: `docs/plans/2026-10-01-browserjs-sessions-design.md`. Read it first.

---

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

### Task 12: Topaz authorizer

**Files:**
- Create: `deploy/base/topaz/manifest.yaml`
- Modify: `backend/internal/authz/authz.go` (add `ListSessions`)
- Modify: `backend/internal/authz/memory.go`, `memory_test.go`
- Create: `backend/internal/authz/reconcile.go`
- Test: `backend/internal/authz/reconcile_test.go`
- Create: `backend/internal/authz/topaz.go`
- Test: `backend/internal/authz/topaz_test.go` (runs only when `TOPAZ_TEST_ADDR` is set)
- Modify: `backend/cmd/server/main.go`

**Step 1: Write the directory model `deploy/base/topaz/manifest.yaml`**

```yaml
# yaml-language-server: $schema=https://www.topaz.sh/schema/manifest.json
---
model:
  version: 3

types:
  user: {}

  group:
    relations:
      member: user

  # A browserjs session. `viewer` is unused by the UI today; it is what
  # sharing will write to.
  session:
    relations:
      owner: user
      viewer: user | group#member
      admin: group#member
    permissions:
      can_manage: owner | admin
      can_view: can_manage | viewer
```

Every session gets `admin` → `group:admins#member`, so admin access is a relation like any other.

**Step 2: Extend the interface (TDD)**

Add to `memory_test.go`:

```go
func TestMemoryListSessions(t *testing.T) {
	ctx := t.Context()
	a := NewMemory()
	_ = a.AddSession(ctx, "s-2", "u")
	_ = a.AddSession(ctx, "s-1", "u")
	got, err := a.ListSessions(ctx)
	if err != nil || len(got) != 2 || got[0] != "s-1" || got[1] != "s-2" {
		t.Errorf("ListSessions = %v, %v", got, err)
	}
}
```

Run: `go test ./internal/authz/` — Expected: FAIL, `a.ListSessions undefined`.

Add to the `Authorizer` interface in `authz.go`:

```go
	// ListSessions returns every session ID the authorizer knows, sorted.
	ListSessions(ctx context.Context) ([]string, error)
```

Add to `memory.go` (import `slices` and `maps`):

```go
func (m *Memory) ListSessions(context.Context) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return slices.Sorted(maps.Keys(m.owner)), nil
}
```

Run: `go test ./internal/authz/` — Expected: `ok`.

**Step 3: Reconcile (TDD)**

The cluster is the record of truth. Reconcile makes the directory match it.

`reconcile_test.go`:

```go
package authz

import "testing"

func TestReconcile(t *testing.T) {
	ctx := t.Context()
	a := NewMemory()
	_ = a.AddSession(ctx, "s-stale", "old-owner") // in the directory, gone from the cluster
	_ = a.AddSession(ctx, "s-kept", "alice")

	cluster := map[string]string{"s-kept": "alice", "s-new": "bob"} // session → owner
	if err := Reconcile(ctx, a, cluster); err != nil {
		t.Fatal(err)
	}
	if ok, _ := a.Check(ctx, "old-owner", "s-stale", View); ok {
		t.Error("stale session not removed")
	}
	if ok, _ := a.Check(ctx, "bob", "s-new", Manage); !ok {
		t.Error("missing session not added")
	}
	if ok, _ := a.Check(ctx, "alice", "s-kept", Manage); !ok {
		t.Error("existing session lost")
	}
}
```

`reconcile.go`:

```go
package authz

import "context"

// Reconcile makes the authorizer's sessions match the cluster's
// (session ID → owner): missing ones are added, leftovers removed.
func Reconcile(ctx context.Context, a Authorizer, cluster map[string]string) error {
	known, err := a.ListSessions(ctx)
	if err != nil {
		return err
	}
	for _, id := range known {
		if _, ok := cluster[id]; !ok {
			if err := a.RemoveSession(ctx, id); err != nil {
				return err
			}
		}
	}
	for id, owner := range cluster {
		if err := a.AddSession(ctx, id, owner); err != nil { // idempotent
			return err
		}
	}
	return nil
}
```

Run: `go test ./internal/authz/` — Expected: `ok`. Commit:

```bash
git add deploy/base/topaz backend/internal/authz
git commit -m "feat(authz): directory model, session listing and reconcile"
```

**Step 4: VERIFY the Topaz Go client API before writing `topaz.go`**

The code in Step 6 was written from the `go-aserto` README, not compiled. Confirm names first:

```bash
cd backend
go get github.com/aserto-dev/go-aserto@latest github.com/aserto-dev/go-directory@latest
go doc github.com/aserto-dev/go-aserto/ds/v3
go doc github.com/aserto-dev/go-aserto | grep -i -E "WithAddr|WithInsecure|WithNoTLS|APIKey"
go doc github.com/aserto-dev/go-directory/aserto/directory/reader/v3 CheckRequest
go doc github.com/aserto-dev/go-directory/aserto/directory/writer/v3 | grep -E "^type .*Request"
go doc github.com/aserto-dev/go-directory/aserto/directory/reader/v3 GetObjectsRequest
```

Establish, and write down in a comment at the top of `topaz.go`:

1. The constructor and the option for a plaintext (no TLS) in-cluster connection.
2. Field names on `CheckRequest`, `SetObjectRequest`, `SetRelationRequest`, `DeleteRelationRequest`, `DeleteObjectRequest` (does it have `WithRelations`?), `GetObjectsRequest` and its pagination.
3. What `Check` returns for an object that does not exist: `false`, or a NotFound error.

Adjust Step 6 to match. The contract test in Step 5 is what proves it.

**Step 5: Start a local Topaz and write the contract test**

Start Topaz (needs a container runtime; see Task 19 for colima):

```bash
mkdir -p /tmp/topaz-test/{cfg,db,certs}
docker run -d --name topaz-test -p 9292:9292 \
  -v /tmp/topaz-test/cfg:/config -v /tmp/topaz-test/db:/db -v /tmp/topaz-test/certs:/certs \
  ghcr.io/aserto-dev/topaz:0.33.22 run --config-file /config/config.yaml
```

The config file is `deploy/base/topaz/config.yaml` from Task 18 (copy it into `/tmp/topaz-test/cfg/`). Load the model with the `topaz` CLI inside the container — check the exact subcommand with `docker exec topaz-test topaz directory set manifest --help` (it is `topaz directory set manifest <file>` in v0.33; flags for host and plaintext vary by version).

`topaz_test.go` runs the same expectations as the memory test against the real thing:

```go
package authz

import (
	"os"
	"testing"
)

func TestTopazMatchesMemoryBehaviour(t *testing.T) {
	addr := os.Getenv("TOPAZ_TEST_ADDR")
	if addr == "" {
		t.Skip("set TOPAZ_TEST_ADDR (e.g. localhost:9292) to run against a real Topaz")
	}
	ctx := t.Context()
	a, err := NewTopaz(addr)
	if err != nil {
		t.Fatal(err)
	}
	owner, admin, stranger := "t-owner", "t-admin", "t-stranger"
	t.Cleanup(func() {
		_ = a.RemoveSession(ctx, "s-contract")
		_ = a.SetAdmin(ctx, admin, false)
	})

	if err := a.SetAdmin(ctx, admin, true); err != nil {
		t.Fatal(err)
	}
	if err := a.AddSession(ctx, "s-contract", owner); err != nil {
		t.Fatal(err)
	}
	if err := a.AddSession(ctx, "s-contract", owner); err != nil {
		t.Fatalf("AddSession must be idempotent: %v", err)
	}
	for _, c := range []struct {
		user string
		perm Permission
		want bool
	}{
		{owner, View, true}, {owner, Manage, true},
		{admin, View, true}, {admin, Manage, true},
		{stranger, View, false}, {stranger, Manage, false},
	} {
		got, err := a.Check(ctx, c.user, "s-contract", c.perm)
		if err != nil || got != c.want {
			t.Errorf("Check(%s,%s) = %v,%v; want %v", c.user, c.perm, got, err, c.want)
		}
	}
	if ok, err := a.Check(ctx, admin, "s-does-not-exist", View); ok || err != nil {
		t.Errorf("missing session: %v, %v; want false, nil", ok, err)
	}
	list, _ := a.ListSessions(ctx)
	found := false
	for _, id := range list {
		found = found || id == "s-contract"
	}
	if !found {
		t.Errorf("ListSessions = %v", list)
	}
	_ = a.SetAdmin(ctx, admin, false)
	if ok, _ := a.Check(ctx, admin, "s-contract", View); ok {
		t.Error("former admin still allowed")
	}
	_ = a.RemoveSession(ctx, "s-contract")
	if ok, _ := a.Check(ctx, owner, "s-contract", View); ok {
		t.Error("owner allowed after removal")
	}
}
```

Run: `TOPAZ_TEST_ADDR=localhost:9292 go test ./internal/authz/ -run Topaz`
Expected: FAIL — `undefined: NewTopaz`.

**Step 6: Implement `topaz.go`**

Shape to implement (adjust identifiers to what Step 4 found):

```go
package authz

import (
	"context"
	"slices"

	"github.com/aserto-dev/go-aserto"
	ds "github.com/aserto-dev/go-aserto/ds/v3"
	dsc "github.com/aserto-dev/go-directory/aserto/directory/common/v3"
	dsr "github.com/aserto-dev/go-directory/aserto/directory/reader/v3"
	dsw "github.com/aserto-dev/go-directory/aserto/directory/writer/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const adminsGroup = "admins"

type Topaz struct{ client *ds.Client }

func NewTopaz(addr string) (*Topaz, error) {
	// In-cluster, Topaz is reached over the pod network without TLS.
	c, err := ds.New(aserto.WithAddr(addr), aserto.WithNoTLS(true))
	if err != nil {
		return nil, err
	}
	return &Topaz{client: c}, nil
}

func (t *Topaz) Check(ctx context.Context, userID, sessionID string, p Permission) (bool, error) {
	resp, err := t.client.Reader.Check(ctx, &dsr.CheckRequest{
		ObjectType: "session", ObjectId: sessionID, Relation: string(p),
		SubjectType: "user", SubjectId: userID,
	})
	if status.Code(err) == codes.NotFound {
		return false, nil // unknown session or user: not allowed
	}
	if err != nil {
		return false, err
	}
	return resp.GetCheck(), nil
}

func (t *Topaz) setObject(ctx context.Context, typ, id string) error {
	_, err := t.client.Writer.SetObject(ctx, &dsw.SetObjectRequest{Object: &dsc.Object{Type: typ, Id: id}})
	return err
}

func (t *Topaz) AddSession(ctx context.Context, sessionID, ownerID string) error {
	for _, o := range [][2]string{{"session", sessionID}, {"user", ownerID}, {"group", adminsGroup}} {
		if err := t.setObject(ctx, o[0], o[1]); err != nil {
			return err
		}
	}
	for _, r := range []*dsc.Relation{
		{ObjectType: "session", ObjectId: sessionID, Relation: "owner", SubjectType: "user", SubjectId: ownerID},
		{ObjectType: "session", ObjectId: sessionID, Relation: "admin", SubjectType: "group", SubjectId: adminsGroup, SubjectRelation: "member"},
	} {
		if _, err := t.client.Writer.SetRelation(ctx, &dsw.SetRelationRequest{Relation: r}); err != nil {
			return err
		}
	}
	return nil
}

func (t *Topaz) RemoveSession(ctx context.Context, sessionID string) error {
	_, err := t.client.Writer.DeleteObject(ctx, &dsw.DeleteObjectRequest{
		ObjectType: "session", ObjectId: sessionID, WithRelations: true,
	})
	if status.Code(err) == codes.NotFound {
		return nil
	}
	return err
}

func (t *Topaz) SetAdmin(ctx context.Context, userID string, admin bool) error {
	if err := t.setObject(ctx, "group", adminsGroup); err != nil {
		return err
	}
	if admin {
		if err := t.setObject(ctx, "user", userID); err != nil {
			return err
		}
		_, err := t.client.Writer.SetRelation(ctx, &dsw.SetRelationRequest{Relation: &dsc.Relation{
			ObjectType: "group", ObjectId: adminsGroup, Relation: "member", SubjectType: "user", SubjectId: userID,
		}})
		return err
	}
	_, err := t.client.Writer.DeleteRelation(ctx, &dsw.DeleteRelationRequest{
		ObjectType: "group", ObjectId: adminsGroup, Relation: "member", SubjectType: "user", SubjectId: userID,
	})
	if status.Code(err) == codes.NotFound {
		return nil
	}
	return err
}

func (t *Topaz) ListSessions(ctx context.Context) ([]string, error) {
	var out []string
	token := ""
	for {
		resp, err := t.client.Reader.GetObjects(ctx, &dsr.GetObjectsRequest{
			ObjectType: "session", Page: &dsc.PaginationRequest{Size: 100, Token: token},
		})
		if err != nil {
			return nil, err
		}
		for _, o := range resp.GetResults() {
			out = append(out, o.GetId())
		}
		if token = resp.GetPage().GetNextToken(); token == "" {
			break
		}
	}
	slices.Sort(out)
	return out, nil
}
```

**Step 7: Run the contract test**

Run: `TOPAZ_TEST_ADDR=localhost:9292 go test ./internal/authz/ -run Topaz -v`
Expected: PASS. Fix identifiers until it compiles and passes; do not change the test's expectations — they are the behaviour the rest of the backend relies on.

**Step 8: Wire it in `main.go`**

Replace `var az authz.Authorizer = authz.NewMemory()` with:

```go
	az, err := authz.NewTopaz(cfg.TopazAddr)
	if err != nil {
		return err
	}
	reconcile := func() {
		all, err := store.List(ctx, "")
		if err != nil {
			slog.Error("reconcile: list sessions", "err", err)
			return
		}
		cluster := make(map[string]string, len(all))
		for _, s := range all {
			cluster[s.ID] = s.Owner
		}
		if err := authz.Reconcile(ctx, az, cluster); err != nil {
			slog.Error("reconcile", "err", err)
		}
	}
	reconcile()
	go func() {
		tick := time.NewTicker(10 * time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				reconcile()
			}
		}
	}()
```

Run: `go vet ./... && go test ./... && go build -o bin/server ./cmd/server` — Expected: clean.

**Step 9: Commit and clean up**

```bash
docker rm -f topaz-test
git add backend
git commit -m "feat(authz): Topaz authorizer with periodic reconcile from the cluster"
```

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

## Phase 4 — Deployment and local integration

### Task 18: Backend image and Kubernetes manifests

**Files:**
- Create: `Dockerfile`
- Create: `deploy/base/kustomization.yaml`, `namespace.yaml`, `backend.yaml`, `blueprint.yaml`, `networkpolicy.yaml`
- Create: `deploy/base/topaz/{config.yaml,topaz.yaml}` (the manifest is from Task 12)
- Create: `deploy/base/keycloak/{keycloak.yaml,postgres.yaml,realm.json}`
- Create: `deploy/local/{kustomization.yaml,patch-backend.yaml,patch-keycloak.yaml,realm-test.json}`

**Step 1: `Dockerfile` (backend + built UI)**

```dockerfile
FROM node:22 AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.25 AS backend
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 go build -o /out/server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=backend /out/server /server
COPY --from=web /web/dist /srv/web
ENTRYPOINT ["/server"]
```

Match the `golang:` tag to the `go` directive in `backend/go.mod`.

**Step 2: `deploy/base/blueprint.yaml`** — the session pod. This is the file the backend renders per session (`{{ .ID }}`, `{{ .PublicURL }}`).

```yaml
# Rendered by the backend into each session's Sandbox spec.
service: false
podTemplate:
  metadata:
    labels:
      app: browserjs-session
  spec:
    automountServiceAccountToken: false
    # Long enough for Chromium to quit cleanly and save its tabs.
    terminationGracePeriodSeconds: 30
    securityContext:
      fsGroup: 1000
    containers:
      - name: browser
        image: browserjs/browser:dev
        env:
          - { name: SESSION_MODE, value: "1" }
          - { name: DATA_DIR, value: /data }
          - { name: TAB_STATE_FILE, value: /data/chrome/browserjs-tabs.json }
        ports:
          - { name: vnc, containerPort: 6080 }
          - { name: browser-mcp, containerPort: 8081 }
        readinessProbe:
          tcpSocket: { port: 6080 }
          periodSeconds: 2
        resources:
          requests: { cpu: 500m, memory: 1Gi }
          limits: { cpu: "2", memory: 3Gi }
        volumeMounts:
          - { name: data, mountPath: /data/chrome, subPath: chrome }
          - { name: shm, mountPath: /dev/shm }
      - name: mcp-js
        image: browserjs/mcp-js:dev
        env:
          - name: MCP_V8_PUBLIC_URL
            value: "{{ .PublicURL }}/s/{{ .ID }}"
        ports:
          - { name: mcp, containerPort: 8080 }
        readinessProbe:
          httpGet: { path: /api/artifacts, port: 8080 }
          periodSeconds: 2
        resources:
          requests: { cpu: 100m, memory: 256Mi }
          limits: { cpu: "1", memory: 1Gi }
        volumeMounts:
          - { name: data, mountPath: /data/memory, subPath: memory }
          - { name: data, mountPath: /data/mcp, subPath: mcp }
    volumes:
      - name: shm
        emptyDir: { medium: Memory, sizeLimit: 1Gi }
volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      accessModes: ["ReadWriteOnce"]
      resources:
        requests:
          storage: 5Gi
```

Two things this file assumes and Task 20 proves: that the controller attaches each `volumeClaimTemplates` entry to the pod as a volume of the same name (as a StatefulSet does), and that the browser container is happy with `DATA_DIR=/data` when only `/data/chrome` is mounted. mcp-js exits at startup if the browser MCP is not up yet; the kubelet restarts it and it settles within a few seconds — expect one or two restarts on a new pod.

**Step 3: `deploy/base/namespace.yaml` and `backend.yaml`**

```yaml
# namespace.yaml
apiVersion: v1
kind: Namespace
metadata:
  name: browserjs-sessions
```

```yaml
# backend.yaml
apiVersion: v1
kind: ServiceAccount
metadata: { name: backend, namespace: browserjs-sessions }
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: { name: backend, namespace: browserjs-sessions }
rules:
  - apiGroups: ["agents.x-k8s.io"]
    resources: ["sandboxes"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: { name: backend, namespace: browserjs-sessions }
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: Role, name: backend }
subjects:
  - { kind: ServiceAccount, name: backend, namespace: browserjs-sessions }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: backend, namespace: browserjs-sessions }
spec:
  # One replica: last-activity times and VNC tickets are held in memory.
  replicas: 1
  strategy: { type: Recreate }
  selector: { matchLabels: { app: backend } }
  template:
    metadata: { labels: { app: backend } }
    spec:
      serviceAccountName: backend
      containers:
        - name: backend
          image: browserjs/backend:dev
          ports: [{ containerPort: 8080 }]
          env:
            - { name: NAMESPACE, value: browserjs-sessions }
            - { name: PUBLIC_URL, value: "https://CHANGE-ME" }
            - { name: OIDC_ISSUER, value: "https://CHANGE-ME/realms/browserjs" }
            - { name: OIDC_JWKS_URL, value: "http://keycloak:8080/realms/browserjs/protocol/openid-connect/certs" }
            - { name: KC_URL, value: "https://CHANGE-ME" }
            - { name: TOPAZ_ADDR, value: "topaz:9292" }
            - { name: BLUEPRINT_PATH, value: /etc/browserjs/blueprint.yaml }
          readinessProbe:
            httpGet: { path: /healthz, port: 8080 }
          volumeMounts:
            - { name: blueprint, mountPath: /etc/browserjs }
      volumes:
        - name: blueprint
          configMap: { name: blueprint }
---
apiVersion: v1
kind: Service
metadata: { name: backend, namespace: browserjs-sessions }
spec:
  selector: { app: backend }
  ports: [{ port: 80, targetPort: 8080 }]
```

**Step 4: `deploy/base/networkpolicy.yaml`**

Session pods accept connections only from the backend, and may reach the internet but nothing inside the cluster.

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: { name: session-pods, namespace: browserjs-sessions }
spec:
  podSelector: { matchLabels: { app: browserjs-session } }
  policyTypes: ["Ingress", "Egress"]
  ingress:
    - from:
        - podSelector: { matchLabels: { app: backend } }
      ports:
        - { port: 6080 }
        - { port: 8080 }
  egress:
    - to: # DNS
        - namespaceSelector: {}
          podSelector: { matchLabels: { k8s-app: kube-dns } }
      ports:
        - { port: 53, protocol: UDP }
        - { port: 53, protocol: TCP }
    - to: # the internet, not the cluster or the cloud metadata server
        - ipBlock:
            cidr: 0.0.0.0/0
            except: ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16"]
```

**Step 5: Topaz — `deploy/base/topaz/config.yaml` and `topaz.yaml`**

`config.yaml`, derived from Topaz's own `docs/deployments/docker-compose/config/local.yaml` (config schema version 2), keeping only the directory services:

```yaml
# yaml-language-server: $schema=https://topaz.sh/schema/config.json
---
version: 2
logging:
  prod: true
  log_level: info
directory:
  db_path: /db/directory.db
  request_timeout: 5s
api:
  health:
    listen_address: "0.0.0.0:9494"
  services:
    reader:
      grpc:
        listen_address: "0.0.0.0:9292"
    writer:
      grpc:
        listen_address: "0.0.0.0:9292"
    model:
      grpc:
        listen_address: "0.0.0.0:9292"
```

**Verify:** Topaz's sample says that when no `certs` are given for a gRPC listener, it generates self-signed ones. If the listener comes up with TLS, either point `certs` at a mounted certificate and give the backend the CA, or (in-cluster, behind NetworkPolicy) use the client's skip-verify option. Settle this in Task 12 Step 4 and keep the config and `NewTopaz` consistent.

`topaz.yaml`: a single-replica `Deployment` (`ghcr.io/aserto-dev/topaz:0.33.22`, args `run --config-file /config/config.yaml`), a 1Gi `PersistentVolumeClaim` mounted at `/db`, a `ConfigMap` with `config.yaml` and `manifest.yaml` mounted at `/config`, a `Service` named `topaz` on port 9292, and a `postStart`-free way to load the model: a `Job` named `topaz-load-manifest` using the same image that runs the CLI's "set manifest" command against `topaz:9292` with `/config/manifest.yaml` (the exact subcommand and flags were established in Task 12 Step 5). Add a NetworkPolicy allowing ingress to Topaz only from `app: backend` and the Job.

**Step 6: Keycloak — `deploy/base/keycloak/`**

- `postgres.yaml`: a single-replica `StatefulSet` of `postgres:17` with a 5Gi volume, a `Secret` for the password, and a `Service` named `keycloak-db`.
- `keycloak.yaml`: a `Deployment` of `quay.io/keycloak/keycloak:26.0` with args `start --import-realm`, env `KC_DB=postgres`, `KC_DB_URL=jdbc:postgresql://keycloak-db/keycloak`, `KC_DB_USERNAME`, `KC_DB_PASSWORD`, `KC_HOSTNAME=https://CHANGE-ME`, `KC_HTTP_ENABLED=true`, `KC_PROXY_HEADERS=xforwarded`, `KC_BOOTSTRAP_ADMIN_USERNAME`/`PASSWORD` from a `Secret`; the realm file mounted at `/opt/keycloak/data/import/realm.json` (owned so the keycloak user can read it — a ConfigMap mount is fine); a `Service` named `keycloak` on 8080.
- `realm.json`: start from `~/railway-browser-mcp/keycloak/mcp-realm.json` and change:
  - realm name `browserjs`;
  - a realm role `admin`;
  - a public client `browserjs-spa`: standard flow, PKCE `S256`, `redirectUris: ["https://CHANGE-ME/*"]`, `webOrigins: ["+"]`;
  - keep the confidential `claude-connector` client and everything that made Claude's connector work there: all default and optional client scopes assigned, and every imported user given the `offline_access` role (imported users get no default roles; without it the token exchange fails with `not_allowed`);
  - no users in the base realm.

**Step 7: `deploy/base/kustomization.yaml`**

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: browserjs-sessions
resources:
  - namespace.yaml
  - backend.yaml
  - networkpolicy.yaml
  - topaz/topaz.yaml
  - keycloak/postgres.yaml
  - keycloak/keycloak.yaml
configMapGenerator:
  - name: blueprint
    files: [blueprint.yaml]
  - name: topaz-config
    files: [topaz/config.yaml, topaz/manifest.yaml]
  - name: keycloak-realm
    files: [realm.json=keycloak/realm.json]
```

**Step 8: `deploy/local/` overlay**

For a kind cluster reached through `kubectl port-forward` (backend on `localhost:8080`, Keycloak on `localhost:8081`):

- `patch-backend.yaml`: `PUBLIC_URL=http://localhost:8080`, `OIDC_ISSUER=http://localhost:8081/realms/browserjs`, `KC_URL=http://localhost:8081`, `IDLE_AFTER=2m`.
- `patch-keycloak.yaml`: args `start-dev --import-realm`, `KC_HOSTNAME=http://localhost:8081`.
- `realm-test.json`: the base realm with `redirectUris: ["http://localhost:8080/*", "http://localhost:5173/*"]`, three users — `alice`, `bob` (both with `offline_access`), `root` (also `admin`) — each with password `test`, and a public client `browserjs-test` with `directAccessGrantsEnabled: true` so the integration script can get tokens by password. **This client and these users must never be in the base realm.**
- `kustomization.yaml`: `resources: [../base]`, the two patches, and a `configMapGenerator` with `behavior: replace` for `keycloak-realm` pointing at `realm-test.json`.

**Step 9: Validate the manifests render**

Run: `kustomize build deploy/base >/dev/null && kustomize build deploy/local >/dev/null && echo ok`
Expected: `ok`.

**Step 10: Commit**

```bash
git add Dockerfile deploy
git commit -m "feat(deploy): backend image and Kubernetes manifests"
```

### Task 19: Local cluster

This machine has no Docker; the dev shell provides colima.

**Files:**
- Create: `hack/local-up.sh`

**Step 1: Write `hack/local-up.sh`**

```bash
#!/usr/bin/env bash
# Bring up a local cluster with everything installed. Run inside `nix develop`.
set -euo pipefail
cd "$(dirname "$0")/.."

colima status >/dev/null 2>&1 || colima start --cpu 6 --memory 12 --disk 60
kind get clusters | grep -qx browserjs || kind create cluster --name browserjs

kubectl apply --server-side -f \
  https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.5/sandbox-with-extensions.yaml
kubectl wait --for=condition=Established crd/sandboxes.agents.x-k8s.io --timeout=120s

docker build -t browserjs/backend:dev .
docker build -t browserjs/mcp-js:dev images/mcp-js
docker build -t browserjs/browser:dev images/browser   # Nix build inside Docker: slow the first time
for image in backend mcp-js browser; do kind load docker-image "browserjs/$image:dev" --name browserjs; done

kubectl apply -k deploy/local
kubectl -n browserjs-sessions rollout status deploy/keycloak --timeout=300s
kubectl -n browserjs-sessions rollout status deploy/topaz --timeout=120s
kubectl -n browserjs-sessions rollout status deploy/backend --timeout=120s

echo "Run in two terminals:"
echo "  kubectl -n browserjs-sessions port-forward svc/backend 8080:80"
echo "  kubectl -n browserjs-sessions port-forward svc/keycloak 8081:8080"
```

**Step 2: Run it**

Run: `chmod +x hack/local-up.sh && hack/local-up.sh`
Expected: all three rollouts complete. Likely first-run problems and where to look:

- backend `CrashLoopBackOff`: `kubectl -n browserjs-sessions logs deploy/backend` — usually Topaz not ready, the TLS question from Task 18 Step 5, or the JWKS URL.
- Keycloak import failure: its log names the offending realm field.
- kind's default network plugin may not enforce NetworkPolicy; that is acceptable locally and is checked on GKE (Task 22).

**Step 3: Open the UI**

With both port-forwards running, open `http://localhost:8080`, sign in as `alice` / `test`, create a session named `first`, and watch it go from `starting` to `running` with the browser visible in the page.

If the pod never becomes ready: `kubectl -n browserjs-sessions describe sandbox <id>` and `kubectl -n browserjs-sessions logs <id> -c browser` / `-c mcp-js`. Fix the blueprint or images and repeat; this is where the assumptions flagged in Task 18 Step 2 get settled.

**Step 4: Commit**

```bash
git add hack
git commit -m "chore: local cluster bring-up script"
```

### Task 20: End-to-end integration test

**Files:**
- Create: `test/integration.py`

Python 3 standard library only. It drives the real stack through the port-forwards as `alice`, `bob` and `root`.

**Step 1: Write `test/integration.py`**

```python
#!/usr/bin/env python3
"""End-to-end test against a local cluster (hack/local-up.sh + port-forwards)."""
import base64, json, os, socket, subprocess, sys, time, urllib.error, urllib.parse, urllib.request

BASE = os.environ.get("BASE", "http://localhost:8080")
KC = os.environ.get("KC", "http://localhost:8081")
NS = "browserjs-sessions"


def token(user):
    data = urllib.parse.urlencode({
        "grant_type": "password", "client_id": "browserjs-test",
        "username": user, "password": "test", "scope": "openid",
    }).encode()
    url = f"{KC}/realms/browserjs/protocol/openid-connect/token"
    return json.load(urllib.request.urlopen(url, data))["access_token"]


def http(method, path, tok=None, body=None, raw=None, headers=None):
    """Returns (status, parsed JSON or text, response headers)."""
    h = dict(headers or {})
    if tok:
        h["Authorization"] = f"Bearer {tok}"
    data = raw
    if body is not None:
        data, h["Content-Type"] = json.dumps(body).encode(), "application/json"
    req = urllib.request.Request(BASE + path, data=data, method=method, headers=h)
    try:
        res = urllib.request.urlopen(req, timeout=240)
    except urllib.error.HTTPError as e:
        res = e
    text = res.read().decode()
    try:
        return res.status, json.loads(text), res.headers
    except ValueError:
        return res.status, text, res.headers


def check(name, cond, detail=""):
    print(("PASS " if cond else "FAIL ") + name + (f" — {detail}" if detail and not cond else ""))
    if not cond:
        sys.exit(1)


def wait_state(sid, tok, want, timeout=600):
    deadline = time.time() + timeout
    state = None
    while time.time() < deadline:
        _, s, _ = http("GET", f"/api/sessions/{sid}", tok)
        state = s.get("state") if isinstance(s, dict) else None
        if state == want:
            return
        if state == "failed":
            break
        time.sleep(3)
    check(f"session reaches {want}", False, f"last state {state}")


class Mcp:
    """Minimal MCP client over Streamable HTTP."""

    def __init__(self, sid, tok):
        self.path, self.tok, self.session, self.n = f"/s/{sid}/mcp", tok, None, 0
        self.rpc("initialize", {"protocolVersion": "2025-03-26", "capabilities": {},
                                "clientInfo": {"name": "integration", "version": "1"}})
        self.rpc("notifications/initialized", notify=True)

    def rpc(self, method, params=None, notify=False):
        self.n += 1
        msg = {"jsonrpc": "2.0", "method": method}
        if not notify:
            msg["id"] = self.n
        if params is not None:
            msg["params"] = params
        h = {"Accept": "application/json, text/event-stream"}
        if self.session:
            h["Mcp-Session-Id"] = self.session
        status, body, headers = http("POST", self.path, self.tok, body=msg, headers=h)
        self.session = headers.get("Mcp-Session-Id") or self.session
        if isinstance(body, str):  # SSE: take the last data line
            lines = [l[6:] for l in body.splitlines() if l.startswith("data: {")]
            body = json.loads(lines[-1]) if lines else None
        return status, body

    def tool(self, name, args):
        status, body = self.rpc("tools/call", {"name": name, "arguments": args})
        check(f"tool {name} answered", status == 200 and body and "result" in body, f"{status} {body}")
        return body["result"]["content"][0]["text"]

    def run_js(self, code):
        return json.loads(self.tool("run_js", {"code": code}))


def websocket_handshake(path):
    """Opens a websocket by hand; returns the HTTP status line."""
    u = urllib.parse.urlparse(BASE)
    sock = socket.create_connection((u.hostname, u.port or 80), timeout=30)
    key = base64.b64encode(os.urandom(16)).decode()
    sock.sendall((f"GET {path} HTTP/1.1\r\nHost: {u.netloc}\r\nUpgrade: websocket\r\n"
                  f"Connection: Upgrade\r\nSec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n"
                  "Sec-WebSocket-Protocol: binary\r\n\r\n").encode())
    line = sock.recv(4096).decode(errors="replace").split("\r\n")[0]
    sock.close()
    return line


def main():
    alice, bob, root = token("alice"), token("bob"), token("root")

    # Clean slate for alice.
    for s in http("GET", "/api/sessions", alice)[1]:
        http("DELETE", f"/api/sessions/{s['id']}", alice)

    status, s, _ = http("POST", "/api/sessions", alice, body={"name": "integration"})
    check("create session", status == 201 and s["state"] == "starting", f"{status} {s}")
    sid = s["id"]
    wait_state(sid, alice, "running")
    check("session becomes running", True)

    # Ownership.
    check("stranger GET is 404", http("GET", f"/api/sessions/{sid}", bob)[0] == 404)
    check("stranger MCP is 404", http("POST", f"/s/{sid}/mcp", bob, body={})[0] == 404)
    check("admin GET is 200", http("GET", f"/api/sessions/{sid}", root)[0] == 200)
    status, _, headers = http("POST", f"/s/{sid}/mcp", body={})
    check("anonymous MCP is 401 with sign-in metadata",
          status == 401 and "resource_metadata=" in headers.get("WWW-Authenticate", ""))

    # MCP through the proxy.
    mcp = Mcp(sid, alice)
    _, tools = mcp.rpc("tools/list")
    names = [t["name"] for t in tools["result"]["tools"]]
    check("MCP tools listed", "run_js" in names and "get_artifact_upload_url" in names, str(names))
    check("run_js works", mcp.run_js("console.log(6*7)")["output"].strip() == "42")

    # Upload: URL points back through this host and needs no login.
    grant = json.loads(mcp.tool("get_artifact_upload_url", {"key": "it.bin", "mime_type": "application/octet-stream"}))
    prefix = f"{BASE}/s/{sid}/api/artifact-uploads/"
    check("upload URL is per-session", grant.get("url", "").startswith(prefix), str(grant))
    payload = os.urandom(20000)
    check("upload without login", http("PUT", grant["url"][len(BASE):], raw=payload)[0] == 200)
    check("upload URL is single-use", http("PUT", grant["url"][len(BASE):], raw=payload)[0] == 404)
    check("run_js reads the upload",
          mcp.run_js("console.log(artifact.get('it.bin').size_bytes)")["output"].strip() == "20000")

    # VNC.
    status, t, _ = http("POST", f"/api/sessions/{sid}/vnc-ticket", alice)
    check("VNC ticket issued", status == 200 and "ticket" in t)
    line = websocket_handshake(f"/s/{sid}/vnc?ticket={t['ticket']}")
    check("VNC websocket upgrades", " 101 " in line, line)
    check("VNC ticket is single-use", " 401 " in websocket_handshake(f"/s/{sid}/vnc?ticket={t['ticket']}"))
    check("stranger gets no VNC ticket", http("POST", f"/api/sessions/{sid}/vnc-ticket", bob)[0] == 404)

    # Tabs and disk survive stop and resume.
    nav = ("const r = await mcp.callTool('browser','browser_execute',{operations:"
           "[{type:'navigate',params:{url:'https://example.com'}}]}); console.log(r.content[0].text.length)")
    mcp.run_js(nav)
    mcp.run_js("await fs.writeFile('/data/memory/it.txt','kept')")
    http("PATCH", f"/api/sessions/{sid}", alice, body={"action": "stop"})
    wait_state(sid, alice, "stopped")
    check("stopped session refuses MCP", http("POST", f"/s/{sid}/mcp", alice, body={})[0] == 409)
    http("PATCH", f"/api/sessions/{sid}", alice, body={"action": "resume"})
    wait_state(sid, alice, "running")
    mcp = Mcp(sid, alice)
    where = mcp.run_js("const r = await mcp.callTool('browser','browser_execute',{operations:[{type:'url'}]});"
                       "console.log(r.content[0].text)")["output"]
    check("tab restored after resume", "example.com" in where, where)
    check("memory survived", mcp.run_js("console.log(await fs.readFile('/data/memory/it.txt','utf8'))")["output"].strip() == "kept")
    check("artifact survived",
          mcp.run_js("console.log(artifact.get('it.bin').size_bytes)")["output"].strip() == "20000")

    # Scale to zero and wake on demand (IDLE_AFTER=2m locally, swept every minute).
    print("waiting for the session to go idle (up to 5 minutes)…")
    wait_state(sid, alice, "asleep", timeout=300)
    check("idle session went to sleep", True)
    started = time.time()
    mcp = Mcp(sid, alice)  # the first call wakes it
    check("MCP call woke the session", mcp.run_js("console.log('awake')")["output"].strip() == "awake")
    print(f"     wake took {time.time() - started:.0f}s")
    check("session is running again", http("GET", f"/api/sessions/{sid}", alice)[1]["state"] == "running")

    # Delete removes the disk.
    check("delete", http("DELETE", f"/api/sessions/{sid}", alice)[0] == 204)
    check("deleted session is 404", http("GET", f"/api/sessions/{sid}", alice)[0] == 404)
    time.sleep(20)
    pvcs = subprocess.run(["kubectl", "-n", NS, "get", "pvc", "-o", "name"], capture_output=True, text=True).stdout
    check("disk deleted with the session", sid not in pvcs, pvcs)

    print("\nALL PASSED")


if __name__ == "__main__":
    main()
```

**Step 2: Run it**

Run: `python3 test/integration.py` (with both port-forwards up)
Expected: a list of `PASS` lines ending in `ALL PASSED`. It takes several minutes because of the idle wait.

Treat each `FAIL` as a real finding and fix the cause, not the test. The ones most likely to need work:

- **"tab restored after resume"** — Chromium did not exit cleanly, or `--restore-last-session` did not apply. Look at `kubectl logs <id> -c browser --previous` and the process name used in `cleanup` (Task 13 Step 3).
- **"artifact survived"** — the mcp-js session database is not on the disk; recheck the variable name from Task 14 Step 2.
- **"disk deleted with the session"** — if the PVC outlives the Sandbox, add an explicit PVC delete to `Store.Delete` (and the `persistentvolumeclaims` `delete` verb to the backend Role) and a unit test for it.
- **"MCP call woke the session"** — if the wake outlasts the client timeout, raise `READY_TIMEOUT`, and note the measured wake time printed by the script.

**Step 3: Check the UI by hand**

In the browser at `http://localhost:8080`: sign in as `alice`; create, open, click inside the VNC view and type in the browser; rename; stop and resume; delete. Sign in as `bob` in a private window and confirm alice's session is not listed and its URL shows "doesn't exist, or isn't yours". Sign in as `root` and confirm the "everyone's" toggle lists it.

**Step 4: Commit**

```bash
git add test
git commit -m "test: end-to-end integration against a local cluster"
```

---

## Phase 5 — GKE (blocked until the cluster exists)

Nothing below can be run or verified yet. Each task starts by reading the current Google documentation, because the commands and resource names here were **not** verified when this plan was written.

### Task 21: Cluster, Agent Sandbox, and Chromium under gVisor

This is the main technical risk in the design; do it first.

1. Read https://docs.cloud.google.com/kubernetes-engine/docs/how-to/agent-sandbox end to end. Note the minimum GKE version, how the feature is enabled, and which node pools get gVisor.
2. Create the cluster and a gVisor node pool for sessions as that page instructs. Confirm `kubectl get crd sandboxes.agents.x-k8s.io` exists and note its served versions; if `v1beta1` is not served, change `SandboxGVR` in `backend/internal/sessions/session.go` and re-run the backend tests.
3. Add `deploy/gke/` as a kustomize overlay on `deploy/base` with a blueprint patch adding what the doc's template shows: `runtimeClassName: gvisor`, the `sandbox.gke.io/runtime: gvisor` node selector and toleration, and container `securityContext` (`capabilities.drop: ["ALL"]`).
4. Push the three images to Artifact Registry and point the overlay at them.
5. **Decide the non-root question.** The browser image runs as root today. GKE's hardening policy requires `runAsNonRoot`; the doc says that policy can be modified. Either make the browser image run as a non-root user (preferred: change `HOME`, the profile directory ownership and the Xvfb socket setup in `entrypoint.sh`, then re-run Task 20 locally), or relax the hardening policy for this namespace and record why.
6. Create one session through the UI and judge it honestly: does Chromium start under gVisor, is the VNC view usable, how long does a page take to load, how much memory does the pod use? Record the numbers in `docs/gke-findings.md`.

**If Chromium does not run acceptably under gVisor, stop and report.** The alternatives (a different sandbox runtime, a non-sandboxed node pool with tighter NetworkPolicy, a different browser build) change the security design and are the user's decision.

### Task 22: Public hostname, TLS and network policy

1. Choose the hostname with the user. Reserve a static IP and create a GKE Gateway (or Ingress) with a Google-managed certificate routing everything to `svc/backend`, and a second hostname or path for Keycloak.
2. Confirm the load balancer passes websockets and long-lived streaming responses: set the backend service timeout high enough for a VNC session (hours, not the 30-second default) — look up the current `GCPBackendPolicy` (or equivalent) field for this.
3. Set `PUBLIC_URL`, `OIDC_ISSUER`, `KC_URL`, `KC_HOSTNAME` and the realm's redirect URIs in the overlay.
4. Verify NetworkPolicy is enforced on this cluster (Dataplane V2): from a session pod, `curl` to the backend service, to Topaz and to `169.254.169.254` must all fail, and to `https://example.com` must succeed.
5. Create a real user in Keycloak, sign in at the public URL, create a session, add its MCP URL to Claude as a connector using the `claude-connector` client, and run a `run_js` call from Claude.
6. Run `test/integration.py` against the public URL with a temporary test client, then remove that client.

### Task 23: Pod Snapshots for exact tab preservation

1. Read the GKE Pod Snapshots documentation (start from the Agent Sandbox page's links, and https://github.com/kubernetes-sigs/agent-sandbox/pull/1540 for how the Python client drives it). Establish: the resources involved in taking a snapshot of a sandboxed pod and restoring from one, the Cloud Storage and IAM setup, and whether Agent Sandbox's own suspend (`operatingMode: Suspended`) can be configured to snapshot automatically or whether the caller must trigger it.
2. Write the findings and the chosen mechanism into `docs/gke-findings.md` before writing code.
3. Add a `Snapshotter` interface to `backend/internal/sessions` with two methods — take a snapshot of a session's pod before suspending, and arrange for the next start to restore from it — a no-op implementation (used locally, where Chromium session restore does the job), and a GKE implementation selected by a `SNAPSHOTS=gke` setting. Unit-test the call order in `Sweep` and in the stop/resume handlers with a recording fake: snapshot before suspend; a snapshot failure logs and falls back to a plain suspend, never blocks it.
4. On the cluster: open several tabs, type into a form without submitting, let the session go idle, wake it, and confirm the same tabs and the typed text are there. Then delete the snapshot by hand, wake again, and confirm the fallback (tabs reopen, pages reload).
5. Confirm what the design doc warns about: pages holding a live connection have to reconnect after a restore. Record what was observed.
6. Decide snapshot retention (delete with the session; replace on each suspend) and implement it so Cloud Storage does not grow without bound.

**If a headed, multi-process browser cannot be snapshotted and restored reliably, say so plainly** and keep Chromium session restore as the only mechanism. The feature is documented for code sandboxes and model servers; a browser with an X server is untested.

### Task 24: Scale to zero at the node level

1. Configure the session node pool to autoscale with a minimum of zero. Keep the backend, Keycloak and Topaz on a separate small pool that does not scale to zero.
2. With every session asleep, confirm the session pool shrinks to no nodes, and measure a cold wake end to end (first MCP call → response) with the integration script's timing. Record it.
3. If the cold wake exceeds what MCP clients tolerate, either raise `READY_TIMEOUT` and document the retry, or keep one small warm node — a cost decision for the user.

---

## Open items for the user

- **Idle period.** The plan uses 15 minutes (`IDLE_AFTER`), which was assumed, not confirmed.
- **Hostname** for the GKE deployment (Task 22).
- **Root or non-root browser image** on GKE (Task 21 Step 5).
- **Events on the detail page.** The design mentioned showing recent cluster events; this plan shows the sandbox's own status message instead, which needs no extra permissions. Say if full events are wanted.
- The local test font stack for the wireframe look relies on fonts present on macOS and Windows; on Linux it falls back to the browser's default cursive font. Bundling a hand-drawn font is a small follow-up if the look matters there.
