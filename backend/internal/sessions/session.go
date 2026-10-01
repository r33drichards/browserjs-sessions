// Package sessions maps browserjs sessions onto Agent Sandbox resources.
package sessions

import (
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var SandboxGVR = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}

const (
	LabelOwner   = "browserjs.dev/owner"
	AnnName      = "browserjs.dev/name"
	AnnStoppedBy = "browserjs.dev/stopped-by"

	StoppedByUser = "user" // stays stopped until resumed
	StoppedByIdle = "idle" // wakes on the next request
)

type State string

const (
	Starting State = "starting"
	Running  State = "running"
	Stopping State = "stopping"
	Asleep   State = "asleep"
	Stopped  State = "stopped"
	Failed   State = "failed"
)

type Session struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Owner   string    `json:"owner"`
	State   State     `json:"state"`
	Message string    `json:"message,omitempty"` // why it is starting or failed
	Created time.Time `json:"created"`
	PodIP   string    `json:"-"`
}

type condition struct{ status, reason, message string }

func conditions(obj *unstructured.Unstructured) map[string]condition {
	out := map[string]condition{}
	list, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		c := condition{}
		c.status, _ = m["status"].(string)
		c.reason, _ = m["reason"].(string)
		c.message, _ = m["message"].(string)
		out[typ] = c
	}
	return out
}

// FromSandbox derives the API view of a session from its Sandbox.
func FromSandbox(obj *unstructured.Unstructured) Session {
	s := Session{
		ID:      obj.GetName(),
		Name:    obj.GetAnnotations()[AnnName],
		Owner:   obj.GetLabels()[LabelOwner],
		Created: obj.GetCreationTimestamp().Time,
	}
	if ips, _, _ := unstructured.NestedStringSlice(obj.Object, "status", "podIPs"); len(ips) > 0 {
		s.PodIP = ips[0]
	}
	mode, _, _ := unstructured.NestedString(obj.Object, "spec", "operatingMode")
	conds := conditions(obj)
	ready := conds["Ready"]

	switch {
	case mode == "Suspended":
		// The Suspended condition is only meaningful while operatingMode is
		// Suspended: the controller leaves a stale one behind after a resume.
		switch {
		case conds["Suspended"].status != "True":
			s.State = Stopping
		case obj.GetAnnotations()[AnnStoppedBy] == StoppedByIdle:
			s.State = Asleep
		default:
			s.State = Stopped
		}
	case ready.status == "True":
		s.State = Running
	case conds["Finished"].reason == "PodFailed" || ready.reason == "InvalidConfiguration":
		s.State = Failed
		s.Message = ready.message
	default:
		s.State = Starting
		s.Message = ready.message
	}
	return s
}
