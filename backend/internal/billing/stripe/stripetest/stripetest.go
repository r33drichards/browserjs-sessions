// Package stripetest is an in-memory Stripe: billing.Stripe, as
// billingtest's is, with what the whole event table needs (subscriptions,
// invoices, refunds, disputes) and calls that fail on demand. It holds the
// state an event refers to, because every handler reads Stripe again before
// acting, and each of its helpers returns the event Stripe would send, for
// a test to sign and post. The other fakes are billingtest's.
package stripetest

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stripe/stripe-go/v86/webhook"

	"github.com/r33drichards/browserjs-sessions/backend/internal/billing"
	"github.com/r33drichards/browserjs-sessions/backend/internal/billing/stripe"
)

func copyOf[T any](v T) T {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		panic(err)
	}
	return out
}

// Card is a card a customer saves.
type Card struct {
	Fingerprint string
	Funding     string // credit, debit, prepaid
	Wallet      string // apple_pay, google_pay; "" for none
	Brand       string
	Last4       string
}

// Event is an event as Stripe would send it.
type Event struct {
	ID      string
	Type    string
	Payload []byte
}

// Sign is the Stripe-Signature header of payload, signed with secret at t.
func Sign(payload []byte, secret string, t time.Time) string {
	return webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: payload, Secret: secret, Timestamp: t}).Header
}

type customer struct {
	email    string
	metadata map[string]string
	def      string
}

// Stripe is Stripe's state in maps.
type Stripe struct {
	mu    sync.Mutex
	clock billing.Clock
	n     int

	// Livemode is the mode of the events the helpers return.
	Livemode bool
	// Err, when set, is what every call answers.
	Err error
	// Decline, when set, makes automatic charges fail with this decline
	// code ("card_declined", "authentication_required").
	Decline string
	// NoAttemptMetadata leaves metadata.month and metadata.seq off automatic
	// charges, as a client that does not write them would.
	NoAttemptMetadata bool
	// LoseRechargeResponse makes CreateRecharge charge and then fail: the
	// answer was lost on its way back.
	LoseRechargeResponse bool
	// Calls counts the calls made, by method name.
	Calls map[string]int
	errOn map[string]error

	rechargeCards []string

	customers    map[string]*customer
	customerKeys map[string]string
	methods      map[string]billing.PaymentMethod
	checkouts    map[string]billing.CheckoutSession
	subs         map[string]billing.Subscription
	intents      map[string]billing.PaymentIntent
	intentKeys   map[string]string
	periods      map[string]stripe.PaidPeriod
	prices       map[string]string
}

func NewStripe(clock billing.Clock) *Stripe {
	return &Stripe{
		clock: clock, Calls: map[string]int{}, errOn: map[string]error{},
		customers: map[string]*customer{}, customerKeys: map[string]string{},
		methods: map[string]billing.PaymentMethod{}, checkouts: map[string]billing.CheckoutSession{},
		subs: map[string]billing.Subscription{}, intents: map[string]billing.PaymentIntent{},
		intentKeys: map[string]string{}, periods: map[string]stripe.PaidPeriod{}, prices: map[string]string{},
	}
}

func (s *Stripe) id(prefix string) string {
	s.n++
	return fmt.Sprintf("%s_%04d", prefix, s.n)
}

// call counts a call and answers Err. The mutex is held by the caller.
func (s *Stripe) call(name string) error {
	s.Calls[name]++
	if err := s.errOn[name]; err != nil {
		return err
	}
	return s.Err
}

// FailOn makes the method named answer err (nil: work again).
func (s *Stripe) FailOn(method string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errOn[method] = err
}

// SetPrices says which lookup keys Stripe has a price for.
func (s *Stripe) SetPrices(lookupKeys ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range lookupKeys {
		s.prices[k] = "price_" + k
	}
}

func (s *Stripe) CreateCustomer(_ context.Context, p billing.CustomerParams) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("CreateCustomer"); err != nil {
		return "", err
	}
	key := "customer-" + p.OwnerHash
	if id, ok := s.customerKeys[key]; ok {
		return id, nil
	}
	id := s.id("cus")
	s.customers[id] = &customer{email: p.Email, metadata: map[string]string{"account": p.Account, "owner_hash": p.OwnerHash}}
	s.customerKeys[key] = id
	return id, nil
}

