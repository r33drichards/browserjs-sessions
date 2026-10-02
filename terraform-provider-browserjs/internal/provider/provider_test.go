package provider

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/r33drichards/browserjs-sessions/terraform-provider-browserjs/internal/fakeapi"
)

func TestProviderNeedsAToken(t *testing.T) {
	h := newHarness(t)
	h.server, h.schema = newServer(t)
	wantError(t, h.configure(cfg{"endpoint": h.url}), "token", "Missing browserjs token", envToken)
}

func TestProviderReadsTheEnvironment(t *testing.T) {
	h := newHarness(t)
	h.server, h.schema = newServer(t)
	t.Setenv(envEndpoint, h.url)
	t.Setenv(envToken, testToken)
	noErrors(t, "configure", h.configure(cfg{}))
	if _, diags := h.data("browserjs_sessions", cfg{}); len(errorsOf(diags)) > 0 {
		t.Fatal(errorsOf(diags))
	}
}

func TestProviderAttributesWinOverTheEnvironment(t *testing.T) {
	h := newHarness(t)
	h.server, h.schema = newServer(t)
	t.Setenv(envEndpoint, "https://unreachable.invalid")
	t.Setenv(envToken, "bjs_wrong")
	noErrors(t, "configure", h.configure(cfg{"endpoint": h.url, "token": testToken}))
	if _, diags := h.data("browserjs_sessions", cfg{}); len(errorsOf(diags)) > 0 {
		t.Fatal(errorsOf(diags))
	}
}

func TestProviderRejectsABadEndpoint(t *testing.T) {
	h := newHarness(t)
	h.server, h.schema = newServer(t)
	wantError(t, h.configure(cfg{"endpoint": "api.browserjs.com", "token": testToken}), "endpoint", "Invalid browserjs endpoint")
}

func TestProviderWarnsAboutPlainHTTP(t *testing.T) {
	h := newHarness(t)
	h.server, h.schema = newServer(t)
	diags := h.configure(cfg{"endpoint": "http://api.example.test", "token": testToken})
	noErrors(t, "configure", diags)
	wantWarning(t, diags, "not https", "api.example.test")
	// The fake is on loopback, where http is expected.
	if w := warningsOf(h.configure(cfg{"endpoint": h.url, "token": testToken})); len(w) > 0 {
		t.Fatalf("unexpected warnings: %q", w)
	}
}

func TestProviderUnknownValues(t *testing.T) {
	h := newHarness(t)
	h.server, h.schema = newServer(t)
	wantError(t, h.configure(cfg{"endpoint": h.url, "token": unknown}), "Unknown browserjs token")
}

func TestTokenIsMarkedSensitive(t *testing.T) {
	_, schema := newServer(t)
	for _, a := range schema.Provider.Block.Attributes {
		if a.Name == "token" && !a.Sensitive {
			t.Fatal("token is not sensitive")
		}
	}
}

func TestEveryRequestCarriesTheTokenAndUserAgent(t *testing.T) {
	h := newHarness(t)
	s := h.mustApply("browserjs_session", h.null("browserjs_session"), cfg{"name": "a"})
	h.mustApply("browserjs_session_policy", h.null("browserjs_session_policy"), policyConfig(str(s, "id"), noScripting))
	reqs := h.fake.Requests()
	if len(reqs) < 3 {
		t.Fatalf("only %d requests", len(reqs))
	}
	for _, r := range reqs {
		if !r.Authorized {
			t.Errorf("%s %s without the bearer token", r.Method, r.Path)
		}
		if r.UserAgent != "terraform-provider-browserjs/test" {
			t.Errorf("%s %s: User-Agent %q", r.Method, r.Path, r.UserAgent)
		}
		if !strings.HasPrefix(r.Path, "/v1/") {
			t.Errorf("%s is not under /v1", r.Path)
		}
	}
}

// A refused token is explained, and the token itself is never in a message.
func TestABadTokenIsNotEchoed(t *testing.T) {
	const wrong = "bjs_wrong_c0ffee"
	fake := fakeapi.New(testToken)
	ts := httptest.NewServer(fake.Handler())
	defer ts.Close()
	h := &harness{t: t, fake: fake, url: ts.URL}
	h.server, h.schema = newServer(t)
	noErrors(t, "configure", h.configure(cfg{"endpoint": ts.URL, "token": wrong}))

	p := h.plan("browserjs_session", h.null("browserjs_session"), cfg{"name": "a"})
	noErrors(t, "plan", p.diags)
	_, diags := h.apply(p)
	wantError(t, diags, "401", "invalid token", envToken)
	_, listDiags := h.data("browserjs_sessions", cfg{})
	for _, text := range append(errorsOf(diags), errorsOf(listDiags)...) {
		if strings.Contains(text, wrong) {
			t.Fatalf("the token is in a diagnostic: %s", text)
		}
	}
}

func TestDefaultPollEvery(t *testing.T) {
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second, 4 * time.Second}
	for n, w := range want {
		if got := defaultPollEvery(n); got != w {
			t.Errorf("poll %d waits %s, want %s", n, got, w)
		}
	}
	if got := defaultPollEvery(1000); got != 4*time.Second {
		t.Errorf("poll 1000 waits %s", got)
	}
}
