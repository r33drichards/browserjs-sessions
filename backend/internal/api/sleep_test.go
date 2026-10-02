package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/r33drichards/computer-use/backend/internal/api"
	"github.com/r33drichards/computer-use/backend/internal/authz"
	"github.com/r33drichards/computer-use/backend/internal/billing"
	"github.com/r33drichards/computer-use/backend/internal/billing/billingtest"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
)

// newSnapshotFixture is newFixture on a cluster with Pod Snapshots.
func newSnapshotFixture(t *testing.T) (*fixture, *sessionstest.GKE) {
	store, client, gke := sessionstest.NewWithSnapshots(t, sessions.SnapshotOptions{Timeout: 50 * time.Millisecond})
	az := &faulty{Checker: authz.NewOwners(store, 0)}
	mux := http.NewServeMux()
	a := api.New(store, az, sessionstest.URLs(), 2)
	a.Register(mux)
	return &fixture{t: t, handler: mux, api: a, store: store, client: client, faults: az}, gke
}

func (f *fixture) sandbox(id string) *unstructured.Unstructured {
	f.t.Helper()
	obj, err := f.client.Resource(sessions.SandboxGVR).Namespace(sessionstest.Namespace).Get(f.t.Context(), id, metav1.GetOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
	return obj
}

// running creates a session of alice's whose pod runs.
func (f *fixture) running() string {
	f.t.Helper()
	id := decode[session](f.t, f.do(alice, "POST", "/api/sessions", `{"name":"a"}`)).ID
	sessionstest.SetStatus(f.t, f.client, id, sessionstest.Ready("10.0.0.7"))
	return id
}

func operatingMode(obj *unstructured.Unstructured) string {
	m, _, _ := unstructured.NestedString(obj.Object, "spec", "operatingMode")
	return m
}

func pool(obj *unstructured.Unstructured) string {
	sel, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "podTemplate", "spec", "nodeSelector")
	return sel[sessions.LabelPool]
}

// A user's sleep is the idle path, asked for: a snapshot, then the suspend,
// recorded as theirs. A second sleep changes nothing, and a wake runs the
// session again from that snapshot.
func TestSleepThenWakeRestoresFromTheSnapshot(t *testing.T) {
	f, _ := newSnapshotFixture(t)
	id := f.running()
	path := "/api/sessions/" + id

	rec := f.do(alice, "POST", path+"/sleep", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"stateSaved":true`) {
		t.Fatalf("sleep: %d %s", rec.Code, rec.Body)
	}
	obj := f.sandbox(id)
	snap := obj.GetAnnotations()[sessions.AnnSnapshot]
	if operatingMode(obj) != "Suspended" || obj.GetAnnotations()[sessions.AnnStoppedBy] != sessions.StoppedBySleep || snap == "" {
		t.Fatalf("mode %q, annotations %v", operatingMode(obj), obj.GetAnnotations())
	}

	// On its way to sleep, and then asleep: both are a sleep already done.
	for _, settle := range []func(){func() {}, func() { sessionstest.SetStatus(t, f.client, id, sessionstest.Suspended()) }} {
		settle()
		rec = f.do(alice, "POST", path+"/sleep", "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"stateSaved":true`) {
			t.Fatalf("second sleep: %d %s", rec.Code, rec.Body)
		}
	}
	if got := decode[session](t, rec); got.State != sessions.Asleep {
		t.Errorf("state = %s, want asleep", got.State)
	}
	if got := sessionstest.Snapshots(t, f.client); len(got) != 1 || got[0] != snap {
		t.Errorf("snapshots = %v, want only %s", got, snap)
	}

	rec = f.do(alice, "POST", path+"/wake", "")
	if rec.Code != http.StatusOK || decode[session](t, rec).State != sessions.Starting {
		t.Fatalf("wake: %d %s", rec.Code, rec.Body)
	}
	obj = f.sandbox(id)
	if operatingMode(obj) != "Running" || obj.GetAnnotations()[sessions.AnnSnapshot] != snap || pool(obj) != sessionstest.Pool {
		t.Errorf("mode %q, snapshot %q, pool %q: want it restored from %s", operatingMode(obj), obj.GetAnnotations()[sessions.AnnSnapshot], pool(obj), snap)
	}
	if by, set := obj.GetAnnotations()[sessions.AnnStoppedBy]; set {
		t.Errorf("stopped-by = %q after wake", by)
	}
	// Awake already: nothing to do.
	if rec := f.do(alice, "POST", path+"/wake", ""); rec.Code != http.StatusOK {
		t.Errorf("wake of a session that is awake: %d %s", rec.Code, rec.Body)
	}
}

// A snapshot that fails does not stop the sleep, as on the idle path; the
// answer says the state was not saved.
func TestSleepWithoutASnapshotSaysSo(t *testing.T) {
	for name, setUp := range map[string]func(t *testing.T) *fixture{
		"checkpoint fails": func(t *testing.T) *fixture {
			f, gke := newSnapshotFixture(t)
			gke.Fail = true
			return f
		},
		"cluster without snapshots": newFixture,
	} {
		t.Run(name, func(t *testing.T) {
			f := setUp(t)
			id := f.running()
			rec := f.do(alice, "POST", "/api/sessions/"+id+"/sleep", "")
			if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "stateSaved") {
				t.Fatalf("sleep: %d %s", rec.Code, rec.Body)
			}
			obj := f.sandbox(id)
			if operatingMode(obj) != "Suspended" || obj.GetAnnotations()[sessions.AnnStoppedBy] != sessions.StoppedBySleep {
				t.Fatalf("mode %q, annotations %v", operatingMode(obj), obj.GetAnnotations())
			}
			sessionstest.SetStatus(t, f.client, id, sessionstest.Suspended())
			if s, _ := f.store.Get(t.Context(), id); s.State != sessions.Asleep || s.StateSaved {
				t.Errorf("session = %+v, want asleep with no state saved", s)
			}
		})
	}
}