// AddCustomer makes a customer no Account has.
func (s *Stripe) AddCustomer() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.id("cus")
	s.customers[id] = &customer{}
	return id
}

func (s *Stripe) CreateCheckout(_ context.Context, p billing.CheckoutParams) (billing.CheckoutSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("CreateCheckout"); err != nil {
		return billing.CheckoutSession{}, err
	}
	if _, ok := s.customers[p.Customer]; !ok {
		return billing.CheckoutSession{}, fmt.Errorf("no such customer %q", p.Customer)
	}
	md := map[string]string{"account": p.Account, "kind": p.Kind}
	if p.Item != "" {
		md["item"] = p.Item
	}
	id := s.id("cs_test")
	cs := billing.CheckoutSession{
		ID: id, URL: "https://checkout.stripe.test/" + id, Mode: p.Mode, Status: "open", PaymentStatus: "unpaid",
		Customer: p.Customer, ClientReferenceID: p.Account, Metadata: md, Created: s.clock.Now(),
	}
	if p.Mode == billing.ModeSetup {
		cs.PaymentStatus = "no_payment_required"
	}
	s.checkouts[id] = cs
	return cs, nil
}

func (s *Stripe) CreatePortal(_ context.Context, cust, returnURL string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("CreatePortal"); err != nil {
		return "", err
	}
	return "https://billing.stripe.test/p/" + cust + "?return=" + returnURL, nil
}

func (s *Stripe) CreateRecharge(_ context.Context, p billing.RechargeParams) (billing.PaymentIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("CreateRecharge"); err != nil {
		return billing.PaymentIntent{}, err
	}
	if id, ok := s.intentKeys[p.IdempotencyKey]; ok {
		return s.intents[id], nil
	}
	if m, ok := s.methods[p.PaymentMethod]; !ok || m.Customer != p.Customer {
		return billing.PaymentIntent{}, fmt.Errorf("no such payment method %q of %q", p.PaymentMethod, p.Customer)
	}
	s.rechargeCards = append(s.rechargeCards, p.PaymentMethod)
	pi := billing.PaymentIntent{
		ID: s.id("pi"), Customer: p.Customer, Status: "succeeded", AmountCents: p.AmountCents, Created: s.clock.Now(),
		Metadata: map[string]string{"account": p.Account, "kind": billing.KindRecharge, "item": p.Item},
	}
	// As the real client does: the attempt, from recharge-<hash>-<yyyy>-<mm>-<seq>.
	if parts := strings.Split(p.IdempotencyKey, "-"); len(parts) == 5 && !s.NoAttemptMetadata {
		pi.Metadata["month"], pi.Metadata["seq"] = parts[2]+"-"+parts[3], parts[4]
	}
	if s.Decline != "" {
		pi.Status, pi.DeclineCode = "requires_payment_method", s.Decline
	}
	s.intents[pi.ID] = pi
	s.intentKeys[p.IdempotencyKey] = pi.ID
	if s.LoseRechargeResponse {
		return billing.PaymentIntent{}, fmt.Errorf("connection reset")
	}
	return pi, nil
}

func (s *Stripe) Checkout(_ context.Context, id string) (billing.CheckoutSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("Checkout"); err != nil {
		return billing.CheckoutSession{}, err
	}
	cs, ok := s.checkouts[id]
	if !ok {
		return billing.CheckoutSession{}, billing.ErrNotFound
	}
	return cs, nil
}

func (s *Stripe) PaymentMethods(_ context.Context, cust string) ([]billing.PaymentMethod, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("PaymentMethods"); err != nil {
		return nil, "", err
	}
	c, ok := s.customers[cust]
	if !ok {
		return nil, "", billing.ErrNotFound
	}
	var out []billing.PaymentMethod
	for _, m := range s.methods {
		if m.Customer == cust {
			out = append(out, m)
		}
	}
	// Newest first, as Stripe lists.
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, c.def, nil
}

