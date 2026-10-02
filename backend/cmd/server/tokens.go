package main

import (
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/auth"
	"github.com/r33drichards/computer-use/backend/internal/config"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/tokens"
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
//     the API under /v1/, a session's MCP endpoint at /<id>/mcp, and the
//     token exchange, each served by app as the token's owner.
//   - On the app's host, /api/tokens is where a signed-in user makes and
//     revokes tokens. Only when there is somebody who may use one
//     (ALLOWED_EMAILS): otherwise the path is unknown, as it is without
//     API_URL, and the API host refuses every token.
//
// Everything else is app's, untouched.
func withAPITokens(cfg config.Config, verifier auth.Verifier, store *tokens.Store, app http.Handler) (http.Handler, error) {
	hostOf := func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		return u.Host
	}
	apiHostName, appHostName := hostOf(cfg.APIURL), hostOf(cfg.PublicURL)
	allowed := auth.NewAllowList(cfg.AllowedEmails)
	signer, err := tokens.NewSigner(cfg.APISigningKey, cfg.APIURL)
	if err != nil {
		return nil, err
	}
	store.EnableExchange(signer)
	apiHost := auth.NewAPIHost(auth.APIHostConfig{
		Tokens:         store,
		Allowed:        allowed,
		Limiter:        auth.NewFailureLimiter(tokenFailures, tokenFailureInterval, time.Now),
		App:            app,
		ValidSessionID: sessions.ValidID,
		SessionBase:    cfg.SessionURLs.Base,
	})

	mux := http.NewServeMux()
	tokens.NewHandlers(store, allowed, cfg.APIURL).Register(mux)
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
	}), nil
}