// Only a running session has state to save.
func TestSleepOfASessionThatIsNotRunning(t *testing.T) {
	f, _ := newSnapshotFixture(t)
	id := decode[session](t, f.do(alice, "POST", "/api/sessions", `{"name":"a"}`)).ID
	path := "/api/sessions/" + id
	sleep := func(want string) {
		t.Helper()
		rec := f.do(alice, "POST", path+"/sleep", "")
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("sleep: %d %s, want 409 %q", rec.Code, rec.Body, want)
		}
	}
	sleep("still starting")
	if obj := f.sandbox(id); operatingMode(obj) != "Running" {
		t.Errorf("a refused sleep suspended the session")
	}

	sessionstest.SetStatus(t, f.client, id, sessionstest.Ready("10.0.0.7"))
	if rec := f.do(alice, "PATCH", path, `{"action":"stop"}`); rec.Code != http.StatusOK {
		t.Fatalf("stop: %d", rec.Code)
	}
	sleep("session is stopping")
	sessionstest.SetStatus(t, f.client, id, sessionstest.Suspended())
	sleep("session is stopped")
	if by := f.sandbox(id).GetAnnotations()[sessions.AnnStoppedBy]; by != sessions.StoppedByUser {
		t.Errorf("stopped-by = %q: a sleep took over a stop", by)
	}
	if got := sessionstest.Snapshots(t, f.client); len(got) != 0 {
		t.Errorf("snapshots = %v", got)
	}

	// A stopped session is woken all the same: it starts fresh.
	if rec := f.do(alice, "POST", path+"/wake", ""); rec.Code != http.StatusOK {
		t.Fatalf("wake: %d %s", rec.Code, rec.Body)
	}
	if obj := f.sandbox(id); operatingMode(obj) != "Running" || obj.GetAnnotations()[sessions.AnnSnapshot] != "" {
		t.Errorf("mode %q, snapshot %q", operatingMode(obj), obj.GetAnnotations()[sessions.AnnSnapshot])
	}
}

func TestSleepAndWakeAreTheOwnersAndAdmins(t *testing.T) {
	f := newFixture(t)
	id := f.running()
	for _, action := range []string{"sleep", "wake"} {
		path := "/api/sessions/" + id + "/" + action
		if rec := f.do(bob, "POST", path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("a stranger's %s: %d, want 404", action, rec.Code)
		}
		if rec := f.do(alice, "POST", "/api/sessions/s-missing222/"+action, ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s of a missing session: %d, want 404", action, rec.Code)
		}
	}
	if obj := f.sandbox(id); operatingMode(obj) != "Running" {
		t.Fatal("a stranger put the session to sleep")
	}
	if rec := f.do(root, "POST", "/api/sessions/"+id+"/sleep", ""); rec.Code != http.StatusOK {
		t.Errorf("an admin's sleep: %d %s", rec.Code, rec.Body)
	}
}

// With billing enforced a sleep is never refused, the view says whose sleep
// it was, and the wake route is judged as a resume is.
func TestWakeIsJudgedAndSleepIsNot(t *testing.T) {
	clock := billingtest.NewClock(billingStart)
	store := billingtest.NewSessions(clock)
	b := newBilled(t, store, store)
	b.withCard(alice.Subject)
	id := decode[session](t, b.do(alice, "POST", "/api/sessions", `{"name":"one"}`)).ID
	path := "/api/sessions/" + id

	// The card goes.
	if _, err := b.accounts.Update(t.Context(), billing.AccountName(alice.Subject), func(spec *billing.AccountSpec) error {
		spec.PaymentMethod.Present = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rec := b.do(alice, "POST", path+"/sleep", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"asleep"`) ||
		!strings.Contains(rec.Body.String(), `"stoppedBy":"sleep"`) || !strings.Contains(rec.Body.String(), `"stateSaved":true`) {
		t.Fatalf("sleep: %d %s", rec.Code, rec.Body)
	}
	for _, user := range []struct {
		name string
		as   func() int
	}{
		{"owner", func() int { return b.do(alice, "POST", path+"/wake", "").Code }},
		{"admin", func() int { return b.do(root, "POST", path+"/wake", "").Code }},
	} {
		if code := user.as(); code != http.StatusPaymentRequired {
			t.Errorf("%s's wake with no card: %d, want 402", user.name, code)
		}
	}
	if rec := b.do(alice, "POST", path+"/wake", ""); strings.TrimSpace(rec.Body.String()) != noCard {
		t.Errorf("wake: %s\nwant %s", rec.Body, noCard)
	}
	if s, _ := store.Get(t.Context(), id); s.State != sessions.Asleep || store.Snapshots(id) != 1 {
		t.Fatalf("a refused wake changed the session: %+v", s)
	}

	b.withCard(alice.Subject)
	rec = b.do(alice, "POST", path+"/wake", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"running"`) || strings.Contains(rec.Body.String(), "stoppedBy") {
		t.Fatalf("wake with a card: %d %s", rec.Code, rec.Body)
	}
}
