// Package metrics is what the backend says about itself to Prometheus.
//
// Nothing here decides anything: the backend reads none of it back, and a
// collector that is down or late changes nothing a user sees. (Whether a
// session is idle is read off the session; see internal/idle.)
//
// The repository and its logs are public, and so are these names. No label
// carries an address or an owner. The only label that grows with use is a
// session's ID, on the per-session gauges, whose series go when this
// replica has nothing left to say of the session.
package metrics

import (
	"bufio"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry holds every metric of the backend.
var Registry = prometheus.NewRegistry()

// Calls that run for minutes are ordinary (an MCP tool call), so the
// buckets reach ten minutes.
var buckets = []float64{.005, .025, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 180, 600}

var (
	// Requests is every request answered, by component ("api", "proxy",
	// "web", "other"), route class (Classify) and status code.
	Requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "browserjs_http_requests_total",
		Help: "Requests answered, by component, route class and status code.",
	}, []string{"component", "route", "code"})
	// RequestDuration is how long they took, to the last byte. A stream or
	// a viewer counts when it closes.
	RequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "browserjs_http_request_duration_seconds",
		Help:    "Time to answer a request, by component and route class.",
		Buckets: buckets,
	}, []string{"component", "route"})
	// RequestsInFlight is the requests being answered now.
	RequestsInFlight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "browserjs_http_requests_in_flight",
		Help: "Requests being answered now, by component and route class.",
	}, []string{"component", "route"})

	// Wakes is the sessions a request to this replica woke, by result:
	// "ok", "refused" (billing), "stopped", "failed", "timeout", "error".
	Wakes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "browserjs_session_wakes_total",
		Help: "Sleeping sessions woken by a request, by result.",
	}, []string{"result"})
	// WakeDuration is from the wake to the session running.
	WakeDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "browserjs_session_wake_duration_seconds",
		Help:    "Time from waking a session to its pod answering, by result.",
		Buckets: buckets,
	}, []string{"result"})
	// Sleeps is the sessions put to sleep through this replica, by reason
	// (idle, sleep, credit, payment-method, blocked) and result: "ok",
	// "changed" (used or stopped first), "error".
	Sleeps = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "browserjs_session_sleeps_total",
		Help: "Sessions put to sleep, by reason and result.",
	}, []string{"reason", "result"})
	// SleepDuration includes the snapshot.
	SleepDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "browserjs_session_sleep_duration_seconds",
		Help:    "Time to put a session to sleep, its snapshot included, by reason.",
		Buckets: buckets,
	}, []string{"reason"})

	// GateHeld counts the looks at a running session that found its first
	// policy not in force yet, and GateWait how long a session was held
	// from the first such look to the policy being in force.
	GateHeld = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "browserjs_policy_gate_held_total",
		Help: "Looks at a running session whose first policy was not in force yet.",
	})
	GateWait = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "browserjs_policy_gate_wait_seconds",
		Help:    "Time a session was held for its first policy, as this replica saw it.",
		Buckets: buckets,
	})

	// ActivityWrites is the writes of a session's activity onto the
	// session, by kind ("mark", "last-use", "look") and result.
	ActivityWrites = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "browserjs_activity_writes_total",
		Help: "Writes of session activity to the cluster (and looks at sessions with streams), by kind and result.",
	}, []string{"kind", "result"})

	// Leader is 1 while this replica runs the periodic passes.
	Leader = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "browserjs_leader",
		Help: "1 while this replica holds the Lease and runs the periodic passes.",
	})
	// Passes is the passes run, by pass ("idle", "billing", "billing-delete",
	// "balance") and result.
	Passes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "browserjs_passes_total",
		Help: "Periodic passes run by this replica, by pass and result.",
	}, []string{"pass", "result"})
)

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		Requests, RequestDuration, RequestsInFlight,
		Wakes, WakeDuration, Sleeps, SleepDuration,
		GateHeld, GateWait, ActivityWrites, Leader, Passes,
	)
}

// Handler serves the metrics. It goes on a port of its own, which Pomerium
// does not route to.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(Registry, promhttp.HandlerOpts{}))
	return mux
}

// SessionState is what one replica knows of one session it proxies to.
type SessionState struct {
	ID         string
	LastActive time.Time // zero if it has recorded no use
	Open       int       // connections and calls holding the session awake
	InFlight   int       // calls in flight
}

var (
	lastActivityDesc = prometheus.NewDesc("browserjs_session_last_activity_timestamp_seconds",
		"When this replica last saw the session used, in seconds since the epoch.", []string{"session"}, nil)
	openDesc = prometheus.NewDesc("browserjs_session_open_connections",
		"Connections and calls this replica holds open to the session.", []string{"session"}, nil)
	inFlightDesc = prometheus.NewDesc("browserjs_session_calls_in_flight",
		"Calls to the session this replica is proxying now.", []string{"session"}, nil)
)

