package sessions_test

import (
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

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
	if podLabels[sessions.LabelOwner] != "user-1" || podLabels["app"] != "browserjs-session" {
		t.Errorf("pod labels = %v", podLabels)
	}
	if vcts, _, _ := unstructured.NestedSlice(obj.Object, "spec", "volumeClaimTemplates"); len(vcts) != 1 {
		t.Errorf("volumeClaimTemplates = %v", vcts)
	}
}

func TestCreateRejectsBadNames(t *testing.T) {
	store, _ := sessionstest.New(t)
	for _, name := range []string{"", "   ", strings.Repeat("x", 64)} {
		if _, err := store.Create(t.Context(), name, "user-1"); !errors.Is(err, sessions.ErrInvalidName) {
			t.Errorf("name %q: err = %v, want ErrInvalidName", name, err)
		}
	}
}

func TestListGetRenameDelete(t *testing.T) {
	ctx := t.Context()
	store, _ := sessionstest.New(t)
	a, _ := store.Create(ctx, "a", "user-1")
	_, _ = store.Create(ctx, "b", "user-2")

	mine, err := store.List(ctx, "user-1")
	if err != nil || len(mine) != 1 || mine[0].ID != a.ID {
		t.Fatalf("List(user-1) = %+v, %v", mine, err)
	}
	if all, _ := store.List(ctx, ""); len(all) != 2 {
		t.Errorf("List(all) returned %d", len(all))
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
	s, _ := store.Create(ctx, "a", "user-1")

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
