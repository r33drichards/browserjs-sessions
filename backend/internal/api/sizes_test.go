package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/r33drichards/computer-use/backend/internal/api"
	"github.com/r33drichards/computer-use/backend/internal/authz"
	"github.com/r33drichards/computer-use/backend/internal/billing/billingtest"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
)

// sized is the fixture with a store that has the three sizes, and room for
// two nodes of sessions.
func sized(t *testing.T) *fixture {
	store, client := sessionstest.NewSized(t)
	az := &faulty{Checker: authz.NewOwners(store, 0)}
	mux := http.NewServeMux()
	a := api.New(store, az, sessionstest.URLs(), 20)
	a.Register(mux)
	return &fixture{t: t, handler: mux, api: a, store: store, client: client, faults: az}
}

func TestSizesAreListed(t *testing.T) {
	rec := sized(t).do(alice, "GET", "/api/sizes", "")
	const want = `{"default":"small","sizes":[` +
		`{"name":"small","cpuMillis":1500,"memoryMiB":2048,"warm":true},` +
		`{"name":"medium","cpuMillis":2000,"memoryMiB":5120,"warm":false},` +
		`{"name":"large","cpuMillis":3000,"memoryMiB":10240,"warm":false}]}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Errorf("%d %s\nwant %s", rec.Code, rec.Body, want)
	}
	// A deployment with no sizes file has small alone, and says so.
	rec = newFixture(t).do(alice, "GET", "/api/sizes", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"sizes":[{"name":"small"`) || strings.Contains(rec.Body.String(), "medium") {
		t.Errorf("without sizes: %d %s", rec.Code, rec.Body)
	}
}

