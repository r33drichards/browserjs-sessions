package sessions

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

func sandbox(mode, stoppedBy string, conds ...map[string]any) *unstructured.Unstructured {
	cs := make([]any, len(conds))
	for i, c := range conds {
		cs[i] = c
	}
	ann := map[string]any{AnnName: "my session", AnnOwner: "user-1"}
	if stoppedBy != "" {
		ann[AnnStoppedBy] = stoppedBy
	}
	spec := map[string]any{}
	if mode != "" {
		spec["operatingMode"] = mode
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agents.x-k8s.io/v1beta1", "kind": "Sandbox",
		"metadata": map[string]any{
			"name": "s-abc", "creationTimestamp": "2026-10-01T00:00:00Z",
			"labels": map[string]any{LabelOwner: OwnerLabel("user-1")}, "annotations": ann,
		},
		"spec":   spec,
		"status": map[string]any{"conditions": cs, "podIPs": []any{"10.0.0.7"}},
	}}
}

func cond(typ, status, reason, msg string) map[string]any {
	return map[string]any{"type": typ, "status": status, "reason": reason, "message": msg}
}

// deleting marks a Sandbox as deleted but still held by a finalizer.
func deleting(obj *unstructured.Unstructured) *unstructured.Unstructured {
	_ = unstructured.SetNestedField(obj.Object, "2026-10-01T01:00:00Z", "metadata", "deletionTimestamp")
	return obj
}

