package authz_test

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/authz"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions/sessionstest"
)

var (
	alice = auth.User{Subject: "alice@example.com"}
	bob   = auth.User{Subject: "bob@example.com"}
	root  = auth.User{Subject: "root@example.com", Admin: true}
)

// counting is a store that counts its reads and can be made to fail.
type counting struct {
	authz.Store
	gets int
	err  error
}

func (c *counting) Get(ctx context.Context, id string) (sessions.Session, error) {
	c.gets++
	if c.err != nil {
		return sessions.Session{}, c.err
	}
	return c.Store.Get(ctx, id)
}

func TestOwnerOrAdmin(t *testing.T) {
	store, _ := sessionstest.New(t)
	ctx := t.Context()
	s, err := store.Create(ctx, "mine", alice.Subject)
	if err != nil {
		t.Fatal(err)
	}
	owners := authz.NewOwners(store, 0)

	for _, c := range []struct {
		name string
		user auth.User
		id   string
		want bool
	}{
		{"owner", alice, s.ID, true},
		{"admin", root, s.ID, true},
		{"stranger", bob, s.ID, false},
		{"nobody", auth.User{}, s.ID, false},
		{"an admin with no identity", auth.User{Admin: true}, s.ID, false},
		{"owner, different case", auth.User{Subject: "Alice@example.com"}, s.ID, false},
		{"owner, missing session", alice, "s-aaaaaaaaaa", false},
		{"admin, missing session", root, "s-aaaaaaaaaa", false},
		{"admin, malformed ID", root, "pods", false},
	} {
		got, err := owners.Allowed(ctx, c.user, c.id)
		if err != nil || got != c.want {
			t.Errorf("%s: Allowed = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
}

// A Sandbox with no owner on record must not be everybody's, nor that of
// whoever arrives with an empty identity.
func TestSessionWithNoOwnerIsOnlyTheAdmins(t *testing.T) {
	store, client := sessionstest.New(t)
	ctx := t.Context()
	s, _ := store.Create(ctx, "mine", alice.Subject)
	res := client.Resource(sessions.SandboxGVR).Namespace(sessionstest.Namespace)
	obj, err := res.Get(ctx, s.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ann := obj.GetAnnotations()
	delete(ann, sessions.AnnOwner)
	obj.SetAnnotations(ann)
	if _, err := res.Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	owners := authz.NewOwners(store, 0)
	for _, u := range []auth.User{alice, bob, {}} {
		if ok, err := owners.Allowed(ctx, u, s.ID); ok || err != nil {
			t.Errorf("%+v on an ownerless session: %v, %v; want denied", u, ok, err)
		}
	}
	if ok, err := owners.Allowed(ctx, root, s.ID); !ok || err != nil {
		t.Errorf("admin on an ownerless session: %v, %v; want allowed", ok, err)
	}
}

// A cluster that cannot be asked is neither a denial nor a grant.
func TestStoreFailureFailsClosed(t *testing.T) {
	store, _ := sessionstest.New(t)
	s, _ := store.Create(t.Context(), "mine", alice.Subject)
	down := &counting{Store: store, err: errors.New("etcd is down")}
	owners := authz.NewOwners(down, time.Minute)

	for _, u := range []auth.User{alice, bob, root} {
		if ok, err := owners.Allowed(t.Context(), u, s.ID); ok || err == nil {
			t.Errorf("%+v with the cluster down: %v, %v; want false and an error", u, ok, err)
		}
	}
	// Nothing was learned from the failure: once the cluster answers, so
	// does the check.
	down.err = nil
	if ok, err := owners.Allowed(t.Context(), alice, s.ID); !ok || err != nil {
		t.Errorf("owner after the cluster recovered: %v, %v", ok, err)
	}
}

func TestOwnerIsRememberedBriefly(t *testing.T) {
	store, _ := sessionstest.New(t)
	ctx := t.Context()
	s, _ := store.Create(ctx, "mine", alice.Subject)
	reads := &counting{Store: store}
	owners := authz.NewOwners(reads, time.Minute)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	owners.SetClock(func() time.Time { return now })

	for range 5 {
		for user, want := range map[auth.User]bool{alice: true, bob: false, root: true} {
			if ok, err := owners.Allowed(ctx, user, s.ID); ok != want || err != nil {
				t.Fatalf("%+v: %v, %v; want %v", user, ok, err, want)
			}
		}
	}
	if reads.gets != 1 {
		t.Errorf("15 checks read the session %d times, want 1", reads.gets)
	}
	now = now.Add(time.Minute)
	_, _ = owners.Allowed(ctx, alice, s.ID)
	if reads.gets != 2 {
		t.Errorf("a check after the owner aged out read the session %d times in all, want 2", reads.gets)
	}

	// A session that is not there is not remembered as anything.
	before := reads.gets
	for range 3 {
		if ok, _ := owners.Allowed(ctx, root, "s-aaaaaaaaaa"); ok {
			t.Fatal("a missing session was allowed")
		}
	}
	if n := reads.gets - before; n != 3 {
		t.Errorf("3 checks of a missing session read it %d times, want 3", n)
	}

	// With no TTL every check reads.
	reads.gets = 0
	every := authz.NewOwners(reads, 0)
	for range 3 {
		_, _ = every.Allowed(ctx, alice, s.ID)
	}
	if reads.gets != 3 {
		t.Errorf("3 checks with no TTL read the session %d times, want 3", reads.gets)
	}
}