func TestCreateWithASize(t *testing.T) {
	f := sized(t)
	for body, want := range map[string]string{
		`{}`:                            "small",
		`{"name":"a"}`:                  "small",
		`{"name":"a","size":"small"}`:   "small",
		`{"name":"a","size":"medium"}`:  "medium",
		`{"name":"big","size":"large"}`: "large",
	} {
		rec := f.do(alice, "POST", "/api/sessions", body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s: %d %s", body, rec.Code, rec.Body)
		}
		created := decode[session](t, rec)
		if created.Size != want || !strings.Contains(rec.Body.String(), `"size":"`+want+`"`) || strings.Contains(rec.Body.String(), "pendingSize") {
			t.Errorf("%s: %s", body, rec.Body)
		}
		// Shown by get and by the list as well.
		if got := decode[session](t, f.do(alice, "GET", "/api/sessions/"+created.ID, "")); got.Size != want {
			t.Errorf("%s: get shows %q", body, got.Size)
		}
		f.do(alice, "DELETE", "/api/sessions/"+created.ID, "")
	}

	rec := f.do(alice, "POST", "/api/sessions", `{"name":"a","size":"huge"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "size must be one of") || !strings.Contains(rec.Body.String(), "medium") {
		t.Errorf("an invalid size: %d %s", rec.Code, rec.Body)
	}
	if list := decode[[]session](t, f.do(alice, "GET", "/api/sessions", "")); len(list) != 0 {
		t.Errorf("an invalid size made %d sessions", len(list))
	}
	// A store with no sizes takes small and nothing else.
	plain := newFixture(t)
	if rec := plain.do(alice, "POST", "/api/sessions", `{"size":"medium"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("medium where there is only small: %d %s", rec.Code, rec.Body)
	}
	if rec := plain.do(alice, "POST", "/api/sessions", `{"size":"small"}`); rec.Code != http.StatusCreated {
		t.Errorf("small where there is only small: %d %s", rec.Code, rec.Body)
	}
}

func TestNoCapacityIsAConflict(t *testing.T) {
	f := sized(t)
	create := func(size, node string) string {
		t.Helper()
		rec := f.do(alice, "POST", "/api/sessions", `{"size":"`+size+`"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s: %d %s", size, rec.Code, rec.Body)
		}
		id := decode[session](t, rec).ID
		sessionstest.SetStatus(t, f.client, id, sessionstest.OnNode("10.0.0.7", node))
		return id
	}
	create("small", "node-1")
	large := create("large", "node-2")

	rec := f.do(alice, "POST", "/api/sessions", `{"name":"second","size":"large"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no capacity for a large session right now") ||
		!strings.Contains(rec.Body.String(), `"code":"no_capacity"`) || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("a second large: %d %s", rec.Code, rec.Body)
	}
	if list := decode[[]session](t, f.do(alice, "GET", "/api/sessions", "")); len(list) != 2 {
		t.Errorf("%d sessions after a refused create", len(list))
	}

	// The same for a start: the session stays asleep.
	if err := f.store.Suspend(t.Context(), large, sessions.StoppedByUser); err != nil {
		t.Fatal(err)
	}
	sessionstest.SetStatus(t, f.client, large, sessionstest.Suspended())
	create("medium", "node-2")
	for _, start := range [][2]string{{"POST", "/wake"}, {"PATCH", ""}} {
		rec := f.do(alice, start[0], "/api/sessions/"+large+start[1], `{"action":"resume"}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no capacity for a large session") {
			t.Errorf("%s: %d %s", start[0], rec.Code, rec.Body)
		}
	}
	if got := decode[session](t, f.do(alice, "GET", "/api/sessions/"+large, "")); got.State != sessions.Stopped {
		t.Errorf("state %q after a refused start", got.State)
	}
}

func TestResizeByPatch(t *testing.T) {
	f := sized(t)
	id := decode[session](t, f.do(alice, "POST", "/api/sessions", `{"name":"a"}`)).ID
	sessionstest.SetStatus(t, f.client, id, sessionstest.Ready("10.0.0.7"))

	// Awake: it waits for the next start.
	rec := f.do(alice, "PATCH", "/api/sessions/"+id, `{"size":"medium"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"size":"small"`) || !strings.Contains(rec.Body.String(), `"pendingSize":"medium"`) {
		t.Fatalf("resize while awake: %d %s", rec.Code, rec.Body)
	}
	// Nothing of a bad request is done: not the size either.
	for _, body := range []string{`{"size":"huge"}`, `{"size":""}`, `{"size":"large","action":"explode"}`, `{"size":"large","name":""}`} {
		rec := f.do(alice, "PATCH", "/api/sessions/"+id, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
		if got := decode[session](t, f.do(alice, "GET", "/api/sessions/"+id, "")); got.PendingSize != "medium" {
			t.Errorf("%s changed the size to %q", body, got.PendingSize)
		}
	}
	// Somebody else's session is not there.
	if rec := f.do(bob, "PATCH", "/api/sessions/"+id, `{"size":"large"}`); rec.Code != http.StatusNotFound {
		t.Errorf("bob: %d %s", rec.Code, rec.Body)
	}

	// With a stop in the same request it has the size at once.
	rec = f.do(alice, "PATCH", "/api/sessions/"+id, `{"size":"large","action":"stop"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"size":"large"`) || strings.Contains(rec.Body.String(), "pendingSize") {
		t.Fatalf("resize and stop: %d %s", rec.Code, rec.Body)
	}
	sessionstest.SetStatus(t, f.client, id, sessionstest.Suspended())
	// Stopped: at once, and a resume in the same request starts it so.
	rec = f.do(alice, "PATCH", "/api/sessions/"+id, `{"size":"medium","action":"resume"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"size":"medium"`) || strings.Contains(rec.Body.String(), "pendingSize") {
		t.Fatalf("resize and resume: %d %s", rec.Code, rec.Body)
	}
	obj, _ := f.store.Get(t.Context(), id)
	if obj.Size != "medium" || obj.State != sessions.Starting {
		t.Errorf("%+v", obj)
	}
}

// With billing enforced, a plan includes some sizes: small always, and the
// ones its tier names. Pay as you go here has medium and not large.
func TestThePlanSaysWhichSizes(t *testing.T) {
	store := billingtest.NewSessions(billingtest.NewClock(billingStart))
	b := newBilled(t, store, store)
	b.withCard(alice.Subject)

	rec := b.do(alice, "POST", "/api/sessions", `{"size":"large"}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"code":"size_not_included"`) || !strings.Contains(rec.Body.String(), "billingUrl") {
		t.Fatalf("large on pay as you go: %d %s", rec.Code, rec.Body)
	}
	if store.Creates() != 0 {
		t.Error("the store was asked to create")
	}
	rec = b.do(alice, "POST", "/api/sessions", `{"size":"medium"}`)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"size":"medium"`) {
		t.Fatalf("medium on pay as you go: %d %s", rec.Code, rec.Body)
	}
	id := decode[session](t, rec).ID
	// A resize is held to the same.
	if rec := b.do(alice, "PATCH", "/api/sessions/"+id, `{"size":"large"}`); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "size_not_included") {
		t.Errorf("resize to large: %d %s", rec.Code, rec.Body)
	}
	if rec := b.do(alice, "PATCH", "/api/sessions/"+id, `{"size":"small"}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"pendingSize":"small"`) {
		t.Errorf("resize to small: %d %s", rec.Code, rec.Body)
	}
}
