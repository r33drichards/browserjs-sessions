package provider

import (
	"net/http/httptest"
	"testing"

	"github.com/r33drichards/computer-use/terraform-provider-metronome/internal/client"
	"github.com/r33drichards/computer-use/terraform-provider-metronome/internal/fakeapi"
)

// bare is a provider that has not been configured, with no environment.
func bare(t *testing.T) *harness {
	t.Helper()
	t.Setenv(envEndpoint, "")
	t.Setenv(envToken, "")
	h := &harness{t: t}
	h.server, h.schema = newServer(t)
	return h
}

func TestProviderNeedsAToken(t *testing.T) {
	h := bare(t)
	wantError(t, h.configure(cfg{}), "Missing Metronome token", envToken)
	wantError(t, h.configure(cfg{"bearer_token": unknown}), "Unknown Metronome token")
	wantError(t, h.configure(cfg{"endpoint": unknown, "bearer_token": "t"}), "Unknown Metronome endpoint")
	wantError(t, h.configure(cfg{"endpoint": "api.metronome.com", "bearer_token": "t"}), "Invalid Metronome endpoint")
}

func TestProviderReadsTheEnvironment(t *testing.T) {
	fake := fakeapi.New(testToken)
	ts := httptest.NewServer(fake.Handler())
	t.Cleanup(ts.Close)
	h := bare(t)
	t.Setenv(envEndpoint, ts.URL)
	t.Setenv(envToken, testToken)
	noErrors(t, "configure", h.configure(cfg{}))

	got, diags := h.data("metronome_pricing_unit", cfg{"name": "USD (cents)"})
	noErrors(t, "read", diags)
	if got["id"] != client.USDCreditTypeID || got["is_currency"] != true {
		t.Errorf("USD read as %v", got)
	}
	reqs := fake.Requests()
	if len(reqs) != 1 || !reqs[0].Authorized || reqs[0].UserAgent != "terraform-provider-metronome/test" {
		t.Errorf("requests %+v", reqs)
	}
}

func TestProviderWarnsAboutPlainHTTP(t *testing.T) {
	h := bare(t)
	diags := h.configure(cfg{"endpoint": "http://metronome.example.test", "bearer_token": "t"})
	noErrors(t, "configure", diags)
	wantWarning(t, diags, "not https", "metronome.example.test")
	// A loopback address, as the tests use, is not warned about.
	if w := warningsOf(h.configure(cfg{"endpoint": "http://127.0.0.1:1", "bearer_token": "t"})); len(w) != 0 {
		t.Errorf("warnings for loopback: %v", w)
	}
}

func TestPricingUnit(t *testing.T) {
	h := newHarness(t)
	got, diags := h.data("metronome_pricing_unit", cfg{"name": "cloud consumption units"})
	noErrors(t, "read", diags)
	if got["id"] != "fa2f1b3d-9d52-4951-a099-25991fd394d6" || got["is_currency"] != false {
		t.Errorf("read %v", got)
	}
	_, diags = h.data("metronome_pricing_unit", cfg{"name": "EUR"})
	wantError(t, diags, "No such pricing unit", "USD (cents), cloud consumption units")
}
