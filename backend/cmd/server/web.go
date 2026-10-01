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
	root := filepath.Clean(cfg.WebDir)
	files := http.FileServer(http.Dir(root))
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
		path := filepath.Join(root, filepath.Clean("/"+r.URL.Path))
		if info, err := os.Stat(path); err != nil || info.IsDir() || !strings.HasPrefix(path, root) {
			http.ServeFile(w, r, filepath.Join(root, "index.html"))
			return
		}
		files.ServeHTTP(w, r)
	})
	return mux
}
