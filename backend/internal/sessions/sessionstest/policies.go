package sessionstest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/r33drichards/computer-use/backend/internal/sessions"
)

// PolicyEnv is the value of mcp-js's MCP_V8_POLICIES_JSON in a session that
// asks OPA for decisions, as docs/contracts/policy/deploy.md has it.
const PolicyEnv = `{"mcp_tools":{"mode":"all","policies":[{"url":"file:///etc/mcp/mcp_tools.rego"},{"url":"http://opa.browserjs-sessions.svc:8181","policy_path":"browserjs/decision/{{ .ID }}/mcp_tools"}]},"filesystem":{"policies":[{"url":"file:///etc/mcp/filesystem.rego"}]}}`

// PolicyBlueprint is Blueprint as it is once policies are deployed.
var PolicyBlueprint = strings.Replace(Blueprint, `            value: "{{ .SessionURL }}"
`, `            value: "{{ .SessionURL }}"
          - name: MCP_V8_POLICIES_JSON
            value: '`+PolicyEnv+`'
`, 1)

// Unrestricted is the policy the fake deployment gives a session that asked
// for none.
var Unrestricted = sessions.PolicySpec{Kind: "rego", Source: "package browserjs.policy\n\nimport rego.v1\n\nallow_tool_call := true\n"}

// NewWithPolicies is New with policies enabled on the store and a blueprint
// whose sessions ask OPA.
func NewWithPolicies(t *testing.T) (*sessions.Store, dynamic.Interface) {
	t.Helper()
	store, client := newStore(t, PolicyBlueprint)
	store.EnablePolicies(contextAware{client}, Namespace, Unrestricted)
	return store, client
}

// PlayPolicyClaimController is PlayClaimController for a pool whose Sandboxes
// ask OPA.
func PlayPolicyClaimController(t *testing.T, client dynamic.Interface, warm ...string) {
	t.Helper()
	playClaimController(t, client, capableSpec, warm)
}

func capableSpec() map[string]any {
	return map[string]any{"podTemplate": map[string]any{"spec": map[string]any{"containers": []any{
		map[string]any{"name": "browser"},
		map[string]any{"name": "mcp-js", "env": []any{
			map[string]any{"name": "SESSION_ID", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.name"}}},
			map[string]any{"name": "MCP_V8_POLICIES_JSON", "value": strings.ReplaceAll(PolicyEnv, "{{ .ID }}", "$(SESSION_ID)")},
		}},
	}}}}
}

// emulatePolicies makes the fake treat SessionPolicy as the API server does a
// resource with the CRD's schema and a status subresource: objects are
// validated, get a UID, and a generation that rises when the spec changes;
// writes through the main resource leave status alone; an update with a
// stale resourceVersion is a conflict.
func emulatePolicies(client *dynfake.FakeDynamicClient) {
	tracker := client.Tracker()
	apply := k8stesting.ObjectReaction(tracker)
	gvr := sessions.PolicyGVR
	client.PrependReactor("*", gvr.Resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		var before *unstructured.Unstructured
		stored := func(name string) error {
			cur, err := tracker.Get(gvr, action.GetNamespace(), name)
			if err != nil {
				return err
			}
			before = cur.(*unstructured.Unstructured).DeepCopy()
			return nil
		}
		switch action.GetVerb() {
		case "create":
			obj := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
			if err := validPolicy(obj); err != nil {
				return true, nil, err
			}
			obj.SetUID(types.UID("policy-uid-" + obj.GetName()))
			obj.SetGeneration(1)
			unstructured.RemoveNestedField(obj.Object, "status")
		case "update":
			obj := action.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured)
			if err := stored(obj.GetName()); err != nil {
				return true, nil, err
			}
			if rv := obj.GetResourceVersion(); rv != "" && rv != before.GetResourceVersion() {
				return true, nil, apierrors.NewConflict(gvr.GroupResource(), obj.GetName(), errors.New("the object has been modified"))
			}
		case "patch":
			if err := stored(action.(k8stesting.PatchAction).GetName()); err != nil {
				return true, nil, err
			}
		default:
			return false, nil, nil
		}
		_, out, err := apply(action)
		if err != nil {
			return true, nil, err
		}
		obj := out.(*unstructured.Unstructured)
		if before != nil {
			if err := validPolicy(obj); err != nil {
				_ = tracker.Update(gvr, before, action.GetNamespace())
				return true, nil, err
			}
			if status, ok := before.Object["status"]; ok {
				obj.Object["status"] = status
			} else {
				unstructured.RemoveNestedField(obj.Object, "status")
			}
			obj.SetUID(before.GetUID())
			obj.SetGeneration(before.GetGeneration())
			if !reflect.DeepEqual(obj.Object["spec"], before.Object["spec"]) {
				obj.SetGeneration(before.GetGeneration() + 1)
			}
			obj.SetResourceVersion(before.GetResourceVersion())
		}
		bumpResourceVersion(obj)
		if err := tracker.Update(gvr, obj, action.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, obj, nil
	})
}

