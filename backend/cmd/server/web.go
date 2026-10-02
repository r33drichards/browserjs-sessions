package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/r33drichards/computer-use/backend/internal/config"
)

// webHandler serves the built UI, falling back to index.html for client-side
// routes, plus /config.js: runtime settings for the UI, so one image works in
// every environment (the fleet pattern).
func webHandler(cfg config.Config) http.Handler {
	root := filepath.Clean(cfg.WebDir)
	files := http.FileServer(http.Dir(root))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /config.js", func(w http.ResponseWriter, _ *http.Request) {
		body, _ := json.Marshal(map[string]string{"signOutUrl": cfg.SignOutURL})
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(append([]byte("window.__BROWSERJS_CFG__ = "), append(body, ';')...))
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// Clean("/"+path) cannot climb out, so this stays inside root.
		file := filepath.Join(root, filepath.Clean("/"+r.URL.Path))
		if info, err := os.Stat(file); err == nil && !info.IsDir() {
			if strings.HasPrefix(r.URL.Path, "/assets/") { // named after their content hash
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			files.ServeHTTP(w, r)
			return
		}
		// A client-side route gets the app shell. A path that names a file
		// (it has an extension) which is not there is simply missing.
		if path.Ext(r.URL.Path) != "" {
			http.NotFound(w, r)
			return
		}
		// The shell names the current build's assets: always revalidate it.
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, filepath.Join(root, "index.html"))
	})
	return mux
}
