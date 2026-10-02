// Package sessions maps browserjs sessions onto Agent Sandbox resources.
package sessions

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var SandboxGVR = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}

const (
	LabelOwner   = "browserjs.dev/owner"    // OwnerLabel(owner), for selecting
	AnnOwner     = "browserjs.dev/owner-id" // the owner: the user's email address
	AnnName      = "browserjs.dev/name"
	AnnStoppedBy = "browserjs.dev/stopped-by"

	StoppedByUser = "user" // stays stopped until resumed
	StoppedByIdle = "idle" // wakes on the next request
)

// OwnerLabel is the value of LabelOwner for an owner. An owner is an email
// address (it has an "@", and may be long or contain "+"), so it cannot be a
// label value or go into a selector itself.
func OwnerLabel(owner string) string {
	sum := sha256.Sum256([]byte(owner))
	return hex.EncodeToString(sum[:])[:32]
}

var idPattern = regexp.MustCompile(`^s-[a-z2-7]{10}$`)

// ValidID reports whether id has the form of a session ID. Anything else
// cannot name a session and need not be sent to the cluster.
func ValidID(id string) bool { return idPattern.MatchString(id) }

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
	Node    string    `json:"-"` // the node its pod is scheduled to, if any
}

type condition struct {
	status, reason, message string
	observedGeneration      int64 // the Sandbox generation the condition is about
}

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
		c.observedGeneration, _, _ = unstructured.NestedInt64(m, "observedGeneration")
		out[typ] = c
	}
	return out
}

// FromSandbox derives the API view of a session from its Sandbox.
func FromSandbox(obj *unstructured.Unstructured) Session {
	s := Session{
		ID:      obj.GetName(),
		Name:    obj.GetAnnotations()[AnnName],
		Owner:   obj.GetAnnotations()[AnnOwner],
		Created: obj.GetCreationTimestamp().Time,
	}
	mode, _, _ := unstructured.NestedString(obj.Object, "spec", "operatingMode")
	conds := conditions(obj)
	ready := conds["Ready"]

	switch {
	case obj.GetDeletionTimestamp() != nil:
		// Deleted but held by a finalizer: going away, whatever the
		// conditions still say.
		s.State = Stopping
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
		if s.Message = ready.message; s.Message == "" {
			s.Message = conds["Finished"].message
		}
	default:
		s.State = Starting
		s.Message = ready.message
	}
	// Only a running session has a pod to reach; in any other state
	// status.podIPs may be left over from one that is gone.
	if s.State == Running {
		if ips, _, _ := unstructured.NestedStringSlice(obj.Object, "status", "podIPs"); len(ips) > 0 {
			s.PodIP = ips[0]
		}
	}
	if mode != "Suspended" {
		s.Node, _, _ = unstructured.NestedString(obj.Object, "status", "nodeName")
	}
	return s
}
