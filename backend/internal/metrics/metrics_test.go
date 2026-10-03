package metrics_test

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/r33drichards/computer-use/backend/internal/metrics"
)

func TestClassifyIsAShortFixedList(t *testing.T) {
	for _, c := range []struct {
		session      bool
		method, path string
		want         string
	}{
		{true, "POST", "/mcp", "proxy/mcp"},
		{true, "DELETE", "/mcp/x", "proxy/mcp"},
		{true, "GET", "/mcp", "proxy/mcp_stream"},
		{true, "PUT", "/api/artifact-uploads/" + strings.Repeat("ab", 32), "proxy/upload"},
		{true, "GET", "/vnc", "proxy/vnc"},
		{true, "GET", "/anything/else/" + strings.Repeat("x", 100), "proxy/other"},
		{false, "GET", "/api/sessions", "api/sessions"},
		{false, "GET", "/api/sessions/s-aaaaaaaaaa", "api/session"},
		{false, "POST", "/api/sessions/s-aaaaaaaaaa/vnc-ticket", "api/vnc_ticket"},
		{false, "GET", "/api/sessions/s-aaaaaaaaaa/files/a.txt", "proxy/files"},
		{false, "POST", "/api/sessions/s-aaaaaaaaaa/sleep", "api/session_sleep_wake"},
		{false, "GET", "/api/billing/usage", "api/billing"},
		{false, "GET", "/api/tokens", "api/tokens"},
		{false, "GET", "/api/whatever/someone@example.com", "api/other"},
		{false, "POST", "/stripe/webhook", "other/webhook"},
		{false, "GET", "/healthz", "other/healthz"},
		{false, "GET", "/sessions/s-aaaaaaaaaa", "web/page"},
	} {
		component, route := metrics.Classify(c.session, c.method, c.path)
		if got := component + "/" + route; got != c.want {
			t.Errorf("Classify(%v, %s %s) = %s, want %s", c.session, c.method, c.path, got, c.want)
		}
	}
}

// Requests are counted by class and status, and a stream or a websocket
// still works through the instrumented handler.
func TestInstrumentCountsAndPassesStreamsThrough(t *testing.T) {
	classify := func(r *http.Request) (string, string) { return "test", strings.TrimPrefix(r.URL.Path, "/") }
	h := metrics.Instrument(classify, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/teapot":
			w.WriteHeader(http.StatusTeapot)
		case "/stream":
			_, _ = io.WriteString(w, "one\n")
			w.(http.Flusher).Flush()
		case "/socket":
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			defer conn.Close()
			_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
			_ = buf.Flush()
		default:
			_, _ = io.WriteString(w, "ok")
		}
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, path := range []string{"/plain", "/teapot", "/stream"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(conn, "GET /socket HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	if resp, err := http.ReadResponse(bufio.NewReader(conn), nil); err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("websocket through the instrumented handler: %v, %v", resp, err)
	}
	_ = conn.Close()

	want := map[[2]string]float64{{"plain", "200"}: 1, {"teapot", "418"}: 1, {"stream", "200"}: 1, {"socket", "101"}: 1}
	deadline := time.Now().Add(5 * time.Second)
	for key, n := range want {
		for testutil.ToFloat64(metrics.Requests.WithLabelValues("test", key[0], key[1])) != n {
			if time.Now().After(deadline) {
				t.Fatalf("requests{route=%s,code=%s} = %v, want %v", key[0], key[1],
					testutil.ToFloat64(metrics.Requests.WithLabelValues("test", key[0], key[1])), n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// The per-session series are labelled by the session's ID and nothing
// else, and a session the replica has nothing more to say of has none.
func TestSessionSeriesComeAndGoWithTheSessions(t *testing.T) {
	reg := prometheus.NewRegistry()
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	states := []metrics.SessionState{
		{ID: "s-aaaaaaaaaa", LastActive: at, Open: 2, InFlight: 1},
		{ID: "s-bbbbbbbbbb"},
	}
	if err := metrics.Sessions(reg, func() []metrics.SessionState { return states }); err != nil {
		t.Fatal(err)
	}
	series := func() map[string]int {
		t.Helper()
		families, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		n := map[string]int{}
		for _, f := range families {
			for _, m := range f.GetMetric() {
				n[f.GetName()]++
				if labels := m.GetLabel(); len(labels) != 1 || labels[0].GetName() != "session" {
					t.Errorf("%s has labels %v, want only session", f.GetName(), labels)
				}
			}
		}
		return n
	}
	got := series()
	if got["browserjs_session_last_activity_timestamp_seconds"] != 1 || got["browserjs_session_open_connections"] != 2 || got["browserjs_session_calls_in_flight"] != 2 {
		t.Fatalf("series = %v", got)
	}
	if v := testutil.ToFloat64(oneOf(t, reg, "browserjs_session_last_activity_timestamp_seconds")); v != float64(at.Unix()) {
		t.Errorf("last activity = %v, want %v", v, at.Unix())
	}
	states = states[:0]
	if got := series(); len(got) != 0 {
		t.Fatalf("series of sessions that are gone: %v", got)
	}
}

// oneOf is a collector of the one series of a family in reg.
func oneOf(t *testing.T, reg *prometheus.Registry, name string) prometheus.Collector {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == name && len(f.GetMetric()) == 1 {
			g := prometheus.NewGauge(prometheus.GaugeOpts{Name: name})
			g.Set(f.GetMetric()[0].GetGauge().GetValue())
			return g
		}
	}
	t.Fatalf("no single series of %s", name)
	return nil
}

func TestMetricsAreServedOnTheirOwnHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "browserjs_leader") {
		t.Fatalf("/metrics: %d %.200s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("anything else on the metrics port: %d, want 404", rec.Code)
	}
}
