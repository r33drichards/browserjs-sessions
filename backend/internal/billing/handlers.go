package billing

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/auth"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
)

// Handlers is the read side of billing and the deletion of an account
// (backend-api.yaml): GET /api/billing, /api/billing/usage,
// /api/billing/catalogue and DELETE /api/account. Buying is the Stripe
// component's, which registers its own routes.
type Handlers struct {
	*Enforcer
	// Observer is the observer's sign of life, nil where it is not looked
	// at.
	Observer Observer
	// Stripe is nil while payments are off: deleting an account then has
	// nothing to cancel or detach.
	Stripe Stripe
	// RevokeTokens deletes every API token of an owner, nil where there
	// are none.
	RevokeTokens func(ctx context.Context, owner string) error
}

// Register adds the routes to the API's mux, behind auth.Middleware. With
// billing off there are none.
func (h *Handlers) Register(mux *http.ServeMux) {
	if h.cfg.Mode == Off {
		return
	}
	mux.HandleFunc("GET /api/billing", h.user(h.billing))
	mux.HandleFunc("GET /api/billing/usage", h.user(h.usage))
	mux.HandleFunc("GET /api/billing/catalogue", h.user(h.catalogueRoute))
	mux.HandleFunc("DELETE /api/account", h.user(h.deleteAccount))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, ErrorBody{Error: msg, Code: code})
}

func (h *Handlers) user(next func(http.ResponseWriter, *http.Request, auth.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := auth.UserFrom(r.Context())
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
			return
		}
		next(w, r, u)
	}
}

