package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/r33drichards/computer-use/backend/internal/auth"
	"github.com/r33drichards/computer-use/backend/internal/policy"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
)

// Session policies (internal/policy). They are off until EnablePolicies is
// called, and while they are off the API is what it was before them: no
// policy routes, no policy on a session, no policy accepted with a new one.

// EnablePolicies adds the policy routes and the policy of each session.
// Call it before Register.
func (a *API) EnablePolicies(p *policy.Handlers) { a.policies = p }

func (a *API) registerPolicies(mux *http.ServeMux) {
	if a.policies == nil {
		return
	}
	p := a.policies
	mux.HandleFunc("GET /api/sessions/{id}/policy", a.session(p.Get))
	mux.HandleFunc("PUT /api/sessions/{id}/policy", a.session(p.Put))
	mux.HandleFunc("DELETE /api/sessions/{id}/policy", a.session(p.Delete))
	mux.HandleFunc("PUT /api/sessions/{id}/policy/management", a.session(p.PutManagement))
	mux.HandleFunc("POST /api/policies/validate", a.user(p.Validate))
	mux.HandleFunc("POST /api/policies/evaluate", a.user(p.Evaluate))
	mux.HandleFunc("GET /api/policy-presets", a.user(p.PresetList))
}

// How large the body of a create may be: a name, and a policy if there are
// policies.
func (a *API) maxCreateBody() int64 {
	if a.policies == nil {
		return 4096
	}
	return policy.MaxBody
}

// policyFor checks the policy a new session is asked to have. It answers
// the request itself when the session must not be created.
func (a *API) policyFor(w http.ResponseWriter, r *http.Request, u auth.User, asked *policy.Input) (*sessions.PolicySpec, bool) {
	if a.policies == nil {
		if asked != nil {
			// Not ignored: the session would be less restricted than asked.
			writeError(w, http.StatusConflict, "policies are not enabled here; create the session without one")
			return nil, false
		}
		return nil, true
	}
	return a.policies.ForCreate(w, r, u, asked)
}

// created follows a new session's policy into force (see policy.Watch).
func (a *API) created(s sessions.Session) {
	if a.policies != nil && s.PolicyCapable {
		go a.policies.Watch(context.Background(), s.ID)
	}
	if a.onCreated != nil {
		a.onCreated(s.ID)
	}
}

// summary is the policy a session's view carries: nil when policies are
// off, or when the cluster could not be asked (the session is still shown).
func (a *API) summary(ctx context.Context, s sessions.Session) *policy.Summary {
	if a.policies == nil {
		return nil
	}
	p, err := a.policies.Summary(ctx, s)
	if err != nil {
		slog.Error("policy not read", "session", s.ID, "err", err)
		return nil
	}
	return &p
}

// summaries is summary for a list of sessions: owner's, or everyone's when
// owner is "".
func (a *API) summaries(ctx context.Context, list []sessions.Session, owner string) map[string]*policy.Summary {
	if a.policies == nil {
		return nil
	}
	found, err := a.policies.Summaries(ctx, list, owner)
	if err != nil {
		slog.Error("policies not listed", "err", err)
		return nil
	}
	out := make(map[string]*policy.Summary, len(found))
	for id, p := range found {
		out[id] = &p
	}
	return out
}
