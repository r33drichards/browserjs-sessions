package scenarios

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/billing"
	"github.com/r33drichards/computer-use/backend/internal/billing/billingtest"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
)

// An address in BILLING_EXEMPT_EMAILS creates a session with no card, and
// is never stopped.
func TestExemptNeedsNoCard(t *testing.T) {
	w := newWorld(t, nil)
	if b := w.billingOf(admin); b.State != billing.StateExempt || b.HasPaymentMethod {
		t.Fatalf("state %s, hasPaymentMethod %v; want exempt, false", b.State, b.HasPaymentMethod)
	}
	id := w.create(admin)
	if s := w.session(id); s.State != sessions.Running || s.Owner != admin {
		t.Fatalf("session %+v", s)
	}
	// With no credit at all, long past any grace.
	w.tick()
	w.clock.Advance(time.Hour)
	w.tick()
	w.sweep()
	if s := w.session(id); s.State != sessions.Running || s.Draining != "" {
		t.Fatalf("an exempt account's session was stopped: %s, draining %q", s.State, s.Draining)
	}
	if res := w.mcp(admin, id); res.Code != http.StatusOK {
		t.Fatalf("MCP call = %d %s", res.Code, res.body())
	}
	// Everyone else still needs a card.
	refusedWith(t, "another user", w.api(user, "POST", "/api/sessions", `{}`), http.StatusPaymentRequired, billing.CodePaymentMethodRequired)
}

// BILLING=meter: accounts are made and decisions computed, but nothing is
// refused and nothing is stopped; what would have been is logged.
func TestMeterModeRefusesNothing(t *testing.T) {
	out := captureLogs(t)
	w := newWorld(t, func(c *billing.Config) { c.Mode = billing.Meter })

	if b := w.billingOf(user); b.Mode != "meter" || b.State != billing.StateNoCard {
		t.Fatalf("mode %s, state %s", b.Mode, b.State)
	}
	id := w.create(user) // no card: it succeeds all the same
	if !strings.Contains(out.String(), "would_refuse") || !strings.Contains(out.String(), "code=payment_method_required") {
		t.Fatalf("no would_refuse line for the create:\n%s", out)
	}

	// The sweep stops nothing either, and says what it would have.
	w.sweep()
	if s := w.session(id); s.State != sessions.Running || s.Draining != "" || w.sessions.Snapshots(id) != 0 {
		t.Fatalf("stopped in meter mode: %s, draining %q", s.State, s.Draining)
	}
	if !strings.Contains(out.String(), "would_stop") || !strings.Contains(out.String(), "reason=payment-method") {
		t.Fatalf("no would_stop line for the session:\n%s", out)
	}

	// Asleep, it wakes on a call, and on resume.
	if err := w.sessions.Sleep(t.Context(), id, sessions.StoppedByIdle, nil); err != nil {
		t.Fatal(err)
	}
	if res := w.mcp(user, id); res.Code != http.StatusOK || w.session(id).State != sessions.Running {
		t.Fatalf("wake = %d %s", res.Code, res.body())
	}
	if res := w.api(user, "PATCH", "/api/sessions/"+id, `{"action":"stop"}`); res.Code != http.StatusOK {
		t.Fatalf("stop = %d", res.Code)
	}
	if res := w.api(user, "PATCH", "/api/sessions/"+id, `{"action":"resume"}`); res.Code != http.StatusOK {
		t.Fatalf("resume = %d %s", res.Code, res.body())
	}
	if got := strings.Count(out.String(), "would_refuse"); got != 3 {
		t.Errorf("%d would_refuse lines, want one each for the create, the wake and the resume:\n%s", got, out)
	}
	// The API's own cap still applies: billing's limit replaces it only
	// when it is enforced.
	for range 4 {
		w.create(user)
	}
	if res := w.api(user, "POST", "/api/sessions", `{}`); res.Code != http.StatusConflict || !strings.Contains(res.body(), "session limit reached") {
		t.Fatalf("sixth session = %d %s, want today's 409", res.Code, res.body())
	}
}

