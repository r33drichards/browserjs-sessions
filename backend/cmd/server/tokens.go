package main

import (
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/config"
	"github.com/r33drichards/browserjs-sessions/backend/internal/tokens"
)

// A source address may fail to present a good token this many times at
// once, and once more every interval after that.
const (
	tokenFailures        = 10
	tokenFailureInterval = 10 * time.Second
)

// withAPITokens puts API tokens in front of the server's handler (app, the
// whole of newHandler's):
//
//   - Requests to the API host (API_URL) are that host's and nothing else's:
//     /v1/..., with a token, served by app's API as the token's owner.
//   - On the app's host, /api/tokens is where a signed-in user makes and
//     revokes tokens. Only when there is somebody who may use one
//     (ALLOWED_EMAILS): otherwise the path is unknown, as it is without
//     API_URL, and the API host refuses every token.
//
// Everything else is app's, untouched.
func withAPITokens(cfg config.Config, verifier auth.Verifier, store *tokens.Store, app http.Handler) http.Handler {
	hostOf := func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		return u.Host
	}
	apiHostName, appHostName := hostOf(cfg.APIURL), hostOf(cfg.PublicURL)
	allowed := auth.NewAllowList(cfg.AllowedEmails)
	apiHost := auth.NewAPIHost(store, allowed, auth.NewFailureLimiter(tokenFailures, tokenFailureInterval, time.Now), app)

	mux := http.NewServeMux()
	tokens.NewHandlers(store, allowed).Register(mux)
	manage := auth.Middleware(verifier)(mux)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case auth.SameHost(r.Host, apiHostName):
			apiHost.ServeHTTP(w, r)
		// A path spelled any other way is app's to refuse: the API has no
		// redirects (noAPIRedirects).
		case cfg.APITokens() && auth.SameHost(r.Host, appHostName) &&
			(p == "/api/tokens" || strings.HasPrefix(p, "/api/tokens/")) && path.Clean(p) == p:
			manage.ServeHTTP(w, r)
		default:
			app.ServeHTTP(w, r)
		}
	})
}
