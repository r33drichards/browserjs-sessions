package billingtest

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/billing"
)

// Stripe is billing.Stripe in maps. It holds the state an event refers to
// (a customer's payment methods, a checkout session), because every
// handler re-reads Stripe before acting. Its helpers change that state as
// a customer would at Stripe and return the event Stripe would send, for
// the test to sign and post.
type Stripe struct {
	clock billing.Clock

	mu        sync.Mutex
	n         int
	customers map[string]*customer
	methods   map[string]billing.PaymentMethod // Customer is "" once detached
	checkouts map[string]billing.CheckoutSession
	subs      map[string]billing.Subscription
	intents   map[string]billing.PaymentIntent
	// Down makes every call fail, as an unreachable Stripe does.
	Down bool
}

type customer struct {
	params         billing.CustomerParams
	defaultMethod  string
	idempotencyKey string
}

func NewStripe(clock billing.Clock) *Stripe {
	return &Stripe{clock: clock, customers: map[string]*customer{}, methods: map[string]billing.PaymentMethod{},
		checkouts: map[string]billing.CheckoutSession{}, subs: map[string]billing.Subscription{}, intents: map[string]billing.PaymentIntent{}}
}

var errStripeDown = errors.New("stripe: connection refused")

func (s *Stripe) id(prefix string) string {
	s.n++
	return prefix + strconv.Itoa(s.n)
}

func (s *Stripe) CreateCustomer(_ context.Context, p billing.CustomerParams) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Down {
		return "", errStripeDown
	}
	// Idempotency-Key: customer-<ownerHash>.
	for id, c := range s.customers {
		if c.idempotencyKey == "customer-"+p.OwnerHash {
			return id, nil
		}
	}
	id := s.id("cus_T")
	s.customers[id] = &customer{params: p, idempotencyKey: "customer-" + p.OwnerHash}
	return id, nil
}

func (s *Stripe) CreateCheckout(_ context.Context, p billing.CheckoutParams) (billing.CheckoutSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Down {
		return billing.CheckoutSession{}, errStripeDown
	}
	if _, ok := s.customers[p.Customer]; !ok {
		return billing.CheckoutSession{}, fmt.Errorf("stripe: no such customer %q", p.Customer)
	}
	id := s.id("cs_test_")
	cs := billing.CheckoutSession{
		ID: id, URL: "https://checkout.stripe.test/c/pay/" + id, Mode: p.Mode, Status: "open",
		Customer: p.Customer, ClientReferenceID: p.Account, Created: s.clock.Now(),
		Metadata: map[string]string{"account": p.Account, "kind": p.Kind},
	}
	if p.Item != "" {
		cs.Metadata["item"] = p.Item
	}
	if p.Mode == billing.ModeSetup {
		cs.PaymentStatus = "no_payment_required"
	} else {
		cs.PaymentStatus = "unpaid"
	}
	s.checkouts[id] = cs
	return cs, nil
}

func (s *Stripe) CreatePortal(_ context.Context, customer, _ string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Down {
		return "", errStripeDown
	}
	return "https://billing.stripe.test/p/session/" + customer, nil
}

func (s *Stripe) CreateRecharge(_ context.Context, p billing.RechargeParams) (billing.PaymentIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Down {
		return billing.PaymentIntent{}, errStripeDown
	}
	for _, pi := range s.intents {
		if pi.Metadata["idempotency_key"] == p.IdempotencyKey {
			return pi, nil
		}
	}
	pi := billing.PaymentIntent{ID: s.id("pi_T"), Customer: p.Customer, Status: "succeeded", AmountCents: p.AmountCents, Created: s.clock.Now(),
		Metadata: map[string]string{"account": p.Account, "kind": billing.KindRecharge, "item": p.Item, "idempotency_key": p.IdempotencyKey}}
	s.intents[pi.ID] = pi
	return pi, nil
}