func (h *Handlers) unavailable(w http.ResponseWriter, what string, err error) {
	slog.Error("billing: "+what, "err", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cluster request failed"})
}

// BillingView is Billing of backend-api.yaml.
type BillingView struct {
	Mode              string            `json:"mode"`
	State             State             `json:"state"`
	Ledger            string            `json:"ledger"`
	HasPaymentMethod  bool              `json:"hasPaymentMethod"`
	SignupCredit      *SignupCreditView `json:"signupCredit,omitempty"`
	Plan              PlanView          `json:"plan"`
	Subscription      *SubscriptionView `json:"subscription,omitempty"`
	Level             string            `json:"level"`
	BalanceMicros     int64             `json:"balanceMicros"`
	Balances          []SourceBalance   `json:"balances,omitempty"`
	BurnMicrosPerHour int64             `json:"burnMicrosPerHour,omitempty"`
	Rates             PublicRates       `json:"rates"`
	ExhaustedAt       *time.Time        `json:"exhaustedAt,omitempty"`
	SleepAt           *time.Time        `json:"sleepAt,omitempty"`
	DeleteAt          *time.Time        `json:"deleteAt,omitempty"`
	Period            PeriodView        `json:"period"`
	Limits            PublicTier        `json:"limits"`
	AutoRecharge      *AutoRechargeView `json:"autoRecharge,omitempty"`
	Payments          string            `json:"payments"`
	HasCustomer       bool              `json:"hasCustomer"`
	TermsRequired     string            `json:"termsRequired,omitempty"`
}

type SignupCreditView struct {
	State        string `json:"state"`
	Reason       string `json:"reason,omitempty"`
	AmountMicros int64  `json:"amountMicros,omitempty"`
}

type PlanView struct {
	Key          string `json:"key"`
	Name         string `json:"name"`
	Amount       int64  `json:"amount,omitempty"`
	CreditMicros int64  `json:"creditMicros,omitempty"`
}

type SubscriptionView struct {
	Status    string     `json:"status"`
	RenewsAt  *time.Time `json:"renewsAt,omitempty"`
	CancelsAt *time.Time `json:"cancelsAt,omitempty"`
}

type PeriodView struct {
	Start            time.Time `json:"start"`
	End              time.Time `json:"end"`
	PlanCreditMicros int64     `json:"planCreditMicros,omitempty"`
	AwakeSeconds     int64     `json:"awakeSeconds"`
	AwakeMicros      int64     `json:"awakeMicros"`
	DiskMicros       int64     `json:"diskMicros"`
}

type AutoRechargeView struct {
	Available       bool   `json:"available"`
	Enabled         bool   `json:"enabled"`
	Pack            string `json:"pack,omitempty"`
	ThresholdMicros int64  `json:"thresholdMicros,omitempty"`
	MonthlyCapCents int64  `json:"monthlyCapCents,omitempty"`
	ChargedCents    int64  `json:"chargedCents,omitempty"`
	LastStatus      string `json:"lastStatus,omitempty"`
	DisabledReason  string `json:"disabledReason,omitempty"`
}

// month is the calendar month of t in UTC: the period usage is shown for,
// for everyone.
func month(t time.Time) (start, end time.Time) {
	t = t.UTC()
	start = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

// observerLate is how old the observer's Lease may be before the ledger
// is shown as stale.
const observerLate = 10 * time.Minute

// observerStale reports whether the observer has stopped sending usage.
func (h *Handlers) observerStale(ctx context.Context) bool {
	if h.Observer == nil {
		return false
	}
	renewed, err := h.Observer.Renewed(ctx)
	return err != nil || h.clock.Now().Sub(renewed) > observerLate
}

// level is the rule of metering.md: exhausted at zero; low at a fifth of
// the plan's credit or a dollar, whichever is more.
func level(b Balance) string {
	var allowance int64
	for _, c := range b.Credits {
		if c.Source == SourcePlan {
			allowance += c.AmountMicros
		}
	}
	switch {
	case b.NetMicros <= 0:
		return LevelExhausted
	case b.NetMicros*5 <= allowance || b.NetMicros <= 1_000_000:
		return LevelLow
	}
	return LevelOK
}

// bySource is a balance by the source of its credits, in the order they
// are used, each with its earliest end.
func bySource(b Balance) []SourceBalance {
	var out []SourceBalance
	at := map[string]int{}
	for _, c := range b.Credits {
		i, ok := at[c.Source]
		if !ok {
			i, at[c.Source] = len(out), len(out)
			out = append(out, SourceBalance{Source: c.Source})
		}
		out[i].Micros += c.RemainingMicros
		if c.RemainingMicros > 0 && c.ExpiresAt != nil && (out[i].ExpiresAt == nil || c.ExpiresAt.Before(*out[i].ExpiresAt)) {
			out[i].ExpiresAt = c.ExpiresAt
		}
	}
	return out
}

// viewOf is the account as GET /api/billing shows it. The balance and the
// usage are read from Metronome at the request; when it cannot be read
// the ledger is stale and the balance is the one last stored on the
// Account.
func (h *Handlers) viewOf(ctx context.Context, st standing) (BillingView, error) {
	cat := h.catalogue.Catalogue()
	spec := st.account.Spec
	now := h.clock.Now()
	v := BillingView{
		Mode:             string(h.cfg.Mode),
		State:            st.state,
		Ledger:           "ok",
		HasPaymentMethod: spec.PaymentMethod != nil && spec.PaymentMethod.Present,
		Rates:            cat.publicRates(),
		ExhaustedAt:      st.exhaustedAt(),
		Limits:           PublicTier{st.tier.MaxSessions, st.tier.MaxAwake},
		Payments:         h.cfg.Payments,
		HasCustomer:      spec.StripeCustomerID != "",
	}
	start, end := month(now)
	v.Period = PeriodView{Start: start, End: end}

	stored := func() {
		v.Level = LevelExhausted
		if c := spec.Credit; c != nil {
			v.BalanceMicros = c.BalanceMicros
			if !c.Exhausted {
				v.Level = level(Balance{NetMicros: c.BalanceMicros})
			}
		}
	}
	switch {
	case spec.MetronomeCustomerID == "":
		v.Ledger = "pending"
		stored()
	default:
		balance, err := h.ledger.Balance(ctx, st.account.Name)
		var used Usage
		if err == nil {
			used, err = h.ledger.Usage(ctx, st.account.Name, start, now)
		}
		if err != nil {
			slog.Warn("billing: Metronome not read; showing the stored balance", "account", st.account.Name, "err", err)
			v.Ledger = "stale"
			stored()
			break
		}
		v.BalanceMicros, v.Level, v.Balances = balance.NetMicros, level(balance), bySource(balance)
		v.Period.AwakeSeconds, v.Period.AwakeMicros, v.Period.DiskMicros = used.AwakeSeconds, used.AwakeMicros, used.DiskMicros
		for _, c := range balance.Credits {
			if c.Source == SourcePlan {
				v.Period.PlanCreditMicros += c.AmountMicros
			}
		}
		if h.observerStale(ctx) {
			v.Ledger = "stale"
		}
	}

	mine, err := h.sessions.List(ctx, spec.Owner)
	if err != nil {
		return v, err
	}
	for _, s := range mine {
		if s.State == sessions.Running {
			v.BurnMicrosPerHour += cat.Rates.AwakeMicrosPerHour
		}
		v.BurnMicrosPerHour += int64(cat.SessionDiskGB) * cat.Rates.DiskMicrosPerGBHour
	}

	v.Plan = PlanView{Key: PlanPayg, Name: cat.Payg.Name}
	if plan, ok := cat.Plan(PlanOf(spec, cat)); ok {
		v.Plan = PlanView{Key: plan.Key, Name: plan.Name, Amount: plan.Amount, CreditMicros: plan.CreditMicros}
	}
	if sub := spec.Subscription; sub != nil {
		v.Subscription = &SubscriptionView{Status: sub.Status, CancelsAt: sub.CancelAt}
		if sub.CancelAt == nil {
			v.Subscription.RenewsAt = sub.CurrentPeriodEnd
		}
	}
	switch sc := spec.SignupCredit; {
	case sc != nil:
		v.SignupCredit = &SignupCreditView{State: sc.State, Reason: sc.Reason}
		if sc.State == SignupGranted {
			v.SignupCredit.AmountMicros = cat.SignupCredit.AmountMicros
		}
	case h.cfg.SignupCredit:
		v.SignupCredit = &SignupCreditView{State: "pending", AmountMicros: cat.SignupCredit.AmountMicros}
	}
	ar := AutoRechargeView{Available: h.cfg.AutoRecharge}
	if a := spec.AutoRecharge; a != nil {
		ar.Enabled, ar.Pack, ar.ThresholdMicros, ar.MonthlyCapCents = a.Enabled, a.Pack, a.ThresholdMicros, a.MonthlyCapCents
		ar.DisabledReason = a.DisabledReason
		if a.Month == now.UTC().Format("2006-01") {
			ar.ChargedCents = a.ChargedCents
		}
		if a.Last != nil {
			ar.LastStatus = a.Last.Status
		}
	}
	v.AutoRecharge = &ar
	if st.state == StateTerms {
		v.TermsRequired = h.cfg.TermsVersion
	}
	if h.cfg.Mode == Enforce {
		v.DeleteAt = h.deleteAt(st)
		// Sessions still running on no credit sleep when the grace is over.
		if at := st.exhaustedAt(); at != nil && st.state != StateExempt && countAwake(mine) > 0 {
			sleepAt := at.Add(h.cfg.Grace)
			v.SleepAt = &sleepAt
		}
	}
	return v, nil
}

// billing is GET /api/billing: the caller's own account, made if there is
// none.
func (h *Handlers) billing(w http.ResponseWriter, r *http.Request, u auth.User) {
	st, err := h.standing(r.Context(), u.Subject)
	if err != nil {
		h.unavailable(w, "account not read", err)
		return
	}
	if st.state == StateBlocked {
		NewRefusal(CodeAccountBlocked, 0, h.cfg.BillingURL()).WriteHTTP(w)
		return
	}
	v, err := h.viewOf(r.Context(), st)
	if err != nil {
		h.unavailable(w, "sessions not listed", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// catalogueRoute is GET /api/billing/catalogue. It needs no Account.
func (h *Handlers) catalogueRoute(w http.ResponseWriter, _ *http.Request, _ auth.User) {
	writeJSON(w, http.StatusOK, h.catalogue.Catalogue().Public())
}

// UsageView is Usage of backend-api.yaml.
type UsageView struct {
	Start        time.Time          `json:"start"`
	End          time.Time          `json:"end"`
	Plan         string             `json:"plan,omitempty"`
	AwakeSeconds int64              `json:"awakeSeconds"`
	AwakeMicros  int64              `json:"awakeMicros"`
	DiskMicros   int64              `json:"diskMicros"`
	Days         []DayUsage         `json:"days"`
	Sessions     []SessionUsageView `json:"sessions"`
	Periods      []time.Time        `json:"periods"`
}

type SessionUsageView struct {
	SessionUsage
	Name string `json:"name,omitempty"` // absent for a deleted session
}

// How many past months can be asked for.
const usageMonths = 12

// usage is GET /api/billing/usage: the current calendar month, or an
// earlier one named by its start. It is read from Metronome at the
// request.
func (h *Handlers) usage(w http.ResponseWriter, r *http.Request, u auth.User) {
	ctx := r.Context()
	st, err := h.standing(ctx, u.Subject)
	if err != nil {
		h.unavailable(w, "account not read", err)
		return
	}
	now := h.clock.Now()
	start, end := month(now)
	// The closed periods: the months since the account was made.
	periods := []time.Time{}
	first, _ := month(st.account.Created)
	for m := start.AddDate(0, -1, 0); !m.Before(first) && len(periods) < usageMonths; m = m.AddDate(0, -1, 0) {
		periods = append(periods, m)
	}
	to := now
	if asked := r.URL.Query().Get("period"); asked != "" {
		at, err := time.Parse(time.RFC3339, asked)
		found := false
		for _, p := range periods {
			if err == nil && p.Equal(at) {
				start, end, found = p, p.AddDate(0, 1, 0), true
				to = end
			}
		}
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such period"})
			return
		}
	}
	out := UsageView{Start: start, End: end, Plan: PlanOf(st.account.Spec, h.catalogue.Catalogue()),
		Days: []DayUsage{}, Sessions: []SessionUsageView{}, Periods: periods}
	if st.account.Spec.MetronomeCustomerID != "" {
		used, err := h.ledger.Usage(ctx, st.account.Name, start, to)
		if err != nil {
			// Metronome cannot be read: the usage is empty, as the billing
			// page says with "stale".
			slog.Warn("billing: usage not read from Metronome", "account", st.account.Name, "err", err)
		} else {
			names := map[string]string{}
			mine, err := h.sessions.List(ctx, u.Subject)
			if err != nil {
				h.unavailable(w, "sessions not listed", err)
				return
			}
			for _, s := range mine {
				names[s.ID] = s.Name
			}
			out.AwakeSeconds, out.AwakeMicros, out.DiskMicros = used.AwakeSeconds, used.AwakeMicros, used.DiskMicros
			if used.Days != nil {
				out.Days = used.Days
			}
			for _, s := range used.Sessions {
				out.Sessions = append(out.Sessions, SessionUsageView{SessionUsage: s, Name: names[s.ID]})
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// deleteAccount is DELETE /api/account: everything the caller has, gone.
// Stripe first, so that a failure there deletes nothing; then the
// sessions, the tokens and the credit; the Account stays, marked deleted,
// with the record that its card has had the sign-up credit.
func (h *Handlers) deleteAccount(w http.ResponseWriter, r *http.Request, u auth.User) {
	ctx := r.Context()
	if u.Token != nil {
		writeError(w, http.StatusForbidden, CodeUIOnly, "Deleting the account is done in the app, not with an API token.")
		return
	}
	var body struct {
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil ||
		!strings.EqualFold(strings.TrimSpace(body.Confirm), u.Subject) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "confirm must be your email address"})
		return
	}
	acc, err := h.accounts.Ensure(ctx, u.Subject)
	if err != nil {
		h.unavailable(w, "account not read", err)
		return
	}
	if acc.Spec.StripeCustomerID != "" {
		if h.Stripe == nil {
			// The account has a customer but this backend cannot reach
			// Stripe: a subscription would go on being charged.
			writeError(w, http.StatusBadGateway, CodeStripeUnavailable, "Payments are unavailable right now. Nothing was deleted.")
			return
		}
		if err := h.leaveStripe(ctx, acc); err != nil {
			slog.Error("billing: account deletion stopped at Stripe", "account", acc.Name, "err", err)
			writeError(w, http.StatusBadGateway, CodeStripeUnavailable, "Payments are unavailable right now. Nothing was deleted.")
			return
		}
	}
	mine, err := h.sessions.List(ctx, u.Subject)
	if err != nil {
		h.unavailable(w, "sessions not listed", err)
		return
	}
	for _, s := range mine {
		if err := h.sessions.Delete(ctx, s.ID); err != nil && !errors.Is(err, sessions.ErrNotFound) {
			h.unavailable(w, "session not deleted", err)
			return
		}
	}
	if h.RevokeTokens != nil {
		if err := h.RevokeTokens(ctx, u.Subject); err != nil {
			h.unavailable(w, "tokens not deleted", err)
			return
		}
	}
	if err := h.ledger.Revoke(ctx, GrantSelector{Account: acc.Name}, "account-deleted"); err != nil {
		h.unavailable(w, "grants not revoked", err)
		return
	}
	now := h.clock.Now()
	_, err = h.accounts.Update(ctx, acc.Name, func(spec *AccountSpec) error {
		// owner, ownerHash and signupCredit are kept, and the two customer
		// IDs, which the CRD does not let go once set (Stripe keeps the
		// invoices, Metronome the usage), with the credit that is now none.
		*spec = AccountSpec{Owner: spec.Owner, OwnerHash: spec.OwnerHash, StripeCustomerID: spec.StripeCustomerID,
			MetronomeCustomerID: spec.MetronomeCustomerID, Credit: spec.Credit, SignupCredit: spec.SignupCredit, DeletedAt: &now}
		return nil
	})
	if err != nil {
		h.unavailable(w, "account not marked deleted", err)
		return
	}
	slog.Warn("billing: account deleted by its owner", "account", acc.Name, "sessions", len(mine))
	w.WriteHeader(http.StatusNoContent)
}

// leaveStripe cancels the account's subscription at once and detaches
// every saved card. The Customer is kept.
func (h *Handlers) leaveStripe(ctx context.Context, acc Account) error {
	subs, err := h.Stripe.Subscriptions(ctx, acc.Spec.StripeCustomerID)
	if err != nil {
		return err
	}
	for _, sub := range subs {
		if sub.Status == "canceled" || sub.Status == "incomplete_expired" {
			continue
		}
		if err := h.Stripe.CancelSubscription(ctx, sub.ID); err != nil {
			return err
		}
	}
	methods, _, err := h.Stripe.PaymentMethods(ctx, acc.Spec.StripeCustomerID)
	if err != nil {
		return err
	}
	for _, pm := range methods {
		if err := h.Stripe.DetachPaymentMethod(ctx, pm.ID); err != nil {
			return err
		}
	}
	return nil
}
