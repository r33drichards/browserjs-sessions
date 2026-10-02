package stripe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/billing"
)

// How long a pack bought at a time that was not recorded is looked back for
// by the reconcile.
const reconcileWindow = 35 * 24 * time.Hour

// Options is the configuration of a Service.
type Options struct {
	// Mode is STRIPE_MODE: "test" or "live". An event of the other mode is
	// refused.
	Mode string
	// WebhookSecret is STRIPE_WEBHOOK_SECRET.
	WebhookSecret string
	// PublicURL is the app's base URL, no trailing slash: where Checkout and
	// the portal return to.
	PublicURL string
	// AutoRecharge is AUTO_RECHARGE: accounts may turn auto-recharge on.
	AutoRecharge bool
	// NoSignupCredit is SIGNUP_CREDIT=off: cards are saved, and no account
	// is decided for or against the sign-up credit.
	NoSignupCredit bool
	// Catalogue is where the catalogue is had from; it may change between
	// calls.
	Catalogue billing.CatalogueSource
	// Pace is the pause between two accounts of a reconcile.
	Pace time.Duration
}

// Service is the Stripe side of billing.
type Service struct {
	accounts billing.Accounts
	ledger   billing.Ledger
	stripe   billing.Stripe
	clock    billing.Clock
	opt      Options

	failures *failureLimiter

	mu     sync.Mutex
	prices map[string]string      // lookup key to price ID
	locks  map[string]*sync.Mutex // by Account name
}

func New(accounts billing.Accounts, ledger billing.Ledger, client billing.Stripe, clock billing.Clock, opt Options) (*Service, error) {
	switch {
	case accounts == nil || ledger == nil || client == nil || clock == nil:
		return nil, errors.New("stripe: accounts, ledger, client and clock are required")
	case opt.Mode != "test" && opt.Mode != "live":
		return nil, fmt.Errorf("stripe: mode must be test or live, got %q", opt.Mode)
	case opt.WebhookSecret == "":
		return nil, errors.New("stripe: a webhook secret is required")
	case opt.PublicURL == "":
		return nil, errors.New("stripe: the public URL is required")
	case opt.Catalogue == nil:
		return nil, errors.New("stripe: a catalogue is required")
	}
	if err := CheckCatalogue(opt.Catalogue.Catalogue()); err != nil {
		return nil, err
	}
	return &Service{
		accounts: accounts, ledger: ledger, stripe: client, clock: clock, opt: opt,
		failures: newFailureLimiter(webhookFailures, webhookFailureInterval, time.Now),
		prices:   map[string]string{},
		locks:    map[string]*sync.Mutex{},
	}, nil
}

