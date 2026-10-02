package sessions

import (
	"net/url"
	"testing"
)

const testID = "s-abcdefg234"

func mustTemplate(t *testing.T, s string) *URLTemplate {
	t.Helper()
	tmpl, err := ParseURLTemplate(s)
	if err != nil {
		t.Fatalf("ParseURLTemplate(%q): %v", s, err)
	}
	return tmpl
}

func TestURLTemplateURLs(t *testing.T) {
	for _, c := range []struct{ template, base, mcp, vnc string }{
		{
			"https://{id}.sessions.example.com",
			"https://s-abcdefg234.sessions.example.com",
			"https://s-abcdefg234.sessions.example.com/mcp",
			"wss://s-abcdefg234.sessions.example.com/vnc?ticket=t%2F1",
		},
		{
			"http://{id}.sessions.localtest.me:8080/",
			"http://s-abcdefg234.sessions.localtest.me:8080",
			"http://s-abcdefg234.sessions.localtest.me:8080/mcp",
			"ws://s-abcdefg234.sessions.localtest.me:8080/vnc?ticket=t%2F1",
		},
		{
			"https://sessions.example.com/{id}",
			"https://sessions.example.com/s-abcdefg234",
			"https://sessions.example.com/s-abcdefg234/mcp",
			"wss://sessions.example.com/s-abcdefg234/vnc?ticket=t%2F1",
		},
		{
			"HTTP://Sessions.Localtest.me:8080/{id}/",
			"http://sessions.localtest.me:8080/s-abcdefg234",
			"http://sessions.localtest.me:8080/s-abcdefg234/mcp",
			"ws://sessions.localtest.me:8080/s-abcdefg234/vnc?ticket=t%2F1",
		},
		{
			"HTTPS://{id}.Sessions.Example.com:443",
			"https://s-abcdefg234.sessions.example.com",
			"https://s-abcdefg234.sessions.example.com/mcp",
			"wss://s-abcdefg234.sessions.example.com/vnc?ticket=t%2F1",
		},
	} {
		tmpl := mustTemplate(t, c.template)
		if got := tmpl.Base(testID); got != c.base {
			t.Errorf("%s: Base = %q, want %q", c.template, got, c.base)
		}
		if got := tmpl.MCP(testID); got != c.mcp {
			t.Errorf("%s: MCP = %q, want %q", c.template, got, c.mcp)
		}
		if got := tmpl.VNC(testID, "t/1"); got != c.vnc {
			t.Errorf("%s: VNC = %q, want %q", c.template, got, c.vnc)
		}
	}
}

func TestParseURLTemplateRejects(t *testing.T) {
	for _, s := range []string{
		"",
		"https://sessions.example.com",                // no {id}
		"https://{id}.a.example.com/{id}",             // twice
		"https://{id}.{id}.example.com",               // twice
		"https://s-{id}.sessions.example.com",         // not the whole label
		"https://{id}-x.sessions.example.com",         // not the whole label
		"https://a.{id}.example.com",                  // not the left-most label
		"https://sessions.example.com/s/{id}",         // not the whole path
		"https://sessions.example.com/{id}/mcp",       // not the whole path
		"https://sessions.example.com/s-{id}",         // not the whole segment
		"https://sessions.example.com/{id}?x=1",       // a query
		"https://sessions.example.com/{id}#f",         // a fragment
		"https://user@sessions.example.com/{id}",      // userinfo
		"https://sessions.example.com:port/{id}",      // bad port
		"https:///{id}",                               // no host
		"sessions.example.com/{id}",                   // no scheme
		"https://{id}",                                // no domain to be under
		"https://{id}.",                               // no domain to be under
		"{id}.sessions.example.com",                   // no scheme
		"ftp://{id}.sessions.example.com",             // not http(s)
		"https://{id}.sessions.example.com/base",      // a path
		"https://{id}.sessions.example.com?x=1",       // a query
		"https://{id}.sessions.example.com#f",         // a fragment
		"https://user@{id}.sessions.example.com",      // userinfo
		"https://{id}.sessions.example.com:port",      // bad port
		"https://{id}.sessions example.com",           // not a host
		"https://{id}.sessions.example.com:8080:8080", // not a host
	} {
		if tmpl, err := ParseURLTemplate(s); err == nil {
			t.Errorf("ParseURLTemplate(%q) = %+v, want an error", s, tmpl)
		}
	}
}

