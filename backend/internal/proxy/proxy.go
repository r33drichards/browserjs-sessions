package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	if !sessions.ValidID(id) {
		http.Error(w, "session not found", http.StatusNotFound)
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
		slog.Error("authorization check failed", "session", id, "err", err)
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
	if err != nil && r.Context().Err() != nil {
		return // the caller hung up while the session was waking; nobody to answer
	}
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
	if !sessions.ValidID(id) {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
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
