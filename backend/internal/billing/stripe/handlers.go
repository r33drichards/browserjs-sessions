package stripe

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/billing"
)

// The limits on saving cards (enforcement.md): the setup Checkout checks a
// card for nothing, which is what card testers want.
const (
	maxSavedCards        = 5
	maxSetupCheckoutsDay = 5
)

var checkoutIDPattern = regexp.MustCompile(`^cs_[A-Za-z0-9_]+$`)

// Paths lists the paths Register serves, for a caller that routes to them.
// A path ending in "/" is a prefix.
var Paths = []string{
	"/api/billing/checkout", "/api/billing/checkout/", "/api/billing/portal", "/api/billing/auto-recharge",
}

// Register adds this package's routes to the app's API mux, whose requests
// carry a signed-in user (auth.Middleware). The webhook is not one of them:
// see Webhook.
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/billing/checkout", s.person(s.checkout))
	mux.HandleFunc("GET /api/billing/checkout/{id}", s.person(s.checkoutState))
	mux.HandleFunc("POST /api/billing/portal", s.person(s.portal))
	mux.HandleFunc("PUT /api/billing/auto-recharge", s.person(s.autoRecharge))
}

// PaymentsOff serves the same routes while billing is on and Stripe is not
// configured: nothing can be bought, and the UI is told why.
func PaymentsOff(publicURL string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, publicURL, http.StatusServiceUnavailable, CodePaymentsOff, "Payments are not turned on.")
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, publicURL string, status int, code, msg string) {
	writeJSON(w, status, billing.ErrorBody{Error: msg, Code: code, BillingURL: publicURL + "/billing"})
}

func (s *Service) fail(w http.ResponseWriter, status int, code, msg string) {
	writeError(w, s.opt.PublicURL, status, code, msg)
}

// unavailable answers for a call to Stripe, or a write to the cluster, that
// did not work.
func (s *Service) unavailable(w http.ResponseWriter, what string, err error) {
	slog.Error("stripe: "+what, "err", err)
	s.fail(w, http.StatusBadGateway, billing.CodeStripeUnavailable, "The payment provider could not be reached. Nothing was charged. Try again in a moment.")
}

// person lets in a person at the UI, with their Account: buying is not for
// an API token, and not for an account that is blocked.
func (s *Service) person(next func(http.ResponseWriter, *http.Request, billing.Account)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := auth.UserFrom(r.Context())
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
			return
		}
		if u.Token != nil {
			s.fail(w, http.StatusForbidden, billing.CodeUIOnly, "This is done in the app, not with an API token.")
			return
		}
		acct, err := s.accounts.Ensure(r.Context(), u.Subject)
		if err != nil {
			slog.Error("stripe: account", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if acct.Spec.Blocked != nil || acct.Spec.DeletedAt != nil {
			billing.NewRefusal(billing.CodeAccountBlocked, 0, s.opt.PublicURL+"/billing").WriteHTTP(w)
			return
		}
		next(w, r, acct)
	}
}

// body reads a small JSON request body into v. An empty body is "{}".
func body(w http.ResponseWriter, r *http.Request, v any) bool {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err == nil && len(data) > 0 {
		err = json.Unmarshal(data, v)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the body must be a JSON object"})
		return false
	}
	return true
}

