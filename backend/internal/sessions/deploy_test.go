package sessions_test

import (
	"bytes"
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
		"ID": "$(SESSION_ID)", "SessionURL": "https://sessions.browserjs.com/$(SESSION_ID)", "PublicURL": "https://app.browserjs.com",
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
