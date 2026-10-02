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
	// StoppedBySleep is a sleep its user asked for: a snapshot is taken, as
	// for an idle one, and it wakes on the next request or when asked to.
	StoppedBySleep = "sleep"
	// Billing's reasons (docs/contracts/billing/enforcement.md). A session
	// asleep for credit or for want of a payment method wakes on the next
	// request, as an idle one does, if its owner's account then allows it.
	// One put to sleep because its owner is blocked stays stopped.
	StoppedByCredit        = "credit"
	StoppedByPaymentMethod = "payment-method"
	StoppedByBlocked       = "blocked"

	// AnnDraining marks a running session that is about to be put to sleep
	// for one of billing's reasons (its value): calls in flight finish, new
	// ones are refused. AnnDrainingSince is when the mark was made.
	AnnDraining      = "browserjs.dev/draining"
	AnnDrainingSince = "browserjs.dev/draining-since"
)

// wakes reports whether a session suspended for reason by wakes on its next
// use (Wake), rather than staying stopped until it is resumed.
func wakes(by string) bool {
	return by == StoppedByIdle || by == StoppedBySleep || by == StoppedByCredit || by == StoppedByPaymentMethod
}

// GoingToSleep reports whether a session that is suspended, or on its way
// there, wakes on its next use: it is asleep, or will be once its pod is gone.
func (s Session) GoingToSleep() bool { return wakes(s.StoppedBy) }

// sleepReason reports whether by is a reason Sleep takes.
func sleepReason(by string) bool { return wakes(by) || by == StoppedByBlocked }

// OwnerLabel is the value of LabelOwner for an owner. An owner is an email
// address (it has an "@", and may be long or contain "+"), so it cannot be a
// label value or go into a selector itself.
func OwnerLabel(owner string) string {
	sum := sha256.Sum256([]byte(owner))
	return hex.EncodeToString(sum[:])[:32]
}

// A session's ID is its Sandbox's name: ten characters from newID for one
// the backend named, five from the API server (generateName "s-") for one
// that came out of the warm pool "s".
var idPattern = regexp.MustCompile(`^s-([a-z2-7]{10}|[a-z0-9]{5})$`)

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
	// StateSaved is whether a suspended session holds a snapshot of its pod
	// to wake from (see snapshots.go). Without one it starts fresh, with its
	// disk only.
	StateSaved bool   `json:"stateSaved,omitempty"`
	PodIP      string `json:"-"`
	Node       string `json:"-"` // the node its pod is scheduled to, if any
	// PolicyCapable is whether the session's mcp-js asks OPA for decisions,
	// and so whether the session can have a policy (see policy.go).
	PolicyCapable bool `json:"-"`
	// StoppedBy is why a suspended session is suspended (StoppedBy*), ""
	// for one that is not. Draining is the reason a running session is
	// being drained for, and DrainingSince when that began. Shown by the
	// API only where billing is on.
	StoppedBy     string    `json:"-"`
	Draining      string    `json:"-"`
	DrainingSince time.Time `json:"-"`
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

		PolicyCapable: PolicyCapable(obj),
	}
	if adopted, err := time.Parse(time.RFC3339, obj.GetAnnotations()[AnnCreated]); err == nil {
		s.Created = adopted
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
		case wakes(obj.GetAnnotations()[AnnStoppedBy]):
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
		if s.Draining = obj.GetAnnotations()[AnnDraining]; s.Draining != "" {
			s.DrainingSince, _ = time.Parse(time.RFC3339, obj.GetAnnotations()[AnnDrainingSince])
		}
	} else if obj.GetDeletionTimestamp() == nil {
		s.StoppedBy = obj.GetAnnotations()[AnnStoppedBy]
		s.StateSaved = obj.GetAnnotations()[AnnSnapshot] != ""
	}
	return s
}
