package sessions

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func sandbox(mode, stoppedBy string, conds ...map[string]any) *unstructured.Unstructured {
	cs := make([]any, len(conds))
	for i, c := range conds {
		cs[i] = c
	}
	ann := map[string]any{AnnName: "my session"}
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
			"labels": map[string]any{LabelOwner: "user-1"}, "annotations": ann,
		},
		"spec":   spec,
		"status": map[string]any{"conditions": cs, "podIPs": []any{"10.0.0.7"}},
	}}
}

func cond(typ, status, reason, msg string) map[string]any {
	return map[string]any{"type": typ, "status": status, "reason": reason, "message": msg}
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
	}
	for _, c := range cases {
		if got := FromSandbox(c.obj).State; got != c.want {
			t.Errorf("%s: state = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestFromSandboxFields(t *testing.T) {
	s := FromSandbox(sandbox("Running", "", cond("Ready", "False", "DependenciesNotReady", "pod pending")))
	if s.ID != "s-abc" || s.Name != "my session" || s.Owner != "user-1" || s.PodIP != "10.0.0.7" {
		t.Errorf("unexpected fields: %+v", s)
	}
	if s.Message != "pod pending" {
		t.Errorf("message = %q", s.Message)
	}
	if s.Created.IsZero() {
		t.Error("created not parsed")
	}
}
