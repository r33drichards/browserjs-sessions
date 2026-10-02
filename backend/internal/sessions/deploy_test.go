package sessions_test

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"text/template"

	"sigs.k8s.io/yaml"
)

// A session is the same pod whether the backend made its Sandbox from
// deploy/gke/blueprint.yaml or the warm pool made it from the SandboxTemplate
// in deploy/gke/warmpool.yaml. The template may differ in one thing: a warm
// pod takes its public URL from its own name, there being no session yet to
// render one for.
func TestWarmPoolTemplateMatchesBlueprint(t *testing.T) {
	const dir = "../../../deploy/gke/"
	source, err := os.ReadFile(dir + "blueprint.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var rendered bytes.Buffer
	if err := template.Must(template.New("blueprint").Option("missingkey=error").Parse(string(source))).Execute(&rendered, map[string]string{
		"ID": "$(SESSION_ID)", "SessionURL": "https://sessions.computeruse.site/$(SESSION_ID)", "PublicURL": "https://app.computeruse.site",
	}); err != nil {
		t.Fatal(err)
	}
	blueprint := map[string]any{}
	if err := yaml.Unmarshal(rendered.Bytes(), &blueprint); err != nil {
		t.Fatal(err)
	}

	manifest, err := os.ReadFile(dir + "warmpool.yaml")
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]map[string]any{}
	for _, doc := range strings.Split(string(manifest), "\n---\n") {
		obj := map[string]any{}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatal(err)
		}
		kinds[obj["kind"].(string)] = obj
	}
	pool, tmpl := kinds["SandboxWarmPool"], kinds["SandboxTemplate"]
	if pool == nil || tmpl == nil {
		t.Fatalf("warmpool.yaml has %d documents; want a SandboxWarmPool and a SandboxTemplate", len(kinds))
	}
	// The pool's name is the prefix of its Sandboxes' names: session IDs.
	if name := pool["metadata"].(map[string]any)["name"]; name != "s" {
		t.Errorf("the pool is named %q, want \"s\"", name)
	}
	if ref := pool["spec"].(map[string]any)["sandboxTemplateRef"].(map[string]any)["name"]; ref != tmpl["metadata"].(map[string]any)["name"] {
		t.Errorf("the pool is of template %q, which is not the one in the file", ref)
	}

	spec := tmpl["spec"].(map[string]any)
	// Left to deploy/base's NetworkPolicy, which a blueprint cannot say.
	if spec["networkPolicyManagement"] != "Unmanaged" {
		t.Errorf("networkPolicyManagement = %v, want Unmanaged", spec["networkPolicyManagement"])
	}
	delete(spec, "networkPolicyManagement")
	containers := spec["podTemplate"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
	mcp := containers[1].(map[string]any)
	env := mcp["env"].([]any)
	if id := env[0].(map[string]any); id["name"] != "SESSION_ID" || id["valueFrom"] == nil {
		t.Fatalf("mcp-js's first variable is %v, want SESSION_ID from the pod's name", id)
	}
	mcp["env"] = env[1:]

	if !reflect.DeepEqual(spec, blueprint) {
		got, _ := yaml.Marshal(spec)
		want, _ := yaml.Marshal(blueprint)
		t.Errorf("the SandboxTemplate is not the blueprint.\ntemplate:\n%s\nblueprint:\n%s", got, want)
	}
}

// With session policies enforcing (hack/policy-stage.sh), mcp-js in every
// pod template has MCP_V8_POLICIES_JSON: the image's own file policies, and
// the shared OPA at this session's own decision path. A blueprint has the ID
// rendered in; the warm template takes it from the pod's name. The test
// above already holds the warm template and the GKE blueprint to the same
// variable, since it renders the blueprint with the ID "$(SESSION_ID)".
// With policies off or only serving, no template has the variable.
func TestPolicyEnvironment(t *testing.T) {
	const id = "s-ab2cd"
	// Templates of one overlay are switched together.
	overlays := map[string][]string{
		"gke":   {"gke/blueprint.yaml", "gke/warmpool.yaml"},
		"local": {"local/blueprint.yaml", "base/blueprint.yaml"},
	}
	for overlay, files := range overlays {
		asks := map[bool][]string{}
		for _, file := range files {
			source, err := os.ReadFile("../../../deploy/" + file)
			if err != nil {
				t.Fatal(err)
			}
			// What the kubelet does with $(SESSION_ID), and the backend with {{ .ID }}.
			text := strings.ReplaceAll(string(source), "$(SESSION_ID)", id)
			var rendered bytes.Buffer
			if err := template.Must(template.New(file).Parse(text)).Execute(&rendered, map[string]string{
				"ID": id, "SessionURL": "https://" + id + ".sessions.example.com", "PublicURL": "https://app.example.com",
			}); err != nil {
				t.Fatal(err)
			}
			value, found := "", false
			for _, doc := range strings.Split(rendered.String(), "\n---\n") {
				obj := map[string]any{}
				if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
					t.Fatal(err)
				}
				if obj["kind"] == "SandboxWarmPool" {
					continue
				}
				if spec, ok := obj["spec"].(map[string]any); ok { // a SandboxTemplate
					obj = spec
				}
				containers := obj["podTemplate"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
				for _, e := range containers[1].(map[string]any)["env"].([]any) {
					if e := e.(map[string]any); e["name"] == "MCP_V8_POLICIES_JSON" {
						value, found = e["value"].(string), true
					}
				}
			}
			asks[found] = append(asks[found], file)
			if !found {
				continue
			}
			var policies struct {
				Tools struct {
					Mode     string `json:"mode"`
					Policies []struct {
						URL  string `json:"url"`
						Path string `json:"policy_path"`
					} `json:"policies"`
				} `json:"mcp_tools"`
				Filesystem struct {
					Policies []struct {
						URL string `json:"url"`
					} `json:"policies"`
				} `json:"filesystem"`
			}
			if err := json.Unmarshal([]byte(value), &policies); err != nil {
				t.Errorf("%s: MCP_V8_POLICIES_JSON is not JSON: %v\n%s", file, err, value)
				continue
			}
			tools := policies.Tools
			if tools.Mode != "all" || len(tools.Policies) != 2 ||
				tools.Policies[0].URL != "file:///etc/mcp/mcp_tools.rego" ||
				tools.Policies[1].URL != "http://opa.browserjs-sessions.svc:8181" ||
				tools.Policies[1].Path != "browserjs/decision/"+id+"/mcp_tools" {
				t.Errorf("%s: mcp_tools is %+v; want mode all, the image's file policy, then OPA at browserjs/decision/%s/mcp_tools", file, tools, id)
			}
			if fs := policies.Filesystem.Policies; len(fs) != 1 || fs[0].URL != "file:///etc/mcp/filesystem.rego" {
				t.Errorf("%s: filesystem is %+v; want the image's file policy only", file, fs)
			}
		}
		if len(asks[true]) > 0 && len(asks[false]) > 0 {
			t.Errorf("deploy/%s: %v ask OPA and %v do not; hack/policy-stage.sh switches them together", overlay, asks[true], asks[false])
		}
	}
}
