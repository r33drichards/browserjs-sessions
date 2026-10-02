package auth

import (
	"os"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Pomerium's policy says who may sign in; the backend's ALLOWED_EMAILS says
// who may use an API token, where Pomerium is not asked. They are the same
// list, kept in two files of each overlay: this holds them together.
func TestAllowedEmailsMirrorPomeriumsPolicy(t *testing.T) {
	for _, c := range []struct {
		name, pomerium, backend string
		on                      bool // API tokens are on in this overlay
	}{
		{"base", "../../../deploy/base/pomerium-config.yaml", "../../../deploy/base/backend.yaml", false},
		{"local", "../../../deploy/local/pomerium-config.yaml", "../../../deploy/local/patch-backend.yaml", true},
		{"gke", "../../../deploy/gke/pomerium-config.yaml", "../../../deploy/gke/patch-backend.yaml", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			routes := pomeriumRoutes(t, c.pomerium)
			var signIn []string
			var apiFrom string
			api := map[string]pomeriumRoute{}
			for _, route := range routes {
				if route.Name == "app" {
					signIn = route.emails()
				}
				host, _ := strings.CutPrefix(route.From, "https://api.")
				if host == route.From {
					continue
				}
				apiFrom = route.From
				api[route.Name] = route
				// The API host: nobody signs in, Pomerium says nothing about
				// the caller, and no websocket (the screen is not here).
				if !route.Public || route.PassIdentity || len(route.Policy) != 0 || route.To != "http://backend" ||
					!route.PreserveHost || route.Websockets || route.MCP != nil {
					t.Errorf("route %s is %+v", route.Name, route)
				}
			}
			// Only the API's paths get through, each matched whole.
			// The fourth is Stripe's webhook: the request's signature is its
			// credential (docs/contracts/billing/deploy.md).
			if len(api) != 4 || api["api"].Prefix != "/v1/" || api["api-token"].Path != "/oauth/token" ||
				api["api-mcp"].Regex != `^/s-[a-z0-9]+/mcp(/.*)?$` || api["api-stripe-webhook"].Path != "/stripe/webhook" {
				t.Errorf("the API host's routes are %+v", api)
			}
			for name, route := range api {
				if n := btoi(route.Prefix != "") + btoi(route.Path != "") + btoi(route.Regex != ""); n != 1 {
					t.Errorf("route %s matches by %d of prefix, path and regex", name, n)
				}
			}
			if len(signIn) == 0 {
				t.Fatal("found no email addresses in the app route's policy")
			}
			if apiFrom == "" {
				t.Fatal("no routes for the API host")
			}
			env := backendEnv(t, c.backend)
			// The base is a template: its API_URL is empty, tokens off. An
			// overlay that routes the API host must tell the backend, or the
			// backend would take that host for the app's.
			want := apiFrom
			if c.name == "base" {
				want = ""
			}
			if got := env["API_URL"]; got != want {
				t.Errorf("API_URL is %q, want %q (the api route is from %q)", got, want, apiFrom)
			}
			var allowed []string
			for _, email := range strings.Split(env["ALLOWED_EMAILS"], ",") {
				if email = strings.TrimSpace(email); email != "" {
					allowed = append(allowed, email)
				}
			}
			if !c.on {
				if len(allowed) != 0 {
					t.Errorf("ALLOWED_EMAILS is %q: API tokens are meant to be off here", allowed)
				}
				return
			}
			slices.Sort(allowed)
			slices.Sort(signIn)
			if !slices.Equal(allowed, signIn) {
				t.Errorf("ALLOWED_EMAILS is %q, Pomerium lets %q sign in", allowed, signIn)
			}
		})
	}
}

type pomeriumRoute struct {
	Name         string `json:"name"`
	From         string `json:"from"`
	To           string `json:"to"`
	Prefix       string `json:"prefix"`
	Path         string `json:"path"`
	Regex        string `json:"regex"`
	MCP          any    `json:"mcp"`
	Public       bool   `json:"allow_public_unauthenticated_access"`
	PassIdentity bool   `json:"pass_identity_headers"`
	PreserveHost bool   `json:"preserve_host_header"`
	Websockets   bool   `json:"allow_websockets"`
	Policy       []struct {
		Allow struct {
			Or []struct {
				Email struct {
					Is string `json:"is"`
				} `json:"email"`
			} `json:"or"`
		} `json:"allow"`
	} `json:"policy"`
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (r pomeriumRoute) emails() (emails []string) {
	for _, p := range r.Policy {
		for _, or := range p.Allow.Or {
			emails = append(emails, or.Email.Is)
		}
	}
	return emails
}

func pomeriumRoutes(t *testing.T, path string) []pomeriumRoute {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Routes []pomeriumRoute `json:"routes"`
	}
	if err := yaml.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	return config.Routes
}

// backendEnv is the backend container's env in the Deployment of a manifest
// (a file of several documents, or a patch).
func backendEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, doc := range strings.Split(string(raw), "\n---\n") {
		var obj struct {
			Kind string `json:"kind"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Env []struct {
								Name  string `json:"name"`
								Value string `json:"value"`
							} `json:"env"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatal(err)
		}
		if obj.Kind != "Deployment" {
			continue
		}
		for _, container := range obj.Spec.Template.Spec.Containers {
			for _, e := range container.Env {
				env[e.Name] = e.Value
			}
		}
	}
	return env
}
