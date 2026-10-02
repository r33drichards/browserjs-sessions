package sessions

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/r33drichards/browserjs-sessions/backend/internal/hosts"
)

// idPlaceholder is where a URL template names the session.
const idPlaceholder = "{id}"

// URLTemplate is where sessions are reached. It turns a session ID into the
// session's URLs, and a request's host and path back into the ID.
//
// A template has one of two forms. In the path form every session is under
// one host, the session ID being the first segment of the path
// ("https://sessions.example.com/{id}"). In the host form, which is older,
// every session has a host of its own, the ID being one label in front of a
// shared domain ("https://{id}.sessions.example.com").
type URLTemplate struct {
	scheme string // http or https
	// host is the sessions' one host (path form), or what follows "<id>." in
	// a session's own (host form). Lower case.
	host   string
	port   string // "" for the scheme's own port
	inPath bool   // the path form
}

var defaultPorts = map[string]string{"http": "80", "https": "443"}

// ParseURLTemplate reads a template such as "https://sessions.example.com/{id}"
// or "https://{id}.sessions.example.com". {id} must appear exactly once, as
// the whole path or as the whole left-most label of the host, and the URL
// must have nothing else but a scheme, a host and an optional port.
func ParseURLTemplate(s string) (*URLTemplate, error) {
	bad := func(why string) (*URLTemplate, error) {
		return nil, fmt.Errorf("session URL template %q: %s", s, why)
	}
	if strings.Count(s, idPlaceholder) != 1 {
		return bad("must contain " + idPlaceholder + " exactly once")
	}
	scheme, rest, ok := strings.Cut(s, "://")
	if !ok {
		return bad("must be an absolute http(s) URL")
	}
	// "{" cannot be part of a URL's host, so {id} is taken out and the rest
	// is parsed on its own.
	inPath := false
	if after, ok := strings.CutPrefix(rest, idPlaceholder+"."); ok {
		rest = after
	} else if before, ok := strings.CutSuffix(strings.TrimSuffix(rest, "/"), "/"+idPlaceholder); ok {
		rest, inPath = before, true
	} else {
		return bad(idPlaceholder + " must be the whole path, or the whole left-most label of the host")
	}
	u, err := url.Parse(scheme + "://" + rest)
	if err != nil {
		return bad(err.Error())
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return bad("must be an http(s) URL")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && (inPath || u.Path != "/")) {
		return bad("must be a scheme, a host and " + idPlaceholder + " only")
	}
	host, port := hosts.Split(u.Host)
	if host == "" || strings.ContainsAny(host, " :") {
		return bad("needs a host besides " + idPlaceholder)
	}
	if port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return bad("port is not a number")
		}
	}
	if port == defaultPorts[u.Scheme] {
		port = ""
	}
	return &URLTemplate{scheme: u.Scheme, host: host, port: port, inPath: inPath}, nil
}

// PerHost reports whether the template is of the host form: every session
// has a host of its own.
func (t *URLTemplate) PerHost() bool { return !t.inPath }

// Match is what a request says under a template.
type Match struct {
	// ID is the session the request names, or "" if it names none (what
	// stands where the ID belongs is not a session ID, or the port is not
	// the template's).
	ID string
	// Base is the part of the path that named the session, "/<id>", and ""
	// when the host did.
	Base string
	// Path is the rest of the path, still escaped: what the session is asked
	// for. It starts with "/".
	Path string
}

// Match takes apart a request's Host and its escaped path. session reports
// whether the host is a session host at all: such a host never belongs to
// the app.
func (t *URLTemplate) Match(host, escapedPath string) (m Match, session bool) {
	name, port := hosts.Split(host)
	if port == defaultPorts[t.scheme] {
		port = ""
	}
	if t.inPath {
		if name != t.host {
			return Match{}, false
		}
		// The ID is read from the path as it was sent: an ID has nothing in
		// it that could be escaped.
		first, rest, _ := strings.Cut(strings.TrimPrefix(escapedPath, "/"), "/")
		if port != t.port || !strings.HasPrefix(escapedPath, "/") || !ValidID(first) {
			return Match{}, true
		}
		return Match{ID: first, Base: "/" + first, Path: "/" + rest}, true
	}
	label, ok := strings.CutSuffix(name, "."+t.host)
	if !ok {
		return Match{}, false
	}
	if port != t.port || !ValidID(label) {
		return Match{}, true
	}
	return Match{ID: label, Path: escapedPath}, true
}

func (t *URLTemplate) authority(id string) string {
	h := t.host
	if !t.inPath {
		h = id + "." + h
	}
	if t.port != "" {
		h += ":" + t.port
	}
	return h
}

// Origin is the scheme and host the session is served from, with no trailing
// slash. In the path form it is the same for every session.
func (t *URLTemplate) Origin(id string) string { return t.scheme + "://" + t.authority(id) }

// Base is the session's base URL, with no trailing slash.
func (t *URLTemplate) Base(id string) string {
	if t.inPath {
		return t.Origin(id) + "/" + id
	}
	return t.Origin(id)
}

// MCP is the URL an MCP client is given for the session.
func (t *URLTemplate) MCP(id string) string { return t.Base(id) + "/mcp" }

// VNC is the websocket URL that shows the session's screen to whoever holds
// ticket.
func (t *URLTemplate) VNC(id, ticket string) string {
	// http becomes ws, and https wss.
	return "ws" + strings.TrimPrefix(t.Base(id), "http") + "/vnc?ticket=" + url.QueryEscape(ticket)
}
