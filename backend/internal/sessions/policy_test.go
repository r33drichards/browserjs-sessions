package sessions_test

import (
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
)

var (
	noScripting = sessions.PolicySpec{
		Kind:   "rego",
		Source: `{"version": 1, "allow": {"operations": ["*"]}, "deny": {"operations": ["evaluate"]}}`,
	}
	asCode = sessions.PolicySpec{
		Kind: "rego", Source: "package browserjs.policy\n\nallow_tool_call := false\n",
		Mode: sessions.PolicyModeIaC, ManagedURL: "https://git.example.com/infra", UpdatedBy: "token:ci",
	}
)

// policySpec is what a SessionPolicy says, in the terms it was asked for in.
func policySpec(t *testing.T, obj *unstructured.Unstructured) sessions.PolicySpec {
	t.Helper()
	if obj == nil {
		t.Fatal("there is no SessionPolicy")
	}
	var p sessions.PolicySpec
	p.Kind, _, _ = unstructured.NestedString(obj.Object, "spec", "kind")
	p.Source, _, _ = unstructured.NestedString(obj.Object, "spec", "source")
	p.Mode, _, _ = unstructured.NestedString(obj.Object, "spec", "management", "mode")
	p.ManagedURL, _, _ = unstructured.NestedString(obj.Object, "spec", "management", "managedURL")
	p.UpdatedBy = obj.GetAnnotations()[sessions.AnnUpdatedBy]
	return p
}

func TestCreateMakesTheSessionsPolicy(t *testing.T) {
	unrestricted := sessionstest.Unrestricted
	unrestricted.Mode, unrestricted.UpdatedBy = sessions.PolicyModeEditor, "ui"
	editor := noScripting
	editor.Mode, editor.UpdatedBy = sessions.PolicyModeEditor, "ui"
	for name, c := range map[string]struct {
		asked *sessions.PolicySpec
		want  sessions.PolicySpec
	}{
		"none asked for":  {nil, unrestricted},
		"one asked for":   {&noScripting, editor},
		"managed as code": {&asCode, asCode},
	} {
		t.Run(name, func(t *testing.T) {
			store, client := sessionstest.NewWithPolicies(t)
			s, err := store.CreateWithPolicy(t.Context(), "a", "alice@example.com", c.asked)
			if err != nil {
				t.Fatal(err)
			}
			if !s.PolicyCapable {
				t.Error("the session is not policy-capable")
			}
			obj := sessionstest.Policy(t, client, s.ID)
			if got := policySpec(t, obj); got != c.want {
				t.Errorf("policy = %+v, want %+v", got, c.want)
			}
			if ref, _, _ := unstructured.NestedString(obj.Object, "spec", "sessionRef", "name"); ref != s.ID {
				t.Errorf("sessionRef = %q", ref)
			}
			if got := obj.GetLabels()[sessions.LabelOwner]; got != sessions.OwnerLabel("alice@example.com") {
				t.Errorf("owner label = %q", got)
			}
			// Owned by the Sandbox, so that it goes when the session does; not
			// controlled by it, and not in the way of its deletion.
			refs := obj.GetOwnerReferences()
			sandbox := raw(t, client, s.ID)
			if len(refs) != 1 || refs[0].APIVersion != "agents.x-k8s.io/v1beta1" || refs[0].Kind != "Sandbox" ||
				refs[0].Name != s.ID || refs[0].UID == "" || refs[0].UID != sandbox.GetUID() ||
				refs[0].Controller == nil || *refs[0].Controller || refs[0].BlockOwnerDeletion == nil || *refs[0].BlockOwnerDeletion {
				t.Errorf("ownerReferences = %+v (Sandbox UID %q)", refs, sandbox.GetUID())
			}
			if _, has := obj.Object["status"]; has {
				t.Error("the backend wrote status")
			}
		})
	}
}

