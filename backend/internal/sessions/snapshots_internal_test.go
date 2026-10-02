package sessions

import "testing"

// The label value the managed controller put on the pod of a real session
// (GKE 1.36.4, read with the cluster-info workflow).
func TestNameHashMatchesTheController(t *testing.T) {
	if got := nameHash("s-yqm5374u4p"); got != "62a0efe4" {
		t.Errorf("nameHash = %q, want 62a0efe4", got)
	}
}
