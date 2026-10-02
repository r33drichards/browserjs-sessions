package computeruse_test

// Loads the built library and makes real calls through it, against a fake
// of the API host that the test starts. Nothing here talks to the real
// service, and the token is not a real one.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/r33drichards/browserjs-sessions/sdk/go/computeruse"
)

const token = "bjs_aaaaaaaaaaaa_c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0"

type call struct{ method, path, authorization, body string }

type fake struct {
	*httptest.Server
	mu    sync.Mutex
	calls []call
}

func session(name, state string) map[string]any {
	return map[string]any{
		"id": "s-abcde", "name": name, "owner": "you@example.com", "state": state,
		"created": "2026-10-01T12:00:00Z", "mcp_url": "https://sessions.example.test/s-abcde/mcp",
	}
}

func newFake(t *testing.T) *fake {
	f := &fake{}
	answer := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		f.mu.Lock()
		f.calls = append(f.calls, call{r.Method, r.URL.Path, r.Header.Get("Authorization"), body})
		f.mu.Unlock()

		route := r.Method + " " + r.URL.Path
		if route == "POST /oauth/token" {
			answer(w, 200, map[string]any{"access_token": "access-1", "token_type": "Bearer", "expires_in": 3600, "scope": ""})
			return
		}
		if r.Header.Get("Authorization") != "Bearer access-1" {
			answer(w, 401, map[string]string{"error": "invalid token"})
			return
		}
		switch route {
		case "GET /v1/me":
			answer(w, 200, map[string]any{"email": "you@example.com", "name": "You", "admin": false})
		case "POST /v1/sessions":
			var in struct{ Name string }
			_ = json.Unmarshal(raw, &in)
			answer(w, 201, session(in.Name, "starting"))
		case "PATCH /v1/sessions/s-abcde":
			answer(w, 200, session("smoke", "stopping"))
		case "POST /s-abcde/mcp":
			var message struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
				Params struct {
					Arguments struct{ Code string } `json:"arguments"`
				} `json:"params"`
			}
			_ = json.Unmarshal(raw, &message)
			if message.Method == "notifications/initialized" {
				w.WriteHeader(202)
				return
			}
			result := map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
			if message.Method == "tools/call" {
				text, _ := json.Marshal(map[string]string{"output": "ran: " + message.Params.Arguments.Code})
				result = map[string]any{"content": []map[string]string{{"type": "text", "text": string(text)}}}
			}
			reply, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": result})
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Mcp-Session-Id", "m-1")
			_, _ = io.WriteString(w, "event: message\ndata: "+string(reply)+"\n\n")
		default:
			answer(w, 404, map[string]string{"error": "session not found"})
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fake) client(t *testing.T) *computeruse.Client {
	options, err := computeruse.NewClientOptionsBuilder().ApiToken(token).BaseUrl(f.URL).MaxRetries(0).Build()
	if err != nil {
		t.Fatal(err)
	}
	client, err := computeruse.NewClient(options)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestCreateRunJsSleep(t *testing.T) {
	f := newFake(t)
	client := f.client(t)

	me, err := client.Me()
	if err != nil || me.Email != "you@example.com" {
		t.Fatalf("me: %+v, %v", me, err)
	}

	request, err := computeruse.NewCreateSessionRequestBuilder().Name("smoke").Build()
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.CreateSession(request)
	if err != nil {
		t.Fatal(err)
	}
	if session.Id() != "s-abcde" || session.McpUrl() != f.URL+"/s-abcde/mcp" {
		t.Fatalf("session: %s %s", session.Id(), session.McpUrl())
	}
	if info := session.LastInfo(); info == nil || info.Name != "smoke" || info.State != computeruse.SessionStateStarting {
		t.Fatalf("last info: %+v", info)
	}

	result, err := session.RunJs("console.log(6 * 7)")
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "ran: console.log(6 * 7)" || result.Error != nil {
		t.Fatalf("run_js: %+v", result)
	}

	info, err := session.Sleep()
	if err != nil || info.State != computeruse.SessionStateStopping {
		t.Fatalf("sleep: %+v, %v", info, err)
	}

	// The API token went to the token endpoint only.
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls[0].path != "/oauth/token" {
		t.Fatalf("first call: %+v", f.calls[0])
	}
	for _, c := range f.calls[1:] {
		if c.authorization != "Bearer access-1" {
			t.Fatalf("%s %s carried %q", c.method, c.path, c.authorization)
		}
		if c.method == "PATCH" && strings.TrimSpace(c.body) != `{"action":"sleep"}` {
			t.Fatalf("sleep sent %s", c.body)
		}
	}
}

func TestErrorsAreTyped(t *testing.T) {
	f := newFake(t)
	_, err := f.client(t).GetSession("s-zzzzz")
	if !errors.Is(err, computeruse.ErrComputerUseErrorNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	var notFound *computeruse.ComputerUseErrorNotFound
	if !errors.As(err, &notFound) || notFound.Message != "session not found" {
		t.Fatalf("want the API's message, got %v", err)
	}

	if _, err := computeruse.ClientWithToken(""); !errors.Is(err, computeruse.ErrComputerUseErrorConfiguration) {
		t.Fatalf("want a configuration error, got %v", err)
	}
	if _, err := computeruse.NewClientOptionsBuilder().Build(); !errors.Is(err, computeruse.ErrBuildErrorMissingRequiredField) {
		t.Fatalf("want a missing field, got %v", err)
	}
}

func TestRecordsAndBuilders(t *testing.T) {
	f := newFake(t)
	name := "direct"
	session, err := f.client(t).CreateSession(computeruse.CreateSessionRequest{Name: &name})
	if err != nil || session.LastInfo().Name != "direct" {
		t.Fatalf("create with a struct: %v", err)
	}

	base := computeruse.NewCreateSessionRequestBuilder()
	named := base.Name("a")
	if unnamed, _ := base.Build(); unnamed.Name != nil {
		t.Fatalf("a setter changed its receiver: %v", *unnamed.Name)
	}
	if built, _ := named.Build(); built.Name == nil || *built.Name != "a" {
		t.Fatal("the setter's builder lost the name")
	}
	if computeruse.SdkVersion() == "" {
		t.Fatal("no version")
	}
}
