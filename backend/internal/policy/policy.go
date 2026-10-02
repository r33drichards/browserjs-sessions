// Package policy is the backend's side of session policies: what an agent
// connected over MCP may ask a session's browser to do.
//
// A session's policy is a SessionPolicy custom resource named after it
// (deploy/base/crd-sessionpolicy.yaml). The backend writes its spec on the
// owner's behalf; the policy operator compiles it, publishes it to the
// shared OPA and reports in its status, which the backend only reads. The
// operator is also the only judge of whether a policy is valid: nothing is
// saved that it has not checked (operator.go).
//
// The HTTP API is docs/contracts/policy/backend-api.yaml, and is described
// for its users in docs/policy-api.md.
package policy

import (
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

// State is where a policy is on its way into force.
type State string

const (
	Ready       State = "ready"       // in force on every OPA replica
	Loading     State = "loading"     // saved; not yet observed, or not yet on every replica
	Invalid     State = "invalid"     // does not compile; the one before it, if any, is still in force
	Unsupported State = "unsupported" // the session predates policies and has none
)

const (
	KindJSON = "json"
	KindRego = "rego"

	// MaxSource is the longest policy source, in bytes (the CRD's limit).
	MaxSource = 65536
	// maxManagedURL is the CRD's limit on management.managedURL.
	maxManagedURL = 2048
)

// Management is who may edit a policy: the editor in the UI, or whatever
// manages it as code (Terraform) with an API token.
type Management struct {
	Mode       string `json:"mode"`
	ManagedURL string `json:"managed_url,omitempty"`
}

// Diagnostic is one problem with a policy's source. Row and Col count from
// 1 and are absent (zero) when the problem has no position.
type Diagnostic struct {
	Row     int64  `json:"row,omitempty"`
	Col     int64  `json:"col,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Summary is a policy as a session carries it: everything but the text.
type Summary struct {
	Kind       string      `json:"kind,omitempty"`
	Version    int64       `json:"version,omitempty"` // the SessionPolicy's metadata.generation
	Hash       string      `json:"hash,omitempty"`    // of the policy in force
	State      State       `json:"state"`
	Management *Management `json:"management,omitempty"`

	// inForce is whether a policy of this session has ever been loaded by
	// OPA. Until one has, the session is denied everything.
	inForce bool
}

// Loaded is how many of the ready OPA replicas serve the policy in force.
type Loaded struct {
	Replicas int64 `json:"replicas"`
	Total    int64 `json:"total"`
}

// Policy is a session's policy in full.
type Policy struct {
	Summary
	Source    string       `json:"source,omitempty"`
	Rego      string       `json:"rego,omitempty"` // the module in force; for kind json, the generated one
	Errors    []Diagnostic `json:"errors,omitempty"`
	Warnings  []Diagnostic `json:"warnings,omitempty"`
	Loaded    *Loaded      `json:"loaded,omitempty"`
	Updated   *time.Time   `json:"updated,omitempty"` // when the policy in force was loaded
	UpdatedBy string       `json:"updated_by,omitempty"`
}

// Gate is s as its users should see it: a session whose first policy is not
// in force yet is not running, whatever its pod says, because OPA denies it
// everything.
func (p Summary) Gate(s sessions.Session) sessions.Session {
	if s.State != sessions.Running || p.inForce {
		return s
	}
	switch p.State {
	case Loading:
		s.State, s.Message = sessions.Starting, "waiting for its policy to be loaded"
	case Invalid:
		s.State, s.Message = sessions.Starting, "its policy does not compile"
	}
	return s
}

// condition returns the status and observedGeneration of one of a
// SessionPolicy's conditions.
func condition(obj *unstructured.Unstructured, typ string) (status string, generation int64) {
	list, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok || m["type"] != typ {
			continue
		}
		status, _ = m["status"].(string)
		generation, _, _ = unstructured.NestedInt64(m, "observedGeneration")
		return status, generation
	}
	return "", 0
}

// stateOf derives a policy's state from its SessionPolicy's status, for the
// generation the object is at now: a condition about an earlier generation
// says nothing about what was just written.
func stateOf(obj *unstructured.Unstructured) State {
	generation := obj.GetGeneration()
	if status, at := condition(obj, "Ready"); status == "True" && at == generation {
		return Ready
	}
	if status, at := condition(obj, "Compiled"); status == "False" && at == generation {
		return Invalid
	}
	return Loading
}

func diagnostics(obj *unstructured.Unstructured, field string) []Diagnostic {
	list, _, _ := unstructured.NestedSlice(obj.Object, "status", field)
	var out []Diagnostic
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		d := Diagnostic{}
		d.Row, _, _ = unstructured.NestedInt64(m, "row")
		d.Col, _, _ = unstructured.NestedInt64(m, "col")
		d.Code, _ = m["code"].(string)
		d.Message, _ = m["message"].(string)
		out = append(out, d)
	}
	return out
}

func summaryOf(obj *unstructured.Unstructured) Summary {
	s := Summary{Version: obj.GetGeneration(), State: stateOf(obj), Management: &Management{Mode: sessions.PolicyModeEditor}}
	s.Kind, _, _ = unstructured.NestedString(obj.Object, "spec", "kind")
	s.Hash, _, _ = unstructured.NestedString(obj.Object, "status", "hash")
	if mode, _, _ := unstructured.NestedString(obj.Object, "spec", "management", "mode"); mode == sessions.PolicyModeIaC {
		s.Management.Mode = mode
		s.Management.ManagedURL, _, _ = unstructured.NestedString(obj.Object, "spec", "management", "managedURL")
	}
	applied, _, _ := unstructured.NestedString(obj.Object, "status", "lastAppliedTime")
	s.inForce = applied != "" || s.State == Ready
	return s
}

// FromObject is the API's view of a SessionPolicy.
func FromObject(obj *unstructured.Unstructured) Policy {
	p := Policy{Summary: summaryOf(obj), UpdatedBy: obj.GetAnnotations()[sessions.AnnUpdatedBy]}
	p.Source, _, _ = unstructured.NestedString(obj.Object, "spec", "source")
	p.Rego, _, _ = unstructured.NestedString(obj.Object, "status", "rego")
	p.Errors = diagnostics(obj, "errors")
	p.Warnings = diagnostics(obj, "warnings")
	if _, has, _ := unstructured.NestedMap(obj.Object, "status", "loaded"); has {
		p.Loaded = &Loaded{}
		p.Loaded.Replicas, _, _ = unstructured.NestedInt64(obj.Object, "status", "loaded", "replicas")
		p.Loaded.Total, _, _ = unstructured.NestedInt64(obj.Object, "status", "loaded", "total")
	}
	if applied, _, _ := unstructured.NestedString(obj.Object, "status", "lastAppliedTime"); applied != "" {
		if at, err := time.Parse(time.RFC3339, applied); err == nil {
			p.Updated = &at
		}
	}
	return p
}
