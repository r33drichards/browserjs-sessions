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
	mux := newMux(cfg, noVerifier{}, store, authz.NewMemory(), idle.New(15*time.Minute, time.Now))

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
}
