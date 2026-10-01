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

// URLTemplate is where sessions are reached: every session has a host of its
// own, the session ID as one label in front of a shared domain. It turns a
// session ID into the session's URLs, and a request's Host back into the ID.
type URLTemplate struct {
	scheme string // http or https
	domain string // what follows "<id>.", lower case
	port   string // "" for the scheme's own port
}

var defaultPorts = map[string]string{"http": "80", "https": "443"}

// ParseURLTemplate reads a template such as "https://{id}.sessions.example.com".
// {id} must appear exactly once, as the whole left-most label of the host,
// and the URL must have nothing after the host but an optional port.
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
	rest, ok = strings.CutPrefix(rest, idPlaceholder+".")
	if !ok {
		return bad(idPlaceholder + " must be the whole left-most label of the host")
	}
	// "{" cannot be part of a URL's host, so the rest is parsed on its own.
	u, err := url.Parse(scheme + "://" + rest)
	if err != nil {
		return bad(err.Error())
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return bad("must be an http(s) URL")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return bad("must be a scheme and a host only")
	}
	domain, port := hosts.Split(u.Host)
	if domain == "" || strings.ContainsAny(domain, " :") {
		return bad("needs a domain after " + idPlaceholder + ".")
	}
	if port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return bad("port is not a number")
		}
	}
	if port == defaultPorts[u.Scheme] {
		port = ""
	}
	return &URLTemplate{scheme: u.Scheme, domain: domain, port: port}, nil
}

// Match takes a request's Host apart. session reports whether the host is
// under the session domain at all: such a host never belongs to the app. id
// is the session it names, or "" if it names none (the label is not a
// session ID, or the port is not the template's).
func (t *URLTemplate) Match(host string) (id string, session bool) {
	name, port := hosts.Split(host)
	label, ok := strings.CutSuffix(name, "."+t.domain)
	if !ok {
		return "", false
	}
	if port == defaultPorts[t.scheme] {
		port = ""
	}
	if port != t.port || !ValidID(label) {
		return "", true
	}
	return label, true
}

func (t *URLTemplate) host(id string) string {
	h := id + "." + t.domain
	if t.port != "" {
		h += ":" + t.port
	}
	return h
}

// Base is the session's base URL, with no trailing slash.
func (t *URLTemplate) Base(id string) string { return t.scheme + "://" + t.host(id) }

// MCP is the URL an MCP client is given for the session.
func (t *URLTemplate) MCP(id string) string { return t.Base(id) + "/mcp" }

// VNC is the websocket URL that shows the session's screen to whoever holds
// ticket.
func (t *URLTemplate) VNC(id, ticket string) string {
	scheme := "ws"
	if t.scheme == "https" {
		scheme = "wss"
	}
	return scheme + "://" + t.host(id) + "/vnc?ticket=" + url.QueryEscape(ticket)
}
