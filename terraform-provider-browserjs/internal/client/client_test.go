package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const token = "bjs_id_secret"

// sameJSON reports whether two JSON documents say the same thing: the SDK
// writes an object's keys in its own order.
func sameJSON(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

func serve(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	c, err := New(ts.URL+"/", token, "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNew(t *testing.T) {
	for _, endpoint := range []string{"", "api.computeruse.site", "ftp://api.computeruse.site", "https://"} {
		if _, err := New(endpoint, token, "1"); err == nil {
			t.Errorf("endpoint %q accepted", endpoint)
		}
	}
	if _, err := New("https://api.computeruse.site", "", "1"); err == nil {
		t.Error("an empty token accepted")
	}
}

func TestRequestShape(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.URL.Path != "/v1/sessions/s-ab2cd/policy" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization %q", got)
		}
		// The provider's name and version, then the SDK's.
		if got := r.UserAgent(); !strings.HasPrefix(got, "terraform-provider-browserjs/1.2.3 computeruse-sdk/") {
			t.Errorf("User-Agent %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		const want = `{"kind":"rego","source":"package browserjs.policy","management":{"mode":"iac","managed_url":"https://example.com/x"}}`
		if !sameJSON(string(body), want) {
			t.Errorf("body %s", body)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"kind":"rego","version":4,"hash":"abc","state":"loading","management":{"mode":"iac","managed_url":"https://example.com/x"}}`))
	})
	p, loading, err := c.PutPolicy(context.Background(), "s-ab2cd", PolicyInput{
		Kind: "rego", Source: "package browserjs.policy",
		Management: &Management{Mode: ModeIaC, ManagedURL: "https://example.com/x"},
	})
	if err != nil || !loading || p.Version != 4 || p.State != StateLoading || p.Management.Mode != ModeIaC {
		t.Errorf("%+v loading=%v err=%v", p, loading, err)
	}
}

func TestValidateSendsOnlyTheSource(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !sameJSON(string(body), `{"kind":"rego","source":"package p"}`) {
			t.Errorf("body %s", body)
		}
		_ = json.NewEncoder(w).Encode(Validation{Errors: []Diagnostic{{Row: 1, Col: 2, Code: "package", Message: "the package must be browserjs.policy"}}})
	})
	v, err := c.ValidatePolicy(context.Background(), "package p")
	if err != nil || v.OK || len(v.Errors) != 1 || v.Errors[0].Row != 1 || v.Errors[0].Col != 2 {
		t.Errorf("%+v %v", v, err)
	}
}

func TestErrors(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sessions/s-aaaaa":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"session not found"}`))
		case "/v1/sessions/s-bbbbb/policy":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":"the policy does not validate","errors":[{"row":3,"col":5,"code":"rego_parse_error","message":"unexpected }"}],"warnings":[{"code":"w","message":"careful"}]}`))
		case "/v1/sessions/s-ccccc/policy":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"this policy is managed externally","managed_url":"https://example.com/x"}`))
		default:
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`<html>bad gateway</html>`))
		}
	})
	ctx := context.Background()

	_, err := c.GetSession(ctx, "s-aaaaa")
	if !IsNotFound(err) || err.Error() != "the API answered 404: session not found" {
		t.Errorf("404: %v", err)
	}

	_, _, err = c.PutPolicy(ctx, "s-bbbbb", PolicyInput{Kind: "rego", Source: "x"})
	var e *APIError
	if !errors.As(err, &e) || e.Status != 422 || len(e.Errors) != 1 || e.Errors[0].Row != 3 || e.Errors[0].Col != 5 || len(e.Warnings) != 1 {
		t.Errorf("422: %+v", e)
	}

	_, _, err = c.PutPolicy(ctx, "s-ccccc", PolicyInput{Kind: "rego", Source: "x"})
	if !errors.As(err, &e) || e.Status != 409 || e.ManagedURL != "https://example.com/x" {
		t.Errorf("409: %+v", e)
	}

	// An answer that is not the API's JSON still reports its status, and
	// what came with it. (The SDK tries a 502 again before it gives up.)
	_, err = c.ListSessions(ctx)
	if StatusOf(err) != 502 || err.Error() != "the API answered 502: <html>bad gateway</html>" {
		t.Errorf("502: %v", err)
	}
	if StatusOf(errors.New("x")) != 0 || StatusOf(nil) != 0 {
		t.Error("StatusOf of something that is not an APIError")
	}
}

func TestSessionCalls(t *testing.T) {
	var seen []string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Method+" "+r.URL.Path+" "+string(body))
		switch r.Method {
		case "DELETE":
			if strings.HasSuffix(r.URL.Path, "/policy") {
				// A reset answers the policy it left.
				_, _ = w.Write([]byte(`{"kind":"rego","version":2,"state":"ready","management":{"mode":"editor"}}`))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case "POST":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"s-ab2cd","name":"n","owner":"o","state":"starting","mcp_url":"https://x/s-ab2cd/mcp","policy":{"state":"loading"}}`))
		default:
			_, _ = w.Write([]byte(`{"id":"s-ab2cd","name":"m","state":"running","policy":{"state":"ready","kind":"rego","version":1}}`))
		}
	})
	ctx := context.Background()
	s, err := c.CreateSession(ctx, "")
	if err != nil || s.ID != "s-ab2cd" || s.Policy.State != StateLoading || s.MCPURL != "https://x/s-ab2cd/mcp" {
		t.Errorf("%+v %v", s, err)
	}
	if _, err := c.CreateSession(ctx, "n"); err != nil {
		t.Error(err)
	}
	if s, err := c.RenameSession(ctx, "s-ab2cd", "m"); err != nil || s.Name != "m" {
		t.Errorf("%+v %v", s, err)
	}
	if err := c.DeleteSession(ctx, "s-ab2cd"); err != nil {
		t.Error(err)
	}
	if err := c.ResetPolicy(ctx, "s-ab2cd"); err != nil {
		t.Error(err)
	}
	want := []string{
		"POST /v1/sessions {}",
		`POST /v1/sessions {"name":"n"}`,
		`PATCH /v1/sessions/s-ab2cd {"name":"m"}`,
		"DELETE /v1/sessions/s-ab2cd ",
		"DELETE /v1/sessions/s-ab2cd/policy ",
	}
	if strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests\n%s\nwant\n%s", strings.Join(seen, "\n"), strings.Join(want, "\n"))
	}
}

// A transport error names what was being done, and never the token.
func TestTransportErrorHidesTheToken(t *testing.T) {
	c, err := New("http://127.0.0.1:1", token, "1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ListSessions(context.Background())
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "list sessions") {
		t.Errorf("%v", err)
	}
}

// A refusal by billing (docs/contracts/billing/enforcement.md) is shown
// with its sentence and the link to put it right.
func TestBillingRefusal(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, `{"error":"Add a payment method to create or wake sessions.","code":"payment_method_required","billingUrl":"https://app.computeruse.site/billing"}`)
	})
	_, err := c.CreateSession(context.Background(), "ci")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusPaymentRequired || apiErr.Code != "payment_method_required" ||
		apiErr.BillingURL != "https://app.computeruse.site/billing" {
		t.Fatalf("err = %#v", err)
	}
	want := "the API answered 402: Add a payment method to create or wake sessions. See https://app.computeruse.site/billing"
	if err.Error() != want {
		t.Errorf("diagnostic %q, want %q", err.Error(), want)
	}
	if strings.Contains(err.Error(), token) {
		t.Error("the token is in the error")
	}
}
