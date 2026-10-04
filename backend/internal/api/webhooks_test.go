package api_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestWebhookSettingsOwnershipScopesAndSecret(t *testing.T) {
	f := newPolicyFixture(t, false)
	id := f.newSession(`{"name":"export"}`)
	path := "/api/sessions/" + id + "/webhook"
	for _, test := range []struct {
		who    string
		method string
		status int
	}{
		{"bob", "GET", 404}, {"bob", "PUT", 404}, {"read", "PUT", 403}, {"write", "GET", 403},
	} {
		u := bob
		if test.who == "read" {
			u = token("sessions:read")
		}
		if test.who == "write" {
			u = token("sessions:write")
		}
		if got := f.do(u, test.method, path, `{"url":"https://example.com"}`); got.Code != test.status {
			t.Fatalf("%+v: %d %s", test, got.Code, got.Body)
		}
	}
	body := `{"url":"https://example.com/hook","batch_size":2,"filter":"","signing_secret":"sixteen-byte-secret"}`
	if rec := f.do(alice, "PUT", path, body); rec.Code != 204 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	rec := f.do(alice, "GET", path, "")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "sixteen-byte-secret") || !strings.Contains(rec.Body.String(), `"has_signing_secret":true`) {
		t.Fatalf("read: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(alice, "PUT", path, `{"url":"https://new.example.com"}`); rec.Code != 204 {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	sent := f.operator.sent("/v1/webhooks/validate")
	if sent["signing_secret"] != "sixteen-byte-secret" {
		t.Fatal("omission did not preserve secret")
	}
	if rec := f.do(alice, "PUT", path, `{"url":"https://bad.example.com","filter":"INVALID"}`); rec.Code != 422 {
		t.Fatalf("bad filter: %d %s", rec.Code, rec.Body)
	}
	rec = f.do(alice, "GET", path, "")
	if !strings.Contains(rec.Body.String(), "new.example.com") {
		t.Fatal("invalid filter replaced existing settings")
	}
	f.operator.setDown(true)
	if rec := f.do(alice, "PUT", path, body); rec.Code != 503 {
		t.Fatalf("unavailable: %d %s", rec.Code, rec.Body)
	}
	f.operator.setDown(false)
	if rec := f.do(alice, "DELETE", path, ""); rec.Code != 204 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	var doc any
	if json.Unmarshal(f.do(alice, "GET", path, "").Body.Bytes(), &doc) != nil || doc != nil {
		t.Fatal("delete left settings")
	}
	obj, err := f.client.Resource(sessions.PolicyGVR).Namespace(sessionstest.Namespace).Get(t.Context(), id, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	source, _, _ := unstructured.NestedString(obj.Object, "spec", "source")
	if source == "" {
		t.Fatal("webhook delete removed enforcement policy")
	}
}