func (s *Stripe) Subscription(_ context.Context, id string) (billing.Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("Subscription"); err != nil {
		return billing.Subscription{}, err
	}
	sub, ok := s.subs[id]
	if !ok {
		return billing.Subscription{}, billing.ErrNotFound
	}
	return sub, nil
}

func (s *Stripe) PaymentIntent(_ context.Context, id string) (billing.PaymentIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("PaymentIntent"); err != nil {
		return billing.PaymentIntent{}, err
	}
	pi, ok := s.intents[id]
	if !ok {
		return billing.PaymentIntent{}, billing.ErrNotFound
	}
	return pi, nil
}

func (s *Stripe) Prices(_ context.Context, lookupKeys []string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("Prices"); err != nil {
		return nil, err
	}
	if len(lookupKeys) > 10 {
		return nil, fmt.Errorf("at most 10 lookup keys a call, got %d", len(lookupKeys))
	}
	out := map[string]string{}
	for _, k := range lookupKeys {
		if id, ok := s.prices[k]; ok {
			out[k] = id
		}
	}
	return out, nil
}

func (s *Stripe) CancelSubscription(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("CancelSubscription"); err != nil {
		return err
	}
	sub, ok := s.subs[id]
	if !ok {
		return billing.ErrNotFound
	}
	sub.Status = "canceled"
	s.subs[id] = sub
	return nil
}

func (s *Stripe) DetachPaymentMethod(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("DetachPaymentMethod"); err != nil {
		return err
	}
	if _, ok := s.methods[id]; !ok {
		return billing.ErrNotFound
	}
	s.detach(id)
	return nil
}

