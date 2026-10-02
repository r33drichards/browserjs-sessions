package stripe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stripe/stripe-go/v86/webhook"

	"github.com/r33drichards/computer-use/backend/internal/billing"
)

// WebhookPath is where Stripe posts its events, on the API host only.
const WebhookPath = "/stripe/webhook"

const (
	// The largest event read.
	maxEventBytes = 1 << 20
	// How old a signature may be.
	signatureTolerance = 5 * time.Minute
	// A source address may post this many events that do not verify at
	// once, and one more every interval after that, before its failures
	// stop being looked at closely (they are still refused).
	webhookFailures        = 20
	webhookFailureInterval = 10 * time.Second
)

// event is what is read of an event: its type, its mode, and of its object
// the IDs that say what to read from Stripe. Nothing is ever written from an
// event's payload.
type event struct {
	ID       string
	Type     string
	Livemode bool
	Object   eventObject
}

type eventObject struct {
	ID            string            `json:"id"`
	Customer      ref               `json:"customer"`
	PaymentIntent ref               `json:"payment_intent"`
	Status        string            `json:"status"`
	Metadata      map[string]string `json:"metadata"`
	Parent        *struct {
		Type                string `json:"type"`
		SubscriptionDetails *struct {
			Subscription ref `json:"subscription"`
		} `json:"subscription_details"`
	} `json:"parent"`
}

// ref is a reference to another object: its ID, or the object, or null.
type ref string

func (r *ref) UnmarshalJSON(data []byte) error {
	var id string
	if err := json.Unmarshal(data, &id); err == nil {
		*r = ref(id)
		return nil
	}
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	*r = ref(obj.ID)
	return nil
}

