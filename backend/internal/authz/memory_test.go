package authz

import (
	"fmt"
	"sync"
	"testing"
)

func TestMemoryOwnerAdminStranger(t *testing.T) {
	ctx := t.Context()
	a := NewMemory()
	owner, admin, stranger := "u-owner", "u-admin", "u-stranger"
	if err := a.SetAdmin(ctx, admin, true); err != nil {
		t.Fatal(err)
	}
	if err := a.AddSession(ctx, "s-1", owner); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		user string
		perm Permission
		want bool
	}{
		{owner, View, true}, {owner, Manage, true},
		{admin, View, true}, {admin, Manage, true},
		{stranger, View, false}, {stranger, Manage, false},
	}
	for _, c := range cases {
		got, err := a.Check(ctx, c.user, "s-1", c.perm)
		if err != nil || got != c.want {
			t.Errorf("Check(%s,%s) = %v,%v; want %v", c.user, c.perm, got, err, c.want)
		}
	}

	// Unknown session: nobody, not even an admin, is allowed.
	if ok, _ := a.Check(ctx, admin, "s-missing", View); ok {
		t.Error("check on a missing session must be false")
	}

	// Removing the session revokes access; revoking admin does too.
	_ = a.RemoveSession(ctx, "s-1")
	if ok, _ := a.Check(ctx, owner, "s-1", View); ok {
		t.Error("owner still allowed after session removal")
	}
	_ = a.AddSession(ctx, "s-2", owner)
	_ = a.SetAdmin(ctx, admin, false)
	if ok, _ := a.Check(ctx, admin, "s-2", View); ok {
		t.Error("former admin still allowed")
	}
}

// A viewer may look but not touch. Only Memory can make one today: it is
// what lets other packages' tests tell View from Manage.
func TestMemoryViewer(t *testing.T) {
	ctx := t.Context()
	a := NewMemory()
	if err := a.AddSession(ctx, "s-1", "u-owner"); err != nil {
		t.Fatal(err)
	}
	if err := a.AddViewer(ctx, "s-1", "u-viewer"); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.Check(ctx, "u-viewer", "s-1", View); !ok || err != nil {
		t.Errorf("viewer View = %v, %v; want true", ok, err)
	}
	if ok, err := a.Check(ctx, "u-viewer", "s-1", Manage); ok || err != nil {
		t.Errorf("viewer Manage = %v, %v; want false", ok, err)
	}

	if err := a.AddViewer(ctx, "s-missing", "u-viewer"); err == nil {
		t.Error("AddViewer on an unknown session must fail")
	}
	if err := a.AddViewer(ctx, "s-1", ""); err == nil {
		t.Error("AddViewer with an empty user must fail")
	}
	// Viewers go with the session.
	_ = a.RemoveSession(ctx, "s-1")
	_ = a.AddSession(ctx, "s-1", "u-owner")
	if ok, _ := a.Check(ctx, "u-viewer", "s-1", View); ok {
		t.Error("viewer survived the session being removed and re-added")
	}
}

func TestMemoryFailsClosed(t *testing.T) {
	ctx := t.Context()
	a := NewMemory()
	if err := a.AddSession(ctx, "s-1", "u-owner"); err != nil {
		t.Fatal(err)
	}
	_ = a.SetAdmin(ctx, "u-admin", true)

	for _, p := range []Permission{"", "can_delete", "CAN_VIEW"} {
		for _, user := range []string{"u-owner", "u-admin"} {
			if ok, err := a.Check(ctx, user, "s-1", p); ok || err != nil {
				t.Errorf("Check(%s, %q) = %v, %v; want false for an unknown permission", user, p, ok, err)
			}
		}
	}

	if err := a.AddSession(ctx, "s-2", ""); err == nil {
		t.Error("AddSession with an empty owner must fail")
	}
	if err := a.AddSession(ctx, "", "u-owner"); err == nil {
		t.Error("AddSession with an empty session ID must fail")
	}
	// An empty user is nobody, even if it were somehow an admin.
	_ = a.SetAdmin(ctx, "", true)
	for _, id := range []string{"s-1", "s-2", ""} {
		if ok, _ := a.Check(ctx, "", id, View); ok {
			t.Errorf("empty user allowed on %q", id)
		}
	}
}

func TestMemoryConcurrentUse(t *testing.T) {
	ctx := t.Context()
	a := NewMemory()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			id := fmt.Sprintf("s-%d", i)
			for range 100 {
				_ = a.AddSession(ctx, id, "u")
				_ = a.AddViewer(ctx, id, "v")
				_, _ = a.Check(ctx, "v", id, View)
				_ = a.SetAdmin(ctx, "u", i%2 == 0)
				_ = a.RemoveSession(ctx, id)
			}
		})
	}
	wg.Wait()
}
