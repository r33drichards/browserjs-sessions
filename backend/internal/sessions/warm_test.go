package sessions_test

import (
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
)

func TestWarmPoolIsOffByDefault(t *testing.T) {
	store, client := sessionstest.New(t)
	sessionstest.PlayClaimController(t, client, "s-bcdfg")
	s, err := store.Create(t.Context(), "a", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID == "s-bcdfg" || len(sessionstest.Claims(t, client)) != 0 {
		t.Errorf("session %q, %d claims: the pool was used without being enabled", s.ID, len(sessionstest.Claims(t, client)))
	}
}

func TestCreateAdoptsAWarmSandbox(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)
	store.EnableWarmPool(sessionstest.WarmPoolName, time.Second)
	sessionstest.PlayClaimController(t, client, "s-bcdfg")

	before := time.Now().Add(-time.Second)
	s, err := store.Create(ctx, " research ", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	// The session is the pool's Sandbox, under the name the pool gave it.
	if s.ID != "s-bcdfg" || !sessions.ValidID(s.ID) {
		t.Fatalf("session ID = %q, want the warm Sandbox's name", s.ID)
	}
	if s.Name != "research" || s.Owner != "user-1" {
		t.Errorf("unexpected session: %+v", s)
	}
	// Not the day the pool made the Sandbox.
	if s.Created.Before(before) {
		t.Errorf("Created = %v, want the time of adoption", s.Created)
	}

	obj := raw(t, client, s.ID)
	if obj.GetLabels()[sessions.LabelOwner] != sessions.OwnerLabel("user-1") || obj.GetAnnotations()[sessions.AnnOwner] != "user-1" {
		t.Errorf("owner label %q, annotation %q", obj.GetLabels()[sessions.LabelOwner], obj.GetAnnotations()[sessions.AnnOwner])
	}
	if mode(obj) != "Running" {
		t.Errorf("operatingMode = %q", mode(obj))
	}
	mine, err := store.List(ctx, "user-1")
	if err != nil || len(mine) != 1 || mine[0].ID != s.ID {
		t.Errorf("List(user-1) = %+v, %v", mine, err)
	}

	claims := sessionstest.Claims(t, client)
	if len(claims) != 1 {
		t.Fatalf("%d claims, want 1", len(claims))
	}
	claim := claims[0]
	if pool, _, _ := unstructured.NestedString(claim.Object, "spec", "warmPoolRef", "name"); pool != sessionstest.WarmPoolName {
		t.Errorf("claim warmPoolRef = %q", pool)
	}
	// The claim says whose it is before any Sandbox does.
	if claim.GetLabels()[sessions.LabelOwner] != sessions.OwnerLabel("user-1") || claim.GetAnnotations()[sessions.AnnOwner] != "user-1" || claim.GetAnnotations()[sessions.AnnName] != "research" {
		t.Errorf("claim labels %v, annotations %v", claim.GetLabels(), claim.GetAnnotations())
	}
}

// An empty pool: the claim controller cold-starts a Sandbox named after the
// claim, and that is the session.
func TestCreateWithAnEmptyPool(t *testing.T) {
	store, client := sessionstest.New(t)
	store.EnableWarmPool(sessionstest.WarmPoolName, time.Second)
	sessionstest.PlayClaimController(t, client)

	s, err := store.Create(t.Context(), "a", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	claims := sessionstest.Claims(t, client)
	if len(claims) != 1 || claims[0].GetName() != s.ID || len(s.ID) != 12 {
		t.Errorf("session %q, claims %v", s.ID, claims)
	}
	if raw(t, client, s.ID).GetAnnotations()[sessions.AnnOwner] != "user-1" {
		t.Error("the cold-started Sandbox has no owner")
	}
}

// Whatever goes wrong with the pool, a session is still made the old way and
// no claim is left behind to hold a Sandbox nobody owns.
func TestCreateFallsBackToAColdStart(t *testing.T) {
	cases := map[string]func(t *testing.T, client *dynfake.FakeDynamicClient){
		"no controller answers the claim": func(*testing.T, *dynfake.FakeDynamicClient) {},
		"claims are refused": func(_ *testing.T, client *dynfake.FakeDynamicClient) {
			client.PrependReactor("create", sessions.ClaimGVR.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("no such resource")
			})
		},
		"the pool's Sandbox is not named like a session": func(t *testing.T, client *dynfake.FakeDynamicClient) {
			sessionstest.PlayClaimController(t, client, "pool-bcdfg")
		},
		"the Sandbox already has an owner": func(t *testing.T, client *dynfake.FakeDynamicClient) {
			sessionstest.PlayClaimController(t, client, "s-bcdfg")
			res := client.Resource(sessions.SandboxGVR).Namespace(sessionstest.Namespace)
			obj, err := res.Get(t.Context(), "s-bcdfg", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			obj.SetAnnotations(map[string]string{sessions.AnnOwner: "user-2"})
			if _, err := res.Update(t.Context(), obj, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			store, client := sessionstest.New(t)
			store.EnableWarmPool(sessionstest.WarmPoolName, 30*time.Millisecond)
			setup(t, client.(*dynfake.FakeDynamicClient))

			s, err := store.Create(t.Context(), "a", "user-1")
			if err != nil {
				t.Fatal(err)
			}
			if len(s.ID) != 12 || s.Owner != "user-1" {
				t.Errorf("unexpected session: %+v", s)
			}
			if vcts, _, _ := unstructured.NestedSlice(raw(t, client, s.ID).Object, "spec", "volumeClaimTemplates"); len(vcts) != 1 {
				t.Error("the fallback session was not rendered from the blueprint")
			}
			if claims := sessionstest.Claims(t, client); len(claims) != 0 {
				t.Errorf("%d claims left behind", len(claims))
			}
			if got, err := store.Get(t.Context(), "s-bcdfg"); err == nil && got.Owner == "user-1" {
				t.Error("user-1 was given a Sandbox that has an owner")
			}
		})
	}
}

// A Sandbox that came from a claim is the claim's: deleted on its own, the
// claim controller would bind the claim to another one.
func TestDeleteRemovesTheClaim(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)
	store.EnableWarmPool(sessionstest.WarmPoolName, time.Second)
	sessionstest.PlayClaimController(t, client, "s-bcdfg")
	s, err := store.Create(ctx, "a", "user-1")
	if err != nil {
		t.Fatal(err)
	}

	// Also with the pool turned off again since the session was made.
	store.EnableWarmPool("", 0)
	if err := store.Delete(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if claims := sessionstest.Claims(t, client); len(claims) != 0 {
		t.Errorf("%d claims left after delete", len(claims))
	}
	if _, err := store.Get(ctx, s.ID); !errors.Is(err, sessions.ErrNotFound) {
		t.Errorf("Get after delete: err = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, s.ID); !errors.Is(err, sessions.ErrNotFound) {
		t.Errorf("second delete: err = %v, want ErrNotFound", err)
	}
}

// A backend that died between the claim being bound and the Sandbox being
// given its owner left a running Sandbox nobody can see.
func TestRecoverClaimsFinishesAnAdoption(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)
	store.EnableWarmPool(sessionstest.WarmPoolName, time.Second)
	sessionstest.PlayClaimController(t, client, "s-bcdfg")
	claim := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": sessions.ClaimGVR.GroupVersion().String(),
		"kind":       "SandboxClaim",
		"metadata": map[string]any{
			"name":        "s-abcdefghij",
			"labels":      map[string]any{sessions.LabelOwner: sessions.OwnerLabel("user-1")},
			"annotations": map[string]any{sessions.AnnOwner: "user-1", sessions.AnnName: "a"},
		},
		"spec": map[string]any{"warmPoolRef": map[string]any{"name": sessionstest.WarmPoolName}},
	}}
	if _, err := client.Resource(sessions.ClaimGVR).Namespace(sessionstest.Namespace).Create(ctx, claim, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if mine, _ := store.List(ctx, "user-1"); len(mine) != 0 {
		t.Fatalf("List before recovery = %+v", mine)
	}

	if err := store.RecoverClaims(ctx); err != nil {
		t.Fatal(err)
	}
	mine, err := store.List(ctx, "user-1")
	if err != nil || len(mine) != 1 || mine[0].ID != "s-bcdfg" || mine[0].Name != "a" {
		t.Errorf("List after recovery = %+v, %v", mine, err)
	}
}

// A session that came from the pool sleeps to a snapshot, wakes from it and
// is deleted like any other: the snapshot's pin goes on the same Sandbox,
// and delete takes the snapshots, the claim and the Sandbox.
func TestAWarmSessionSleepsToASnapshotAndWakes(t *testing.T) {
	ctx := t.Context()
	store, client, _ := sessionstest.NewWithSnapshots(t, sessions.SnapshotOptions{Timeout: 2 * time.Second})
	store.EnableWarmPool(sessionstest.WarmPoolName, time.Second)
	sessionstest.PlayClaimController(t, client, "s-bcdfg")
	s, err := store.Create(ctx, "a", "user-1")
	if err != nil || s.ID != "s-bcdfg" {
		t.Fatalf("Create = %+v, %v", s, err)
	}
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Ready("10.0.0.7"))

	if err := store.Suspend(ctx, s.ID, sessions.StoppedByIdle); err != nil {
		t.Fatal(err)
	}
	obj := sandbox(t, client, s.ID)
	if mode(obj) != "Suspended" || obj.GetAnnotations()[sessions.AnnSnapshot] == "" || pin(obj) != sessionstest.Pool {
		t.Errorf("asleep: mode %q, snapshot %q, pin %q", mode(obj), obj.GetAnnotations()[sessions.AnnSnapshot], pin(obj))
	}
	if err := store.Wake(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if obj = sandbox(t, client, s.ID); mode(obj) != "Running" || pin(obj) != sessionstest.Pool {
		t.Errorf("awake: mode %q, pin %q", mode(obj), pin(obj))
	}
	if len(sessionstest.Claims(t, client)) != 1 {
		t.Error("the claim did not outlive sleep and wake")
	}

	if err := store.Delete(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if got := sessionstest.Snapshots(t, client); len(got) != 0 {
		t.Errorf("snapshots left after delete: %v", got)
	}
	if len(sessionstest.Claims(t, client)) != 0 {
		t.Error("the claim was left after delete")
	}
	if _, err := store.Get(ctx, s.ID); !errors.Is(err, sessions.ErrNotFound) {
		t.Errorf("session still there: %v", err)
	}
}

// Pool names are short and come round again. A pod is restored from the
// newest snapshot of its Sandbox's name: a pooled Sandbox named like a
// session that was removed without its snapshots would come up as that
// session, memory and all. It is never given to anyone.
func TestCreateRefusesAWarmSandboxThatHasSnapshots(t *testing.T) {
	ctx := t.Context()
	store, client, _ := sessionstest.NewWithSnapshots(t, sessions.SnapshotOptions{Timeout: 2 * time.Second})
	store.EnableWarmPool(sessionstest.WarmPoolName, time.Second)
	sessionstest.PlayClaimController(t, client, "s-bcdfg")
	addSnapshot(t, client, "left-behind", "s-bcdfg")

	s, err := store.Create(ctx, "a", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID == "s-bcdfg" {
		t.Error("user-1 was given a Sandbox that may have been restored from somebody's snapshot")
	}
	if got := sessionstest.Snapshots(t, client); len(got) != 0 {
		t.Errorf("the stale snapshots are still there: %v", got)
	}
	if len(sessionstest.Claims(t, client)) != 0 {
		t.Error("the refused Sandbox's claim was kept")
	}
}
