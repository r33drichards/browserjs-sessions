package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/auth"
	"github.com/r33drichards/computer-use/backend/internal/authz"
	"github.com/r33drichards/computer-use/backend/internal/idle"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
)

// Ports inside a session pod.
const (
	mcpPort = 8080 // mcp-js
	vncPort = 6080 // websockify in front of the VNC server
	// browserPort (files.go) is the third.
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
	// Verifier and Authz establish who is calling a session's MCP endpoint
	// and whether they may.
	Verifier auth.Verifier
	Authz    authz.Checker
	Waker    *Waker
	Idle     *idle.Tracker
	// Billing, if set, is asked before a sleeping session is woken and
	// refuses new requests to one that is draining (billing.go).
	Billing Billing
	// URLs is where sessions are: it tells a request for a session from one
	// for the app, names the session, and makes the URLs given out.
	URLs *sessions.URLTemplate
	// LegacyURLs is where sessions used to be, if anywhere: requests there
	// are still served, by the same routes, but no URL given out leads there.
	LegacyURLs *sessions.URLTemplate
	// Target returns host:port for a port of a session's pod. Defaults to
	// the pod IP; tests override it.
	Target func(s sessions.Session, port int) string
	// MaxUploadBytes caps the body of the upload route, which has no login.
	// DefaultMaxUploadBytes if unset.
	MaxUploadBytes int64
	// MaxFileBytes caps a file sent to a session's folder (files.go).
	// DefaultMaxFileBytes if unset.
	MaxFileBytes int64

	setup   sync.Once
	tickets *tickets
	viewers viewers
	flights flights // calls and streams in progress, by session (billing.go)
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

func (p *Proxy) init() {
	p.setup.Do(func() {
		if p.Target == nil {
			p.Target = func(s sessions.Session, port int) string { return net.JoinHostPort(s.PodIP, strconv.Itoa(port)) }
		}
		if p.MaxUploadBytes <= 0 {
			p.MaxUploadBytes = DefaultMaxUploadBytes
		}
		if p.MaxFileBytes <= 0 {
			p.MaxFileBytes = DefaultMaxFileBytes
		}
		if p.Billing != nil && p.Waker.Allow == nil {
			p.Waker.Allow = p.Billing.Start
		}
		p.tickets = newTickets(time.Now)
		p.quick = podTransport(uploadResponseTimeout)
		p.patient = podTransport(mcpResponseTimeout)
	})
}

// RegisterApp adds the proxy's routes on the app's host: the UI asks for a
// ticket to open a session's screen with, and moves files to and from the
// session's browser. They go on the API's mux, behind auth.Middleware.
func (p *Proxy) RegisterApp(mux *http.ServeMux) {
	p.init()
	mux.HandleFunc("POST /api/sessions/{id}/vnc-ticket", p.vncTicket)
	p.registerFiles(mux)
}

// route is what a request for a session was matched to.
type route struct {
	id string
	// base is the part of the request's path that named the session
	// ("/<id>"), which the routes do not see. "" on a session's own host.
	base string
	// urls is the template the request matched.
	urls *sessions.URLTemplate
}

type routeKey struct{}

func routeOf(r *http.Request) route {
	rt, _ := r.Context().Value(routeKey{}).(route)
	return rt
}

// sessionID is the session the request names.
func sessionID(r *http.Request) string { return routeOf(r).id }

// sessionRoutes is the whole of what a session offers, in every form of
// session URL: a new route to a session's pod is added here and nowhere
// else. The patterns are relative to the session; r.URL has lost whatever
// part of the path named it.
func (p *Proxy) sessionRoutes(mux *http.ServeMux) {
	// These answer with what the pod answers.
	pod := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, notAPage(h)) }
	pod("/mcp", p.mcp)
	pod("/mcp/{rest...}", p.mcp)
	pod("PUT /api/artifact-uploads/{token}", p.upload)
	// A websocket and nothing else, which it sees to itself.
	mux.HandleFunc("GET /vnc", p.vnc)
	// The rest of a pod's API is not exposed, and the app is not served here.
	mux.Handle("/", http.NotFoundHandler())
}

// notAPage refuses what a browser asks for in order to show or run it: a
// navigation, or a script, image, frame or the like. Every session is served
// from the same origin, so a pod's answer must not become a page of it. The
// response headers see to that (neuter); this is the same thing said before
// the pod is asked. Browsers state the kind of request in Sec-Fetch-Mode;
// fetch() from a page (an MCP client that runs in one) is "cors", and
// clients that are not browsers send nothing.
func notAPage(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.ToLower(r.Header.Get("Sec-Fetch-Mode")) {
		case "", "cors", "same-origin":
			next(w, r)
		default:
			http.Error(w, "this is an API endpoint, not a page", http.StatusForbidden)
		}
	})
}

