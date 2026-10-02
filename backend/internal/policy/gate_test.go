package policy_test

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/r33drichards/computer-use/backend/internal/policy"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
)

const alice = "alice@example.com"

// counting counts the reads of the store beneath a gate.
type counting struct {
	policy.SessionStore
	gets int
}

func (c *counting) Get(ctx context.Context, id string) (sessions.Session, error) {
	c.gets++
	return c.SessionStore.Get(ctx, id)
}

func TestGateOffIsTheStoreItself(t *testing.T) {
	store, _ := sessionstest.New(t) // policies not enabled
	if policy.New(store, nil) != nil {
		t.Fatal("handlers for a store without policies")
	}
	if got := policy.Gate(store, policy.New(store, nil)); got != policy.SessionStore(store) {
		t.Errorf("Gate with policies off = %T, want the store", got)
	}
}

// A session is not running, to whoever asks the gated store, until its
// first policy is in force; after that it is, whatever its policy's state.
func TestGatedSessionRunsOnlyOnceItsFirstPolicyIsInForce(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.NewWithPolicies(t)
	store.EnablePolicies(client, sessionstest.Namespace, policy.Unrestricted())
	gated := policy.Gate(store, policy.New(store, nil))

	s, err := store.Create(ctx, "work", alice)
	if err != nil {
		t.Fatal(err)
	}
	state := func() (sessions.State, string) {
		t.Helper()
		got, err := gated.Get(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.State, got.Message
	}

	// The pod is not up: the gate has nothing to say.
	if got, _ := state(); got != sessions.Starting {
		t.Errorf("before the pod is ready: %s", got)
	}
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Ready("10.0.0.7"))
	if plain, _ := store.Get(ctx, s.ID); plain.State != sessions.Running {
		t.Fatalf("the store says %s", plain.State)
	}
	// The pod is up; the operator has not seen the policy.
	if got, why := state(); got != sessions.Starting || why != "waiting for its policy to be loaded" {
		t.Errorf("policy not seen by the operator: %s (%s)", got, why)
	}
	// Compiled, on one OPA replica of two: the other would still deny.
	sessionstest.SetPolicyStatus(t, client, s.ID, sessionstest.PolicyCompiled(1, true))
	if got, _ := state(); got != sessions.Starting {
		t.Errorf("policy on one replica of two: %s", got)
	}
	// A first policy that does not compile is not in force either.
	sessionstest.SetPolicyStatus(t, client, s.ID, sessionstest.PolicyRejected(1, true))
	if got, why := state(); got != sessions.Starting || why != "its policy does not compile" {
		t.Errorf("first policy rejected: %s (%s)", got, why)
	}
	sessionstest.SetPolicyStatus(t, client, s.ID, sessionstest.PolicyReady(1))
	if got, _ := state(); got != sessions.Running {
		t.Errorf("policy on every replica: %s", got)
	}

	// An edit of a running session does not make it unready: the policy
	// before it stays in force while the new one loads, or fails to compile.
	for name, status := range map[string]map[string]any{
		"loading": sessionstest.PolicyCompiled(2, false), "rejected": sessionstest.PolicyRejected(2, false),
	} {
		sessionstest.SetPolicyStatus(t, client, s.ID, status)
		if got, _ := state(); got != sessions.Running {
			t.Errorf("after an edit that is %s: %s", name, got)
		}
	}
}

// Once in force, always in force for that session: the policy is not read
// again. The same ID as another session is another matter.
func TestGateRemembersASessionNotAnID(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.NewWithPolicies(t)
	store.EnablePolicies(client, sessionstest.Namespace, policy.Unrestricted())
	under := &counting{SessionStore: store}
	handlers := policy.New(store, nil)
	gated := policy.Gate(under, handlers)

	s, err := store.Create(ctx, "work", alice)
	if err != nil {
		t.Fatal(err)
	}
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Ready("10.0.0.7"))
	sessionstest.SetPolicyStatus(t, client, s.ID, sessionstest.PolicyReady(1))
	if got, err := gated.Get(ctx, s.ID); err != nil || got.State != sessions.Running {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	// The policy is deleted behind the backend. A session seen in force is
	// not asked about again; one not seen yet would be starting.
	if err := store.Policies().Delete(ctx, s.ID, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, err := gated.Get(ctx, s.ID); err != nil || got.State != sessions.Running {
		t.Errorf("a session seen in force: %+v, %v", got, err)
	}
	if got, err := policy.Gate(store, handlers).Get(ctx, s.ID); err != nil || got.State != sessions.Starting {
		t.Errorf("the same session to a gate that has not seen it: %+v, %v", got, err)
	}

	// Asleep, stopped, missing: the store's answer, untouched.
	if _, err := gated.Get(ctx, "s-nosuchsess"); !errors.Is(err, sessions.ErrNotFound) {
		t.Errorf("a session that does not exist: %v", err)
	}
	if under.gets != 3 {
		t.Errorf("%d reads of the store for 3 of the gate", under.gets)
	}
}

// A session that predates policies (its mcp-js does not ask OPA) has none
// and waits for none.
func TestGateLeavesASessionThatAsksNoPolicyAlone(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)
	store.EnablePolicies(client, sessionstest.Namespace, policy.Unrestricted())
	gated := policy.Gate(store, policy.New(store, nil))
	s, err := store.Create(ctx, "old", alice)
	if err != nil {
		t.Fatal(err)
	}
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Ready("10.0.0.7"))
	if got, err := gated.Get(ctx, s.ID); err != nil || got.State != sessions.Running {
		t.Errorf("Get = %+v, %v", got, err)
	}
}
