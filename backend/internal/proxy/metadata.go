package proxy

import (
	"encoding/json"
	"net/http"
)

// OAuth discovery for a session's MCP endpoint, on a host per session.
//
// This is a workaround, for the sessions' old hosts only. Pomerium is the
// OAuth server for MCP clients, and serves these two documents itself on a
// host that has an exact route, as the sessions' one host has: there the
// backend has nothing to add. Pomerium v0.33.3 does not serve them on hosts
// matched by a wildcard route, and answers 404 (hosts with a "*" are left
// out of the virtual hosts that get the documents, see
// config/envoyconfig/route_configurations.go at that tag). Everything the
// documents point to works on a wildcard host. So for those the backend
// answers them, with what Pomerium itself says on an exact-route host, behind
// a public route for /.well-known/oauth-. Remove this file and that route
// with the old hosts.

// oauthMetadata registers the two documents on the mux of a session's own
// host.
func (p *Proxy) oauthMetadata(mux *http.ServeMux) {
	// RFC 9728: the resource's path may follow the well-known one. Clients
	// ask for either spelling; the resource is the MCP endpoint in both.
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", p.protectedResource)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", p.protectedResource)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", p.authorizationServer)
}

func writeMetadata(w http.ResponseWriter, doc any) {
	w.Header().Set("Content-Type", "application/json")
	// Public documents, read by MCP clients that run in a browser too.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "max-age=60")
	_ = json.NewEncoder(w).Encode(doc)
}

func (p *Proxy) protectedResource(w http.ResponseWriter, r *http.Request) {
	rt := routeOf(r)
	writeMetadata(w, map[string]any{
		"resource":                 rt.urls.MCP(rt.id),
		"authorization_servers":    []string{rt.urls.Origin(rt.id)},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "Pomerium",
	})
}

func (p *Proxy) authorizationServer(w http.ResponseWriter, r *http.Request) {
	rt := routeOf(r)
	base := rt.urls.Origin(rt.id)
	writeMetadata(w, map[string]any{
		"issuer":                                     base,
		"authorization_endpoint":                     base + "/.pomerium/mcp/authorize",
		"token_endpoint":                             base + "/.pomerium/mcp/token",
		"response_types_supported":                   []string{"code"},
		"grant_types_supported":                      []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported":      []string{"client_secret_basic", "none"},
		"service_documentation":                      "https://pomerium.com/docs",
		"revocation_endpoint":                        base + "/.pomerium/mcp/revoke",
		"revocation_endpoint_auth_methods_supported": []string{"client_secret_post"},
		"code_challenge_methods_supported":           []string{"S256"},
		"client_id_metadata_document_supported":      true,
	})
}