// The account judged is always the session's owner's, whoever asks: an
// admin (who is exempt) waking a user's session spends the user's credit
// and needs the user's card.
func TestTheOwnersAccountIsJudged(t *testing.T) {
	w := newWorld(t, nil)
	card := w.saveCard(user, billingtest.Card{Fingerprint: "fpA"})
	w.tick()
	id := w.create(user)
	w.webhook(w.stripe.DetachCard(card))
	w.sweep()
	if s := w.session(id); s.State != sessions.Asleep {
		t.Fatalf("setup: the session is %s", s.State)
	}

	res := w.api(admin, "PATCH", "/api/sessions/"+id, `{"action":"resume"}`)
	refusedWith(t, "an admin's resume", res, http.StatusPaymentRequired, billing.CodePaymentMethodRequired)
	mcpRefused(t, "an admin's call", w.mcp(admin, id), http.StatusPaymentRequired, "no payment method")
	if s := w.session(id); s.State != sessions.Asleep || w.sessions.Wakes() != 0 {
		t.Fatalf("an admin woke a session whose owner has no card: %s", s.State)
	}
	// The admin's own sessions are another matter.
	w.create(admin)

	// The owner's card back, the admin can wake it.
	_, attached := w.stripe.AttachCard(w.account(user).Spec.StripeCustomerID, billingtest.Card{Fingerprint: "fpB"})
	w.webhook(attached)
	if res := w.api(admin, "PATCH", "/api/sessions/"+id, `{"action":"resume"}`); res.Code != http.StatusOK {
		t.Fatalf("resume with the owner's card back = %d %s", res.Code, res.body())
	}
}

// Reading, stopping, renaming and deleting always work: with no card and
// at zero a user can still see their sessions and remove them.
func TestNoCardCanStillReadStopAndDelete(t *testing.T) {
	w := newWorld(t, func(c *billing.Config) { c.SignupCredit = false })
	card := w.saveCard(user, billingtest.Card{Fingerprint: "fpA"})
	w.grant(user, 3000)
	w.tick()
	first, second := w.create(user), w.create(user)
	w.clock.Advance(time.Minute)
	w.tick() // the balance is gone
	w.webhook(w.stripe.DetachCard(card))
	if b := w.billingOf(user); b.State != billing.StateNoCard || b.BalanceMicros != 0 {
		t.Fatalf("setup: state %s, balance %d", b.State, b.BalanceMicros)
	}

	if res := w.api(user, "GET", "/api/sessions", ""); res.Code != http.StatusOK || !strings.Contains(res.body(), first) {
		t.Fatalf("list = %d %s", res.Code, res.body())
	}
	if res := w.api(user, "GET", "/api/sessions/"+first, ""); res.Code != http.StatusOK {
		t.Fatalf("get = %d", res.Code)
	}
	if res := w.api(user, "PATCH", "/api/sessions/"+first, `{"action":"stop"}`); res.Code != http.StatusOK {
		t.Fatalf("stop = %d %s", res.Code, res.body())
	}
	if v := w.view(user, first); v.State != "stopped" || v.StoppedBy != "user" {
		t.Fatalf("after stop: %+v", v)
	}
	if res := w.api(user, "PATCH", "/api/sessions/"+first, `{"name":"renamed"}`); res.Code != http.StatusOK {
		t.Fatalf("rename = %d %s", res.Code, res.body())
	}
	for _, id := range []string{first, second} {
		if res := w.api(user, "DELETE", "/api/sessions/"+id, ""); res.Code != http.StatusNoContent {
			t.Fatalf("delete = %d %s", res.Code, res.body())
		}
	}
	if w.sessions.Len() != 0 {
		t.Fatalf("%d sessions left", w.sessions.Len())
	}
	// The read routes too.
	for _, path := range []string{"/api/billing", "/api/billing/usage", "/api/billing/catalogue"} {
		if res := w.api(user, "GET", path, ""); res.Code != http.StatusOK {
			t.Errorf("GET %s = %d %s", path, res.Code, res.body())
		}
	}
}

