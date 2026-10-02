package billingtest

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/billing"
)

// StripeStub stands where track C's package (backend/internal/billing/stripe)
// will: the checkout route and the webhook handler, with the ensure
// functions of docs/contracts/billing/stripe.md behind them, written
// against the same interfaces and with the same signatures. It exists so
// that the card-gate scenario can post correctly signed events to a
// handler before C's is merged.
//
// DELETE THIS FILE when track C merges: the scenarios then use C's handler
// and must pass unchanged. It covers what the scenarios need (saving and
// removing a card, the sign-up credit, buying a pack) and nothing else of
// the event table.
type StripeStub struct {
	Accounts  billing.Accounts
	Ledger    billing.Ledger
	Stripe    billing.Stripe
	Clock     billing.Clock
	Catalogue billing.CatalogueSource
	// Secret is the webhook's signing secret. Livemode is STRIPE_MODE=live.
	Secret   string
	Livemode bool
	// PublicURL is the app's base URL. SignupCredit is SIGNUP_CREDIT.
	PublicURL    string
	SignupCredit bool
}

// Register adds POST /api/billing/checkout to the API's mux.
func (s *StripeStub) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/billing/checkout", s.checkout)
}

func stubError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(billing.ErrorBody{Error: msg, Code: code})
}

// checkout starts a Checkout: in setup mode with no item, in payment mode
// for a pack.
func (s *StripeStub) checkout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, ok := auth.UserFrom(ctx)
	if !ok {
		stubError(w, http.StatusUnauthorized, "", "not signed in")
		return
	}
	if u.Token != nil {
		stubError(w, http.StatusForbidden, billing.CodeUIOnly, "Buying is done in the app, not with an API token.")
		return
	}
	var body struct {
		Item string `json:"item"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		stubError(w, http.StatusBadRequest, "", "body must be JSON")
		return
	}
	acc, err := s.Accounts.Ensure(ctx, u.Subject)
	if err != nil {
		stubError(w, http.StatusInternalServerError, "", "cluster request failed")
		return
	}
	if acc.Spec.Blocked != nil || acc.Spec.DeletedAt != nil {
		billing.NewRefusal(billing.CodeAccountBlocked, 0, s.PublicURL+"/billing").WriteHTTP(w)
		return
	}
	params := billing.CheckoutParams{Mode: billing.ModeSetup, Kind: billing.KindSetup, Account: acc.Name,
		SuccessURL: s.PublicURL + "/billing?checkout={CHECKOUT_SESSION_ID}", CancelURL: s.PublicURL + "/billing"}
	if body.Item != "" {
		pack, ok := s.Catalogue.Catalogue().Pack(body.Item)
		if !ok || !pack.Enabled {
			stubError(w, http.StatusBadRequest, "unknown_item", "No such item.")
			return
		}
		prices, err := s.Stripe.Prices(ctx, []string{pack.LookupKey})
		if err != nil {
			stubError(w, http.StatusBadGateway, billing.CodeStripeUnavailable, "Payments are unavailable right now. Nothing was charged.")
			return
		}
		params.Mode, params.Kind, params.Item, params.Price = billing.ModePayment, billing.KindPurchase, pack.LookupKey, prices[pack.LookupKey]
	}
	// The customer ID is written to the Account before anything else is
	// made.
	if acc.Spec.StripeCustomerID == "" {
		id, err := s.Stripe.CreateCustomer(ctx, billing.CustomerParams{Email: acc.Spec.Owner, Account: acc.Name, OwnerHash: acc.Spec.OwnerHash})
		if err != nil {
			stubError(w, http.StatusBadGateway, billing.CodeStripeUnavailable, "Payments are unavailable right now. Nothing was charged.")
			return
		}
		if acc, err = s.Accounts.Update(ctx, acc.Name, func(spec *billing.AccountSpec) error {
			if spec.StripeCustomerID == "" {
				spec.StripeCustomerID = id
			}
			return nil
		}); err != nil {
			stubError(w, http.StatusInternalServerError, "", "cluster request failed")
			return
		}
	}
	params.Customer = acc.Spec.StripeCustomerID
	cs, err := s.Stripe.CreateCheckout(ctx, params)
	if err != nil {
		stubError(w, http.StatusBadGateway, billing.CodeStripeUnavailable, "Payments are unavailable right now. Nothing was charged.")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"url": cs.URL})
}

// webhookTolerance is how old a signed event may be.
const webhookTolerance = 5 * time.Minute

// verified checks a Stripe-Signature header against the body.
func (s *StripeStub) verified(header string, body []byte) bool {
	var stamp string
	var signatures []string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			stamp = v
		case "v1":
			signatures = append(signatures, v)
		}
	}
	seconds, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return false
	}
	if age := s.Clock.Now().Sub(time.Unix(seconds, 0)); age > webhookTolerance || age < -webhookTolerance {
		return false
	}
	mac := hmac.New(sha256.New, []byte(s.Secret))
	mac.Write([]byte(stamp + "."))
	mac.Write(body)
	want := mac.Sum(nil)
	for _, sig := range signatures {
		if got, err := hex.DecodeString(sig); err == nil && hmac.Equal(got, want) {
			return true
		}
	}
	return false
}

// Webhook is POST /stripe/webhook: the raw body, the signature, the mode,
// then the event.
func (s *StripeStub) Webhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil || !s.verified(r.Header.Get("Stripe-Signature"), body) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var event struct {
		Type     string `json:"type"`
		Livemode bool   `json:"livemode"`
		Data     struct {
			Object struct {
				ID       string  `json:"id"`
				Customer *string `json:"customer"`
			} `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &event); err != nil || event.Livemode != s.Livemode {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ctx, object := r.Context(), event.Data.Object
	switch event.Type {
	case "checkout.session.completed", "checkout.session.async_payment_succeeded":
		err = s.EnsurePurchase(ctx, object.ID)
	case "payment_method.attached", "payment_method.updated", "payment_method.automatically_updated":
		if object.Customer != nil {
			err = s.EnsurePaymentMethods(ctx, *object.Customer)
		}
	case "payment_method.detached":
		// The object names no customer: its account is the one whose list
		// of payment methods has it.
		var acc billing.Account
		if acc, err = s.Accounts.ByPaymentMethod(ctx, object.ID); err == nil {
			err = s.EnsurePaymentMethods(ctx, acc.Spec.StripeCustomerID)
		} else if errors.Is(err, billing.ErrNotFound) {
			err = nil // the reconcile covers it
		}
	}
	if err != nil {
		slog.Error("stripe stub: event not handled", "type", event.Type, "err", err)
		w.WriteHeader(http.StatusInternalServerError) // Stripe sends it again
		return
	}
	w.WriteHeader(http.StatusOK)
}

