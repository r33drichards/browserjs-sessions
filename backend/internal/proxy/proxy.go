package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
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

// DefaultMaxUploadBytes is the largest upload body passed on to a pod:
// mcp-js's own limit of 16 MiB, plus room for it to be the one that refuses.
const DefaultMaxUploadBytes = 16<<20 + 1<<10

// How long a pod may take to start answering. An MCP call can legitimately
// run for minutes before its first byte; nothing limits a response once it
// has started, so streams and websockets live as long as they need to.
const (
	dialTimeout           = 5 * time.Second
	uploadResponseTimeout = 60 * time.Second
	mcpResponseTimeout    = 10 * time.Minute
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
	// MaxUploadBytes caps the body of the upload route, which has no login.
	// DefaultMaxUploadBytes if unset.
	MaxUploadBytes int64

	tickets *tickets
	viewers viewers
	// Pod traffic has its own transports: no environment proxy, a bounded
	// dial, and a bound on the wait for response headers that suits the route.
	quick, patient http.RoundTripper
}

func podTransport(responseHeaderTimeout time.Duration) *http.Transport {
	return &http.Transport{
		DialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: responseHeaderTimeout,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true, // hand the pod's bytes on as they are
	}
}

func (p *Proxy) Register(mux *http.ServeMux) {
	if p.Target == nil {
		p.Target = func(s sessions.Session, port int) string { return net.JoinHostPort(s.PodIP, strconv.Itoa(port)) }
	}
	if p.MaxUploadBytes <= 0 {
		p.MaxUploadBytes = DefaultMaxUploadBytes
	}
	p.tickets = newTickets(time.Now)
	p.quick = podTransport(uploadResponseTimeout)
	p.patient = podTransport(mcpResponseTimeout)

	mux.HandleFunc("GET /.well-known/oauth-protected-resource/s/{id}/mcp", p.metadata)
	mux.HandleFunc("/s/{id}/mcp", p.mcp)
	mux.HandleFunc("/s/{id}/mcp/{rest...}", p.mcp)
	mux.HandleFunc("PUT /s/{id}/api/artifact-uploads/{token}", p.upload)
	mux.HandleFunc("POST /api/sessions/{id}/vnc-ticket", p.vncTicket)
	mux.HandleFunc("GET /s/{id}/vnc", p.vnc)
	// Nothing else under /s/ exists: the rest of a pod's API is not exposed,
	// and no other handler (the UI's fallback) may answer for it.
	mux.Handle("/s/", http.NotFoundHandler())
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
//
// It is the only place the proxy establishes who is calling.
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

// lookupFailed answers a request whose session could not be had from find
// (EnsureAwake or Running).
func lookupFailed(w http.ResponseWriter, r *http.Request, id string, err error) {
	switch {
	case r.Context().Err() != nil:
		// The caller hung up while the session was waking; nobody to answer.
	case errors.Is(err, sessions.ErrNotFound):
		http.Error(w, "session not found", http.StatusNotFound)
	case errors.Is(err, ErrStopped):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, ErrNotRunning):
		http.Error(w, "session is asleep; make an MCP call to wake it, then upload", http.StatusConflict)
	case errors.Is(err, ErrFailed):
		http.Error(w, err.Error(), http.StatusBadGateway)
	case errors.Is(err, ErrNotReady):
		w.Header().Set("Retry-After", "10")
		http.Error(w, "session is waking up; retry shortly", http.StatusGatewayTimeout)
	default: // the cluster could not be asked
		slog.Error("session lookup failed", "session", id, "err", err)
		http.Error(w, "session lookup failed", http.StatusBadGateway)
	}
}

// A pod runs whatever its user's agent runs, so what it answers is untrusted
// content. It is handed on, but stripped of the means to act on this origin:
// it cannot set cookies or other origin-wide state, and a browser that is
// made to navigate to it will not run it or let it load anything.
func neuter(resp *http.Response) {
	h := resp.Header
	h.Del("Set-Cookie")
	h.Del("Clear-Site-Data")
	h.Del("Service-Worker-Allowed")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
}

