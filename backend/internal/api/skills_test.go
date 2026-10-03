package api_test

import (
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/r33drichards/computer-use/backend/internal/api"
	"github.com/r33drichards/computer-use/backend/internal/auth"
	"github.com/r33drichards/computer-use/backend/internal/authz"
	"github.com/r33drichards/computer-use/backend/internal/features"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
)

func TestSkillsFlagSelectsImageUsingAuthenticatedOwner(t *testing.T) {
	store, client := sessionstest.NewPinned(t)
	// A canary must bypass warm adoption: the pool still runs the stable image.
	store.EnableWarmPool("s", 0)
	mux := http.NewServeMux()
	a := api.New(store, authz.NewOwners(store, 0), sessionstest.URLs(), 5)
	digest := "sha256:" + strings.Repeat("2", 64)
	flags, err := features.NewSkills(t.Context(), digest, []string{alice.Subject})
	if err != nil {
		t.Fatal(err)
	}
	a.SetSkillsImage(flags.ImageDigest)
	a.SetCanary([]string{alice.Subject})
	a.Register(mux)
	f := &fixture{t: t, handler: mux, api: a, store: store, client: client}
	for _, c := range []struct {
		name       string
		user       auth.User
		body, want string
	}{
		{"targeted", alice, `{}`, digest},
		{"other user cannot forge identity", bob, `{"email":"alice@example.com","skills":true}`, sessionstest.OldDigest},
		{"explicit canary takes precedence", alice, `{"canary":{"mcp-js":"sha256:` + strings.Repeat("3", 64) + `"}}`, "sha256:" + strings.Repeat("3", 64)},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := f.do(c.user, "POST", "/api/sessions", c.body)
			if rec.Code != http.StatusCreated {
				t.Fatalf("create: %d %s", rec.Code, rec.Body)
			}
			id := decode[session](t, rec).ID
			obj, err := client.Resource(sessions.SandboxGVR).Namespace(sessionstest.Namespace).Get(t.Context(), id, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "podTemplate", "spec", "containers")
			for _, raw := range containers {
				container := raw.(map[string]any)
				if container["name"] == "mcp-js" && container["image"] != "registry.test/mcp-js@"+c.want {
					t.Errorf("image: %v", container["image"])
				}
			}
			if err := store.Delete(t.Context(), id); err != nil {
				t.Fatal(err)
			}
		})
	}
}
