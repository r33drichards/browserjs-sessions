package sessions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Session policies: every session made while they are enabled gets a
// SessionPolicy (deploy/base/crd-sessionpolicy.yaml) named after it and owned
// by its Sandbox, which the policy operator turns into what the shared OPA
// answers that session's mcp-js with. A session without one is denied
// everything, so the policy is made with the session: after the Sandbox for a
// cold start (which has its owner from the start, and is denied until the
// policy exists), and before the owner is written for a Sandbox from the warm
// pool (which nobody can use until then).
//
// This file only makes the object. Reading and changing it afterwards is
// internal/policy's business.

var PolicyGVR = schema.GroupVersionResource{Group: "browserjs.dev", Version: "v1alpha1", Resource: "sessionpolicies"}

const (
	// AnnUpdatedBy is who last wrote a SessionPolicy: "ui" or "token:<name>".
	AnnUpdatedBy = "browserjs.dev/updated-by"

	// On a SandboxClaim: the policy the session was asked to be created
	// with, so that RecoverClaims does not replace it with the default.
	AnnPolicyKind   = "browserjs.dev/policy-kind"
	AnnPolicySource = "browserjs.dev/policy-source"
	AnnPolicyMode   = "browserjs.dev/policy-mode"
	AnnPolicyURL    = "browserjs.dev/policy-url"

	PolicyModeEditor = "editor"
	PolicyModeIaC    = "iac"

	// What a policy-capable session's mcp-js is configured with, and what
	// that configuration has to mention (docs/contracts/policy/deploy.md).
	policyEnv      = "MCP_V8_POLICIES_JSON"
	policyEnvPath  = "browserjs/decision/"
	mcpJSContainer = "mcp-js"
)

// ErrPolicyUnsupported is the answer to a request for a policy on a session
// that cannot have one: policies are not enabled, or its pod never asks OPA.
var ErrPolicyUnsupported = errors.New("this session cannot be given a policy")

// PolicySpec is what a SessionPolicy is created with.
type PolicySpec struct {
	Kind       string // "rego"
	Source     string
	Mode       string // PolicyModeEditor or PolicyModeIaC; "" is editor
	ManagedURL string // where it is managed, when Mode is iac
	UpdatedBy  string // AnnUpdatedBy; "" is "ui"
}

// EnablePolicies makes Create give every new session a SessionPolicy:
// the one it was asked for, or unrestricted when it was asked for none.
func (s *Store) EnablePolicies(client dynamic.Interface, namespace string, unrestricted PolicySpec) {
	s.policies = client.Resource(PolicyGVR).Namespace(namespace)
	s.defaultPolicy = unrestricted
}

// Policies is where SessionPolicy objects are read and written, or nil when
// policies are not enabled.
func (s *Store) Policies() dynamic.ResourceInterface { return s.policies }

// PolicyCapable reports whether a Sandbox's mcp-js asks OPA for decisions. A
// Sandbox's pod template is fixed when it is made, so one from before
// policies never will, and has no SessionPolicy.
func PolicyCapable(obj *unstructured.Unstructured) bool {
	spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
	return policyCapableSpec(spec)
}

func policyCapableSpec(spec map[string]any) bool {
	containers, _, _ := unstructured.NestedSlice(spec, "podTemplate", "spec", "containers")
	for _, c := range containers {
		container, ok := c.(map[string]any)
		if !ok || container["name"] != mcpJSContainer {
			continue
		}
		env, _, _ := unstructured.NestedSlice(container, "env")
		for _, e := range env {
			v, ok := e.(map[string]any)
			if !ok || v["name"] != policyEnv {
				continue
			}
			value, _ := v["value"].(string)
			return strings.Contains(value, policyEnvPath)
		}
	}
	return false
}

func claimPolicyAnnotations(p PolicySpec) map[string]any {
	ann := map[string]any{
		AnnPolicyKind: p.Kind, AnnPolicySource: p.Source,
		AnnPolicyMode: policyMode(p), AnnUpdatedBy: updatedBy(p),
	}
	if p.ManagedURL != "" {
		ann[AnnPolicyURL] = p.ManagedURL
	}
	return ann
}