// validPolicy is the CRD's schema, as far as the backend could break it.
func validPolicy(obj *unstructured.Unstructured) error {
	invalid := func(format string, args ...any) error {
		return apierrors.NewBadRequest("SessionPolicy " + obj.GetName() + ": " + fmt.Sprintf(format, args...))
	}
	spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
	if ref, _, _ := unstructured.NestedString(spec, "sessionRef", "name"); ref != obj.GetName() || !sessions.ValidID(ref) {
		return invalid("a SessionPolicy is named after its session, not %q", ref)
	}
	if kind := spec["kind"]; kind != "rego" {
		return invalid("kind %v", kind)
	}
	if source, _ := spec["source"].(string); len(source) < 1 || len(source) > 65536 {
		return invalid("source of %d bytes", len(source))
	}
	mode, _, _ := unstructured.NestedString(spec, "management", "mode")
	url, _, _ := unstructured.NestedString(spec, "management", "managedURL")
	switch mode {
	case "editor":
	case "iac":
		if !strings.HasPrefix(url, "https://") {
			return invalid("management.managedURL must be an https URL when management.mode is iac")
		}
	default:
		return invalid("management.mode %q", mode)
	}
	return nil
}

// Policy reads a session's SessionPolicy from the fake cluster, or nil if it
// has none.
func Policy(t *testing.T, client dynamic.Interface, id string) *unstructured.Unstructured {
	t.Helper()
	obj, err := client.Resource(sessions.PolicyGVR).Namespace(Namespace).Get(context.Background(), id, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return obj
}

// SetPolicyStatus overwrites a SessionPolicy's status, as the operator
// would: through the status subresource, leaving the generation alone.
func SetPolicyStatus(t *testing.T, client dynamic.Interface, id string, status map[string]any) {
	t.Helper()
	if err := TrySetPolicyStatus(client, id, status); err != nil {
		t.Fatal(err)
	}
}

// TrySetPolicyStatus is SetPolicyStatus for use off the test goroutine.
func TrySetPolicyStatus(client dynamic.Interface, id string, status map[string]any) error {
	tracker := client.(*dynfake.FakeDynamicClient).Tracker()
	cur, err := tracker.Get(sessions.PolicyGVR, Namespace, id)
	if err != nil {
		return err
	}
	obj := cur.(*unstructured.Unstructured).DeepCopy()
	obj.Object["status"] = status
	bumpResourceVersion(obj)
	return tracker.Update(sessions.PolicyGVR, obj, Namespace)
}

// PolicyReady is the status of a policy that compiled at generation and is
// served by both OPA replicas.
func PolicyReady(generation int64) map[string]any {
	return map[string]any{
		"observedGeneration": generation,
		"hash":               "sha256:0f0f",
		"rego":               "package browserjs.policy\n\nallow_tool_call := true\n",
		"regoGeneration":     generation,
		"warnings":           []any{map[string]any{"code": "unknown_operation", "message": "nothing in version 1 is called that"}},
		"loaded":             map[string]any{"replicas": int64(2), "total": int64(2), "revision": "17"},
		"lastAppliedTime":    "2026-10-02T12:01:07Z",
		"conditions": []any{
			map[string]any{"type": "Compiled", "status": "True", "reason": "Compiled", "observedGeneration": generation},
			map[string]any{"type": "Loaded", "status": "True", "reason": "AllReplicas", "message": "2/2 replicas", "observedGeneration": generation},
			map[string]any{"type": "Ready", "status": "True", "observedGeneration": generation},
		},
	}
}

// PolicyCompiled is the status of a policy that compiled at generation and
// has reached one OPA replica of two. first says it is the session's first
// policy: nothing was in force before it.
func PolicyCompiled(generation int64, first bool) map[string]any {
	status := PolicyReady(generation)
	status["loaded"] = map[string]any{"replicas": int64(1), "total": int64(2), "revision": "17"}
	if first {
		delete(status, "lastAppliedTime")
	}
	conds := status["conditions"].([]any)
	conds[1] = map[string]any{"type": "Loaded", "status": "False", "reason": "Waiting", "message": "1/2 replicas", "observedGeneration": generation}
	conds[2] = map[string]any{"type": "Ready", "status": "False", "observedGeneration": generation}
	return status
}

// PolicyRejected is the status of a policy whose generation does not
// compile. first says it is the session's first policy; otherwise the one
// before it (generation-1) is still in force.
func PolicyRejected(generation int64, first bool) map[string]any {
	status := map[string]any{
		"observedGeneration": generation,
		"errors":             []any{map[string]any{"row": int64(3), "col": int64(1), "code": "rego_parse_error", "message": "unexpected } token"}},
		"conditions": []any{
			map[string]any{"type": "Compiled", "status": "False", "reason": "Invalid", "message": "unexpected } token", "observedGeneration": generation},
			map[string]any{"type": "Ready", "status": "False", "observedGeneration": generation},
		},
	}
	if !first {
		prev := PolicyReady(generation - 1)
		for _, k := range []string{"hash", "rego", "regoGeneration", "loaded", "lastAppliedTime"} {
			status[k] = prev[k]
		}
	}
	return status
}
