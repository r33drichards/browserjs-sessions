package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// The documents are what Pomerium v0.33.3 serves on a host with an exact
// MCP route, captured from one, with the session's host in place of that one.
// Only the sessions' old hosts need them from the backend.
func TestSessionHostServesOAuthMetadata(t *testing.T) {
	e := newLegacyEnv(t)
	base := "https://" + e.host
	resource := map[string]any{
		"resource":                 base + "/mcp",
		"authorization_servers":    []any{base},
		"bearer_methods_supported": []any{"header"},
		"resource_name":            "Pomerium",
	}
	server := map[string]any{
		"issuer":                                     base,
		"authorization_endpoint":                     base + "/.pomerium/mcp/authorize",
		"token_endpoint":                             base + "/.pomerium/mcp/token",
		"response_types_supported":                   []any{"code"},
		"grant_types_supported":                      []any{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported":      []any{"client_secret_basic", "none"},
		"service_documentation":                      "https://pomerium.com/docs",
		"revocation_endpoint":                        base + "/.pomerium/mcp/revoke",
		"revocation_endpoint_auth_methods_supported": []any{"client_secret_post"},
		"code_challenge_methods_supported":           []any{"S256"},
		"client_id_metadata_document_supported":      true,
	}
	for path, want := range map[string]map[string]any{
		"/.well-known/oauth-protected-resource/mcp": resource,
		"/.well-known/oauth-protected-resource":     resource,
		"/.well-known/oauth-authorization-server":   server,
	} {
		// Nobody is signed in yet when a client asks for these.
		rec := e.do("GET", path, "", "")
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: %d %q", path, rec.Code, rec.Body)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("GET %s: Content-Type %q", path, ct)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("GET %s: Access-Control-Allow-Origin %q, want * (browser-based clients read these)", path, got)
		}
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Errorf("GET %s: %v", path, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("GET %s:\n got %v\nwant %v", path, got, want)
		}
	}
	if n := len(e.seen()); n != 0 {
		t.Errorf("%d metadata requests reached the pod", n)
	}
}

func TestOAuthMetadataIsOnlyThoseDocuments(t *testing.T) {
	e := newLegacyEnv(t)
	for _, c := range []struct{ method, path string }{
		{"POST", "/.well-known/oauth-protected-resource/mcp"},
		{"POST", "/.well-known/oauth-authorization-server"},
		{"GET", "/.well-known/oauth-protected-resource/other"},
		{"GET", "/.well-known/oauth-protected-resource/mcp/x"},
		{"GET", "/.well-known/oauth-authorization-server/mcp"},
		{"GET", "/.well-known/openid-configuration"},
		{"GET", "/.well-known/"},
	} {
		if rec := e.do(c.method, c.path, alice, ""); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: %d, want 404 or 405", c.method, c.path, rec.Code)
		}
	}
	// A host under the session domain that names no session has no documents,
	// and neither has the app's host.
	if rec := e.doAt("nope", "GET", "/.well-known/oauth-authorization-server", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("on a host that names no session: %d", rec.Code)
	}
	if rec := e.app("GET", "/.well-known/oauth-authorization-server", "", ""); strings.Contains(rec.Body.String(), "issuer") {
		t.Errorf("the app's host served the document: %q", rec.Body)
	}
}

// The sessions' one host has an exact route in Pomerium, which answers the
// documents there itself (RFC 9728: the resource's path follows the
// well-known one). The backend has none to offer on that host.
func TestSessionsHostHasNoOAuthMetadataOfTheBackend(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/" + e.id + "/mcp",
		"/.well-known/oauth-authorization-server",
		"/" + e.id + "/.well-known/oauth-protected-resource",
		"/" + e.id + "/.well-known/oauth-protected-resource/mcp",
		"/" + e.id + "/.well-known/oauth-authorization-server",
	} {
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, request("GET", sessionsHost, path, "", ""))
		if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "issuer") || strings.Contains(rec.Body.String(), "the app") {
			t.Errorf("GET %s: %d %q, want 404", path, rec.Code, rec.Body)
		}
	}
}
