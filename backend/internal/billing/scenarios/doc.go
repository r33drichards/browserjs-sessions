// Package scenarios holds the scenario tests of
// docs/contracts/billing/testing.md: the card gate, exhaustion with a call
// in flight, disk charges while asleep. They run the real API mux, the real
// proxy and the real sweep over the in-memory fakes of billingtest: no
// Kubernetes API, no Stripe.
package scenarios