func (s *Stripe) Checkout(_ context.Context, id string) (billing.CheckoutSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Down {
		return billing.CheckoutSession{}, errStripeDown
	}
	cs, ok := s.checkouts[id]
	if !ok {
		return billing.CheckoutSession{}, billing.ErrNotFound
	}
	return cs, nil
}

// PaymentMethods lists the customer's attached payment methods, oldest
// first.
func (s *Stripe) PaymentMethods(_ context.Context, customer string) ([]billing.PaymentMethod, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Down {
		return nil, "", errStripeDown
	}
	c, ok := s.customers[customer]
	if !ok {
		return nil, "", billing.ErrNotFound
	}
	var out []billing.PaymentMethod
	for _, pm := range s.methods {
		if pm.Customer == customer {
			out = append(out, pm)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.Before(out[j].Created)
		}
		return out[i].ID < out[j].ID
	})
	return out, c.defaultMethod, nil
}

func (s *Stripe) Subscription(_ context.Context, id string) (billing.Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Down {
		return billing.Subscription{}, errStripeDown
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
	if s.Down {
		return billing.PaymentIntent{}, errStripeDown
	}
	pi, ok := s.intents[id]
	if !ok {
		return billing.PaymentIntent{}, billing.ErrNotFound
	}
	return pi, nil
}

// Prices gives every lookup key a price: the setup command has been run.
func (s *Stripe) Prices(_ context.Context, lookupKeys []string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Down {
		return nil, errStripeDown
	}
	out := map[string]string{}
	for _, key := range lookupKeys {
		out[key] = "price_" + key
	}
	return out, nil
}

func (s *Stripe) CancelSubscription(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Down {
		return errStripeDown
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
	if s.Down {
		return errStripeDown
	}
	return s.detach(id)
}

func (s *Stripe) detach(id string) error {
	pm, ok := s.methods[id]
	if !ok {
		return billing.ErrNotFound
	}
	if c := s.customers[pm.Customer]; c != nil && c.defaultMethod == id {
		c.defaultMethod = ""
	}
	pm.Customer = ""
	s.methods[id] = pm
	return nil
}

func (s *Stripe) Subscriptions(_ context.Context, customer string) ([]billing.Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Down {
		return nil, errStripeDown
	}
	var out []billing.Subscription
	for _, sub := range s.subs {
		if sub.Customer == customer {
			out = append(out, sub)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Stripe) Checkouts(_ context.Context, customer string, since time.Time) ([]billing.CheckoutSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Down {
		return nil, errStripeDown
	}
	var out []billing.CheckoutSession
	for _, cs := range s.checkouts {
		if cs.Customer == customer && !cs.Created.Before(since) {
			out = append(out, cs)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Stripe) PaymentIntents(_ context.Context, customer string, since time.Time) ([]billing.PaymentIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Down {
		return nil, errStripeDown
	}
	var out []billing.PaymentIntent
	for _, pi := range s.intents {
		if pi.Customer == customer && !pi.Created.Before(since) {
			out = append(out, pi)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Card is a card a customer saves.
type Card struct {
	Fingerprint string
	Funding     string // credit if empty
	Wallet      string // "apple_pay", "google_pay", "" for none
	Brand       string
	Last4       string
}

// Event is an event as Stripe sends it.
type Event struct {
	ID       string `json:"id"`
	Object   string `json:"object"`
	Type     string `json:"type"`
	Created  int64  `json:"created"`
	Livemode bool   `json:"livemode"`
	Data     struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

// Body is the event's JSON: what is signed and posted.
func (e Event) Body() []byte {
	raw, err := json.Marshal(e)
	if err != nil {
		panic(err)
	}
	return raw
}

func (s *Stripe) event(typ string, object any) Event {
	raw, err := json.Marshal(object)
	if err != nil {
		panic(err)
	}
	e := Event{ID: s.id("evt_T"), Object: "event", Type: typ, Created: s.clock.Now().Unix()}
	e.Data.Object = raw
	return e
}

// methodObject is a PaymentMethod as an event carries it.
func methodObject(pm billing.PaymentMethod) map[string]any {
	card := map[string]any{"fingerprint": pm.Fingerprint, "funding": pm.Funding, "brand": pm.Brand, "last4": pm.Last4, "wallet": nil}
	if pm.Wallet != "" {
		card["wallet"] = map[string]any{"type": pm.Wallet}
	}
	var customer any
	if pm.Customer != "" {
		customer = pm.Customer
	}
	return map[string]any{"id": pm.ID, "object": "payment_method", "type": "card", "customer": customer, "card": card}
}

func (s *Stripe) attach(customerID string, card Card) billing.PaymentMethod {
	if card.Funding == "" {
		card.Funding = "credit"
	}
	pm := billing.PaymentMethod{ID: s.id("pm_T"), Customer: customerID, Type: "card", Fingerprint: card.Fingerprint,
		Funding: card.Funding, Wallet: card.Wallet, Brand: card.Brand, Last4: card.Last4, Created: s.clock.Now()}
	s.methods[pm.ID] = pm
	return pm
}

// AttachCard saves a card to a customer, as the portal does, and returns
// it with the payment_method.attached Stripe would send.
func (s *Stripe) AttachCard(customerID string, card Card) (billing.PaymentMethod, Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.customers[customerID]; !ok {
		panic("billingtest: AttachCard to an unknown customer " + customerID)
	}
	pm := s.attach(customerID, card)
	return pm, s.event("payment_method.attached", methodObject(pm))
}

// MethodEvent is an event of the given type about a payment method as it is
// now: a replay, or an event that arrives late.
func (s *Stripe) MethodEvent(typ, id string) Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	pm, ok := s.methods[id]
	if !ok {
		panic("billingtest: MethodEvent of an unknown payment method " + id)
	}
	return s.event(typ, methodObject(pm))
}

// DetachCard removes a saved card and returns the payment_method.detached
// Stripe would send: its object has customer null.
func (s *Stripe) DetachCard(id string) Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.detach(id); err != nil {
		panic("billingtest: DetachCard: " + err.Error())
	}
	return s.event("payment_method.detached", methodObject(s.methods[id]))
}

// CompleteCheckout is the customer finishing a Checkout: in setup mode
// card is saved; in payment mode the payment is made (and card saved, if
// one is given). It returns the checkout.session.completed Stripe would
// send.
func (s *Stripe) CompleteCheckout(id string, card *Card) Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs, ok := s.checkouts[id]
	if !ok {
		panic("billingtest: CompleteCheckout of an unknown session " + id)
	}
	if card != nil {
		s.attach(cs.Customer, *card)
	}
	cs.Status = "complete"
	if cs.Mode == billing.ModePayment {
		cs.PaymentStatus = "paid"
		pi := billing.PaymentIntent{ID: s.id("pi_T"), Customer: cs.Customer, Status: "succeeded", Created: s.clock.Now(), Metadata: cs.Metadata}
		s.intents[pi.ID] = pi
		cs.PaymentIntent = pi.ID
	}
	s.checkouts[id] = cs
	return s.event("checkout.session.completed", map[string]any{
		"id": cs.ID, "object": "checkout.session", "mode": cs.Mode, "status": cs.Status, "payment_status": cs.PaymentStatus,
		"customer": cs.Customer, "client_reference_id": cs.ClientReferenceID, "metadata": cs.Metadata,
	})
}

// CheckoutSessions is every Checkout Session made, by ID.
func (s *Stripe) CheckoutSessions() []billing.CheckoutSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]billing.CheckoutSession, 0, len(s.checkouts))
	for _, cs := range s.checkouts {
		out = append(out, cs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// WebhookSecret is the made-up signing secret tests sign their events
// with.
const WebhookSecret = "whsec_test"

// Sign is the Stripe-Signature header for body, signed with secret at t:
// an HMAC-SHA256 of "<timestamp>.<body>".
func Sign(secret string, body []byte, t time.Time) string {
	stamp := strconv.FormatInt(t.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(stamp + "."))
	mac.Write(body)
	return "t=" + stamp + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}
