package sessions_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"

	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions/sessionstest"
)

func TestCreateRendersBlueprint(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)

	s, err := store.Create(ctx, "  research  ", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.ID, "s-") || len(s.ID) != 12 {
		t.Errorf("unexpected id %q", s.ID)
	}
	if s.Name != "research" || s.Owner != "user-1" || s.State != sessions.Starting {
		t.Errorf("unexpected session: %+v", s)
	}

	obj, err := client.Resource(sessions.SandboxGVR).Namespace(sessionstest.Namespace).Get(ctx, s.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if mode, _, _ := unstructured.NestedString(obj.Object, "spec", "operatingMode"); mode != "Running" {
		t.Errorf("operatingMode = %q", mode)
	}
	containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "podTemplate", "spec", "containers")
	env := containers[1].(map[string]any)["env"].([]any)[0].(map[string]any)
	if want := "https://sessions.example.com/s/" + s.ID; env["value"] != want {
		t.Errorf("public URL env = %v, want %s", env["value"], want)
	}
	// The owner label is also on the pod, for NetworkPolicy and debugging.
	podLabels, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "podTemplate", "metadata", "labels")
	if podLabels[sessions.LabelOwner] != sessions.OwnerLabel("user-1") || podLabels["app"] != "browserjs-session" {
		t.Errorf("pod labels = %v", podLabels)
	}
	if obj.GetLabels()[sessions.LabelOwner] != sessions.OwnerLabel("user-1") || obj.GetAnnotations()[sessions.AnnOwner] != "user-1" {
		t.Errorf("owner label %q, annotation %q", obj.GetLabels()[sessions.LabelOwner], obj.GetAnnotations()[sessions.AnnOwner])
	}
	if vcts, _, _ := unstructured.NestedSlice(obj.Object, "spec", "volumeClaimTemplates"); len(vcts) != 1 {
		t.Errorf("volumeClaimTemplates = %v", vcts)
	}
}

func TestCreateRejectsBadNames(t *testing.T) {
	store, _ := sessionstest.New(t)
	for _, name := range []string{"", "   ", strings.Repeat("x", 64), "two\nlines", "bell\a", "nul\x00", "bad\xffutf8"} {
		if _, err := store.Create(t.Context(), name, "user-1"); !errors.Is(err, sessions.ErrInvalidName) {
			t.Errorf("name %q: err = %v, want ErrInvalidName", name, err)
		}
	}
	// The limit is in characters, not bytes.
	for _, name := range []string{strings.Repeat("é", 63), "调研 🧪", strings.Repeat("x", 63)} {
		if s, err := store.Create(t.Context(), name, "user-1"); err != nil || s.Name != name {
			t.Errorf("name %q: %+v, %v", name, s, err)
		}
	}
	if _, err := store.Create(t.Context(), "fine", ""); !errors.Is(err, sessions.ErrOwnerRequired) {
		t.Errorf("empty owner: err = %v, want ErrOwnerRequired", err)
	}
}

// A subject is whatever the identity provider issues; it need not be a valid
// label value, and must not be able to steer the label selector.
func TestOwnersThatAreNotLabelValues(t *testing.T) {
	ctx := t.Context()
	store, _ := sessionstest.New(t)
	alice, err := store.Create(ctx, "alices", "alice")
	if err != nil {
		t.Fatal(err)
	}

	for _, subject := range []string{"auth0|abc:def@example.com", strings.Repeat("x", 100), "f:6b1c:robert wendt", "-leading.dash-"} {
		s, err := store.Create(ctx, "mine", subject)
		if err != nil {
			t.Fatalf("Create as %q: %v", subject, err)
		}
		if s.Owner != subject {
			t.Errorf("Create as %q: owner = %q", subject, s.Owner)
		}
		mine, err := store.List(ctx, subject)
		if err != nil || len(mine) != 1 || mine[0].ID != s.ID || mine[0].Owner != subject {
			t.Errorf("List(%q) = %+v, %v", subject, mine, err)
		}
		if got, err := store.Get(ctx, s.ID); err != nil || got.Owner != subject {
			t.Errorf("Get: owner = %q, %v", got.Owner, err)
		}
	}

	// Selector syntax in a subject selects nothing but that subject's own.
	for _, subject := range []string{
		"alice," + sessions.LabelOwner, "alice,", "alice)", "x," + sessions.LabelOwner + "!=x", "!" + sessions.LabelOwner, "in (alice)",
	} {
		got, err := store.List(ctx, subject)
		if err != nil || len(got) != 0 {
			t.Errorf("List(%q) = %+v, %v; want nothing", subject, got, err)
		}
	}
	if mine, _ := store.List(ctx, "alice"); len(mine) != 1 || mine[0].ID != alice.ID {
		t.Errorf("List(alice) = %+v", mine)
	}
}

