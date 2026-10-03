package proxy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// recordToolCalls accepts the event durably before a request can execute.
// Refusing ingestion refuses the call: no bounded best-effort background queue.
func (p *Proxy) recordToolCalls(w http.ResponseWriter, r *http.Request, sid string) bool {
	if p.ToolEvents == nil || r.Method != http.MethodPost || r.Body == nil {
		return true
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	_ = r.Body.Close()
	if err != nil {
		http.Error(w, "cannot read tool request (maximum 16 MiB)", http.StatusRequestEntityTooLarge)
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	data = bytes.TrimSpace(data)
	var messages []json.RawMessage
	if len(data) > 0 && data[0] == '[' {
		if json.Unmarshal(data, &messages) != nil {
			return true
		} // pod rejects malformed MCP
	} else {
		messages = []json.RawMessage{data}
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
			http.Error(w, "cannot identify tool event", http.StatusServiceUnavailable)
			return false
		}
		event := map[string]any{"id": hex.EncodeToString(nonce[:]), "session_id": sid,
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano), "type": "tool_call", "stage": "request", "server": "mcp-js",
			"tool": call.Params.Name, "request_id": call.ID}
		if len(call.Params.Arguments) <= 1<<20 {
			event["arguments"] = call.Params.Arguments
		} else {
			event["arguments_truncated"] = true
		}
		events = append(events, event)
	}
	if len(events) == 0 {
		return true
	}
	if err := p.ToolEvents(r.Context(), events); err != nil {
		http.Error(w, "tool event could not be durably recorded; call was not forwarded", http.StatusServiceUnavailable)
		return false
	}
	return true
}
