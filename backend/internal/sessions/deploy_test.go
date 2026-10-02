package sessions_test

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"text/template"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
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

// The sizes of deploy/gke/sizes.yaml are the blueprint with other numbers
// and nothing else: a session of any size is the same pod but for each
// container's resources, the size of /dev/shm and the variables the sizes
// set. Small is the blueprint itself, which the test above holds the warm
// pool's template to. The numbers are held to the node they have to fit on
// (the file's capacity, which is warmpool.yaml's "left for sessions"), and
// the sizes to the billing catalogue's, which prices them.
func TestSizesAreTheBlueprintButForResources(t *testing.T) {
	const dir = "../../../deploy/"
	blueprint, err := os.ReadFile(dir + "gke/blueprint.yaml")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(dir + "gke/sizes.yaml")
	if err != nil {
		t.Fatal(err)
	}
	file, err := sessions.ParseSizes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if file.Capacity == nil {
		t.Fatal("sizes.yaml has no capacity: nothing would be refused for want of room")
	}
	store, client := sessionstest.NewWith(t, string(blueprint))
	// Refuses a size that names a container the blueprint lacks, sets a
	// variable the blueprint sets, or asks for more than a node has.
	if err := store.EnableSizes(file); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, size := range store.Sizes() {
		names = append(names, size.Name)
	}
	if strings.Join(names, " ") != "small medium large" {
		t.Fatalf("sizes are %v, want small, medium, large (the API, the UI and the docs name them)", names)
	}

	// The variables a size may set, by container.
	sizeEnv := map[string]map[string]bool{}
	for _, size := range file.Sizes {
		for container, c := range size.Containers {
			for key := range c.Env {
				if sizeEnv[container] == nil {
					sizeEnv[container] = map[string]bool{}
				}
				sizeEnv[container][key] = true
			}
		}
	}
	type pod struct {
		rest     string // the Sandbox spec without what a size decides
		cpu, mem int64  // what the pod asks the scheduler for: millicores, bytes
	}
	pods := map[string]pod{}
	for _, size := range names {
		s, err := store.CreateSized(t.Context(), "a", "alice@example.com", size, nil)
		if err != nil {
			t.Fatalf("%s: %v", size, err)
		}
		obj, err := client.Resource(sessions.SandboxGVR).Namespace(sessionstest.Namespace).Get(t.Context(), s.ID, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var p pod
		containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "podTemplate", "spec", "containers")
		for _, c := range containers {
			c := c.(map[string]any)
			requests, _, _ := unstructured.NestedStringMap(c, "resources", "requests")
			limits, _, _ := unstructured.NestedStringMap(c, "resources", "limits")
			for _, res := range []string{"cpu", "memory"} {
				req, lim := resource.MustParse(requests[res]), resource.MustParse(limits[res])
				if req.Cmp(lim) > 0 {
					t.Errorf("%s, %s: the %s request %s is above the limit %s", size, c["name"], res, requests[res], limits[res])
				}
			}
			cpu, mem := resource.MustParse(requests["cpu"]), resource.MustParse(requests["memory"])
			p.cpu, p.mem = p.cpu+cpu.MilliValue(), p.mem+mem.Value()
			delete(c, "resources")
			var env []any
			for _, e := range c["env"].([]any) {
				if !sizeEnv[c["name"].(string)][e.(map[string]any)["name"].(string)] {
					env = append(env, e)
				}
			}
			c["env"] = env
		}
		_ = unstructured.SetNestedSlice(obj.Object, containers, "spec", "podTemplate", "spec", "containers")
		volumes, _, _ := unstructured.NestedSlice(obj.Object, "spec", "podTemplate", "spec", "volumes")
		for _, v := range volumes {
			if v := v.(map[string]any); v["name"] == "shm" {
				unstructured.RemoveNestedField(v, "emptyDir", "sizeLimit")
			}
		}
		_ = unstructured.SetNestedSlice(obj.Object, volumes, "spec", "podTemplate", "spec", "volumes")
		rest, err := yaml.Marshal(obj.Object["spec"])
		if err != nil {
			t.Fatal(err)
		}
		p.rest = strings.ReplaceAll(string(rest), s.ID, "ID")
		pods[size] = p
		if err := store.Delete(t.Context(), s.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, size := range names[1:] {
		if pods[size].rest != pods["small"].rest {
			t.Errorf("a %s session is not a small one but for its resources.\n%s:\n%s\nsmall:\n%s", size, size, pods[size].rest, pods["small"].rest)
		}
	}

	// How they pack a node, which is what the docs and the prices say.
	nodeCPU, nodeMem := resource.MustParse(file.Capacity.CPU), resource.MustParse(file.Capacity.Memory)
	fit := func(size string) int64 {
		return min(nodeCPU.MilliValue()/pods[size].cpu, nodeMem.Value()/pods[size].mem)
	}
	for size, want := range map[string]int64{"small": 9, "medium": 3, "large": 1} {
		if got := fit(size); got != want {
			t.Errorf("%d %s sessions fit an empty node, want %d (docs/session-sizes.md, the catalogue's rates)", got, size, want)
		}
	}
	// A large session has its node to itself: not even a small one fits beside it.
	if left := nodeMem.Value() - pods["large"].mem; left >= pods["small"].mem {
		t.Errorf("a large session leaves %d bytes of its node, which fit a small one (%d)", left, pods["small"].mem)
	}

	// Every size other than small has a rate, and nothing else has.
	var catalogue struct {
		Sizes map[string]struct {
			AwakeMicrosPerHour int64 `json:"awakeMicrosPerHour"`
		} `json:"sizes"`
	}
	raw, err = os.ReadFile(dir + "base/catalogue.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &catalogue); err != nil {
		t.Fatal(err)
	}
	for _, size := range names[1:] {
		if catalogue.Sizes[size].AwakeMicrosPerHour <= 0 {
			t.Errorf("deploy/base/catalogue.yaml has no rate for %s sessions: they would be charged as small", size)
		}
	}
	if len(catalogue.Sizes) != len(names)-1 {
		t.Errorf("the catalogue prices %d sizes, sizes.yaml has %d besides small", len(catalogue.Sizes), len(names)-1)
	}
}