// A blocked account can do nothing, and its sessions are put to sleep at
// once and stay stopped.
func TestBlockedAccount(t *testing.T) {
	w := newWorld(t, nil)
	w.saveCard(user, billingtest.Card{Fingerprint: "fpA"})
	w.tick()
	id := w.create(user)
	if _, err := w.accounts.Update(t.Context(), billing.AccountName(user), func(spec *billing.AccountSpec) error {
		spec.Blocked = &billing.Blocked{Reason: "abuse"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	refusedWith(t, "create", w.api(user, "POST", "/api/sessions", `{}`), http.StatusForbidden, billing.CodeAccountBlocked)
	refusedWith(t, "GET /api/billing", w.api(user, "GET", "/api/billing", ""), http.StatusForbidden, billing.CodeAccountBlocked)
	w.sweep()
	if s := w.session(id); s.State != sessions.Stopped || s.StoppedBy != sessions.StoppedByBlocked || w.sessions.Snapshots(id) != 1 {
		t.Fatalf("%s, stopped by %q, %d snapshots; want stopped, blocked, one", s.State, s.StoppedBy, w.sessions.Snapshots(id))
	}
	refusedWith(t, "resume", w.api(user, "PATCH", "/api/sessions/"+id, `{"action":"resume"}`), http.StatusForbidden, billing.CodeAccountBlocked)
}

// The limits of rows 9 to 12, as their callers see them.
func TestLimits(t *testing.T) {
	active := func(t *testing.T, with func(*billing.Config)) *world {
		w := newWorld(t, with)
		w.saveCard(user, billingtest.Card{Fingerprint: "fpA"})
		w.tick()
		return w
	}

	t.Run("session_limit and awake_limit are the plan's", func(t *testing.T) {
		w := active(t, nil) // pay as you go: 3 sessions, 2 awake
		a, b := w.create(user), w.create(user)
		res := w.api(user, "POST", "/api/sessions", `{}`)
		if body := refusedWith(t, "a third awake", res, http.StatusConflict, billing.CodeAwakeLimit); body.Limit != 2 {
			t.Errorf("limit = %d, want 2", body.Limit)
		}
		if res := w.api(user, "PATCH", "/api/sessions/"+a, `{"action":"stop"}`); res.Code != http.StatusOK {
			t.Fatal(res.body())
		}
		c := w.create(user)
		// Three sessions, two awake: resuming the stopped one is refused.
		refusedWith(t, "resume a third", w.api(user, "PATCH", "/api/sessions/"+a, `{"action":"resume"}`), http.StatusConflict, billing.CodeAwakeLimit)
		if res := w.api(user, "PATCH", "/api/sessions/"+b, `{"action":"stop"}`); res.Code != http.StatusOK {
			t.Fatal(res.body())
		}
		res = w.api(user, "POST", "/api/sessions", `{}`)
		if body := refusedWith(t, "a fourth session", res, http.StatusConflict, billing.CodeSessionLimit); body.Limit != 3 || !strings.Contains(body.Error, "3 sessions") {
			t.Errorf("body %+v", body)
		}
		_ = c
	})

	t.Run("at_capacity counts everyone's", func(t *testing.T) {
		w := active(t, func(c *billing.Config) { c.MaxAwakeSessions = 2 })
		w.create(admin)
		w.create(user)
		res := w.api(user, "POST", "/api/sessions", `{}`)
		refusedWith(t, "a third in the cluster", res, http.StatusServiceUnavailable, billing.CodeAtCapacity)
		if res.Header().Get("Retry-After") != "120" {
			t.Errorf("Retry-After = %q", res.Header().Get("Retry-After"))
		}
	})

	t.Run("rate_limited counts the account's starts in an hour", func(t *testing.T) {
		w := active(t, func(c *billing.Config) { c.WakesPerHour = 3 })
		id := w.create(user)
		for i := range 2 {
			if res := w.api(user, "PATCH", "/api/sessions/"+id, `{"action":"stop"}`); res.Code != http.StatusOK {
				t.Fatal(res.body())
			}
			if res := w.api(user, "PATCH", "/api/sessions/"+id, `{"action":"resume"}`); res.Code != http.StatusOK {
				t.Fatalf("resume %d = %d %s", i, res.Code, res.body())
			}
		}
		if err := w.sessions.Sleep(t.Context(), id, sessions.StoppedByIdle, nil); err != nil {
			t.Fatal(err)
		}
		res := w.mcp(user, id)
		mcpRefusedTransient(t, res, http.StatusTooManyRequests)
		if w.session(id).State != sessions.Asleep {
			t.Fatalf("woken past the limit")
		}
		// An hour on, the starts are forgotten.
		w.clock.Advance(time.Hour + time.Second)
		w.tick()
		if res := w.mcp(user, id); res.Code != http.StatusOK {
			t.Fatalf("after an hour = %d %s", res.Code, res.body())
		}
	})
}

// mcpRefusedTransient checks the MCP form of a refusal a client may retry.
func mcpRefusedTransient(t *testing.T, res response, status int) {
	t.Helper()
	if res.Code != status || res.Header().Get("Retry-After") == "" || res.Header().Get("Content-Type") != "application/json" ||
		!strings.Contains(res.body(), `"code":-32002`) || !strings.Contains(res.body(), billingURL) {
		t.Fatalf("%d %s (Retry-After %q)", res.Code, res.body(), res.Header().Get("Retry-After"))
	}
}

// DELETE /api/account: everything the caller has goes; the Account stays,
// marked, with the record that its card has had the sign-up credit.
func TestDeleteAccount(t *testing.T) {
	w := newWorld(t, nil)
	w.saveCard(user, billingtest.Card{Fingerprint: "fpA"})
	w.tick()
	w.create(user)
	w.create(user)
	theirs := w.create(admin)

	if res := w.api(user, "DELETE", "/api/account", `{"confirm":"someone@else.example"}`); res.Code != http.StatusBadRequest {
		t.Fatalf("a confirm that does not match = %d %s", res.Code, res.body())
	}
	// Stripe unreachable: nothing is deleted.
	w.stripe.Down = true
	res := w.api(user, "DELETE", "/api/account", `{"confirm":"`+user+`"}`)
	if res.Code != http.StatusBadGateway || !strings.Contains(res.body(), billing.CodeStripeUnavailable) || w.sessions.Len() != 3 {
		t.Fatalf("with Stripe down = %d %s; %d sessions", res.Code, res.body(), w.sessions.Len())
	}
	w.stripe.Down = false

	if res := w.api(user, "DELETE", "/api/account", `{"confirm":"`+user+`"}`); res.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", res.Code, res.body())
	}
	if w.sessions.Len() != 1 || w.session(theirs).Owner != admin {
		t.Fatalf("%d sessions left, want only the other user's", w.sessions.Len())
	}
	spec := w.account(user).Spec
	if spec.DeletedAt == nil || spec.SignupCredit == nil || spec.PaymentMethod != nil || spec.Owner != user {
		t.Fatalf("the account after deletion: %+v", spec)
	}
	if methods, _, _ := w.stripe.PaymentMethods(t.Context(), spec.StripeCustomerID); len(methods) != 0 {
		t.Fatalf("%d cards still attached", len(methods))
	}
	if all, left := w.metronome.AllCredits(), w.credits(); len(all) != 1 || len(left) != 0 {
		t.Fatalf("%d credits, %d not archived; want the sign-up credit, archived", len(all), len(left))
	}
	if spec.Credit == nil || !spec.Credit.Exhausted || spec.MetronomeCustomerID == "" {
		t.Fatalf("credit %+v, Metronome customer %q", spec.Credit, spec.MetronomeCustomerID)
	}
	// A deleted account can do nothing.
	refusedWith(t, "create", w.api(user, "POST", "/api/sessions", `{}`), http.StatusForbidden, billing.CodeAccountBlocked)
}