// match finds the template a request is for a session under, if any.
func (p *Proxy) match(r *http.Request) (m sessions.Match, urls *sessions.URLTemplate) {
	for _, urls := range []*sessions.URLTemplate{p.URLs, p.LegacyURLs} {
		if urls == nil {
			continue
		}
		if m, session := urls.Match(r.Host, r.URL.EscapedPath()); session {
			return m, urls
		}
	}
	return sessions.Match{}, nil
}

// cleanPath is what http.ServeMux would redirect path to, were they not the
// same.
func cleanPath(p string) string {
	clean := path.Clean(p)
	if strings.HasSuffix(p, "/") && clean != "/" {
		clean += "/"
	}
	return clean
}

// Handler is the server's whole handler. A request that names a session (by
// the first segment of its path on the sessions' host, or by a host of the
// session's own) gets the session's routes, and nothing else exists there.
// Every other request is the app's.
func (p *Proxy) Handler(app http.Handler) http.Handler {
	p.init()
	session := http.NewServeMux()
	p.sessionRoutes(session)
	// A host per session has the same routes, and two documents more.
	perHost := http.NewServeMux()
	p.sessionRoutes(perHost)
	p.oauthMetadata(perHost)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m, urls := p.match(r)
		switch {
		case urls == nil:
			app.ServeHTTP(w, r)
			return
		case m.ID == "":
			// A session host, but no session named: answered without a
			// cluster lookup.
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		// The routes see the path without the part that named the session.
		// It is taken off the path as it was sent, so what remains is still
		// escaped as the caller escaped it (an encoded "/" stays one), and
		// it is not cleaned: a mux redirects an unclean path to its clean
		// spelling, which here would lack the session.
		rest, err := url.PathUnescape(m.Path)
		if err != nil || cleanPath(m.Path) != m.Path {
			http.NotFound(w, r)
			return
		}
		mux := session
		if urls.PerHost() {
			mux = perHost
		}
		r = r.WithContext(context.WithValue(r.Context(), routeKey{}, route{id: m.ID, base: m.Base, urls: urls}))
		u := *r.URL
		u.Path, u.RawPath = rest, m.Path
		r.URL = &u
		mux.ServeHTTP(w, r)
	})
}