// claimPolicy is the policy a claim was made with, if it was made with one.
func claimPolicy(claim *unstructured.Unstructured) (PolicySpec, bool) {
	ann := claim.GetAnnotations()
	if ann[AnnPolicyKind] == "" {
		return PolicySpec{}, false
	}
	return PolicySpec{
		Kind: ann[AnnPolicyKind], Source: ann[AnnPolicySource],
		Mode: ann[AnnPolicyMode], ManagedURL: ann[AnnPolicyURL], UpdatedBy: ann[AnnUpdatedBy],
	}, true
}

func policyMode(p PolicySpec) string {
	if p.Mode == "" {
		return PolicyModeEditor
	}
	return p.Mode
}

func updatedBy(p PolicySpec) string {
	if p.UpdatedBy == "" {
		return "ui"
	}
	return p.UpdatedBy
}

// ensurePolicy makes the SessionPolicy of the session that sandbox is, for
// owner. One that is there already, and belongs to this Sandbox, is left as
// it is: its owner may have changed it since.
func (s *Store) ensurePolicy(ctx context.Context, sandbox *unstructured.Unstructured, owner string, p PolicySpec) error {
	id := sandbox.GetName()
	management := map[string]any{"mode": policyMode(p)}
	if policyMode(p) == PolicyModeIaC {
		management["managedURL"] = p.ManagedURL
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": PolicyGVR.GroupVersion().String(),
		"kind":       "SessionPolicy",
		"metadata": map[string]any{
			"name":        id,
			"labels":      map[string]any{LabelOwner: OwnerLabel(owner)},
			"annotations": map[string]any{AnnUpdatedBy: updatedBy(p)},
		},
		"spec": map[string]any{
			"sessionRef": map[string]any{"name": id},
			"kind":       p.Kind,
			"source":     p.Source,
			"management": management,
		},
	}}
	// Deleting the session garbage-collects its policy.
	no := false
	obj.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: SandboxGVR.GroupVersion().String(), Kind: "Sandbox",
		Name: id, UID: sandbox.GetUID(), Controller: &no, BlockOwnerDeletion: &no,
	}})
	_, err := s.policies.Create(ctx, obj, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	existing, err := s.policies.Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return err
	}
	for _, ref := range existing.GetOwnerReferences() {
		if ref.Kind == "Sandbox" && ref.UID == sandbox.GetUID() {
			return nil
		}
	}
	// The warm pool's names come round again: this one is of an earlier
	// Sandbox of the same name, and on its way out. It must not become this
	// session's policy.
	if derr := s.policies.Delete(ctx, id, metav1.DeleteOptions{}); derr != nil && !apierrors.IsNotFound(derr) {
		slog.Error("could not delete the policy of an earlier session", "session", id, "err", derr)
	}
	return fmt.Errorf("session %s: a policy of an earlier session of that name was in the way", id)
}

// EnsurePolicy gives session id the SessionPolicy p if it has none. It is
// how a session whose policy was removed behind the backend gets one again.
func (s *Store) EnsurePolicy(ctx context.Context, id string, p PolicySpec) error {
	if s.policies == nil {
		return ErrPolicyUnsupported
	}
	sandbox, err := s.client.Get(ctx, id, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	owner := sandbox.GetAnnotations()[AnnOwner]
	if owner == "" || !PolicyCapable(sandbox) {
		return ErrPolicyUnsupported
	}
	return s.ensurePolicy(ctx, sandbox, owner, p)
}

// adoptPolicy makes the policy a claim was made with, for the Sandbox the
// claim was bound to, before that Sandbox is given its owner.
func (s *Store) adoptPolicy(ctx context.Context, claim *unstructured.Unstructured, id, owner string) error {
	p, ok := claimPolicy(claim)
	if !ok || s.policies == nil {
		return nil
	}
	sandbox, err := s.client.Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if !PolicyCapable(sandbox) {
		// From a pool that predates policies: as a session it would ignore
		// the policy it was asked to have.
		return fmt.Errorf("Sandbox %s does not ask OPA for decisions: %w", id, ErrPolicyUnsupported)
	}
	if err := s.ensurePolicy(ctx, sandbox, owner, p); err != nil {
		return fmt.Errorf("policy of %s: %w", id, err)
	}
	return nil
}

// deletePolicy removes a deleted session's policy without waiting for the
// garbage collector, so that the name is free when the warm pool uses it
// again.
func (s *Store) deletePolicy(ctx context.Context, id string) {
	if s.policies == nil {
		return
	}
	if err := s.policies.Delete(ctx, id, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		slog.Warn("could not delete a deleted session's policy; the garbage collector will", "session", id, "err", err)
	}
}
