package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// A new session's browser is started in the background, so that the first
// browser_execute call finds it ready: starting Chromium under gVisor takes
// tens of seconds. Only for a session just created or adopted from the warm
// pool, never for a pod waiting in the pool (which idles without it) and
// never on a wake (a restored pod keeps whatever was running).
const (
	// How long a new session may take to come up and answer.
	browserStartWait = 5 * time.Minute
	// Between tries while its pod is not running or not answering yet.
	browserStartPoll = time.Second
)

// StartBrowser asks session id's pod to start its Chromium, without a window,
// once the session runs. It is not use of the session: nothing is recorded
// through Idle, so a session nobody calls still sleeps when it is idle. It returns at once; the work is in the background
// and only logged. The pod's answer to a repeat is to do nothing, so a retry
// never starts a second browser or reopens one somebody closed.
func (p *Proxy) StartBrowser(id string) {
	p.init()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), browserStartWait)
		defer cancel()
		if err := p.startBrowser(ctx, id); err != nil {
			slog.Warn("browser not started ahead of use; the first call starts it", "session", id, "err", err)
		}
	}()
}

func (p *Proxy) startBrowser(ctx context.Context, id string) error {
	return p.startBrowserWithPoll(ctx, id, browserStartPoll)
}

func (p *Proxy) startBrowserWithPoll(ctx context.Context, id string, poll time.Duration) error {
	p.init()
	for {
		err := p.askBrowserStart(ctx, id)
		if err == nil || errors.Is(err, errNoBrowserStart) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(poll):
		}
	}
}

// errNoBrowserStart: the pod's image cannot (from before it could).
var errNoBrowserStart = errors.New("the session's image does not start its browser ahead of use")

func (p *Proxy) askBrowserStart(ctx context.Context, id string) error {
	// Not EnsureAwake: a session that is not running is not woken for this.
	s, err := p.Waker.Running(ctx, id)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+p.Target(s, browserPort)+"/browser/start", strings.NewReader("{}"))
	if err != nil {
		return err
	}
	// As the pod's own server expects from a caller that is not a web page.
	req.Host = "localhost:" + strconv.Itoa(browserPort)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.quick.RoundTrip(req)
	if err != nil {
		p.Waker.Invalidate(id)
		return err
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode/100 == 2:
		return nil
	case resp.StatusCode == http.StatusNotFound:
		return errNoBrowserStart
	default:
		return errors.New("the pod answered " + resp.Status)
	}
}
