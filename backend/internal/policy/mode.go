package policy

import (
	"net/url"

	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

// What a token must be allowed to do to read and to write a session's
// policy. What is not about a session (validate, evaluate, the schema, the
// presets) any token may use.
const (
	ScopeRead  = auth.ScopePoliciesRead
	ScopeWrite = auth.ScopePoliciesWrite
)

// hasScope reports whether the caller may do what scope names ("" for what
// any caller may). Someone signed in through the UI may; a token only if
// it was given the scope.
func hasScope(u auth.User, scope string) bool {
	return scope == "" || u.Token == nil || u.Token.Has(scope)
}

// forSession reports whether the caller's credential reaches session id. A
// token made for one session reaches no other, whoever owns them.
func forSession(u auth.User, id string) bool {
	return u.Token == nil || u.Token.Session == "" || u.Token.Session == id
}

// updatedBy is what browserjs.dev/updated-by says of a write by u.
func updatedBy(u auth.User) string {
	if u.Token == nil {
		return "ui"
	}
	return "token:" + u.Token.Name
}

// ModeError is the refusal of a write by a credential that the policy's
// management mode does not let write.
type ModeError struct {
	Error      string `json:"error"`
	ManagedURL string `json:"managed_url,omitempty"`
}

// mayWrite decides whether u may save or reset a policy that is in the
// management mode current, with a request that asks for the management
// requested (nil: none asked for). It returns nil, or the refusal.
//
//	         | cookie    | token
//	editor   | may write | refused, unless the request sets mode iac
//	iac      | refused   | may write
//
// A policy is edited in one place: in the editor by its owner, or as code
// by a token. A token takes one over by saying so in the same request; the
// owner takes it back by changing the mode, which either may always do.
func mayWrite(current Management, u auth.User, requested *Management) *ModeError {
	token := u.Token != nil
	switch {
	case current.Mode == sessions.PolicyModeIaC && !token:
		return &ModeError{Error: "this policy is managed externally", ManagedURL: current.ManagedURL}
	case current.Mode != sessions.PolicyModeIaC && token && (requested == nil || requested.Mode != sessions.PolicyModeIaC):
		return &ModeError{Error: "this policy is managed in the editor"}
	}
	return nil
}

// cleanManagement checks a management as a request states it. The link
// belongs to iac alone and is dropped from editor.
func cleanManagement(m Management) (Management, bool) {
	switch m.Mode {
	case sessions.PolicyModeEditor:
		return Management{Mode: m.Mode}, true
	case sessions.PolicyModeIaC:
		u, err := url.Parse(m.ManagedURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || len(m.ManagedURL) > maxManagedURL {
			return Management{}, false
		}
		return m, true
	}
	return Management{}, false
}

const badManagement = `management.mode must be "editor" or "iac", and managed_url an https URL when it is "iac"`