func (s *Stripe) Subscriptions(_ context.Context, cust string) ([]billing.Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("Subscriptions"); err != nil {
		return nil, err
	}
	var out []billing.Subscription
	for _, sub := range s.subs {
		if sub.Customer == cust {
			out = append(out, sub)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Stripe) Checkouts(_ context.Context, cust string, since time.Time) ([]billing.CheckoutSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("Checkouts"); err != nil {
		return nil, err
	}
	var out []billing.CheckoutSession
	for _, cs := range s.checkouts {
		if cs.Customer == cust && !cs.Created.Before(since) {
			out = append(out, cs)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Stripe) PaymentIntents(_ context.Context, cust string, since time.Time) ([]billing.PaymentIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("PaymentIntents"); err != nil {
		return nil, err
	}
	var out []billing.PaymentIntent
	for _, pi := range s.intents {
		if pi.Customer == cust && !pi.Created.Before(since) {
			out = append(out, pi)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Stripe) SubscriptionPeriodOf(_ context.Context, paymentIntent string) (stripe.PaidPeriod, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("SubscriptionPeriodOf"); err != nil {
		return stripe.PaidPeriod{}, false, err
	}
	p, ok := s.periods[paymentIntent]
	return p, ok, nil
}

// The helpers: what a user, or Stripe, does. Each returns the event Stripe
// would send.

func (s *Stripe) event(typ string, object map[string]any) Event {
	id := s.id("evt")
	payload, err := json.Marshal(map[string]any{
		"id": id, "object": "event", "api_version": "2026-08-26.dahlia",
		"created": s.clock.Now().Unix(), "livemode": s.Livemode, "type": typ,
		"data": map[string]any{"object": object},
	})
	if err != nil {
		panic(err)
	}
	return Event{ID: id, Type: typ, Payload: payload}
}

// Event is an event of any type about any object.
func (s *Stripe) Event(typ string, object map[string]any) Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.event(typ, object)
}

func (s *Stripe) attach(cust string, card Card) string {
	id := s.id("pm")
	// A moment apart, so that "oldest" and "newest" mean something.
	s.methods[id] = billing.PaymentMethod{
		ID: id, Customer: cust, Type: "card", Created: s.clock.Now().Add(time.Duration(s.n) * time.Millisecond),
		Fingerprint: card.Fingerprint, Funding: card.Funding, Wallet: card.Wallet, Brand: card.Brand, Last4: card.Last4,
	}
	return id
}

func (s *Stripe) detach(pm string) {
	m := s.methods[pm]
	if c := s.customers[m.Customer]; c != nil && c.def == pm {
		c.def = ""
	}
	delete(s.methods, pm)
}

// AttachCard saves a card to the customer: payment_method.attached.
func (s *Stripe) AttachCard(cust string, card Card) (pm string, ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pm = s.attach(cust, card)
	return pm, s.event("payment_method.attached", map[string]any{"id": pm, "object": "payment_method", "customer": cust, "type": "card"})
}

// AttachedEvent is the payment_method.attached of a card, as when it was
// attached, whatever has become of the card since.
func (s *Stripe) AttachedEvent(pm, cust string) Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.event("payment_method.attached", map[string]any{"id": pm, "object": "payment_method", "customer": cust, "type": "card"})
}

// DetachCard removes a card: payment_method.detached, whose object names no
// customer.
func (s *Stripe) DetachCard(pm string) Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.detach(pm)
	return s.event("payment_method.detached", map[string]any{"id": pm, "object": "payment_method", "customer": nil, "type": "card"})
}

// SetDefault makes a card the customer's default: customer.updated.
func (s *Stripe) SetDefault(cust, pm string) Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.customers[cust].def = pm
	return s.event("customer.updated", map[string]any{"id": cust, "object": "customer"})
}

// DeleteCustomer deletes a customer and its cards: customer.deleted.
func (s *Stripe) DeleteCustomer(cust string) Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, m := range s.methods {
		if m.Customer == cust {
			delete(s.methods, id)
		}
	}
	delete(s.customers, cust)
	return s.event("customer.deleted", map[string]any{"id": cust, "object": "customer", "deleted": true})
}

func checkoutObject(cs billing.CheckoutSession) map[string]any {
	return map[string]any{
		"id": cs.ID, "object": "checkout.session", "mode": cs.Mode, "status": cs.Status,
		"payment_status": cs.PaymentStatus, "customer": cs.Customer,
		"client_reference_id": cs.ClientReferenceID, "metadata": cs.Metadata,
	}
}

// CompleteCheckout is the user finishing a Checkout with card:
// checkout.session.completed. A setup saves the card; a payment is paid and
// saves the card; a subscription starts, its first invoice paid.
func (s *Stripe) CompleteCheckout(id string, card Card) Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs, ok := s.checkouts[id]
	if !ok {
		panic("no checkout session " + id)
	}
	now := s.clock.Now()
	cs.Status = "complete"
	s.attach(cs.Customer, card)
	switch cs.Mode {
	case billing.ModePayment:
		pi := billing.PaymentIntent{ID: s.id("pi"), Customer: cs.Customer, Status: "succeeded", Created: now,
			Metadata: map[string]string{"account": cs.Metadata["account"], "kind": billing.KindPurchase, "item": cs.Metadata["item"]}}
		s.intents[pi.ID] = pi
		cs.PaymentStatus, cs.PaymentIntent = "paid", pi.ID
	case billing.ModeSubscription:
		sub := billing.Subscription{
			ID: s.id("sub"), Customer: cs.Customer, Status: "active",
			Metadata:       map[string]string{"account": cs.Metadata["account"], "kind": billing.KindPlan, "item": cs.Metadata["item"]},
			PriceLookupKey: cs.Metadata["item"], CurrentPeriodStart: now, CurrentPeriodEnd: now.AddDate(0, 1, 0),
			Created: now, LatestInvoice: s.id("in"), LatestInvoiceStatus: "paid",
		}
		s.subs[sub.ID] = sub
		pi := billing.PaymentIntent{ID: s.id("pi"), Customer: cs.Customer, Status: "succeeded", Created: now}
		s.intents[pi.ID] = pi
		s.periods[pi.ID] = stripe.PaidPeriod{Subscription: sub.ID, Invoice: sub.LatestInvoice, Start: now}
		cs.PaymentStatus, cs.Subscription = "paid", sub.ID
	}
	s.checkouts[id] = cs
	return s.event("checkout.session.completed", checkoutObject(cs))
}

// ExpireCheckout is a Checkout abandoned.
func (s *Stripe) ExpireCheckout(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.checkouts[id]
	cs.Status = "expired"
	s.checkouts[id] = cs
}

