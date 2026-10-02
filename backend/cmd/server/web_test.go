package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/r33drichards/computer-use/backend/internal/config"
)

func TestWebHandler(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>app</html>"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("console.log(1)"), 0o644)

	h := webHandler(config.Config{WebDir: dir, SignOutURL: "/.pomerium/sign_out"})
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
	rec := get("/config.js")
	if got, want := rec.Body.String(), `window.__BROWSERJS_CFG__ = {"signOutUrl":"/.pomerium/sign_out"};`; got != want {
		t.Errorf("config.js = %s, want %s", got, want)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/javascript" {
		t.Errorf("config.js Content-Type = %q", got)
	}
	// The sign-out link is the deployment's to choose.
	rec = httptest.NewRecorder()
	webHandler(config.Config{WebDir: dir, SignOutURL: "https://app.example.com/bye?a=1&b=</script>"}).
		ServeHTTP(rec, httptest.NewRequest("GET", "/config.js", nil))
	if got := rec.Body.String(); !strings.Contains(got, `"signOutUrl":"https://app.example.com/bye?a=1\u0026b=\u003c/script\u003e"`) {
		t.Errorf("config.js = %s", got)
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
