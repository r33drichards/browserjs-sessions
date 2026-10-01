package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/proxy"
)

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// On a signal, serve lets the requests in flight finish before it returns
// (and the process exits).
func TestServeWaitsForRequestsInFlight(t *testing.T) {
	started := make(chan struct{})
	var finished atomic.Bool
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		finished.Store(true)
		_, _ = io.WriteString(w, "done")
	})}
	ln := listen(t)
	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	served := make(chan error, 1)
	go func() { served <- serve(ctx, srv, ln, 5*time.Second, &proxy.Proxy{}) }()
	body := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			body <- err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		body <- string(b)
	}()
	<-started
	stop() // SIGTERM

	if err := <-served; err != nil {
		t.Errorf("serve: %v", err)
	}
	if !finished.Load() {
		t.Error("serve returned while a request was still in flight")
	}
	if got := <-body; got != "done" {
		t.Errorf("the request in flight was answered %q, want done", got)
	}
}

// A request that does not finish does not hold the process past its budget.
func TestServeGivesUpAfterTheGracePeriod(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	srv := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	})}
	ln := listen(t)
	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	served := make(chan error, 1)
	go func() { served <- serve(ctx, srv, ln, 200*time.Millisecond, &proxy.Proxy{}) }()
	go func() {
		if resp, err := http.Get("http://" + ln.Addr().String() + "/"); err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	begin := time.Now()
	stop()
	select {
	case <-served:
		if d := time.Since(begin); d < 200*time.Millisecond {
			t.Errorf("serve returned after %s, before the grace period was over", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve never returned")
	}
}

func TestServeReportsAListenerFailure(t *testing.T) {
	ln := listen(t)
	_ = ln.Close()
	if err := serve(t.Context(), &http.Server{}, ln, time.Second, &proxy.Proxy{}); err == nil {
		t.Error("serve on a closed listener returned nil")
	}
}