func TestWithoutPoliciesNothingChanges(t *testing.T) {
	store, client := sessionstest.New(t)
	if store.Policies() != nil {
		t.Fatal("policies are on by default")
	}
	s, err := store.Create(t.Context(), "a", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if s.PolicyCapable || sessionstest.Policy(t, client, s.ID) != nil {
		t.Errorf("capable = %v, policy = %v", s.PolicyCapable, sessionstest.Policy(t, client, s.ID))
	}
	// A policy cannot be asked for, rather than be asked for and ignored.
	if _, err := store.CreateWithPolicy(t.Context(), "b", "alice@example.com", &noScripting); !errors.Is(err, sessions.ErrPolicyUnsupported) {
		t.Errorf("err = %v, want ErrPolicyUnsupported", err)
	}
	if mine, _ := store.List(t.Context(), "alice@example.com"); len(mine) != 1 {
		t.Errorf("%d sessions, want the first only", len(mine))
	}
	if err := store.EnsurePolicy(t.Context(), s.ID, noScripting); !errors.Is(err, sessions.ErrPolicyUnsupported) {
		t.Errorf("EnsurePolicy: %v", err)
	}
}

// Policies enabled before the blueprint was changed: its sessions never ask
// OPA, so they are made as before and have no SessionPolicy.
func TestABlueprintFromBeforePolicies(t *testing.T) {
	store, client := sessionstest.New(t)
	store.EnablePolicies(client, sessionstest.Namespace, sessionstest.Unrestricted)
	s, err := store.Create(t.Context(), "a", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if s.PolicyCapable || sessionstest.Policy(t, client, s.ID) != nil {
		t.Error("a session that never asks OPA was given a policy")
	}
	if _, err := store.CreateWithPolicy(t.Context(), "b", "alice@example.com", &noScripting); !errors.Is(err, sessions.ErrPolicyUnsupported) {
		t.Errorf("err = %v, want ErrPolicyUnsupported", err)
	}
	if mine, _ := store.List(t.Context(), "alice@example.com"); len(mine) != 1 {
		t.Errorf("%d sessions: one asked to be restricted was made unrestricted", len(mine))
	}
	if err := store.EnsurePolicy(t.Context(), s.ID, noScripting); !errors.Is(err, sessions.ErrPolicyUnsupported) {
		t.Errorf("EnsurePolicy: %v", err)
	}
}

func TestPolicyCapable(t *testing.T) {
	sandbox := func(env ...any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"podTemplate": map[string]any{"spec": map[string]any{
			"containers": []any{
				map[string]any{"name": "browser", "env": []any{map[string]any{"name": "MCP_V8_POLICIES_JSON", "value": "browserjs/decision/"}}},
				map[string]any{"name": "mcp-js", "env": env},
			},
		}}}}}
	}
	for name, c := range map[string]struct {
		obj  *unstructured.Unstructured
		want bool
	}{
		"asks OPA":                 {sandbox(map[string]any{"name": "MCP_V8_POLICIES_JSON", "value": sessionstest.PolicyEnv}), true},
		"static policies":          {sandbox(map[string]any{"name": "MCP_V8_POLICIES_JSON", "value": `{"mcp_tools":{"policies":[{"url":"file:///etc/mcp/mcp_tools.rego"}]}}`}), false},
		"no such variable":         {sandbox(map[string]any{"name": "MCP_V8_PUBLIC_URL", "value": "https://browserjs/decision/"}), false},
		"only another container's": {sandbox(), false},
		"no pod template":          {&unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}, false},
	} {
		if got := sessions.PolicyCapable(c.obj); got != c.want {
			t.Errorf("%s: PolicyCapable = %v, want %v", name, got, c.want)
		}
	}
}

// A session whose policy cannot be made would be denied everything.
func TestCreateFailsWithItsPolicy(t *testing.T) {
	store, client := sessionstest.NewWithPolicies(t)
	client.(*dynfake.FakeDynamicClient).PrependReactor("create", sessions.PolicyGVR.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("no such resource")
	})
	if _, err := store.Create(t.Context(), "a", "alice@example.com"); err == nil {
		t.Fatal("Create succeeded")
	}
	if all, _ := store.ListAll(t.Context()); len(all) != 0 {
		t.Errorf("%d sessions left behind", len(all))
	}
}

