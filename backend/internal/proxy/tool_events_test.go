package proxy

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestToolEventCapturePreservesBody(t *testing.T) {
	body := `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"run_js","arguments":{"code":"await browser.title()"}}},{"id":2,"method":"tools/list"}]`
	var events []map[string]any
	p := &Proxy{ToolEvents: func(_ context.Context, batch []map[string]any) error { events = batch; return nil }}
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	if !p.recordToolCalls(httptest.NewRecorder(), r, "s-aaaaa") {
		t.Fatal("durable capture refused")
	}
	got, err := io.ReadAll(r.Body)
	if err != nil || string(got) != body {
		t.Fatal("observer changed the forwarded body")
	}
	if len(events) != 1 || events[0]["tool"] != "run_js" || events[0]["session_id"] != "s-aaaaa" || events[0]["stage"] != "request" {
		t.Fatalf("events: %#v", events)
	}
	if _, present := events[0]["allowed"]; present {
		t.Fatal("request invented an authorization decision")
	}
}

func TestRecorderFailureNeverForwardsToolCall(t *testing.T) {
	e := newEnv(t)
	e.proxy.ToolEvents = func(context.Context, []map[string]any) error { return errors.New("disk full") }
	rec := e.do("POST", "/mcp", alice, `{"method":"tools/call","params":{"name":"run_js","arguments":{}}}`)
	if rec.Code != 503 {
		t.Fatalf("failed capture: %d %s", rec.Code, rec.Body)
	}
	if len(e.seen()) != 0 {
		t.Fatal("unrecorded call reached the pod")
	}
	e.proxy.ToolEvents = func(context.Context, []map[string]any) error { return nil }
	rec = e.do("POST", "/mcp", alice, `{"method":"tools/call","params":{"name":"run_js","arguments":{}}}`)
	if rec.Code != 200 || len(e.seen()) != 1 {
		t.Fatalf("durably accepted call not forwarded: %d %s", rec.Code, rec.Body)
	}
}

func TestNonToolRequestDoesNotRequireRecorder(t *testing.T) {
	p := &Proxy{ToolEvents: func(context.Context, []map[string]any) error { t.Fatal("non-tool request was captured"); return nil }}
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"method":"tools/list"}`))
	if !p.recordToolCalls(httptest.NewRecorder(), r, "s-aaaaa") {
		t.Fatal("non-tool request refused")
	}
}
