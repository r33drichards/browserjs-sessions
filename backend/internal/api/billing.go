package api

import (
	"context"
	"net/http"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/billing"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

// Metering and billing (internal/billing). It is off until EnableBilling is
// called, and while it is off the API is what it was before: nothing is
// refused for an account's sake, and a session shows nothing of it.

// Billing is what the API asks of billing (a *billing.Enforcer).
type Billing interface {
	// Enforcing reports whether refusals are made. The plan's limit on
	// sessions then replaces the API's own cap.
	Enforcing() bool
	// Create judges a new session for owner, who has the sessions mine;
	// Start judges making s awake (a resume). Each answers nil, a
	// *billing.Refusal, or the error that kept it from deciding.
	Create(ctx context.Context, owner string, mine []sessions.Session) error
	Start(ctx context.Context, s sessions.Session) error
	// View is what a session's view carries of billing.
	View(ctx context.Context, s sessions.Session) billing.SessionView
}

// EnableBilling puts the checks before create and resume, and billing's
// fields on each session. Call it before Register.
func (a *API) EnableBilling(b Billing) { a.billing = b }

func (a *API) enforcing() bool { return a.billing != nil && a.billing.Enforcing() }

func (a *API) mayCreate(ctx context.Context, owner string, mine []sessions.Session) error {
	if a.billing == nil {
		return nil
	}
	return a.billing.Create(ctx, owner, mine)
}

// mayResume judges a resume of a session that is not awake. The account is
// the session's owner's, whoever asks: an admin resuming a user's session
// spends the user's credit.
func (a *API) mayResume(ctx context.Context, id, action string) error {
	if a.billing == nil || action != sessions.ActionResume {
		return nil
	}
	s, err := a.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if s.State == sessions.Running || s.State == sessions.Starting {
		return nil
	}
	return a.billing.Start(ctx, s)
}

// refused answers a create or resume that billing did not let through.
func (a *API) refused(w http.ResponseWriter, err error) {
	if ref, ok := billing.AsRefusal(err); ok {
		ref.WriteHTTP(w)
		return
	}
	a.storeError(w, err)
}

// billed is billing's part of a session's view (backend-api.yaml).
type billed struct {
	// Why an asleep or stopped session is: user, idle, credit,
	// payment-method, blocked.
	StoppedBy string `json:"stoppedBy,omitempty"`
	// The same reasons, while it finishes its calls before such a sleep.
	Draining string `json:"draining,omitempty"`
	// When it will be deleted for its account being at zero.
	DeleteAfter *time.Time `json:"deleteAfter,omitempty"`
}

func (a *API) billed(ctx context.Context, s sessions.Session) billed {
	if a.billing == nil {
		return billed{}
	}
	v := a.billing.View(ctx, s)
	return billed{StoppedBy: v.StoppedBy, Draining: v.Draining, DeleteAfter: v.DeleteAfter}
}