func TestDeleteRemovesThePolicy(t *testing.T) {
	store, client := sessionstest.NewWithPolicies(t)
	s, err := store.Create(t.Context(), "a", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(t.Context(), s.ID); err != nil {
		t.Fatal(err)
	}
	if sessionstest.Policy(t, client, s.ID) != nil {
		t.Error("the policy outlived its session")
	}
}

func TestWarmAdoptionKeepsTheRequestedPolicy(t *testing.T) {
	store, client := sessionstest.NewWithPolicies(t)
	store.EnableWarmPool(sessionstest.WarmPoolName, time.Second)
	sessionstest.PlayPolicyClaimController(t, client, "s-bcdfg")

	// The policy is there before the Sandbox has an owner: until it has
	// one nobody can use it, so it is never usable and unrestricted.
	var policyFirst bool
	client.(*dynfake.FakeDynamicClient).PrependReactor("update", sessions.SandboxGVR.Resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj := action.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured)
		if obj.GetAnnotations()[sessions.AnnOwner] != "" {
			// The tracker, not the client: the fake is locked while it reacts.
			_, err := client.(*dynfake.FakeDynamicClient).Tracker().Get(sessions.PolicyGVR, sessionstest.Namespace, obj.GetName())
			policyFirst = err == nil
		}
		return false, nil, nil
	})

	s, err := store.CreateWithPolicy(t.Context(), "a", "alice@example.com", &asCode)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "s-bcdfg" || !s.PolicyCapable {
		t.Fatalf("session = %+v, want the warm Sandbox", s)
	}
	if !policyFirst {
		t.Error("the Sandbox was given its owner before its policy existed")
	}
	obj := sessionstest.Policy(t, client, s.ID)
	if got := policySpec(t, obj); got != asCode {
		t.Errorf("policy = %+v, want %+v", got, asCode)
	}
	if refs := obj.GetOwnerReferences(); len(refs) != 1 || refs[0].Name != "s-bcdfg" || refs[0].UID != raw(t, client, s.ID).GetUID() {
		t.Errorf("ownerReferences = %+v", refs)
	}
	if got := obj.GetLabels()[sessions.LabelOwner]; got != sessions.OwnerLabel("alice@example.com") {
		t.Errorf("owner label = %q", got)
	}
	// The claim says what was asked for.
	ann := sessionstest.Claims(t, client)[0].GetAnnotations()
	if ann[sessions.AnnPolicyKind] != "rego" || ann[sessions.AnnPolicySource] != asCode.Source ||
		ann[sessions.AnnPolicyMode] != "iac" || ann[sessions.AnnPolicyURL] != asCode.ManagedURL {
		t.Errorf("claim annotations = %v", ann)
	}

	// With none asked for, the default.
	sessionstest.PlayPolicyClaimController(t, client, "s-ghjkl")
	s, err = store.Create(t.Context(), "b", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got := policySpec(t, sessionstest.Policy(t, client, s.ID)); s.ID != "s-ghjkl" || got.Source != sessionstest.Unrestricted.Source || got.Mode != "editor" {
		t.Errorf("session %s, policy %+v", s.ID, got)
	}
}

// A backend that died after the claim was bound: recovery must not give the
// session the default policy in place of the one it was asked to have.
func TestClaimRecoveryKeepsTheRequestedPolicy(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.NewWithPolicies(t)
	store.EnableWarmPool(sessionstest.WarmPoolName, time.Second)
	sessionstest.PlayPolicyClaimController(t, client, "s-bcdfg")
	claim := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": sessions.ClaimGVR.GroupVersion().String(),
		"kind":       "SandboxClaim",
		"metadata": map[string]any{
			"name":   "s-abcdefghij",
			"labels": map[string]any{sessions.LabelOwner: sessions.OwnerLabel("alice@example.com")},
			"annotations": map[string]any{
				sessions.AnnOwner: "alice@example.com", sessions.AnnName: "a",
				sessions.AnnPolicyKind: asCode.Kind, sessions.AnnPolicySource: asCode.Source,
				sessions.AnnPolicyMode: asCode.Mode, sessions.AnnPolicyURL: asCode.ManagedURL,
				sessions.AnnUpdatedBy: asCode.UpdatedBy,
			},
		},
		"spec": map[string]any{"warmPoolRef": map[string]any{"name": sessionstest.WarmPoolName}},
	}}
	if _, err := client.Resource(sessions.ClaimGVR).Namespace(sessionstest.Namespace).Create(ctx, claim, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if sessionstest.Policy(t, client, "s-bcdfg") != nil {
		t.Fatal("there is a policy before recovery")
	}
	if err := store.RecoverClaims(ctx); err != nil {
		t.Fatal(err)
	}
	if s, err := store.Get(ctx, "s-bcdfg"); err != nil || s.Owner != "alice@example.com" {
		t.Fatalf("after recovery: %+v, %v", s, err)
	}
	if got := policySpec(t, sessionstest.Policy(t, client, "s-bcdfg")); got != asCode {
		t.Errorf("policy = %+v, want %+v", got, asCode)
	}

	// Every start recovers every claim. A policy changed since is left as
	// its owner made it.
	res := client.Resource(sessions.PolicyGVR).Namespace(sessionstest.Namespace)
	obj := sessionstest.Policy(t, client, "s-bcdfg")
	_ = unstructured.SetNestedField(obj.Object, "package browserjs.policy\n", "spec", "source")
	if _, err := res.Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecoverClaims(ctx); err != nil {
		t.Fatal(err)
	}
	if got := policySpec(t, sessionstest.Policy(t, client, "s-bcdfg")); got.Source != "package browserjs.policy\n" {
		t.Errorf("recovery overwrote the policy: %+v", got)
	}
}