func TestURLTemplateMatch(t *testing.T) {
	type want struct {
		id      string
		session bool
	}
	app := want{"", false}    // not a session host: the app's
	nothing := want{"", true} // under the session domain, but no session
	session := want{testID, true}

	for _, c := range []struct {
		template string
		hosts    map[string]want
	}{
		{"https://{id}.sessions.example.com", map[string]want{
			"s-abcdefg234.sessions.example.com":      session,
			"s-abcdefg234.sessions.example.com:443":  session,
			"S-ABCDEFG234.Sessions.Example.COM":      session,
			"s-abcdefg234.sessions.example.com.":     session,
			"s-abcdefg234.sessions.example.com:8443": nothing, // another port
			"s-abcdefg234.sessions.example.com:80":   nothing, // http's port, not https's
			"nope.sessions.example.com":              nothing,
			"s-abcdefg23.sessions.example.com":       nothing, // too short
			"s-abcdefg2341.sessions.example.com":     nothing,
			"s-abcdefg189.sessions.example.com":      nothing, // 1, 8, 9 are not in the alphabet
			"a.s-abcdefg234.sessions.example.com":    nothing,
			"s-abcdefg234.x.sessions.example.com":    nothing,
			"sessions.example.com":                   app,
			"app.example.com":                        app,
			"s-abcdefg234.example.com":               app,
			"s-abcdefg234.sessions.example.com.evil": app,
			"evilsessions.example.com":               app,
			"s-abcdefg234.evilsessions.example.com":  app,
			"10.0.0.7:8080":                          app,
			"":                                       app,
		}},
		{"http://{id}.sessions.localtest.me:8080", map[string]want{
			"s-abcdefg234.sessions.localtest.me:8080": session,
			"S-Abcdefg234.SESSIONS.localtest.me:8080": session,
			"s-abcdefg234.sessions.localtest.me":      nothing, // the template names a port
			"s-abcdefg234.sessions.localtest.me:80":   nothing,
			"s-abcdefg234.sessions.localtest.me:8081": nothing,
			"app.localtest.me:8080":                   app,
			"localhost:8080":                          app,
		}},
		{"http://{id}.sessions.localtest.me", map[string]want{
			"s-abcdefg234.sessions.localtest.me":     session,
			"s-abcdefg234.sessions.localtest.me:80":  session,
			"s-abcdefg234.sessions.localtest.me:443": nothing,
		}},
	} {
		tmpl := mustTemplate(t, c.template)
		for host, w := range c.hosts {
			m, isSession := tmpl.Match(host, "/mcp%2Fx")
			if m.ID != w.id || isSession != w.session {
				t.Errorf("%s: Match(%q) = %q, %v; want %q, %v", c.template, host, m.ID, isSession, w.id, w.session)
			}
			// The host names the session: the path is all the session's.
			if w.id != "" && (m.Base != "" || m.Path != "/mcp%2Fx") {
				t.Errorf("%s: Match(%q) = %+v, want no base and the whole path", c.template, host, m)
			}
		}
	}
}

// A session's own URL is one the template matches back to the session.
func TestURLTemplateRoundTrip(t *testing.T) {
	for _, s := range []string{
		"https://{id}.sessions.example.com", "http://{id}.sessions.localtest.me:8080",
		"https://sessions.example.com/{id}", "http://sessions.localtest.me:8080/{id}",
	} {
		tmpl := mustTemplate(t, s)
		u, err := url.Parse(tmpl.MCP(testID))
		if err != nil {
			t.Fatal(err)
		}
		if m, ok := tmpl.Match(u.Host, u.EscapedPath()); !ok || m.ID != testID || m.Path != "/mcp" {
			t.Errorf("%s: Match(%q, %q) = %+v, %v", s, u.Host, u.EscapedPath(), m, ok)
		}
	}
}

func TestURLTemplateMatchInPath(t *testing.T) {
	tmpl := mustTemplate(t, "https://sessions.example.com/{id}")
	if tmpl.PerHost() || !mustTemplate(t, "https://{id}.sessions.example.com").PerHost() {
		t.Error("PerHost is true of the host form only")
	}
	if got := tmpl.Origin(testID); got != "https://sessions.example.com" {
		t.Errorf("Origin = %q", got)
	}
	const base = "/" + testID
	for _, c := range []struct {
		host, path string
		want       Match
		session    bool
	}{
		{"sessions.example.com", base + "/mcp", Match{testID, base, "/mcp"}, true},
		{"Sessions.Example.COM.:443", base + "/mcp/a/b", Match{testID, base, "/mcp/a/b"}, true},
		// The rest stays as it was sent: nothing is decoded or cleaned here.
		{"sessions.example.com", base + "/mcp/a%2Fb", Match{testID, base, "/mcp/a%2Fb"}, true},
		{"sessions.example.com", base + "//mcp/../x", Match{testID, base, "//mcp/../x"}, true},
		{"sessions.example.com", base + "/", Match{testID, base, "/"}, true},
		{"sessions.example.com", base, Match{testID, base, "/"}, true},
		{"sessions.example.com", "/s-bcdfg/vnc", Match{"s-bcdfg", "/s-bcdfg", "/vnc"}, true}, // a warm pool's ID
		// On the host, but naming no session.
		{"sessions.example.com", "/", Match{}, true},
		{"sessions.example.com", "", Match{}, true},
		{"sessions.example.com", "/mcp", Match{}, true},
		{"sessions.example.com", "//" + testID + "/mcp", Match{}, true},
		{"sessions.example.com", "/s/" + testID + "/mcp", Match{}, true},
		{"sessions.example.com", "/s-abcdefg189/mcp", Match{}, true},
		{"sessions.example.com", "/s%2Dabcdefg234/mcp", Match{}, true}, // an ID has nothing to escape
		{"sessions.example.com", "/S-ABCDEFG234/mcp", Match{}, true},
		{"sessions.example.com", "/.well-known/oauth-protected-resource" + base + "/mcp", Match{}, true},
		{"sessions.example.com:8443", base + "/mcp", Match{}, true}, // another port
		// Not the sessions' host: the app's.
		{"app.example.com", base + "/mcp", Match{}, false},
		{testID + ".sessions.example.com", "/mcp", Match{}, false},
		{"evilsessions.example.com", base + "/mcp", Match{}, false},
		{"sessions.example.com.evil", base + "/mcp", Match{}, false},
		{"", base + "/mcp", Match{}, false},
	} {
		if got, session := tmpl.Match(c.host, c.path); got != c.want || session != c.session {
			t.Errorf("Match(%q, %q) = %+v, %v; want %+v, %v", c.host, c.path, got, session, c.want, c.session)
		}
	}
}