// Webhook is the handler of POST /stripe/webhook. Nobody is signed in: the
// signature is the only credential.
func (s *Service) Webhook() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		addr := clientAddr(r)
		// One byte more than is allowed: an event that long is refused, not
		// cut short and then found to have a bad signature.
		body, err := io.ReadAll(io.LimitReader(r.Body, maxEventBytes+1))
		if err != nil || len(body) > maxEventBytes {
			s.refuse(w, addr, "unreadable or too long")
			return
		}
		ev, err := webhook.ConstructEventWithOptions(body, r.Header.Get("Stripe-Signature"), s.opt.WebhookSecret,
			webhook.ConstructEventOptions{
				Tolerance: signatureTolerance,
				// Only the type and a few IDs are read of an event, and
				// everything else is read from Stripe: an endpoint made
				// with another API version is no reason to refuse it.
				IgnoreAPIVersionMismatch: true,
			})
		if err != nil {
			s.refuse(w, addr, "bad signature")
			return
		}
		if ev.Livemode != (s.opt.Mode == "live") {
			s.refuse(w, addr, "wrong mode")
			return
		}
		e := event{ID: ev.ID, Type: string(ev.Type), Livemode: ev.Livemode}
		if ev.Data == nil || json.Unmarshal(ev.Data.Raw, &e.Object) != nil {
			s.refuse(w, addr, "no object")
			return
		}
		if err := s.handle(r.Context(), e); err != nil {
			// Stripe sends it again.
			slog.Error("stripe: event not handled", "event", e.ID, "type", e.Type, "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

// refuse answers 400 with no body, and logs the fact and the source address
// and nothing else, and that only while the address has not failed too
// often.
func (s *Service) refuse(w http.ResponseWriter, addr, why string) {
	if _, blocked := s.failures.Blocked(addr); !blocked {
		slog.Warn("stripe: webhook request refused", "why", why, "addr", addr)
	}
	s.failures.Failed(addr)
	w.WriteHeader(http.StatusBadRequest)
}

// handle is the event table of stripe.md. An error is something that may
// work the next time (Stripe or the cluster could not be reached).
func (s *Service) handle(ctx context.Context, e event) error {
	o := e.Object
	switch e.Type {
	case "checkout.session.completed", "checkout.session.async_payment_succeeded":
		_, err := s.EnsurePurchase(ctx, o.ID)
		return err
	case "checkout.session.async_payment_failed":
		slog.Info("stripe: a checkout's payment failed", "checkoutSession", o.ID)
		return nil
	case "setup_intent.succeeded",
		"payment_method.attached", "payment_method.updated", "payment_method.automatically_updated":
		return s.EnsurePaymentMethods(ctx, string(o.Customer))
	case "payment_method.detached":
		// The object no longer names a customer.
		acct, err := s.accounts.ByPaymentMethod(ctx, o.ID)
		if errors.Is(err, billing.ErrNotFound) {
			return nil // the reconcile covers it
		}
		if err != nil {
			return err
		}
		return s.EnsurePaymentMethods(ctx, acct.Spec.StripeCustomerID)
	case "customer.updated":
		return s.EnsurePaymentMethods(ctx, o.ID)
	case "customer.deleted":
		return s.customerDeleted(ctx, o.ID)
	case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted",
		"customer.subscription.paused", "customer.subscription.resumed":
		return s.EnsureSubscription(ctx, o.ID)
	case "invoice.paid", "invoice.payment_failed":
		if p := o.Parent; p != nil && p.Type == "subscription_details" && p.SubscriptionDetails != nil {
			return s.EnsureSubscription(ctx, string(p.SubscriptionDetails.Subscription))
		}
		return nil
	case "payment_intent.succeeded", "payment_intent.payment_failed":
		// Checkout's own are handled by its session's events.
		if o.Metadata["kind"] == billing.KindRecharge {
			return s.EnsureRecharge(ctx, o.ID)
		}
		return nil
	case "refund.created":
		if o.Status == "failed" || o.Status == "canceled" {
			return nil
		}
		return s.refunded(ctx, string(o.PaymentIntent))
	case "charge.refunded":
		return s.refunded(ctx, string(o.PaymentIntent))
	case "charge.dispute.created":
		return s.disputed(ctx, string(o.PaymentIntent))
	case "charge.dispute.closed":
		if o.Status == "won" {
			return s.disputeWon(ctx, string(o.PaymentIntent))
		}
		return nil
	}
	return nil
}

// customerDeleted records that a customer Stripe no longer has has no card.
func (s *Service) customerDeleted(ctx context.Context, customer string) error {
	acct, err := s.accounts.ByCustomer(ctx, customer)
	if errors.Is(err, billing.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if deleted(acct, "a deleted customer") {
		return nil
	}
	slog.Error("stripe: A CUSTOMER WAS DELETED AT STRIPE; this product deletes none", "account", acct.Name, "customer", customer)
	defer s.lock(acct.Name)()
	now := s.clock.Now()
	_, err = s.accounts.Update(ctx, acct.Name, func(spec *billing.AccountSpec) error {
		pm := &billing.PaymentMethods{ReadAt: now}
		if old := spec.PaymentMethod; old != nil {
			pm.RemovedAt = old.RemovedAt
			if old.Present {
				pm.RemovedAt = &now
			}
		}
		spec.PaymentMethod = pm
		return nil
	})
	return err
}

// refunded revokes what a refunded payment bought.
func (s *Service) refunded(ctx context.Context, paymentIntent string) error {
	acct, err := s.accountOfPayment(ctx, paymentIntent)
	if errors.Is(err, billing.ErrNotFound) {
		slog.Warn("stripe: a refund of a payment no account made", "paymentIntent", paymentIntent)
		return nil
	}
	if err != nil {
		return err
	}
	return s.revokePaid(ctx, acct.Name, paymentIntent, RevokedRefund)
}

// revokePaid revokes what an account's payment bought: the pack whose Grant
// carries the PaymentIntent, or the plan Grant of the period its invoice
// was for. A partial refund revokes the whole Grant.
func (s *Service) revokePaid(ctx context.Context, account, paymentIntent, reason string) error {
	if err := s.ledger.Revoke(ctx, billing.GrantSelector{Account: account, PaymentIntent: paymentIntent}, reason); err != nil {
		return err
	}
	finder, ok := s.stripe.(PeriodFinder)
	if !ok {
		return nil
	}
	period, ok, err := finder.SubscriptionPeriodOf(ctx, paymentIntent)
	if err != nil || !ok {
		return err
	}
	key := "plan/" + period.Subscription + "/" + strconv.FormatInt(period.Start.Unix(), 10)
	return s.ledger.Revoke(ctx, billing.GrantSelector{Account: account, Name: billing.GrantName(key)}, reason)
}

// accountOfPayment is the Account whose customer made a payment.
func (s *Service) accountOfPayment(ctx context.Context, paymentIntent string) (billing.Account, error) {
	if paymentIntent == "" {
		return billing.Account{}, billing.ErrNotFound
	}
	pi, err := s.stripe.PaymentIntent(ctx, paymentIntent)
	if err != nil {
		return billing.Account{}, err
	}
	if pi.Customer == "" {
		return billing.Account{}, billing.ErrNotFound
	}
	return s.accounts.ByCustomer(ctx, pi.Customer)
}

// disputed blocks the account whose payment is disputed and revokes what
// the payment bought.
func (s *Service) disputed(ctx context.Context, paymentIntent string) error {
	acct, err := s.accountOfPayment(ctx, paymentIntent)
	if errors.Is(err, billing.ErrNotFound) {
		slog.Warn("stripe: a dispute on a payment no account made", "paymentIntent", paymentIntent)
		return nil
	}
	if err != nil {
		return err
	}
	slog.Error("stripe: A PAYMENT IS DISPUTED; the account is blocked", "account", acct.Name, "paymentIntent", paymentIntent)
	now := s.clock.Now()
	if _, err := s.accounts.Update(ctx, acct.Name, func(spec *billing.AccountSpec) error {
		// A block an admin made is theirs to keep or lift; a deleted
		// account can do nothing as it is.
		if spec.Blocked == nil && spec.DeletedAt == nil {
			spec.Blocked = &billing.Blocked{Reason: "dispute", Since: &now}
		}
		return nil
	}); err != nil {
		return err
	}
	return s.revokePaid(ctx, acct.Name, paymentIntent, RevokedDispute)
}

// disputeWon lifts the block a dispute made. What was revoked stays
// revoked: an admin's Grant restores it.
func (s *Service) disputeWon(ctx context.Context, paymentIntent string) error {
	acct, err := s.accountOfPayment(ctx, paymentIntent)
	if errors.Is(err, billing.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = s.accounts.Update(ctx, acct.Name, func(spec *billing.AccountSpec) error {
		if spec.Blocked != nil && spec.Blocked.Reason == "dispute" {
			spec.Blocked = nil
		}
		return nil
	})
	return err
}

// clientAddr is the address a request came from, as the proxy in front saw
// it: Pomerium appends that to X-Forwarded-For, so the last entry is its own
// word.
func clientAddr(r *http.Request) string {
	if values := r.Header.Values("X-Forwarded-For"); len(values) > 0 {
		list := strings.Split(values[len(values)-1], ",")
		if addr := strings.TrimSpace(list[len(list)-1]); addr != "" {
			return addr
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// failureLimiter counts each source address's failures up to limit and
// forgets one every interval.
type failureLimiter struct {
	limit    float64
	interval time.Duration
	now      func() time.Time

	mu     sync.Mutex
	counts map[string]*failureCount
}

type failureCount struct {
	n  float64
	at time.Time
}

// How many addresses are remembered before the limiter starts over.
const limiterAddresses = 10000

func newFailureLimiter(limit int, interval time.Duration, now func() time.Time) *failureLimiter {
	return &failureLimiter{limit: float64(limit), interval: interval, now: now, counts: map[string]*failureCount{}}
}

func (l *failureLimiter) current(addr string) float64 {
	c := l.counts[addr]
	if c == nil {
		return 0
	}
	now := l.now()
	c.n = math.Max(0, c.n-float64(now.Sub(c.at))/float64(l.interval))
	c.at = now
	return c.n
}

// Blocked reports whether addr has failed too often lately.
func (l *failureLimiter) Blocked(addr string) (float64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.current(addr)
	return n, n > l.limit-1
}

func (l *failureLimiter) Failed(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.current(addr)
	if l.counts[addr] == nil {
		if len(l.counts) >= limiterAddresses {
			clear(l.counts)
		}
		l.counts[addr] = &failureCount{at: l.now()}
	}
	l.counts[addr].n = math.Min(l.limit, n+1)
}