// allowed reports whether u may use the session. It writes the response
// itself when it returns false: a session that is not the caller's is
// answered like one that does not exist.
func (p *Proxy) allowed(w http.ResponseWriter, r *http.Request, u auth.User, id string) bool {
	allowed, err := p.Authz.Allowed(r.Context(), u, id)
	if err != nil {
		if r.Context().Err() == nil { // not just the caller hanging up
			slog.Error("authorization check failed", "session", id, "err", err)
			http.Error(w, "authorization unavailable", http.StatusServiceUnavailable)
		}
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
	case refused(w, r, err):
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
// content. It is handed on, but stripped of the means to act on its origin:
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

// rewriteLocation is where a redirect from the pod leads for the caller. The
// pod knows its own paths only: a redirect within the pod (no scheme, no
// host) is resolved against podPath, the path it was asked for, and put
// under base, the session's prefix. A redirect to another origin is handed
// on as it is.
func rewriteLocation(loc, base, podPath string) string {
	u, err := url.Parse(loc)
	if base == "" || err != nil || u.IsAbs() || u.Host != "" {
		return loc
	}
	// Resolving removes "." and "..", so the result cannot climb out of base.
	resolved := (&url.URL{Path: podPath}).ResolveReference(u)
	resolved.Path = base + resolved.Path
	if resolved.RawPath != "" {
		resolved.RawPath = base + resolved.RawPath
	}
	return resolved.String()
}

// forward proxies the request to path on port of the session's pod, and
// returns the status the pod answered with (0 if it did not answer).
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, s sessions.Session, port int, path string, transport http.RoundTripper) int {
	return p.forwardWith(w, r, s, port, path, transport, nil)
}

// forwardWith is forward for a route that says more about the pod's answer
// than neuter does: rewrite, if any, sees the response first.
func (p *Proxy) forwardWith(w http.ResponseWriter, r *http.Request, s sessions.Session, port int, path string, transport http.RoundTripper, rewrite func(*http.Response)) int {
	target := p.Target(s, port)
	status := 0
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// A fresh URL: path is escaped anew and nothing of the caller's
			// spelling (RawPath, query) survives.
			pr.Out.URL = &url.URL{Scheme: "http", Host: target, Path: path}
			pr.Out.Host = "localhost:" + strconv.Itoa(port)
			// The pod is not shown who is calling, or anything it could
			// present as them.
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
			for name := range pr.Out.Header {
				if strings.HasPrefix(name, "X-Pomerium-") {
					pr.Out.Header.Del(name)
				}
			}
		},
		Transport:     transport,
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			status = resp.StatusCode
			if rewrite != nil {
				rewrite(resp)
			}
			neuter(resp)
			if loc := resp.Header.Get("Location"); loc != "" {
				resp.Header.Set("Location", rewriteLocation(loc, routeOf(r).base, path))
			}
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

// mcp is the session's MCP endpoint. The route is Pomerium's: it runs the
// MCP client's sign-in and says who the user is in its signed assertion.
// A call (POST, DELETE) wakes a sleeping session and holds it awake until
// the call is over; see mcpStream for GET.
//
// It never answers 401. That would be the cue for an MCP client to sign in,
// which is Pomerium's to give; from here Pomerium would turn it into a 502.
func (p *Proxy) mcp(w http.ResponseWriter, r *http.Request) {
	id := sessionID(r)
	path, ok := mcpPath(r)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	u, err := auth.Authenticate(p.Verifier, r)
	if err != nil {
		slog.Debug("assertion rejected", "session", id, "err", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !p.allowed(w, r, u, id) {
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		p.mcpStream(w, r, id, path)
		return
	}
	// The call holds the session awake until it is over, however long the
	// pod takes: a tool call may outlast the idle period.
	done := p.Idle.Open(id)
	defer done()
	defer p.flights.call(id)() // in flight: a drain waits for it
	s, err := p.awake(r.Context(), id)
	if err != nil {
		lookupFailed(w, r, id, err)
		return
	}
	p.forward(w, r, s, mcpPort, path, p.patient)
}

// mcpStream answers a GET on the MCP endpoint: the stream on which the
// server sends events to the client. Some clients keep one open for as long
// as the server is configured, whether or not anything is using it, so it
// is not taken for use of the session: it does not wake a session, is not
// activity, and does not keep the pod. A session that is not running has no
// stream to offer, which MCP lets a server say with 405; the client's next
// call wakes it.
func (p *Proxy) mcpStream(w http.ResponseWriter, r *http.Request, id, path string) {
	s, err := p.running(r.Context(), id)
	if errors.Is(err, ErrNotRunning) {
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "session is not running; there is no event stream until a call wakes it", http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		lookupFailed(w, r, id, err)
		return
	}
	// Not work in progress: a drain ends it.
	ctx, leave := p.flights.stream(r.Context(), id)
	defer leave()
	p.forward(w, r.WithContext(ctx), s, mcpPort, path, p.patient)
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
	id, token := sessionID(r), r.PathValue("token")
	if !uploadTokenPattern.MatchString(token) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.ContentLength > p.MaxUploadBytes {
		http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
		return
	}
	s, err := p.running(r.Context(), id)
	if err != nil {
		lookupFailed(w, r, id, err)
		return
	}
	defer p.flights.call(id)()
	r.Body = http.MaxBytesReader(w, r.Body, p.MaxUploadBytes)
	if status := p.forward(w, r, s, mcpPort, "/api/artifact-uploads/"+token, p.quick); status/100 == 2 {
		p.Idle.Touch(id)
	}
}

// vncTicket issues a one-time ticket for a session's screen, with the URL
// to open. The screen is on the sessions' host, where the browser's sign-in
// with the app does not reach; the ticket stands in.
func (p *Proxy) vncTicket(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.UserFrom(r.Context())
	if !ok {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	if !sessions.ValidID(id) {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if !p.allowed(w, r, u, id) {
		return
	}
	ticket := p.tickets.Issue(id)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{"ticket": ticket, "url": p.URLs.VNC(id, ticket)})
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

// vnc is the session's screen. There is no login: the ticket, issued to a
// user who may see the session, is the credential.
func (p *Proxy) vnc(w http.ResponseWriter, r *http.Request) {
	id := sessionID(r)
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
	ctx, left := p.flights.stream(ctx, id) // a drain ends it
	defer left()
	r = r.WithContext(ctx)

	done := p.Idle.Open(id) // a viewer keeps the session awake
	defer done()
	s, err := p.awake(ctx, id)
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