// What the pool hands over cannot always be given the policy; the session is
// then made cold, with the policy, and nothing of the attempt is left.
func TestWarmSandboxesThatCannotTakeThePolicy(t *testing.T) {
	cases := map[string]func(t *testing.T, client *dynfake.FakeDynamicClient){
		"the pool predates policies": func(t *testing.T, client *dynfake.FakeDynamicClient) {
			sessionstest.PlayClaimController(t, client, "s-bcdfg")
		},
		"a policy of an earlier session of that name": func(t *testing.T, client *dynfake.FakeDynamicClient) {
			sessionstest.PlayPolicyClaimController(t, client, "s-bcdfg")
			stale := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": sessions.PolicyGVR.GroupVersion().String(), "kind": "SessionPolicy",
				"metadata": map[string]any{
					"name": "s-bcdfg",
					"ownerReferences": []any{map[string]any{
						"apiVersion": "agents.x-k8s.io/v1beta1", "kind": "Sandbox", "name": "s-bcdfg", "uid": "uid-of-the-earlier-one",
					}},
				},
				"spec": map[string]any{
					"sessionRef": map[string]any{"name": "s-bcdfg"}, "kind": "rego",
					"source": sessionstest.Unrestricted.Source, "management": map[string]any{"mode": "editor"},
				},
			}}
			if _, err := client.Resource(sessions.PolicyGVR).Namespace(sessionstest.Namespace).Create(t.Context(), stale, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			store, client := sessionstest.NewWithPolicies(t)
			store.EnableWarmPool(sessionstest.WarmPoolName, time.Second)
			setup(t, client.(*dynfake.FakeDynamicClient))

			s, err := store.CreateWithPolicy(t.Context(), "a", "alice@example.com", &noScripting)
			if err != nil {
				t.Fatal(err)
			}
			if len(s.ID) != 12 {
				t.Fatalf("session %q: the warm Sandbox was used", s.ID)
			}
			if got := policySpec(t, sessionstest.Policy(t, client, s.ID)); got.Source != noScripting.Source {
				t.Errorf("policy = %+v", got)
			}
			if claims := sessionstest.Claims(t, client); len(claims) != 0 {
				t.Errorf("%d claims left behind", len(claims))
			}
			if got, err := store.Get(t.Context(), "s-bcdfg"); err == nil && got.Owner != "" {
				t.Error("the warm Sandbox was given an owner")
			}
			if stale := sessionstest.Policy(t, client, "s-bcdfg"); stale != nil {
				t.Errorf("the earlier session's policy is still there: %v", stale.GetOwnerReferences())
			}
		})
	}
}

// A capable session whose policy was removed behind the backend can be
// given one again; one that has its policy keeps it.
func TestEnsurePolicy(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.NewWithPolicies(t)
	s, err := store.CreateWithPolicy(ctx, "a", "alice@example.com", &noScripting)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsurePolicy(ctx, s.ID, asCode); err != nil {
		t.Fatal(err)
	}
	if got := policySpec(t, sessionstest.Policy(t, client, s.ID)); got.Source != noScripting.Source {
		t.Errorf("EnsurePolicy replaced the policy: %+v", got)
	}
	if err := client.Resource(sessions.PolicyGVR).Namespace(sessionstest.Namespace).Delete(ctx, s.ID, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsurePolicy(ctx, s.ID, asCode); err != nil {
		t.Fatal(err)
	}
	if got := policySpec(t, sessionstest.Policy(t, client, s.ID)); got != asCode {
		t.Errorf("policy = %+v, want %+v", got, asCode)
	}
	if err := store.EnsurePolicy(ctx, "s-aaaaaaaaaa", asCode); !errors.Is(err, sessions.ErrNotFound) {
		t.Errorf("missing session: %v", err)
	}
}
