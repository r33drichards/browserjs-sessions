package proxy

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestToolEventCapturePreservesBodyAndIgnoresOtherMethods(t *testing.T) {
	body := `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"run_js","arguments":{"code":"await browser.title()"}}},{"id":2,"method":"tools/list"}]`
	events := make(chan []map[string]any, 1)
	p := &Proxy{ToolEvents: func(batch []map[string]any) { events <- batch }, eventSlots: make(chan struct{}, 8)}
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	finish := p.observeToolCalls(r, "s-aaaaa")
	got, err := io.ReadAll(r.Body)
	if err != nil || string(got) != body {
		t.Fatal("observer changed the forwarded body")
	}
	finish()
	select {
	case batch := <-events:
		if len(batch) != 1 || batch[0]["tool"] != "run_js" || batch[0]["session_id"] != "s-aaaaa" || batch[0]["stage"] != "request" {
			t.Fatalf("events: %#v", batch)
		}
		if _, present := batch[0]["allowed"]; present {
			t.Fatal("request invented an authorization decision")
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
}

func TestSlowCollectorDoesNotBlockToolCall(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	p := &Proxy{eventSlots: make(chan struct{}, 1), ToolEvents: func([]map[string]any) { close(entered); <-release }}
	defer close(release)
	body := `{"method":"tools/call","params":{"name":"run_js","arguments":{}}}`
	collect := func() {
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
		finish := p.observeToolCalls(r, "s-aaaaa")
		_, _ = io.ReadAll(r.Body)
		finish()
	}
	collect()
	<-entered
	done := make(chan struct{})
	go func() { collect(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("collector blocked execution")
	}
	if len(p.eventSlots) != 1 {
		t.Fatal("collector capacity exceeded")
	}
}