// "Everyone's sessions" is asked for by name, never by an owner that
// happens to be empty.
func TestListRequiresAnOwner(t *testing.T) {
	ctx := t.Context()
	store, _ := sessionstest.New(t)
	for _, owner := range []string{"user-1", "user-2"} {
		if _, err := store.Create(ctx, "a", owner); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := store.List(ctx, ""); !errors.Is(err, sessions.ErrOwnerRequired) || got != nil {
		t.Errorf(`List("") = %+v, %v; want ErrOwnerRequired`, got, err)
	}
	if all, err := store.ListAll(ctx); err != nil || len(all) != 2 {
		t.Errorf("ListAll = %+v, %v", all, err)
	}
}

func TestListGetRenameDelete(t *testing.T) {
	ctx := t.Context()
	store, _ := sessionstest.New(t)
	a, err := store.Create(ctx, "a", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, "b", "user-2"); err != nil {
		t.Fatal(err)
	}

	mine, err := store.List(ctx, "user-1")
	if err != nil || len(mine) != 1 || mine[0].ID != a.ID {
		t.Fatalf("List(user-1) = %+v, %v", mine, err)
	}
	if all, err := store.ListAll(ctx); err != nil || len(all) != 2 {
		t.Errorf("ListAll returned %d, %v", len(all), err)
	}

	if err := store.Rename(ctx, a.ID, "renamed"); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Get(ctx, a.ID); got.Name != "renamed" {
		t.Errorf("name after rename = %q", got.Name)
	}

	if err := store.Delete(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, a.ID); !errors.Is(err, sessions.ErrNotFound) {
		t.Errorf("Get after delete: err = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, a.ID); !errors.Is(err, sessions.ErrNotFound) {
		t.Errorf("second delete: err = %v, want ErrNotFound", err)
	}
}

func TestSuspendAndResume(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)
	s, err := store.Create(ctx, "a", "user-1")
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Suspend(ctx, s.ID, sessions.StoppedByIdle); err != nil {
		t.Fatal(err)
	}
	sessionstest.SetStatus(t, client, s.ID, sessionstest.Suspended())
	if got, _ := store.Get(ctx, s.ID); got.State != sessions.Asleep {
		t.Errorf("state after idle suspend = %s", got.State)
	}

	if err := store.Resume(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	obj, _ := client.Resource(sessions.SandboxGVR).Namespace(sessionstest.Namespace).Get(ctx, s.ID, metav1.GetOptions{})
	if mode, _, _ := unstructured.NestedString(obj.Object, "spec", "operatingMode"); mode != "Running" {
		t.Errorf("operatingMode after resume = %q", mode)
	}
	if _, ok := obj.GetAnnotations()[sessions.AnnStoppedBy]; ok {
		t.Error("stopped-by annotation not cleared on resume")
	}
}

func raw(t *testing.T, client dynamic.Interface, id string) *unstructured.Unstructured {
	t.Helper()
	obj, err := client.Resource(sessions.SandboxGVR).Namespace(sessionstest.Namespace).Get(context.Background(), id, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return obj
}

func mode(obj *unstructured.Unstructured) string {
	m, _, _ := unstructured.NestedString(obj.Object, "spec", "operatingMode")
	return m
}

func writes(client dynamic.Interface) int {
	n := 0
	for _, a := range client.(*dynfake.FakeDynamicClient).Actions() {
		if v := a.GetVerb(); v == "patch" || v == "update" {
			n++
		}
	}
	return n
}

func TestUpdateAppliesNameAndActionTogether(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.New(t)
	s, err := store.Create(ctx, "a", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	before := writes(client)
	name := "  renamed "
	if err := store.Update(ctx, s.ID, &name, sessions.ActionStop); err != nil {
		t.Fatal(err)
	}
	if n := writes(client) - before; n != 1 {
		t.Errorf("rename and stop took %d writes, want 1", n)
	}
	obj := raw(t, client, s.ID)
	if obj.GetAnnotations()[sessions.AnnName] != "renamed" || mode(obj) != "Suspended" ||
		obj.GetAnnotations()[sessions.AnnStoppedBy] != sessions.StoppedByUser {
		t.Errorf("after rename+stop: annotations %v, mode %s", obj.GetAnnotations(), mode(obj))
	}

	if err := store.Update(ctx, s.ID, nil, sessions.ActionResume); err != nil {
		t.Fatal(err)
	}
	obj = raw(t, client, s.ID)
	if _, stopped := obj.GetAnnotations()[sessions.AnnStoppedBy]; stopped || mode(obj) != "Running" || obj.GetAnnotations()[sessions.AnnName] != "renamed" {
		t.Errorf("after resume: annotations %v, mode %s", obj.GetAnnotations(), mode(obj))
	}

	// Nothing is written when any part of the request is bad.
	before = writes(client)
	other := "other"
	if err := store.Update(ctx, s.ID, &other, "explode"); !errors.Is(err, sessions.ErrInvalidAction) {
		t.Errorf("bad action: err = %v, want ErrInvalidAction", err)
	}
	empty := " "
	if err := store.Update(ctx, s.ID, &empty, sessions.ActionStop); !errors.Is(err, sessions.ErrInvalidName) {
		t.Errorf("bad name: err = %v, want ErrInvalidName", err)
	}
	if n := writes(client) - before; n != 0 {
		t.Errorf("rejected updates made %d writes", n)
	}
	obj = raw(t, client, s.ID)
	if obj.GetAnnotations()[sessions.AnnName] != "renamed" || mode(obj) != "Running" {
		t.Errorf("rejected updates changed the session: %v, %s", obj.GetAnnotations(), mode(obj))
	}
	// An update that asks for nothing is not an error and writes nothing.
	if err := store.Update(ctx, s.ID, nil, ""); err != nil || writes(client) != before {
		t.Errorf("empty update: err %v, %d writes", err, writes(client)-before)
	}
}

func TestChangesToAMissingSession(t *testing.T) {
	ctx := t.Context()
	store, _ := sessionstest.New(t)
	const id = "s-aaaaaaaaaa"
	name := "x"
	for what, err := range map[string]error{
		"Rename":       store.Rename(ctx, id, "x"),
		"Suspend user": store.Suspend(ctx, id, sessions.StoppedByUser),
		"Suspend idle": store.Suspend(ctx, id, sessions.StoppedByIdle),
		"Resume":       store.Resume(ctx, id),
		"Wake":         store.Wake(ctx, id),
		"Update":       store.Update(ctx, id, &name, sessions.ActionStop),
	} {
		if !errors.Is(err, sessions.ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", what, err)
		}
	}
	if err := store.Suspend(ctx, id, "boredom"); err == nil || errors.Is(err, sessions.ErrNotFound) {
		t.Errorf("Suspend with an unknown reason: err = %v", err)
	}
}

// The idle sweep must never take over a stop the user asked for: that would
// relabel it as an idle sleep, and the next request would wake the session.
func TestIdleSuspendDoesNotOverrideAUserStop(t *testing.T) {
	ctx := t.Context()
	stoppedByUser := func(t *testing.T, client dynamic.Interface, id string) {
		t.Helper()
		obj := raw(t, client, id)
		if by := obj.GetAnnotations()[sessions.AnnStoppedBy]; by != sessions.StoppedByUser || mode(obj) != "Suspended" {
			t.Errorf("stopped-by = %q, mode %s; want the user's stop intact", by, mode(obj))
		}
	}

	t.Run("user stopped first", func(t *testing.T) {
		store, client := sessionstest.New(t)
		s, _ := store.Create(ctx, "a", "user-1")
		if err := store.Suspend(ctx, s.ID, sessions.StoppedByUser); err != nil {
			t.Fatal(err)
		}
		if err := store.Suspend(ctx, s.ID, sessions.StoppedByIdle); !errors.Is(err, sessions.ErrStateChanged) {
			t.Errorf("idle suspend of a stopped session: err = %v, want ErrStateChanged", err)
		}
		stoppedByUser(t, client, s.ID)
	})

	t.Run("user stops between the sweep's read and its write", func(t *testing.T) {
		store, client := sessionstest.New(t)
		s, _ := store.Create(ctx, "a", "user-1")
		sessionstest.RaceNextGet(t, client, s.ID, sessionstest.UserStop)
		if err := store.Suspend(ctx, s.ID, sessions.StoppedByIdle); !errors.Is(err, sessions.ErrStateChanged) {
			t.Errorf("idle suspend racing a user stop: err = %v, want ErrStateChanged", err)
		}
		stoppedByUser(t, client, s.ID)
	})

	t.Run("an unrelated write in between is retried", func(t *testing.T) {
		store, client := sessionstest.New(t)
		s, _ := store.Create(ctx, "a", "user-1")
		sessionstest.RaceNextGet(t, client, s.ID, func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedMap(obj.Object, sessionstest.Ready("10.0.0.9"), "status")
		})
		if err := store.Suspend(ctx, s.ID, sessions.StoppedByIdle); err != nil {
			t.Fatal(err)
		}
		obj := raw(t, client, s.ID)
		if obj.GetAnnotations()[sessions.AnnStoppedBy] != sessions.StoppedByIdle || mode(obj) != "Suspended" {
			t.Errorf("stopped-by = %q, mode %s", obj.GetAnnotations()[sessions.AnnStoppedBy], mode(obj))
		}
		if ips, _, _ := unstructured.NestedStringSlice(obj.Object, "status", "podIPs"); len(ips) != 1 {
			t.Error("the suspend overwrote the concurrent status write")
		}
	})

	t.Run("idle first, then the user stops: the user's stop wins", func(t *testing.T) {
		store, client := sessionstest.New(t)
		s, _ := store.Create(ctx, "a", "user-1")
		if err := store.Suspend(ctx, s.ID, sessions.StoppedByIdle); err != nil {
			t.Fatal(err)
		}
		if err := store.Suspend(ctx, s.ID, sessions.StoppedByUser); err != nil {
			t.Fatal(err)
		}
		stoppedByUser(t, client, s.ID)
	})
}

// Wake is the on-demand resume: it applies only to a session that went to
// sleep for being idle, never to one the user stopped.
func TestWakeOnlyResumesAnIdleSleep(t *testing.T) {
	ctx := t.Context()

	t.Run("asleep", func(t *testing.T) {
		store, client := sessionstest.New(t)
		s, _ := store.Create(ctx, "a", "user-1")
		_ = store.Suspend(ctx, s.ID, sessions.StoppedByIdle)
		if err := store.Wake(ctx, s.ID); err != nil {
			t.Fatal(err)
		}
		obj := raw(t, client, s.ID)
		if _, stopped := obj.GetAnnotations()[sessions.AnnStoppedBy]; stopped || mode(obj) != "Running" {
			t.Errorf("after wake: annotations %v, mode %s", obj.GetAnnotations(), mode(obj))
		}
		// Waking a session that is already awake changes nothing.
		before := writes(client)
		if err := store.Wake(ctx, s.ID); err != nil || writes(client) != before {
			t.Errorf("second wake: err %v, %d writes", err, writes(client)-before)
		}
	})

	t.Run("stopped by the user", func(t *testing.T) {
		store, client := sessionstest.New(t)
		s, _ := store.Create(ctx, "a", "user-1")
		_ = store.Suspend(ctx, s.ID, sessions.StoppedByUser)
		if err := store.Wake(ctx, s.ID); !errors.Is(err, sessions.ErrStateChanged) {
			t.Errorf("err = %v, want ErrStateChanged", err)
		}
		if obj := raw(t, client, s.ID); mode(obj) != "Suspended" || obj.GetAnnotations()[sessions.AnnStoppedBy] != sessions.StoppedByUser {
			t.Errorf("a stopped session was woken: %v, %s", obj.GetAnnotations(), mode(obj))
		}
	})

	t.Run("user stops between the waker's read and its write", func(t *testing.T) {
		store, client := sessionstest.New(t)
		s, _ := store.Create(ctx, "a", "user-1")
		_ = store.Suspend(ctx, s.ID, sessions.StoppedByIdle)
		sessionstest.RaceNextGet(t, client, s.ID, sessionstest.UserStop)
		if err := store.Wake(ctx, s.ID); !errors.Is(err, sessions.ErrStateChanged) {
			t.Errorf("err = %v, want ErrStateChanged", err)
		}
		if obj := raw(t, client, s.ID); mode(obj) != "Suspended" || obj.GetAnnotations()[sessions.AnnStoppedBy] != sessions.StoppedByUser {
			t.Errorf("the user's stop was undone: %v, %s", obj.GetAnnotations(), mode(obj))
		}
	})

	t.Run("the user can still resume their own stop", func(t *testing.T) {
		store, client := sessionstest.New(t)
		s, _ := store.Create(ctx, "a", "user-1")
		_ = store.Suspend(ctx, s.ID, sessions.StoppedByUser)
		if err := store.Resume(ctx, s.ID); err != nil {
			t.Fatal(err)
		}
		if obj := raw(t, client, s.ID); mode(obj) != "Running" {
			t.Errorf("mode after resume = %s", mode(obj))
		}
	})
}