// checkout starts a Stripe Checkout: with no item, one that saves a card
// and charges nothing; with a plan's or a pack's lookup key, one that buys
// it.
func (s *Service) checkout(w http.ResponseWriter, r *http.Request, acct billing.Account) {
	ctx := r.Context()
	var req struct {
		Item string `json:"item"`
	}
	if !body(w, r, &req) {
		return
	}
	cat := s.opt.Catalogue.Catalogue()
	p := billing.CheckoutParams{
		Account:    acct.Name,
		SuccessURL: s.opt.PublicURL + "/billing?checkout={CHECKOUT_SESSION_ID}",
		CancelURL:  s.opt.PublicURL + "/billing",
	}
	if req.Item == "" {
		p.Mode, p.Kind = billing.ModeSetup, billing.KindSetup
		if pm := acct.Spec.PaymentMethod; pm != nil && len(pm.IDs) >= maxSavedCards {
			s.fail(w, http.StatusConflict, CodeTooManyCards, "You can save at most 5 cards. Remove one in the billing portal first.")
			return
		}
	} else {
		plan, isPlan := planOf(cat, req.Item)
		pack, isPack := packOf(cat, req.Item)
		price, priced := s.price(req.Item)
		if !(isPlan && plan.Enabled) && !(isPack && pack.Enabled) || !priced {
			s.fail(w, http.StatusBadRequest, CodeUnknownItem, "That plan or credit pack is not available.")
			return
		}
		p.Mode, p.Kind, p.Item, p.Price = billing.ModePayment, billing.KindPurchase, req.Item, price
		if isPlan {
			p.Mode, p.Kind = billing.ModeSubscription, billing.KindPlan
			if sub := acct.Spec.Subscription; sub != nil && subscribed(sub.Status) {
				s.fail(w, http.StatusConflict, CodeAlreadySubscribed, "You already have a subscription. A plan cannot be changed in place yet: cancel it in the billing portal, and choose another when it ends. Credit packs can be bought at any time.")
				return
			}
		}
	}
	// The customer is made, and written to the Account, before anything
	// else is made for it.
	if acct.Spec.StripeCustomerID == "" {
		id, err := s.stripe.CreateCustomer(ctx, billing.CustomerParams{Email: acct.Spec.Owner, Account: acct.Name, OwnerHash: acct.Spec.OwnerHash})
		if err != nil {
			s.unavailable(w, "create customer", err)
			return
		}
		if acct, err = s.accounts.Update(ctx, acct.Name, func(spec *billing.AccountSpec) error {
			if spec.StripeCustomerID == "" {
				spec.StripeCustomerID = id
			}
			return nil
		}); err != nil {
			s.unavailable(w, "write customer", err)
			return
		}
	} else if p.Mode == billing.ModeSetup {
		recent, err := s.stripe.Checkouts(ctx, acct.Spec.StripeCustomerID, s.clock.Now().Add(-24*time.Hour))
		if err != nil {
			s.unavailable(w, "list checkouts", err)
			return
		}
		n := 0
		for _, cs := range recent {
			if cs.Mode == billing.ModeSetup {
				n++
			}
		}
		if n >= maxSetupCheckoutsDay {
			w.Header().Set("Retry-After", "3600")
			s.fail(w, http.StatusTooManyRequests, billing.CodeRateLimited, "Too many attempts to add a card today. Try again tomorrow.")
			return
		}
	}
	p.Customer = acct.Spec.StripeCustomerID
	cs, err := s.stripe.CreateCheckout(ctx, p)
	if err != nil {
		s.unavailable(w, "create checkout", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": cs.URL})
}

type signupCreditView struct {
	State        string `json:"state"` // pending, granted, refused
	Reason       string `json:"reason,omitempty"`
	AmountMicros int64  `json:"amountMicros,omitempty"`
}

type checkoutStateView struct {
	Status       string            `json:"status"` // open, complete, expired
	Kind         string            `json:"kind,omitempty"`
	SignupCredit *signupCreditView `json:"signupCredit,omitempty"`
	Item         string            `json:"item,omitempty"`
}

// checkoutState says what became of a Checkout the caller started, and
// fulfils it: webhooks can be late, so the page the user lands on does what
// the webhook would.
func (s *Service) checkoutState(w http.ResponseWriter, r *http.Request, acct billing.Account) {
	ctx := r.Context()
	id := r.PathValue("id")
	notFound := func() { writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"}) }
	if !checkoutIDPattern.MatchString(id) {
		notFound()
		return
	}
	cs, err := s.stripe.Checkout(ctx, id)
	if errors.Is(err, billing.ErrNotFound) || (err == nil && cs.ClientReferenceID != acct.Name) {
		notFound()
		return
	}
	if err != nil {
		s.unavailable(w, "read checkout", err)
		return
	}
	if cs, err = s.EnsurePurchase(ctx, id); err != nil {
		s.unavailable(w, "fulfil checkout", err)
		return
	}
	view := checkoutStateView{Status: cs.Status, Kind: cs.Metadata["kind"], Item: cs.Metadata["item"]}
	// Complete means fulfilled: a payment that is not made yet is still open.
	if cs.Mode == billing.ModePayment && cs.Status == "complete" && cs.PaymentStatus != "paid" {
		view.Status = "open"
	}
	if cs.Mode == billing.ModeSetup {
		view.SignupCredit = &signupCreditView{State: "pending"}
		if acct, err = s.accounts.Get(ctx, acct.Name); err == nil && acct.Spec.SignupCredit != nil {
			sc := acct.Spec.SignupCredit
			view.SignupCredit = &signupCreditView{State: sc.State, Reason: sc.Reason}
			if sc.State == billing.SignupGranted {
				view.SignupCredit.AmountMicros = s.opt.Catalogue.Catalogue().SignupCredit.AmountMicros
			}
		}
	}
	writeJSON(w, http.StatusOK, view)
}

// portal opens Stripe's Customer Portal for the caller.
func (s *Service) portal(w http.ResponseWriter, r *http.Request, acct billing.Account) {
	if acct.Spec.StripeCustomerID == "" {
		s.fail(w, http.StatusConflict, CodeNoCustomer, "There is nothing to manage yet: add a payment method first.")
		return
	}
	url, err := s.stripe.CreatePortal(r.Context(), acct.Spec.StripeCustomerID, s.opt.PublicURL+"/billing")
	if err != nil {
		s.unavailable(w, "create portal session", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": url})
}

type autoRechargeView struct {
	Available       bool   `json:"available"`
	Enabled         bool   `json:"enabled"`
	Pack            string `json:"pack,omitempty"`
	ThresholdMicros int64  `json:"thresholdMicros,omitempty"`
	MonthlyCapCents int64  `json:"monthlyCapCents,omitempty"`
	ChargedCents    int64  `json:"chargedCents"`
	LastStatus      string `json:"lastStatus,omitempty"`
	DisabledReason  string `json:"disabledReason,omitempty"`
}

// AutoRechargeView is the account's auto-recharge as GET /api/billing and
// PUT /api/billing/auto-recharge show it.
func (s *Service) AutoRechargeView(spec billing.AccountSpec) any {
	return s.autoRechargeView(spec)
}

func (s *Service) autoRechargeView(spec billing.AccountSpec) autoRechargeView {
	v := autoRechargeView{Available: s.opt.AutoRecharge}
	if a := spec.AutoRecharge; a != nil {
		v.Enabled, v.Pack, v.ThresholdMicros, v.MonthlyCapCents = a.Enabled, a.Pack, a.ThresholdMicros, a.MonthlyCapCents
		v.DisabledReason = a.DisabledReason
		if a.Month == month(s.clock.Now()) {
			v.ChargedCents = a.ChargedCents
		}
		if a.Last != nil {
			v.LastStatus = a.Last.Status
		}
	}
	return v
}

// autoRecharge turns the caller's automatic top-up on or off and sets its
// pack, threshold and monthly cap. Turning it on is agreeing to be charged
// with nobody present, and needs a saved card.
func (s *Service) autoRecharge(w http.ResponseWriter, r *http.Request, acct billing.Account) {
	if !s.opt.AutoRecharge {
		s.fail(w, http.StatusNotFound, CodeAutoRechargeOff, "Automatic top-up is not available.")
		return
	}
	var req struct {
		Enabled         *bool  `json:"enabled"`
		Pack            string `json:"pack"`
		ThresholdMicros *int64 `json:"thresholdMicros"`
		MonthlyCapCents *int64 `json:"monthlyCapCents"`
		Agree           bool   `json:"agree"`
	}
	if !body(w, r, &req) {
		return
	}
	bad := func(msg string) { s.fail(w, http.StatusBadRequest, CodeInvalidRequest, msg) }
	if req.Enabled == nil {
		bad("enabled is required.")
		return
	}
	cat := s.opt.Catalogue.Catalogue()
	// What is not sent stays as it was, or is the catalogue's default.
	next := billing.AutoRecharge{
		Pack:            cat.AutoRecharge.DefaultPack,
		ThresholdMicros: cat.AutoRecharge.DefaultThresholdMicros,
		MonthlyCapCents: cat.AutoRecharge.DefaultMonthlyCapCents,
	}
	if a := acct.Spec.AutoRecharge; a != nil && a.Pack != "" {
		next.Pack, next.ThresholdMicros, next.MonthlyCapCents = a.Pack, a.ThresholdMicros, a.MonthlyCapCents
	}
	if req.Pack != "" {
		next.Pack = req.Pack
	}
	if req.ThresholdMicros != nil {
		next.ThresholdMicros = *req.ThresholdMicros
	}
	if req.MonthlyCapCents != nil {
		next.MonthlyCapCents = *req.MonthlyCapCents
	}
	pack, ok := packByKey(cat, next.Pack)
	switch {
	case !ok || !pack.Enabled:
		s.fail(w, http.StatusBadRequest, CodeUnknownItem, "That credit pack is not available.")
		return
	case next.ThresholdMicros < 0 || next.MonthlyCapCents < 0:
		bad("The threshold and the monthly cap cannot be negative.")
		return
	case next.MonthlyCapCents > cat.AutoRecharge.MaxMonthlyCapCents:
		bad("The monthly cap is above the most that is allowed.")
		return
	case *req.Enabled && next.MonthlyCapCents < pack.Amount:
		bad("The monthly cap is less than one top-up.")
		return
	case *req.Enabled && !req.Agree:
		bad("Turning automatic top-up on requires agreeing to it.")
		return
	case *req.Enabled && (acct.Spec.PaymentMethod == nil || !acct.Spec.PaymentMethod.Present):
		s.fail(w, http.StatusPaymentRequired, billing.CodePaymentMethodRequired, "Add a payment method to turn automatic top-up on.")
		return
	}
	now := s.clock.Now()
	acct, err := s.accounts.Update(r.Context(), acct.Name, func(spec *billing.AccountSpec) error {
		a := spec.AutoRecharge
		if a == nil {
			a = &billing.AutoRecharge{}
			spec.AutoRecharge = a
		}
		a.Pack, a.ThresholdMicros, a.MonthlyCapCents = next.Pack, next.ThresholdMicros, next.MonthlyCapCents
		a.Enabled, a.DisabledReason = *req.Enabled, ""
		if *req.Enabled {
			a.AgreedAt = &now
		}
		return nil
	})
	if err != nil {
		slog.Error("stripe: write auto-recharge", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, s.autoRechargeView(acct.Spec))
}