func TestFromSandboxState(t *testing.T) {
	cases := []struct {
		name string
		obj  *unstructured.Unstructured
		want State
	}{
		{"no conditions yet", sandbox("", ""), Starting},
		{"not ready", sandbox("Running", "", cond("Ready", "False", "DependenciesNotReady", "pod pending")), Starting},
		{"ready", sandbox("Running", "", cond("Ready", "True", "DependenciesReady", "")), Running},
		{"suspending", sandbox("Suspended", StoppedByIdle, cond("Suspended", "False", "PodTerminating", "")), Stopping},
		{"asleep", sandbox("Suspended", StoppedByIdle, cond("Suspended", "True", "PodTerminated", "")), Asleep},
		{"stopped by user", sandbox("Suspended", StoppedByUser, cond("Suspended", "True", "PodTerminated", "")), Stopped},
		// A stale Suspended=True lingers after resume; operatingMode decides.
		{"resumed, stale suspended condition", sandbox("Running", "",
			cond("Suspended", "True", "PodTerminated", ""), cond("Ready", "False", "DependenciesNotReady", "")), Starting},
		{"pod failed", sandbox("Running", "", cond("Finished", "True", "PodFailed", "OOMKilled"),
			cond("Ready", "False", "PodFailed", "OOMKilled")), Failed},
		{"invalid", sandbox("Running", "", cond("Ready", "False", "InvalidConfiguration", "name too long")), Failed},
		// Nothing recorded who stopped it: stay stopped rather than wake on demand.
		{"suspended, no stopped-by", sandbox("Suspended", "", cond("Suspended", "True", "PodTerminated", "")), Stopped},
		{"suspending, pod still ready", sandbox("Suspended", StoppedByUser, cond("Ready", "True", "DependenciesReady", "")), Stopping},
		// A deleted Sandbox held by a finalizer is on its way out whatever its conditions say.
		{"deleting while ready", deleting(sandbox("Running", "", cond("Ready", "True", "DependenciesReady", ""))), Stopping},
		{"deleting while starting", deleting(sandbox("Running", "")), Stopping},
		{"deleting while asleep", deleting(sandbox("Suspended", StoppedByIdle, cond("Suspended", "True", "PodTerminated", ""))), Stopping},
		{"deleting after failing", deleting(sandbox("Running", "", cond("Ready", "False", "InvalidConfiguration", ""))), Stopping},
	}
	for _, c := range cases {
		if got := FromSandbox(c.obj).State; got != c.want {
			t.Errorf("%s: state = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestFromSandboxFields(t *testing.T) {
	s := FromSandbox(sandbox("Running", "", cond("Ready", "False", "DependenciesNotReady", "pod pending")))
	if s.ID != "s-abc" || s.Name != "my session" || s.Owner != "user-1" {
		t.Errorf("unexpected fields: %+v", s)
	}
	if s.Message != "pod pending" {
		t.Errorf("message = %q", s.Message)
	}
	if s.Created.IsZero() {
		t.Error("created not parsed")
	}
}

// The owner is the raw subject from the annotation; the label is only a
// hash of it and must never be reported as the owner.
func TestFromSandboxOwner(t *testing.T) {
	obj := sandbox("Running", "")
	ann := obj.GetAnnotations()
	delete(ann, AnnOwner)
	obj.SetAnnotations(ann)
	if got := FromSandbox(obj).Owner; got != "" {
		t.Errorf("owner without the annotation = %q, want empty", got)
	}
}

// The pod IP is only offered while the session is running: in every other
// state status.podIPs may be left over from a pod that is gone.
func TestFromSandboxPodIP(t *testing.T) {
	ready := cond("Ready", "True", "DependenciesReady", "")
	if got := FromSandbox(sandbox("Running", "", ready)).PodIP; got != "10.0.0.7" {
		t.Errorf("running: PodIP = %q", got)
	}
	for name, obj := range map[string]*unstructured.Unstructured{
		"starting": sandbox("Running", "", cond("Ready", "False", "DependenciesNotReady", "")),
		"stopping": sandbox("Suspended", StoppedByUser, ready),
		"asleep":   sandbox("Suspended", StoppedByIdle, cond("Suspended", "True", "PodTerminated", "")),
		"failed":   sandbox("Running", "", cond("Ready", "False", "InvalidConfiguration", "")),
		"deleting": deleting(sandbox("Running", "", ready)),
	} {
		if got := FromSandbox(obj).PodIP; got != "" {
			t.Errorf("%s: PodIP = %q, want none", name, got)
		}
	}
}

func TestFromSandboxFailedMessage(t *testing.T) {
	s := FromSandbox(sandbox("Running", "", cond("Ready", "False", "InvalidConfiguration", "name too long")))
	if s.Message != "name too long" {
		t.Errorf("message = %q", s.Message)
	}
	// Ready carries no message: say what the Finished condition says.
	s = FromSandbox(sandbox("Running", "", cond("Finished", "True", "PodFailed", "OOMKilled"),
		cond("Ready", "False", "PodFailed", "")))
	if s.State != Failed || s.Message != "OOMKilled" {
		t.Errorf("state %s, message %q; want failed, OOMKilled", s.State, s.Message)
	}
}

func TestFromSandboxMalformedConditions(t *testing.T) {
	obj := sandbox("Running", "")
	_ = unstructured.SetNestedSlice(obj.Object, []any{"nonsense", map[string]any{"type": int64(3)}, map[string]any{}}, "status", "conditions")
	if got := FromSandbox(obj).State; got != Starting {
		t.Errorf("state = %s, want starting", got)
	}
	_ = unstructured.SetNestedField(obj.Object, "nonsense", "status", "conditions")
	if got := FromSandbox(obj).State; got != Starting {
		t.Errorf("state = %s, want starting", got)
	}
}

func TestOwnerLabel(t *testing.T) {
	seen := map[string]string{}
	for _, subject := range []string{"user-1", "user-2", "auth0|abc:def@example.com", "a,b", strings.Repeat("x", 100), "f:1234:robert"} {
		l := OwnerLabel(subject)
		if len(l) != 32 || len(validation.IsValidLabelValue(l)) != 0 {
			t.Errorf("OwnerLabel(%q) = %q is not a 32-character label value", subject, l)
		}
		if other, dup := seen[l]; dup {
			t.Errorf("%q and %q share label %q", subject, other, l)
		}
		seen[l] = subject
		if l != OwnerLabel(subject) {
			t.Errorf("OwnerLabel(%q) is not stable", subject)
		}
	}
}

func TestValidID(t *testing.T) {
	for range 50 {
		if id := newID(); !ValidID(id) {
			t.Fatalf("generated ID %q is not valid", id)
		}
	}
	for _, id := range []string{"", "s-", "s-missing000", "s-ABCDEFGHIJ", "s-abcdefghijk", "x-abcdefghij", "s-abcde/ghij", "../sandboxes", "s-abcdefghij\n"} {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true", id)
		}
	}
}
