package scenarios

import (
	"net/http"
	"testing"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/billing"
	"github.com/r33drichards/computer-use/backend/internal/billing/billingtest"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
)

// The body of payment_method_required, byte for byte.
const noCardBody = `{"error":"Add a payment method to create or wake sessions.","code":"payment_method_required","billingUrl":"https://app.example.test/billing"}`

// TestCardGateScenario is scenario 1 of docs/contracts/billing/testing.md,
// the product owner's requirement: a user without a card cannot create a
// session; with a card, create works; the card removed, the session sleeps
// and create fails. One user, BILLING=enforce, the real API mux, the
// webhook handlers posted correctly signed events (Stripe's, and
// Metronome's for each alert a tick returns), the real sweep (run by the
// test), the real Ledger over the fake Metronome.
//
// It runs twice: ending with step e3 (resume in the app), and with its
// variant e4 (the MCP call wakes the session).
func TestCardGateScenario(t *testing.T) {
	for _, ending := range []string{"e3 resume", "e4 MCP call"} {
		t.Run(ending, func(t *testing.T) { cardGate(t, ending == "e4 MCP call") })
	}
}

func cardGate(t *testing.T, endWithMCP bool) {
	w := newWorld(t, nil)
	signup := w.catalogue.SignupCredit.AmountMicros // 5000000

	// a1: a new user has an account with no card and no credit.
	if b := w.billingOf(user); b.State != billing.StateNoCard || b.HasPaymentMethod || b.BalanceMicros != 0 {
		t.Fatalf("a1: state %s, hasPaymentMethod %v, balance %d; want no_card, false, 0", b.State, b.HasPaymentMethod, b.BalanceMicros)
	}

	// a2: without a card, create fails, and nothing is made.
	res := w.api(user, "POST", "/api/sessions", `{}`)
	if res.Code != http.StatusPaymentRequired || res.body() != noCardBody {
		t.Fatalf("a2: %d %s\nwant 402 %s", res.Code, res.body(), noCardBody)
	}
	if w.sessions.Len() != 0 || w.sessions.Creates() != 0 {
		t.Fatalf("a2: %d sessions, Create called %d times", w.sessions.Len(), w.sessions.Creates())
	}
	if grants := w.metronome.AllCredits(); len(grants) != 0 {
		t.Fatalf("a2: grants exist: %+v", grants)
	}

	// b1: a Checkout in setup mode, for the account's customer.
	res = w.api(user, "POST", "/api/billing/checkout", `{}`)
	var checkout struct {
		URL string `json:"url"`
	}
	res.json(t, &checkout)
	checkouts := w.stripe.CheckoutSessions()
	if res.Code != http.StatusOK || checkout.URL == "" || len(checkouts) != 1 {
		t.Fatalf("b1: %d %s, %d checkout sessions", res.Code, res.body(), len(checkouts))
	}
	customer := w.account(user).Spec.StripeCustomerID
	if cs := checkouts[0]; cs.Mode != billing.ModeSetup || customer == "" || cs.Customer != customer {
		t.Fatalf("b1: checkout %+v, the account's customer is %q", cs, customer)
	}

	// b2: card A is saved; the signed checkout.session.completed arrives.
	completed := w.stripe.CompleteCheckout(checkouts[0].ID, &billingtest.Card{Fingerprint: "fpA", Funding: "credit"})
	if res := w.webhook(completed); res.Code != http.StatusOK {
		t.Fatalf("b2: webhook %d %s", res.Code, res.body())
	}
	// "Grant" reads: a credit in the fake Metronome, by its uniqueness key.
	oneGrant := func(step string) {
		t.Helper()
		grants := w.metronome.AllCredits()
		if len(grants) != 1 || grants[0].CustomFields[billing.FieldGrantKey] != "signup/fpA" || grants[0].AmountMicros != 5000000 || grants[0].Archived {
			t.Fatalf("%s: grants %+v, want exactly one, signup/fpA, 5000000", step, grants)
		}
	}
	spec := w.account(user).Spec
	if spec.PaymentMethod == nil || !spec.PaymentMethod.Present || spec.SignupCredit == nil || spec.SignupCredit.State != billing.SignupGranted {
		t.Fatalf("b2: paymentMethod %+v, signupCredit %+v", spec.PaymentMethod, spec.SignupCredit)
	}
	oneGrant("b2")
	cardA := spec.PaymentMethod.IDs[0]

	// b3: the same event again, then payment_method.attached for the card.
	for _, e := range []billingtest.Event{completed, w.stripe.MethodEvent("payment_method.attached", cardA)} {
		if res := w.webhook(e); res.Code != http.StatusOK {
			t.Fatalf("b3: %s: %d %s", e.Type, res.Code, res.body())
		}
	}
	oneGrant("b3")

	// b4: the operator ticks; the credit is in the balance.
	w.tick()
	if b := w.billingOf(user); b.State != billing.StateActive || b.BalanceMicros != signup || b.SignupCredit == nil || b.SignupCredit.State != billing.SignupGranted {
		t.Fatalf("b4: state %s, balance %d, signupCredit %+v", b.State, b.BalanceMicros, b.SignupCredit)
	}

	// b5: with a card, create works.
	res = w.api(user, "POST", "/api/sessions", `{}`)
	var made sessionView
	res.json(t, &made)
	if res.Code != http.StatusCreated || made.State != "running" || made.Owner != user {
		t.Fatalf("b5: %d %s", res.Code, res.body())
	}
	id := made.ID

	// The session is used for a minute; the operator sees it.
	w.clock.Advance(time.Minute)
	w.tick()

	// c1: the card is removed. The event names no customer.
	if res := w.webhook(w.stripe.DetachCard(cardA)); res.Code != http.StatusOK {
		t.Fatalf("c1: webhook %d %s", res.Code, res.body())
	}
	if pm := w.account(user).Spec.PaymentMethod; pm == nil || pm.Present || pm.RemovedAt == nil {
		t.Fatalf("c1: paymentMethod %+v, want present false and removedAt set", pm)
	}
	oneGrant("c1") // untouched

	// c2: the sweep puts the session to sleep, with a snapshot.
	w.sweep()
	if s := w.session(id); w.sessions.Snapshots(id) != 1 || s.State != sessions.Asleep || s.StoppedBy != sessions.StoppedByPaymentMethod {
		t.Fatalf("c2: %d snapshots, %s, stopped by %q", w.sessions.Snapshots(id), s.State, s.StoppedBy)
	}
	if v := w.view(user, id); v.State != "asleep" || v.StoppedBy != "payment-method" {
		t.Fatalf("c2: GET shows %+v", v)
	}

	// c3: the credit is kept, less what was used.
	w.tick()
	charged, _ := w.metronome.Charged(billing.AccountName(user))
	if b := w.billingOf(user); b.State != billing.StateNoCard || charged <= 0 || b.BalanceMicros != signup-charged {
		t.Fatalf("c3: state %s, balance %d, charged %d", b.State, b.BalanceMicros, charged)
	}

	// d1: without a card, create fails again.
	res = w.api(user, "POST", "/api/sessions", `{}`)
	if res.Code != http.StatusPaymentRequired || res.body() != noCardBody {
		t.Fatalf("d1: %d %s\nwant 402 %s", res.Code, res.body(), noCardBody)
	}
	if w.sessions.Len() != 1 {
		t.Fatalf("d1: %d sessions, want still one", w.sessions.Len())
	}

	// d2: and so does resume.
	res = w.api(user, "PATCH", "/api/sessions/"+id, `{"action":"resume"}`)
	refusedWith(t, "d2", res, http.StatusPaymentRequired, billing.CodePaymentMethodRequired)
	if s := w.session(id); s.State != sessions.Asleep {
		t.Fatalf("d2: the session is %s", s.State)
	}

	// d3: and an MCP call does not wake it.
	mcpRefused(t, "d3", w.mcp(user, id), http.StatusPaymentRequired, "no payment method")
	if w.sessions.Wakes() != 0 || w.podCalls.Load() != 0 || w.session(id).State != sessions.Asleep {
		t.Fatalf("d3: Wake called %d times, %d calls reached the pod", w.sessions.Wakes(), w.podCalls.Load())
	}

	// e1: card B is saved. The sign-up credit was decided already.
	_, attached := w.stripe.AttachCard(customer, billingtest.Card{Fingerprint: "fpB"})
	if res := w.webhook(attached); res.Code != http.StatusOK {
		t.Fatalf("e1: webhook %d %s", res.Code, res.body())
	}
	if pm := w.account(user).Spec.PaymentMethod; pm == nil || !pm.Present || pm.RemovedAt != nil {
		t.Fatalf("e1: paymentMethod %+v, want present and removedAt cleared", pm)
	}
	oneGrant("e1")

	// e2: the session is not woken automatically, not even by the sweep.
	w.sweep()
	if v := w.view(user, id); v.State != "asleep" {
		t.Fatalf("e2: the session is %s", v.State)
	}

	if !endWithMCP {
		// e3: resume works.
		res = w.api(user, "PATCH", "/api/sessions/"+id, `{"action":"resume"}`)
		if res.Code != http.StatusOK || w.session(id).State != sessions.Running {
			t.Fatalf("e3: %d %s; the session is %s", res.Code, res.body(), w.session(id).State)
		}
		return
	}
	// e4: the MCP call of d3 wakes the session and is forwarded.
	res = w.mcp(user, id)
	if res.Code != http.StatusOK || w.podCalls.Load() != 1 || w.sessions.Wakes() != 1 || w.session(id).State != sessions.Running {
		t.Fatalf("e4: %d %s; %d calls reached the pod, Wake called %d times, the session is %s",
			res.Code, res.body(), w.podCalls.Load(), w.sessions.Wakes(), w.session(id).State)
	}
}