// EnsurePaymentMethods is ensurePaymentMethods of stripe.md: the Account's
// paymentMethod written from a fresh list, and the sign-up credit decided
// once, when the first card is saved.
func (s *StripeStub) EnsurePaymentMethods(ctx context.Context, customerID string) error {
	acc, err := s.Accounts.ByCustomer(ctx, customerID)
	if errors.Is(err, billing.ErrNotFound) {
		slog.Info("stripe stub: no account has this customer", "customer", customerID)
		return nil
	}
	if err != nil {
		return err
	}
	methods, def, err := s.Stripe.PaymentMethods(ctx, customerID)
	if err != nil {
		return err
	}
	now := s.Clock.Now()
	var decided *billing.SignupCredit
	if len(methods) > 0 && acc.Spec.SignupCredit == nil && s.SignupCredit {
		if decided, err = s.decideSignupCredit(ctx, acc, methods[0], now); err != nil {
			return err
		}
	}
	_, err = s.Accounts.Update(ctx, acc.Name, func(spec *billing.AccountSpec) error {
		was := spec.PaymentMethod != nil && spec.PaymentMethod.Present
		next := billing.PaymentMethods{Present: len(methods) > 0, Default: def, ReadAt: now}
		for _, pm := range methods {
			next.IDs = append(next.IDs, pm.ID)
		}
		switch {
		case next.Present:
		case was:
			next.RemovedAt = &now
		case spec.PaymentMethod != nil:
			next.RemovedAt = spec.PaymentMethod.RemovedAt
		}
		spec.PaymentMethod = &next
		if spec.SignupCredit == nil && decided != nil {
			spec.SignupCredit = decided
		}
		return nil
	})
	return err
}

