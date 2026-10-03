package sessions_test

import (
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
)

const newDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

func images(t *testing.T, client dynamic.Interface, id string) map[string]string {
	t.Helper()
	obj, err := client.Resource(sessions.SandboxGVR).Namespace(sessionstest.Namespace).Get(t.Context(), id, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "podTemplate", "spec", "containers")
	got := map[string]string{"annotation": obj.GetAnnotations()[sessions.AnnCanary]}
	for _, c := range containers {
		container := c.(map[string]any)
		got[container["name"].(string)], _ = container["image"].(string)
	}
	return got
}

// A canary session is the blueprint with the digests it was asked to have,
// in the blueprint's own repositories, and nothing else different.
func TestCanaryReplacesOnlyTheDigestsNamed(t *testing.T) {
	store, client := sessionstest.NewPinned(t)

	ctx := sessions.WithImageDigests(t.Context(), map[string]string{"mcp-js": newDigest})
	s, err := store.Create(ctx, "canary", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	got := images(t, client, s.ID)
	if got["mcp-js"] != "registry.test/mcp-js@"+newDigest || got["browser"] != "registry.test/browser@"+sessionstest.OldDigest {
		t.Errorf("images = %v", got)
	}
	if got["annotation"] != "mcp-js="+newDigest {
		t.Errorf("annotation = %q", got["annotation"])
	}

	// Without the option: the blueprint as it is, and no mark.
	plain, err := store.Create(t.Context(), "plain", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	got = images(t, client, plain.ID)
	if got["mcp-js"] != "registry.test/mcp-js@"+sessionstest.OldDigest || got["annotation"] != "" {
		t.Errorf("a session that is not a canary: %v", got)
	}
}

// A warm pod runs the pool's images: a canary session never comes from it.
func TestCanaryIsNeverTakenFromTheWarmPool(t *testing.T) {
	store, client := sessionstest.NewPinned(t)
	sessionstest.PlayClaimController(t, client, "s-warm1")
	store.EnableWarmPool(sessionstest.WarmPoolName, time.Second)

	ctx := sessions.WithImageDigests(t.Context(), map[string]string{"browser": newDigest, "mcp-js": newDigest})
	s, err := store.Create(ctx, "canary", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID == "s-warm1" {
		t.Fatal("the canary session is the warm pod")
	}
	got := images(t, client, s.ID)
	if got["browser"] != "registry.test/browser@"+newDigest || got["mcp-js"] != "registry.test/mcp-js@"+newDigest {
		t.Errorf("images = %v", got)
	}
	if got["annotation"] != "browser="+newDigest+",mcp-js="+newDigest {
		t.Errorf("annotation = %q", got["annotation"])
	}
	claims, err := client.Resource(sessions.ClaimGVR).Namespace(sessionstest.Namespace).List(t.Context(), metav1.ListOptions{})
	if err != nil || len(claims.Items) != 0 {
		t.Errorf("claims = %d, %v: a canary makes none", len(claims.Items), err)
	}
}

// What cannot be honoured creates nothing.
func TestCanaryRefusals(t *testing.T) {
	pinned, pinnedClient := sessionstest.NewPinned(t)
	tagged, taggedClient := sessionstest.New(t) // images by tag, as deploy/local's
	for name, c := range map[string]struct {
		store   *sessions.Store
		client  dynamic.Interface
		digests map[string]string
	}{
		"not a digest":         {pinned, pinnedClient, map[string]string{"browser": "latest"}},
		"an image reference":   {pinned, pinnedClient, map[string]string{"browser": "evil.test/browser@" + newDigest}},
		"no such container":    {pinned, pinnedClient, map[string]string{"sidecar": newDigest}},
		"nothing named":        {pinned, pinnedClient, map[string]string{}},
		"a blueprint of tags":  {tagged, taggedClient, map[string]string{"browser": newDigest}},
		"one good and one bad": {pinned, pinnedClient, map[string]string{"browser": newDigest, "nope": newDigest}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := c.store.Create(sessions.WithImageDigests(t.Context(), c.digests), "canary", "user-1")
			if !errors.Is(err, sessions.ErrCanary) {
				t.Errorf("err = %v, want ErrCanary", err)
			}
			list, lerr := c.client.Resource(sessions.SandboxGVR).Namespace(sessionstest.Namespace).List(t.Context(), metav1.ListOptions{})
			if lerr != nil || len(list.Items) != 0 {
				t.Errorf("sandboxes = %d, %v: nothing is to be made", len(list.Items), lerr)
			}
		})
	}
}
