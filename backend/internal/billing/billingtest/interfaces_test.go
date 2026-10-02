package billingtest_test

import (
	"github.com/r33drichards/browserjs-sessions/backend/internal/api"
	"github.com/r33drichards/browserjs-sessions/backend/internal/authz"
	"github.com/r33drichards/browserjs-sessions/backend/internal/billing"
	"github.com/r33drichards/browserjs-sessions/backend/internal/billing/billingtest"
	"github.com/r33drichards/browserjs-sessions/backend/internal/idle"
	"github.com/r33drichards/browserjs-sessions/backend/internal/proxy"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

// The fakes are every interface the real things sit behind, and the real
// session store is each of the interfaces its consumers declare.
var (
	_ billing.Clock     = (*billingtest.Clock)(nil)
	_ billing.Accounts  = (*billingtest.Accounts)(nil)
	_ billing.Metronome = (*billingtest.Metronome)(nil)
	_ billing.Stripe    = (*billingtest.Stripe)(nil)
	_ billing.Sessions  = (*billingtest.Sessions)(nil)
	_ billing.InFlight  = (*billingtest.InFlight)(nil)

	_ api.Store   = (*billingtest.Sessions)(nil)
	_ proxy.Store = (*billingtest.Sessions)(nil)
	_ idle.Store  = (*billingtest.Sessions)(nil)
	_ authz.Store = (*billingtest.Sessions)(nil)

	_ api.Store        = (*sessions.Store)(nil)
	_ proxy.Store      = (*sessions.Store)(nil)
	_ idle.Store       = (*sessions.Store)(nil)
	_ billing.Sessions = billing.Store{}
	_ billing.InFlight = (*proxy.Proxy)(nil)
)
