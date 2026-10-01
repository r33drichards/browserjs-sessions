package sessions

import (
	"strings"
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
		"https://sessions.example.com/{id}",           // not in the host
		"https://s-aaaaaaaaaa.example.com/{id}",       // not in the host
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
			id, isSession := tmpl.Match(host)
			if id != w.id || isSession != w.session {
				t.Errorf("%s: Match(%q) = %q, %v; want %q, %v", c.template, host, id, isSession, w.id, w.session)
			}
		}
	}
}

// A session's own URL is a host the template matches back to the session.
func TestURLTemplateRoundTrip(t *testing.T) {
	for _, s := range []string{"https://{id}.sessions.example.com", "http://{id}.sessions.localtest.me:8080"} {
		tmpl := mustTemplate(t, s)
		host := tmpl.Base(testID)[strings.Index(tmpl.Base(testID), "://")+3:]
		if id, ok := tmpl.Match(host); !ok || id != testID {
			t.Errorf("%s: Match(%q) = %q, %v", s, host, id, ok)
		}
	}
}
