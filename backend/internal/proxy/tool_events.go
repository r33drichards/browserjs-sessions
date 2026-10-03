package proxy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// callBody observes the bytes the pod consumes without changing streaming
// or the response. Tool results are deliberately excluded from exports.
type callBody struct {
	io.ReadCloser
	data     []byte
	overflow bool
}

func (b *callBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if len(b.data)+n <= 16<<20 {
		b.data = append(b.data, p[:n]...)
	} else {
		b.overflow = true
		b.data = nil
	}
	return n, err
}

func (p *Proxy) observeToolCalls(r *http.Request, sid string) func() {
	if p.ToolEvents == nil || r.Method != http.MethodPost || r.Body == nil {
		return func() {}
	}
	body := &callBody{ReadCloser: r.Body}
	r.Body = body
	started := time.Now().UTC().Format(time.RFC3339Nano)
	return func() {
		if body.overflow {
			return
		}
		body.data = bytes.TrimSpace(body.data)
		var messages []json.RawMessage
		if len(body.data) > 0 && body.data[0] == '[' {
			if json.Unmarshal(body.data, &messages) != nil {
				return
			}
		} else {
			messages = []json.RawMessage{body.data}
		}
		events := []map[string]any{}
		for _, message := range messages {
			var call struct {
				Method string          `json:"method"`
				ID     json.RawMessage `json:"id"`
				Params struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"params"`
			}
			if json.Unmarshal(message, &call) != nil || call.Method != "tools/call" || call.Params.Name == "" {
				continue
			}
			var nonce [16]byte
			if _, err := rand.Read(nonce[:]); err != nil {
				continue
			}
			event := map[string]any{"id": hex.EncodeToString(nonce[:]), "session_id": sid,
				"timestamp": started, "type": "tool_call", "stage": "request", "server": "mcp-js",
				"tool": call.Params.Name, "request_id": call.ID}
			if len(call.Params.Arguments) <= 1<<20 {
				event["arguments"] = call.Params.Arguments
			} else {
				event["arguments_truncated"] = true
			}
			events = append(events, event)
		}
		if len(events) == 0 {
			return
		}
		// A slow or unavailable collector must not create unbounded goroutines.
		select {
		case p.eventSlots <- struct{}{}:
			go func() { defer func() { <-p.eventSlots }(); p.ToolEvents(events) }()
		default:
			slog.Warn("tool event collector busy; request events omitted", "session", sid)
		}
	}
}