// forward proxies the request to path on port of the session's pod, and
// returns the status the pod answered with (0 if it did not answer).
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, s sessions.Session, port int, path string, transport http.RoundTripper) int {
	target := p.Target(s, port)
	status := 0
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// A fresh URL: path is escaped anew and nothing of the caller's
			// spelling (RawPath, query) survives.
			pr.Out.URL = &url.URL{Scheme: "http", Host: target, Path: path}
			pr.Out.Host = "localhost:" + strconv.Itoa(port)
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
		},
		Transport:     transport,
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			status = resp.StatusCode
			neuter(resp)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// Whatever went wrong, do not trust the remembered address.
			p.Waker.Invalidate(s.ID)
			var tooLarge *http.MaxBytesError
			switch {
			case errors.As(err, &tooLarge):
				http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
				return
			case r.Context().Err() == nil: // not just the caller hanging up
				slog.Error("session pod did not answer", "session", s.ID, "port", port, "err", err)
			}
			http.Error(w, "session is not responding", http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
	return status
}

// mcpPath is the pod path for the {rest...} of an MCP URL. rest arrives
// unescaped, so "..%2F" has become "../": only plain segments are let
// through, and never an encoded separator, which would make one segment
// into several on the way.
func mcpPath(r *http.Request) (string, bool) {
	rest := r.PathValue("rest")
	if rest == "" {
		return "/mcp", true
	}
	if raw := strings.ToLower(r.URL.EscapedPath()); strings.Contains(raw, "%2f") || strings.Contains(raw, "%5c") {
		return "", false
	}
	for _, seg := range strings.Split(rest, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.Contains(seg, `\`) {
			return "", false
		}
	}
	return "/mcp/" + rest, true
}

func (p *Proxy) mcp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path, ok := mcpPath(r)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !p.authorize(w, r, id, authz.Manage) {
		return
	}
	// The call holds the session awake until it is over, however long the
	// pod takes: a tool call may outlast the idle period.
	done := p.Idle.Open(id)
	defer done()
	s, err := p.Waker.EnsureAwake(r.Context(), id)
	if err != nil {
		lookupFailed(w, r, id, err)
		return
	}
	p.forward(w, r, s, mcpPort, path, p.patient)
}

// The one-time upload tokens mcp-js issues.
var uploadTokenPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// upload forwards mcp-js's one-time upload URL. There is no login: the
// token in the path is the credential, and mcp-js checks it.
//
// Because anyone can call it, it does nothing for a session that is not
// already running (an upload URL comes out of an MCP call, which woke the
// session), reads a bounded body, and counts as activity only if the pod
// took the upload.
func (p *Proxy) upload(w http.ResponseWriter, r *http.Request) {
	id, token := r.PathValue("id"), r.PathValue("token")
	if !sessions.ValidID(id) || !uploadTokenPattern.MatchString(token) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.ContentLength > p.MaxUploadBytes {
		http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
		return
	}
	s, err := p.Waker.Running(r.Context(), id)
	if err != nil {
		lookupFailed(w, r, id, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, p.MaxUploadBytes)
	if status := p.forward(w, r, s, mcpPort, "/api/artifact-uploads/"+token, p.quick); status/100 == 2 {
		p.Idle.Touch(id)
	}
}

func (p *Proxy) vncTicket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !p.authorize(w, r, id, authz.View) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{"ticket": p.tickets.Issue(id)})
}

func isWebsocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, v := range r.Header.Values("Connection") {
		for _, token := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

func (p *Proxy) vnc(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Only a websocket. A browser navigating here (someone was sent the
	// link) must never be shown what the pod answers, and such a request
	// does not use up the ticket.
	if !isWebsocketUpgrade(r) {
		w.Header().Set("Upgrade", "websocket")
		w.Header().Set("Connection", "Upgrade")
		http.Error(w, "this endpoint only speaks websocket", http.StatusUpgradeRequired)
		return
	}
	if !p.tickets.Redeem(r.URL.Query().Get("ticket"), id) {
		http.Error(w, "invalid or expired ticket", http.StatusUnauthorized)
		return
	}
	// The server does not wait for or close a hijacked connection when it
	// shuts down; Shutdown ends it through this context.
	ctx, leave := p.viewers.join(r.Context())
	defer leave()
	r = r.WithContext(ctx)

	done := p.Idle.Open(id) // a viewer keeps the session awake
	defer done()
	s, err := p.Waker.EnsureAwake(ctx, id)
	if err != nil {
		lookupFailed(w, r, id, err)
		return
	}
	p.forward(w, r, s, vncPort, "/websockify", p.quick)
}

// Shutdown ends every open viewer connection and waits for them to be gone,
// or for ctx.
func (p *Proxy) Shutdown(ctx context.Context) error {
	return p.viewers.shutdown(ctx)
}