type sessionCollector struct{ states func() []SessionState }

func (sessionCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- lastActivityDesc
	ch <- openDesc
	ch <- inFlightDesc
}

func (c sessionCollector) Collect(ch chan<- prometheus.Metric) {
	for _, s := range c.states() {
		if !s.LastActive.IsZero() {
			ch <- prometheus.MustNewConstMetric(lastActivityDesc, prometheus.GaugeValue, float64(s.LastActive.UnixMilli())/1000, s.ID)
		}
		ch <- prometheus.MustNewConstMetric(openDesc, prometheus.GaugeValue, float64(s.Open), s.ID)
		ch <- prometheus.MustNewConstMetric(inFlightDesc, prometheus.GaugeValue, float64(s.InFlight), s.ID)
	}
}

// Sessions has the per-session gauges read, at each scrape, from states:
// the sessions a replica has something to say of, and no others. A session
// that states stops returning has no series any more.
func Sessions(reg prometheus.Registerer, states func() []SessionState) error {
	return reg.Register(sessionCollector{states})
}

// Classify names the class of a request for the labels: a short, fixed
// list, whatever the path. session is whether the request named a session
// (it was the proxy's), and path is then the path within the session.
func Classify(session bool, method, path string) (component, route string) {
	if session {
		switch {
		case path == "/mcp" || strings.HasPrefix(path, "/mcp/"):
			if method == http.MethodGet || method == http.MethodHead {
				return "proxy", "mcp_stream"
			}
			return "proxy", "mcp"
		case strings.HasPrefix(path, "/api/artifact-uploads/"):
			return "proxy", "upload"
		case path == "/vnc":
			return "proxy", "vnc"
		}
		return "proxy", "other"
	}
	switch {
	case path == "/healthz":
		return "other", "healthz"
	case strings.HasPrefix(path, "/api/sessions"):
		rest := strings.TrimPrefix(path, "/api/sessions")
		switch {
		case strings.HasSuffix(rest, "/vnc-ticket"):
			return "api", "vnc_ticket"
		case strings.Contains(rest, "/files"):
			return "proxy", "files"
		case strings.Contains(rest, "/policy"):
			return "api", "session_policy"
		case strings.HasSuffix(rest, "/sleep"), strings.HasSuffix(rest, "/wake"):
			return "api", "session_sleep_wake"
		case rest == "" || rest == "/":
			return "api", "sessions"
		}
		return "api", "session"
	case strings.HasPrefix(path, "/api/billing"):
		return "api", "billing"
	case strings.HasPrefix(path, "/api/tokens"):
		return "api", "tokens"
	case strings.HasPrefix(path, "/api/polic"):
		return "api", "policies"
	case path == "/api" || strings.HasPrefix(path, "/api/"):
		return "api", "other"
	case strings.HasPrefix(path, "/stripe/"), strings.HasPrefix(path, "/metronome/"):
		return "other", "webhook"
	case strings.HasPrefix(path, "/oauth/"):
		return "api", "token_exchange"
	}
	return "web", "page"
}

// Instrument counts and times every request next answers. classify is
// asked before next runs, of the request as it arrived.
func Instrument(classify func(r *http.Request) (component, route string), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		component, route := classify(r)
		inFlight := RequestsInFlight.WithLabelValues(component, route)
		inFlight.Inc()
		start := time.Now()
		rec := &recorder{ResponseWriter: w}
		defer func() {
			inFlight.Dec()
			RequestDuration.WithLabelValues(component, route).Observe(time.Since(start).Seconds())
			Requests.WithLabelValues(component, route, strconv.Itoa(rec.status())).Inc()
		}()
		next.ServeHTTP(rec, r)
	})
}

// recorder notes the status a handler answers with. It passes on flushing
// and hijacking: streams and websockets go through it.
type recorder struct {
	http.ResponseWriter
	code     int
	hijacked bool
}

func (r *recorder) status() int {
	switch {
	case r.code != 0:
		return r.code
	case r.hijacked:
		return http.StatusSwitchingProtocols
	}
	return http.StatusOK
}

func (r *recorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *recorder) Flush() { _ = http.NewResponseController(r.ResponseWriter).Flush() }

func (r *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(r.ResponseWriter).Hijack()
	if err == nil {
		r.hijacked = true
	}
	return conn, rw, err
}

// Unwrap lets http.ResponseController reach the writer underneath.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