// decideSignupCredit is decideSignupCredit of stripe.md, on the oldest
// card.
func (s *StripeStub) decideSignupCredit(ctx context.Context, acc billing.Account, card billing.PaymentMethod, now time.Time) (*billing.SignupCredit, error) {
	offer := s.Catalogue.Catalogue().SignupCredit
	refused := func(reason string) (*billing.SignupCredit, error) {
		return &billing.SignupCredit{State: billing.SignupRefused, Reason: reason, At: now}, nil
	}
	if card.Fingerprint == "" {
		return refused(billing.RefusedNoFingerprint)
	}
	for _, funding := range offer.RefuseFunding {
		if card.Funding == funding {
			return refused(billing.RefusedPrepaid)
		}
	}
	if card.Wallet != "" && offer.RefuseWallets {
		return refused(billing.RefusedWallet)
	}
	expires := now.AddDate(0, 0, offer.ValidDays)
	_, existing, err := s.Ledger.EnsureGrant(ctx, billing.Grant{
		Account: acc.Name, Source: billing.SourceSignup, AmountMicros: offer.AmountMicros,
		ValidFrom: now, ExpiresAt: &expires, Key: "signup/" + card.Fingerprint,
	})
	if err != nil {
		return nil, err
	}
	// Created now, or there already for this account (a replay): granted.
	// A key another account used comes back with no account.
	if existing.Account != acc.Name {
		return refused(billing.RefusedCardUsed)
	}
	return &billing.SignupCredit{State: billing.SignupGranted, At: now}, nil
}

// EnsurePurchase is ensurePurchase of stripe.md, for the setup and
// payment modes.
func (s *StripeStub) EnsurePurchase(ctx context.Context, checkoutID string) error {
	cs, err := s.Stripe.Checkout(ctx, checkoutID)
	if err != nil {
		return err
	}
	switch {
	case cs.Mode == billing.ModeSetup && cs.Status == "complete":
		return s.EnsurePaymentMethods(ctx, cs.Customer)
	case cs.Mode != billing.ModePayment || cs.Status != "complete" || cs.PaymentStatus != "paid":
		return nil
	}
	acc, err := s.Accounts.Get(ctx, cs.ClientReferenceID)
	if err != nil || cs.Metadata["account"] != acc.Name || acc.Spec.StripeCustomerID != cs.Customer {
		slog.Error("stripe stub: a checkout that is not its account's", "checkout", cs.ID)
		return nil
	}
	pack, ok := s.Catalogue.Catalogue().Pack(cs.Metadata["item"])
	if !ok {
		slog.Error("stripe stub: a checkout for an item the catalogue does not have", "checkout", cs.ID)
		return nil
	}
	now := s.Clock.Now()
	expires := now.AddDate(0, 0, pack.ValidDays)
	if _, _, err := s.Ledger.EnsureGrant(ctx, billing.Grant{
		Account: acc.Name, Source: billing.SourcePurchase, AmountMicros: pack.CreditMicros, ValidFrom: now, ExpiresAt: &expires,
		Key: "purchase/" + cs.ID, Item: pack.LookupKey, Ref: &billing.GrantRef{CheckoutSession: cs.ID, PaymentIntent: cs.PaymentIntent},
	}); err != nil {
		return err
	}
	return s.EnsurePaymentMethods(ctx, cs.Customer)
}