// lock serialises the ensure functions of one account, so that of two reads
// of Stripe the older is never written after the newer.
func (s *Service) lock(account string) (unlock func()) {
	s.mu.Lock()
	l := s.locks[account]
	if l == nil {
		l = &sync.Mutex{}
		s.locks[account] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// RefreshPrices reads the price of everything that is sold. An enabled item
// Stripe has no price for is logged and is not offered.
func (s *Service) RefreshPrices(ctx context.Context) error {
	keys := LookupKeys(s.opt.Catalogue.Catalogue())
	prices := map[string]string{}
	// Stripe takes at most 10 lookup keys a call.
	for len(keys) > 0 {
		n := min(10, len(keys))
		got, err := s.stripe.Prices(ctx, keys[:n])
		if err != nil {
			return err
		}
		for _, key := range keys[:n] {
			if id := got[key]; id != "" {
				prices[key] = id
			} else {
				slog.Error("stripe: no price for an enabled item; it is not offered (apply infra/billing)", "lookupKey", key)
			}
		}
		keys = keys[n:]
	}
	s.mu.Lock()
	s.prices = prices
	s.mu.Unlock()
	return nil
}

func (s *Service) price(lookupKey string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.prices[lookupKey]
	return id, ok
}

// EnsurePaymentMethods writes what Stripe says of the customer's saved
// payment methods to their Account, and decides the sign-up credit when the
// first card is there. Safe to run any number of times, in any order.
func (s *Service) EnsurePaymentMethods(ctx context.Context, customer string) error {
	if customer == "" {
		return nil
	}
	acct, err := s.accounts.ByCustomer(ctx, customer)
	if errors.Is(err, billing.ErrNotFound) {
		slog.Warn("stripe: no account has this customer", "customer", customer)
		return nil
	}
	if err != nil {
		return err
	}
	defer s.lock(acct.Name)()
	return s.paymentMethods(ctx, acct)
}

// deleted reports whether the account was deleted by its user: only its
// owner, its sign-up credit's outcome and the deletion are kept, and nothing
// Stripe says is written to it any more.
func deleted(acct billing.Account, what string) bool {
	if acct.Spec.DeletedAt == nil {
		return false
	}
	slog.Warn("stripe: "+what+" for a deleted account; nothing is written", "account", acct.Name)
	return true
}

// paymentMethods is EnsurePaymentMethods for an account whose lock is held.
func (s *Service) paymentMethods(ctx context.Context, acct billing.Account) error {
	customer := acct.Spec.StripeCustomerID
	if customer == "" || deleted(acct, "payment methods") {
		return nil
	}
	methods, def, err := s.stripe.PaymentMethods(ctx, customer)
	// A customer Stripe does not have has no card.
	if errors.Is(err, billing.ErrNotFound) {
		slog.Error("stripe: an account's customer is not at Stripe (deleted, or of the other mode)", "account", acct.Name, "customer", customer)
		methods, def, err = nil, "", nil
	}
	if err != nil {
		return err
	}
	sortOldestFirst(methods)
	var ids []string
	for _, m := range methods {
		// The CRD keeps at most 20; the product allows 5.
		if len(ids) < 20 {
			ids = append(ids, m.ID)
		}
	}
	now := s.clock.Now()
	acct, err = s.accounts.Update(ctx, acct.Name, func(spec *billing.AccountSpec) error {
		pm := &billing.PaymentMethods{Present: len(methods) > 0, IDs: ids, Default: def, ReadAt: now}
		if old := spec.PaymentMethod; !pm.Present && old != nil {
			if old.Present {
				pm.RemovedAt = &now
			} else {
				pm.RemovedAt = old.RemovedAt
			}
		}
		spec.PaymentMethod = pm
		return nil
	})
	if err != nil {
		return err
	}
	if len(methods) == 0 || acct.Spec.SignupCredit != nil || s.opt.NoSignupCredit {
		return nil
	}
	decision, err := s.decideSignupCredit(ctx, acct, methods[0])
	if err != nil {
		return err
	}
	_, err = s.accounts.Update(ctx, acct.Name, func(spec *billing.AccountSpec) error {
		// Decided once.
		if spec.SignupCredit == nil {
			spec.SignupCredit = &decision
		}
		return nil
	})
	return err
}

// decideSignupCredit decides, on the account's oldest card, whether the
// account earns the sign-up credit, and makes its Grant if it does. The
// Grant is the card's, not the account's: the same card on another account
// finds it there.
func (s *Service) decideSignupCredit(ctx context.Context, acct billing.Account, oldest billing.PaymentMethod) (billing.SignupCredit, error) {
	cat := s.opt.Catalogue.Catalogue()
	now := s.clock.Now()
	refused := func(reason string) (billing.SignupCredit, error) {
		slog.Info("stripe: sign-up credit refused", "account", acct.Name, "reason", reason)
		return billing.SignupCredit{State: billing.SignupRefused, Reason: reason, At: now}, nil
	}
	switch {
	case oldest.Fingerprint == "":
		return refused(billing.RefusedNoFingerprint)
	case refusesFunding(cat, oldest.Funding):
		return refused(billing.RefusedPrepaid)
	case oldest.Wallet != "" && cat.SignupCredit.RefuseWallets:
		return refused(billing.RefusedWallet)
	}
	expires := now.Add(time.Duration(cat.SignupCredit.ValidDays) * 24 * time.Hour)
	created, existing, err := s.ledger.EnsureGrant(ctx, billing.Grant{
		Account: acct.Name, Source: billing.SourceSignup, AmountMicros: cat.SignupCredit.AmountMicros,
		ValidFrom: now, ExpiresAt: &expires, Key: "signup/" + oldest.Fingerprint,
	})
	if err != nil {
		return billing.SignupCredit{}, err
	}
	// A key that was used by this account is a replay. Used by another
	// (the Grant there is another account's, or names none), the card has
	// had its credit.
	if !created && existing.Account != acct.Name {
		return refused(billing.RefusedCardUsed)
	}
	slog.Info("stripe: sign-up credit granted", "account", acct.Name, "micros", cat.SignupCredit.AmountMicros)
	return billing.SignupCredit{State: billing.SignupGranted, At: now}, nil
}

func sortOldestFirst(methods []billing.PaymentMethod) {
	for i := 1; i < len(methods); i++ {
		for j := i; j > 0 && methods[j].Created.Before(methods[j-1].Created); j-- {
			methods[j], methods[j-1] = methods[j-1], methods[j]
		}
	}
}

// A subscription in one of these states is one the account has: a second
// cannot be bought, and it is not displaced by one that has ended.
func subscribed(status string) bool {
	switch status {
	case "active", "trialing", "past_due", "incomplete":
		return true
	}
	return false
}

// EnsureSubscription writes what Stripe says of a subscription to its
// Account and makes the plan Grant of its current period if that is paid.
func (s *Service) EnsureSubscription(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	sub, err := s.stripe.Subscription(ctx, id)
	if errors.Is(err, billing.ErrNotFound) {
		slog.Warn("stripe: no such subscription", "subscription", id)
		return nil
	}
	if err != nil {
		return err
	}
	acct, err := s.accountOf(ctx, sub.Metadata["account"], sub.Customer)
	if errors.Is(err, billing.ErrNotFound) {
		slog.Warn("stripe: no account for this subscription", "subscription", id, "customer", sub.Customer)
		return nil
	}
	if err != nil {
		return err
	}
	if deleted(acct, "a subscription") {
		return nil
	}
	defer s.lock(acct.Name)()
	return s.ensureSubscription(ctx, acct, sub)
}

// subscription is EnsureSubscription for an account whose lock is held.
func (s *Service) subscription(ctx context.Context, acct billing.Account, id string) error {
	sub, err := s.stripe.Subscription(ctx, id)
	if err != nil {
		return err
	}
	return s.ensureSubscription(ctx, acct, sub)
}

func (s *Service) ensureSubscription(ctx context.Context, acct billing.Account, sub billing.Subscription) error {
	var err error
	// The account has one subscription. If it names another that is still
	// going, the one made later is kept.
	kept := true
	if cur := acct.Spec.Subscription; cur != nil && cur.ID != sub.ID && subscribed(cur.Status) {
		other, err := s.stripe.Subscription(ctx, cur.ID)
		switch {
		case errors.Is(err, billing.ErrNotFound):
		case err != nil:
			return err
		case subscribed(other.Status):
			if subscribed(sub.Status) {
				slog.Error("stripe: AN ACCOUNT HAS TWO SUBSCRIPTIONS; the later one is kept, the other needs cancelling by hand",
					"account", acct.Name, "subscription", sub.ID, "other", other.ID)
			}
			kept = subscribed(sub.Status) && sub.Created.After(other.Created)
		}
	}
	now := s.clock.Now()
	if kept {
		start, end := sub.CurrentPeriodStart, sub.CurrentPeriodEnd
		_, err = s.accounts.Update(ctx, acct.Name, func(spec *billing.AccountSpec) error {
			spec.Subscription = &billing.SubscriptionState{
				ID: sub.ID, Status: sub.Status, PriceLookupKey: sub.PriceLookupKey,
				CurrentPeriodStart: &start, CurrentPeriodEnd: &end, CancelAt: sub.CancelAt, ReadAt: &now,
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	// A period that is paid for is credit, whatever else is true. One that
	// has ended, or is not paid, grants nothing: the Grant of a period
	// already paid runs to its expiry.
	if (sub.Status == "active" || sub.Status == "trialing") && sub.LatestInvoiceStatus == "paid" {
		plan, ok := planOf(s.opt.Catalogue.Catalogue(), sub.PriceLookupKey)
		if !ok {
			slog.Error("stripe: a paid subscription's price is not in the catalogue; NO CREDIT WAS GRANTED",
				"account", acct.Name, "subscription", sub.ID, "lookupKey", sub.PriceLookupKey)
		} else {
			start, end := sub.CurrentPeriodStart, sub.CurrentPeriodEnd
			key := fmt.Sprintf("plan/%s/%d", sub.ID, start.Unix())
			if _, _, err := s.ledger.EnsureGrant(ctx, billing.Grant{
				Account: acct.Name, Source: billing.SourcePlan, AmountMicros: plan.CreditMicros,
				ValidFrom: start, ExpiresAt: &end, Key: key, Item: plan.LookupKey,
				Ref: &billing.GrantRef{Subscription: sub.ID, Invoice: sub.LatestInvoice},
			}); err != nil {
				return err
			}
			if kept {
				// An upgrade starts a new period at once: what was left of
				// the old one's credit goes.
				if err := s.ledger.Revoke(ctx, billing.GrantSelector{
					Account: acct.Name, Source: billing.SourcePlan, Except: billing.GrantName(key), ExpiresAfter: &start,
				}, RevokedSuperseded); err != nil {
					return err
				}
			}
		}
	}
	return s.paymentMethods(ctx, acct)
}

// accountOf is the Account named name, or failing that the one whose
// customer is customer. An Account that is named and has another customer
// is not it.
func (s *Service) accountOf(ctx context.Context, name, customer string) (billing.Account, error) {
	if name != "" {
		acct, err := s.accounts.Get(ctx, name)
		if err == nil {
			if acct.Spec.StripeCustomerID != customer {
				slog.Error("stripe: an object names an account that has another customer", "account", name, "customer", customer)
				return billing.Account{}, billing.ErrNotFound
			}
			return acct, nil
		}
		if !errors.Is(err, billing.ErrNotFound) {
			return billing.Account{}, err
		}
	}
	if customer == "" {
		return billing.Account{}, billing.ErrNotFound
	}
	return s.accounts.ByCustomer(ctx, customer)
}

// EnsurePurchase fulfils a Checkout Session: a saved card, a subscription,
// or a pack. It answers the session as Stripe has it.
func (s *Service) EnsurePurchase(ctx context.Context, id string) (billing.CheckoutSession, error) {
	cs, err := s.stripe.Checkout(ctx, id)
	if errors.Is(err, billing.ErrNotFound) {
		slog.Warn("stripe: no such checkout session", "checkoutSession", id)
		return billing.CheckoutSession{}, nil
	}
	if err != nil {
		return billing.CheckoutSession{}, err
	}
	switch cs.Mode {
	case billing.ModeSetup:
		if cs.Status == "complete" {
			err = s.EnsurePaymentMethods(ctx, cs.Customer)
		}
	case billing.ModeSubscription:
		if cs.Subscription != "" {
			err = s.EnsureSubscription(ctx, cs.Subscription)
		}
	case billing.ModePayment:
		err = s.pack(ctx, cs)
	}
	return cs, err
}

// pack makes the Grant of a pack that was paid for at a checkout.
func (s *Service) pack(ctx context.Context, cs billing.CheckoutSession) error {
	if cs.Status != "complete" || cs.PaymentStatus != "paid" {
		return nil
	}
	acct, err := s.accounts.Get(ctx, cs.ClientReferenceID)
	if errors.Is(err, billing.ErrNotFound) || (err == nil &&
		(cs.Metadata["account"] != acct.Name || cs.Customer == "" || cs.Customer != acct.Spec.StripeCustomerID)) {
		slog.Error("stripe: a paid checkout does not match an account; NO CREDIT WAS GRANTED",
			"checkoutSession", cs.ID, "reference", cs.ClientReferenceID, "customer", cs.Customer)
		return nil
	}
	if err != nil {
		return err
	}
	if deleted(acct, "a paid checkout") {
		return nil
	}
	pack, ok := packOf(s.opt.Catalogue.Catalogue(), cs.Metadata["item"])
	if !ok {
		slog.Error("stripe: a paid checkout's item is not a pack of the catalogue; NO CREDIT WAS GRANTED",
			"checkoutSession", cs.ID, "item", cs.Metadata["item"])
		return nil
	}
	defer s.lock(acct.Name)()
	now := s.clock.Now()
	expires := now.Add(time.Duration(pack.ValidDays) * 24 * time.Hour)
	// ref.paymentIntent is how a refund finds it.
	if _, _, err := s.ledger.EnsureGrant(ctx, billing.Grant{
		Account: acct.Name, Source: billing.SourcePurchase, AmountMicros: pack.CreditMicros,
		ValidFrom: now, ExpiresAt: &expires, Key: "purchase/" + cs.ID, Item: pack.LookupKey,
		Ref: &billing.GrantRef{CheckoutSession: cs.ID, PaymentIntent: cs.PaymentIntent},
	}); err != nil {
		return err
	}
	return s.paymentMethods(ctx, acct)
}
