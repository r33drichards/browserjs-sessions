package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/authz"
	"github.com/r33drichards/browserjs-sessions/backend/internal/config"
	"github.com/r33drichards/browserjs-sessions/backend/internal/idle"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions/sessionstest"
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

// A WEB_DIR that is not already in clean form ("/srv/web/", "./dist") must
// still serve its files rather than answering everything with the app shell.
func TestWebHandlerUncleanDir(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>app</html>"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log(1)"), 0o644)

	h := webHandler(config.Config{WebDir: dir + "/./"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/app.js", nil))
	if rec.Body.String() != "console.log(1)" {
		t.Errorf("asset: %q", rec.Body)
	}
}

type noVerifier struct{}

func (noVerifier) Verify(context.Context, string) (auth.User, error) {
	return auth.User{}, errors.New("no tokens are valid here")
}

// The whole route table, registered on one mux exactly as run() does it:
// ServeMux panics at registration if two patterns conflict.
func TestNewMux(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>app</html>"), 0o644)
	cfg := config.Config{
		WebDir: dir, PublicURL: "https://sessions.example.com", OIDCIssuer: "https://kc.example.com/realms/browserjs",
		KCURL: "https://kc.example.com", KCRealm: "browserjs", KCClientID: "browserjs-spa",
		ReadyTimeout: time.Second, MaxSessionsPerUser: 5,
	}
	store, _ := sessionstest.New(t)
	mux, _ := newMux(cfg, noVerifier{}, store, authz.NewMemory(), idle.New(15*time.Minute, time.Now))

	do := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}
	if rec := do("GET", "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("healthz: %d", rec.Code)
	}
	if rec := do("GET", "/api/sessions"); rec.Code != http.StatusUnauthorized {
		t.Errorf("API without a token: %d, want 401", rec.Code)
	}
	if rec := do("GET", "/config.js"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "window.__BROWSERJS_CFG__") {
		t.Errorf("config.js: %d %q", rec.Code, rec.Body)
	}
	if rec := do("POST", "/api/sessions/s-abcdefghij/vnc-ticket"); rec.Code != http.StatusUnauthorized {
		t.Errorf("vnc-ticket without a token: %d, want 401", rec.Code)
	}
	if rec := do("POST", "/s/s-abcdefghij/mcp"); rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("MCP without a token: %d, want 401 with WWW-Authenticate", rec.Code)
	}
	if rec := do("GET", "/.well-known/oauth-protected-resource/s/s-abcdefghij/mcp"); rec.Code != http.StatusOK {
		t.Errorf("metadata: %d", rec.Code)
	}
	// Unknown API paths are the API's to refuse, never the app shell.
	if rec := do("GET", "/api/nope"); rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown API path: %d", rec.Code)
	}
	if rec := do("GET", "/sessions/s-abcdefghij"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "app") {
		t.Errorf("SPA route: %d %q", rec.Code, rec.Body)
	}
	// Nor is anything under the proxy's or the metadata's prefixes: an MCP
	// client probing for OAuth metadata must get a 404, not a page.
	for _, path := range []string{
		"/s/s-abcdefghij/api/artifacts",
		"/s/s-abcdefghij",
		"/s/",
		"/.well-known/oauth-authorization-server",
		"/.well-known/oauth-protected-resource",
		"/.well-known/openid-configuration/s/s-abcdefghij/mcp",
		"/assets/missing.js",
	} {
		rec := do("GET", path)
		if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "<html>") {
			t.Errorf("GET %s: %d %q, want 404 and not the app shell", path, rec.Code, rec.Body)
		}
	}
	if rec := do("POST", "/s/s-abcdefghij/vnc"); rec.Code != http.StatusNotFound {
		t.Errorf("POST to the VNC route: %d, want 404", rec.Code)
	}
}

// The app shell is looked for again on every visit (it names the hashed
// assets of the current build); a path that names a file which is not there
// is a 404, not the shell.
func TestWebHandlerShellAndMissingFiles(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>app</html>"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "assets", "app-abc123.js"), []byte("console.log(1)"), 0o644)
	h := webHandler(config.Config{WebDir: dir})
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}

	for _, path := range []string{"/", "/sessions/s-abc"} {
		rec := get(path)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "app") {
			t.Errorf("GET %s: %d %q, want the app shell", path, rec.Code, rec.Body)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET %s: Cache-Control = %q, want no-cache", path, got)
		}
	}
	if got := get("/config.js").Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("config.js: Cache-Control = %q, want no-store", got)
	}
	if rec := get("/assets/app-abc123.js"); rec.Body.String() != "console.log(1)" || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("asset: %q, Cache-Control %q", rec.Body, rec.Header().Get("Cache-Control"))
	}
	for _, path := range []string{"/assets/missing.js", "/favicon.ico", "/sessions/app.css.map"} {
		if rec := get(path); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "<html>") {
			t.Errorf("GET %s: %d %q, want 404", path, rec.Code, rec.Body)
		}
	}
}

// WEB_DIR=. (run from the build's directory) serves the files there.
func TestWebHandlerCurrentDir(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>app</html>"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log(1)"), 0o644)
	t.Chdir(dir)

	h := webHandler(config.Config{WebDir: "."})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/app.js", nil))
	if rec.Body.String() != "console.log(1)" {
		t.Errorf("asset: %q", rec.Body)
	}
}