// CheckoutSessions is every Checkout Session made, ordered by ID.
func (s *Stripe) CheckoutSessions() []billing.CheckoutSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []billing.CheckoutSession
	for _, cs := range s.checkouts {
		out = append(out, cs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// PutSubscription makes or replaces a subscription as given.
func (s *Stripe) PutSubscription(sub billing.Subscription) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs[sub.ID] = sub
}

// ChangeSubscription changes a subscription: customer.subscription.updated.
func (s *Stripe) ChangeSubscription(id string, change func(*billing.Subscription)) Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[id]
	if !ok {
		panic("no subscription " + id)
	}
	change(&sub)
	s.subs[id] = sub
	return s.event("customer.subscription.updated", map[string]any{"id": id, "object": "subscription", "customer": sub.Customer, "status": sub.Status})
}

// SubscriptionEvent is an event of typ about the subscription.
func (s *Stripe) SubscriptionEvent(typ, id string) Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub := s.subs[id]
	return s.event(typ, map[string]any{"id": id, "object": "subscription", "customer": sub.Customer, "status": sub.Status})
}

// InvoiceEvent is invoice.paid or invoice.payment_failed for the
// subscription's latest invoice.
func (s *Stripe) InvoiceEvent(typ, subscription string) Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub := s.subs[subscription]
	return s.event(typ, map[string]any{
		"id": sub.LatestInvoice, "object": "invoice", "customer": sub.Customer,
		"parent": map[string]any{"type": "subscription_details", "subscription_details": map[string]any{"subscription": subscription}},
	})
}

// PaymentIntentEvent is payment_intent.succeeded or .payment_failed.
func (s *Stripe) PaymentIntentEvent(typ, id string) Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	pi := s.intents[id]
	return s.event(typ, map[string]any{"id": id, "object": "payment_intent", "customer": pi.Customer, "status": pi.Status, "metadata": pi.Metadata})
}

// PutPaymentIntent makes or replaces a PaymentIntent as given.
func (s *Stripe) PutPaymentIntent(pi billing.PaymentIntent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.intents[pi.ID] = pi
}

// PaymentIntentsMade is every PaymentIntent, ordered by ID.
func (s *Stripe) PaymentIntentsMade() []billing.PaymentIntent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []billing.PaymentIntent
	for _, pi := range s.intents {
		out = append(out, pi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// RefundEvent is refund.created for a payment.
func (s *Stripe) RefundEvent(paymentIntent string) Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.event("refund.created", map[string]any{"id": s.id("re"), "object": "refund", "payment_intent": paymentIntent, "status": "succeeded"})
}

// ChargeRefundedEvent is charge.refunded for a payment.
func (s *Stripe) ChargeRefundedEvent(paymentIntent string) Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.event("charge.refunded", map[string]any{"id": s.id("ch"), "object": "charge", "payment_intent": paymentIntent, "refunded": true})
}

// DisputeEvent is charge.dispute.created or .closed for a payment.
func (s *Stripe) DisputeEvent(typ, paymentIntent, status string) Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.event(typ, map[string]any{"id": "dp_" + paymentIntent, "object": "dispute", "payment_intent": paymentIntent, "status": status})
}

// SubscriptionPayment is the PaymentIntent that paid the subscription's
// first invoice.
func (s *Stripe) SubscriptionPayment(subscription string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for pi, p := range s.periods {
		if p.Subscription == subscription {
			return pi
		}
	}
	return ""
}

// ChangeCheckout changes a Checkout Session as Stripe has it.
func (s *Stripe) ChangeCheckout(id string, change func(*billing.CheckoutSession)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs, ok := s.checkouts[id]
	if !ok {
		panic("no checkout session " + id)
	}
	cs.Metadata = copyOf(cs.Metadata)
	change(&cs)
	s.checkouts[id] = cs
}

// RechargeCards is the payment method of each automatic charge, in order.
func (s *Stripe) RechargeCards() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.rechargeCards...)
}

var (
	_ billing.Stripe      = (*Stripe)(nil)
	_ stripe.PeriodFinder = (*Stripe)(nil)
)
