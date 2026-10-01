package authz

import "testing"

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
