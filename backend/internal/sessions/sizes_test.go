package sessions_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/r33drichards/computer-use/backend/internal/sessions"
	"github.com/r33drichards/computer-use/backend/internal/sessions/sessionstest"
)

// shapeOf is what a size decides of a Sandbox: each container's resources
// and the sizes' variable, and /dev/shm.
func shapeOf(t *testing.T, obj *unstructured.Unstructured) string {
	t.Helper()
	var out []string
	containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "podTemplate", "spec", "containers")
	for _, c := range containers {
		c := c.(map[string]any)
		req, _, _ := unstructured.NestedStringMap(c, "resources", "requests")
		lim, _, _ := unstructured.NestedStringMap(c, "resources", "limits")
		heap := ""
		env, _ := c["env"].([]any)
		for _, e := range env {
			if e := e.(map[string]any); e["name"] == "MCP_V8_HEAP_MEMORY_MAX" {
				heap = " heap=" + e["value"].(string)
			}
		}
		out = append(out, fmt.Sprintf("%s %s/%s..%s/%s%s", c["name"], req["cpu"], req["memory"], lim["cpu"], lim["memory"], heap))
	}
	volumes, _, _ := unstructured.NestedSlice(obj.Object, "spec", "podTemplate", "spec", "volumes")
	for _, v := range volumes {
		limit, _, _ := unstructured.NestedString(v.(map[string]any), "emptyDir", "sizeLimit")
		out = append(out, "shm "+limit)
	}
	return strings.Join(out, "; ")
}

const (
	smallShape  = "browser 150m/1Gi..1500m/2Gi; mcp-js 50m/256Mi..500m/1Gi; shm 1Gi"
	mediumShape = "browser 500m/3Gi..2/5Gi; mcp-js 50m/256Mi..500m/1Gi heap=16; shm 2Gi"
	largeShape  = "browser 2/10Gi..3/10Gi; mcp-js 50m/1Gi..500m/1Gi heap=32; shm 4Gi"
)

func TestCreateEachSize(t *testing.T) {
	store, client := sessionstest.NewSized(t)
	for size, want := range map[string]string{"": smallShape, "small": smallShape, "medium": mediumShape, "large": largeShape} {
		s, err := store.CreateSized(t.Context(), "a", "alice@example.com", size, nil)
		if err != nil {
			t.Fatalf("%q: %v", size, err)
		}
		wantSize := size
		if size == "" {
			wantSize = "small"
		}
		if s.Size != wantSize || s.PendingSize != "" {
			t.Errorf("%q: size %q, pending %q", size, s.Size, s.PendingSize)
		}
		obj := sandbox(t, client, s.ID)
		if got := shapeOf(t, obj); got != want {
			t.Errorf("%q:\n got %s\nwant %s", size, got, want)
		}
		// Small has no annotation: it is what every session before sizes is.
		if ann, has := obj.GetAnnotations()[sessions.AnnSize]; has != (wantSize != "small") || (has && ann != wantSize) {
			t.Errorf("%q: annotation %q (%v)", size, ann, has)
		}
		// The rest of the blueprint is the same at every size.
		env, _, _ := unstructured.NestedSlice(obj.Object, "spec", "podTemplate", "spec", "containers")
		if first := env[1].(map[string]any)["env"].([]any)[0].(map[string]any); first["name"] != "MCP_V8_PUBLIC_URL" {
			t.Errorf("%q: mcp-js's own variable is gone: %v", size, first)
		}
		if err := store.Delete(t.Context(), s.ID); err != nil {
			t.Fatal(err)
		}
	}
	if got := store.Sizes(); len(got) != 3 ||
		got[0] != (sessions.SizeInfo{Name: "small", CPUMillis: 1500, MemoryMiB: 2048, Warm: true}) ||
		got[1] != (sessions.SizeInfo{Name: "medium", CPUMillis: 2000, MemoryMiB: 5120}) ||
		got[2] != (sessions.SizeInfo{Name: "large", CPUMillis: 3000, MemoryMiB: 10240}) {
		t.Errorf("sizes %+v", got)
	}
}

func TestAnInvalidSizeMakesNothing(t *testing.T) {
	store, client := sessionstest.NewSized(t)
	_, err := store.CreateSized(t.Context(), "a", "alice@example.com", "huge", nil)
	if !errors.Is(err, sessions.ErrInvalidSize) || !strings.Contains(err.Error(), `["small" "medium" "large"]`) {
		t.Fatalf("err = %v", err)
	}
	if n := writes(client); n != 0 {
		t.Errorf("%d writes", n)
	}
	// A store that was given no sizes has small only.
	plain, _ := sessionstest.New(t)
	if _, err := plain.CreateSized(t.Context(), "a", "alice@example.com", "medium", nil); !errors.Is(err, sessions.ErrInvalidSize) {
		t.Errorf("without sizes: %v", err)
	}
	if s, err := plain.CreateSized(t.Context(), "a", "alice@example.com", "small", nil); err != nil || s.Size != "small" {
		t.Errorf("without sizes, small: %+v %v", s, err)
	}
}

func TestSizesFileIsHeldToWhatASizeIs(t *testing.T) {
	for name, file := range map[string]string{
		"a key that is not a size's":  "sizes:\n  - name: medium\n    image: other\n",
		"small":                       strings.Replace(sessionstest.Sizes, "name: medium", "name: small", 1),
		"twice":                       strings.Replace(sessionstest.Sizes, "name: large", "name: medium", 1),
		"a request above its limit":   strings.Replace(sessionstest.Sizes, "requests: {cpu: 500m, memory: 3Gi}", "requests: {cpu: 500m, memory: 6Gi}", 1),
		"not a quantity":              strings.Replace(sessionstest.Sizes, "shm: 2Gi", "shm: lots", 1),
		"no nodes":                    strings.Replace(sessionstest.Sizes, "nodes: 2", "nodes: 0", 1),
		"a container without numbers": "sizes:\n  - name: medium\n    containers:\n      browser: {}\n",
	} {
		if _, err := sessions.ParseSizes([]byte(file)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	store, _ := sessionstest.NewSized(t)
	for name, file := range map[string]string{
		"a container the blueprint lacks": strings.Replace(sessionstest.Sizes, "      mcp-js:", "      sidecar:", 1),
		"bigger than a node":              strings.Replace(sessionstest.Sizes, "memory: 12097Mi", "memory: 8Gi", 1),
		"a variable the blueprint sets":   strings.Replace(sessionstest.Sizes, `MCP_V8_HEAP_MEMORY_MAX: "16"`, `MCP_V8_PUBLIC_URL: "x"`, 1),
	} {
		parsed, err := sessions.ParseSizes([]byte(file))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := store.EnableSizes(parsed); err == nil {
			t.Errorf("%s: enabled", name)
		}
	}
}

// place makes a session of size whose pod runs on node.
func place(t *testing.T, store *sessions.Store, client dynamic.Interface, size, node string) string {
	t.Helper()
	s, err := store.CreateSized(t.Context(), "a", "alice@example.com", size, nil)
	if err != nil {
		t.Fatalf("%s on %s: %v", size, node, err)
	}
	sessionstest.SetStatus(t, client, s.ID, sessionstest.OnNode("10.0.0.7", node))
	return s.ID
}

func TestNoCapacityIsAnErrorNotAWait(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.NewSized(t)
	// Two nodes at most. The first holds seven small sessions, as the warm
	// pool's does: a medium no longer fits there, and takes the second.
	for range 7 {
		place(t, store, client, "small", "node-1")
	}
	medium := place(t, store, client, "medium", "node-2")
	before := writes(client)

	// A large session needs a node to itself, and there is none to add.
	_, err := store.CreateSized(ctx, "big", "alice@example.com", "large", nil)
	var none *sessions.NoCapacityError
	if !errors.Is(err, sessions.ErrNoCapacity) || !errors.As(err, &none) || none.Size != "large" ||
		!strings.Contains(err.Error(), "no capacity for a large session") {
		t.Fatalf("err = %v", err)
	}
	if writes(client) != before {
		t.Error("something was made")
	}
	// Smaller ones still fit: two more medium on the second node, and then
	// a fourth does not.
	place(t, store, client, "medium", "node-2")
	place(t, store, client, "medium", "node-2")
	if _, err := store.CreateSized(ctx, "m", "alice@example.com", "medium", nil); !errors.Is(err, sessions.ErrNoCapacity) {
		t.Errorf("a fourth medium: %v", err)
	}
	// The first node has room for two more small ones.
	place(t, store, client, "small", "node-1")
	last := place(t, store, client, "small", "node-1")
	// What three medium sessions leave of the second fits one small.
	place(t, store, client, "small", "node-2")
	if _, err := store.CreateSized(ctx, "s", "alice@example.com", "small", nil); !errors.Is(err, sessions.ErrNoCapacity) {
		t.Errorf("a small with both nodes full: %v", err)
	}

	// A session that sleeps gives its room back; waking it needs room again.
	if err := store.Suspend(ctx, last, sessions.StoppedByIdle); err != nil {
		t.Fatal(err)
	}
	sessionstest.SetStatus(t, client, last, sessionstest.Suspended())
	other := place(t, store, client, "small", "node-1")
	if err := store.Wake(ctx, last); !errors.Is(err, sessions.ErrNoCapacity) {
		t.Errorf("wake with no room: %v", err)
	}
	if err := store.Resume(ctx, last); !errors.Is(err, sessions.ErrNoCapacity) {
		t.Errorf("resume with no room: %v", err)
	}
	if got := mode(sandbox(t, client, last)); got != "Suspended" {
		t.Errorf("mode %q after a refused start", got)
	}
	if err := store.Delete(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := store.Wake(ctx, last); err != nil {
		t.Errorf("wake with room: %v", err)
	}

	// A pod that has no node yet was there first: it is counted.
	for _, id := range []string{medium} {
		if err := store.Delete(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.CreateSized(ctx, "waiting", "alice@example.com", "medium", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSized(ctx, "m", "alice@example.com", "medium", nil); !errors.Is(err, sessions.ErrNoCapacity) {
		t.Errorf("a medium beside two running and one waiting: %v", err)
	}
}

func TestALargeSessionTakesANewNode(t *testing.T) {
	store, client := sessionstest.NewSized(t)
	for range 7 {
		place(t, store, client, "small", "node-1")
	}
	// One node in use of two: the second can be added, and is the large one's.
	large := place(t, store, client, "large", "node-2")
	if _, err := store.CreateSized(t.Context(), "b", "alice@example.com", "large", nil); !errors.Is(err, sessions.ErrNoCapacity) {
		t.Errorf("a second large: %v", err)
	}
	// Nothing fits beside it either.
	if _, err := store.CreateSized(t.Context(), "m", "alice@example.com", "medium", nil); !errors.Is(err, sessions.ErrNoCapacity) {
		t.Errorf("a medium beside a large: %v", err)
	}
	if err := store.Delete(t.Context(), large); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSized(t.Context(), "b", "alice@example.com", "large", nil); err != nil {
		t.Errorf("a large once the first is gone: %v", err)
	}
}

func TestWithoutCapacityNothingIsRefused(t *testing.T) {
	store, _ := sessionstest.New(t)
	for range 30 {
		if _, err := store.Create(t.Context(), "a", "alice@example.com"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOnlySmallComesFromTheWarmPool(t *testing.T) {
	store, client := sessionstest.NewSized(t)
	store.EnableWarmPool(sessionstest.WarmPoolName, time.Second)
	sessionstest.PlayClaimController(t, client, "s-bcdfg")

	medium, err := store.CreateSized(t.Context(), "m", "alice@example.com", "medium", nil)
	if err != nil {
		t.Fatal(err)
	}
	if medium.ID == "s-bcdfg" || len(sessionstest.Claims(t, client)) != 0 {
		t.Errorf("a medium session took %s from the pool (claims: %d)", medium.ID, len(sessionstest.Claims(t, client)))
	}
	small, err := store.CreateSized(t.Context(), "s", "alice@example.com", "small", nil)
	if err != nil {
		t.Fatal(err)
	}
	if small.ID != "s-bcdfg" || small.Size != "small" {
		t.Errorf("a small session is %+v, want the pool's s-bcdfg", small)
	}
}

func TestResizeAnAwakeSessionWaitsForItsNextStart(t *testing.T) {
	ctx := t.Context()
	store, client := sessionstest.NewSized(t)
	id := place(t, store, client, "small", sessionstest.Node)

	if err := store.Resize(ctx, id, "huge"); !errors.Is(err, sessions.ErrInvalidSize) {
		t.Fatalf("an invalid size: %v", err)
	}
	if err := store.Resize(ctx, id, "medium"); err != nil {
		t.Fatal(err)
	}
	// The pod template is not touched while the pod runs.
	if got := shapeOf(t, sandbox(t, client, id)); got != smallShape {
		t.Errorf("resized while running: %s", got)
	}
	if s, _ := store.Get(ctx, id); s.Size != "small" || s.PendingSize != "medium" {
		t.Errorf("size %q, pending %q", s.Size, s.PendingSize)
	}
	// Asking again changes nothing; asking for what it has withdraws it.
	before := writes(client)
	if err := store.Resize(ctx, id, "medium"); err != nil || writes(client) != before {
		t.Errorf("again: %v, %d writes", err, writes(client)-before)
	}
	if err := store.Resize(ctx, id, "small"); err != nil {
		t.Fatal(err)
	}
	if s, _ := store.Get(ctx, id); s.Size != "small" || s.PendingSize != "" {
		t.Errorf("withdrawn: size %q, pending %q", s.Size, s.PendingSize)
	}

	// A stop applies it; the session then starts at the new size.
	if err := store.Resize(ctx, id, "large"); err != nil {
		t.Fatal(err)
	}
	if err := store.Suspend(ctx, id, sessions.StoppedByUser); err != nil {
		t.Fatal(err)
	}
	if s, _ := store.Get(ctx, id); s.Size != "large" || s.PendingSize != "" {
		t.Errorf("after the stop: size %q, pending %q", s.Size, s.PendingSize)
	}
	if got := shapeOf(t, sandbox(t, client, id)); got != largeShape {
		t.Errorf("after the stop:\n got %s\nwant %s", got, largeShape)
	}
	// And back to small: the sizes' variable goes, the blueprint's stays.
	sessionstest.SetStatus(t, client, id, sessionstest.Suspended())
	if err := store.Resize(ctx, id, "small"); err != nil {
		t.Fatal(err)
	}
	obj := sandbox(t, client, id)
	if got := shapeOf(t, obj); got != smallShape {
		t.Errorf("back to small:\n got %s\nwant %s", got, smallShape)
	}
	if _, has := obj.GetAnnotations()[sessions.AnnSize]; has {
		t.Error("a small session has a size annotation")
	}
}

func TestAResizeMissedAtTheStopIsAppliedAtTheStart(t *testing.T) {
	ctx := t.Context()
	for name, start := range map[string]func(*sessions.Store, string) error{
		"wake":   func(s *sessions.Store, id string) error { return s.Wake(ctx, id) },
		"resume": func(s *sessions.Store, id string) error { return s.Resume(ctx, id) },
	} {
		store, client := sessionstest.NewSized(t)
		id := place(t, store, client, "small", sessionstest.Node)
		// Suspended by something that knows nothing of sizes, with a resize
		// still waiting.
		obj := sandbox(t, client, id)
		ann := obj.GetAnnotations()
		ann[sessions.AnnResizeTo], ann[sessions.AnnStoppedBy] = "medium", sessions.StoppedByIdle
		obj.SetAnnotations(ann)
		_ = unstructured.SetNestedField(obj.Object, "Suspended", "spec", "operatingMode")
		if _, err := client.Resource(sessions.SandboxGVR).Namespace(sessionstest.Namespace).Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := start(store, id); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		obj = sandbox(t, client, id)
		if got := shapeOf(t, obj); got != mediumShape || mode(obj) != "Running" {
			t.Errorf("%s: mode %s, shape %s", name, mode(obj), got)
		}
		if s := sessions.FromSandbox(obj); s.Size != "medium" || s.PendingSize != "" {
			t.Errorf("%s: size %q, pending %q", name, s.Size, s.PendingSize)
		}
	}
}

// sizesWithoutShm is for the plain blueprint, which has no /dev/shm volume.
const sizesWithoutShm = `
sizes:
  - name: medium
    containers:
      browser:
        resources:
          requests: {cpu: 500m, memory: 3Gi}
          limits: {cpu: "2", memory: 5Gi}
`

// A snapshot is of a pod of one size and is not restored into another: a
// resize drops it, and the session starts fresh from its disk.
func TestResizeDropsTheSnapshot(t *testing.T) {
	ctx := t.Context()
	t.Run("asleep: at once", func(t *testing.T) {
		store, client, _ := sessionstest.NewWithSnapshots(t, sessions.SnapshotOptions{Timeout: 2 * time.Second})
		sessionstest.EnableSizes(t, store, sizesWithoutShm)
		id := running(t, store, client)
		if err := store.Sleep(ctx, id, sessions.StoppedBySleep, nil); err != nil {
			t.Fatal(err)
		}
		sessionstest.SetStatus(t, client, id, sessionstest.Suspended())
		if s, _ := store.Get(ctx, id); !s.StateSaved || len(sessionstest.Snapshots(t, client)) != 1 {
			t.Fatalf("asleep without a snapshot: %+v", s)
		}
		if err := store.Resize(ctx, id, "medium"); err != nil {
			t.Fatal(err)
		}
		obj := sandbox(t, client, id)
		s := sessions.FromSandbox(obj)
		if s.Size != "medium" || s.PendingSize != "" || s.StateSaved || s.State != sessions.Asleep {
			t.Errorf("%+v", s)
		}
		if pin(obj) != "" || obj.GetAnnotations()[sessions.AnnSnapshot] != "" || len(sessionstest.Snapshots(t, client)) != 0 {
			t.Errorf("pin %q, snapshot %q, snapshots %v", pin(obj), obj.GetAnnotations()[sessions.AnnSnapshot], sessionstest.Snapshots(t, client))
		}
	})
	t.Run("awake: the sleep takes none", func(t *testing.T) {
		store, client, _ := sessionstest.NewWithSnapshots(t, sessions.SnapshotOptions{Timeout: 2 * time.Second})
		sessionstest.EnableSizes(t, store, sizesWithoutShm)
		id := running(t, store, client)
		if err := store.Resize(ctx, id, "medium"); err != nil {
			t.Fatal(err)
		}
		if err := store.Sleep(ctx, id, sessions.StoppedBySleep, nil); err != nil {
			t.Fatal(err)
		}
		obj := sandbox(t, client, id)
		s := sessions.FromSandbox(obj)
		if s.Size != "medium" || s.PendingSize != "" || obj.GetAnnotations()[sessions.AnnSnapshot] != "" || pin(obj) != "" {
			t.Errorf("%+v, snapshot %q, pin %q", s, obj.GetAnnotations()[sessions.AnnSnapshot], pin(obj))
		}
		if got := sessionstest.Snapshots(t, client); len(got) != 0 {
			t.Errorf("snapshots %v", got)
		}
	})
	t.Run("the same size keeps it", func(t *testing.T) {
		store, client, _ := sessionstest.NewWithSnapshots(t, sessions.SnapshotOptions{Timeout: 2 * time.Second})
		sessionstest.EnableSizes(t, store, sizesWithoutShm)
		id := running(t, store, client)
		if err := store.Sleep(ctx, id, sessions.StoppedBySleep, nil); err != nil {
			t.Fatal(err)
		}
		if err := store.Resize(ctx, id, "small"); err != nil {
			t.Fatal(err)
		}
		if s, _ := store.Get(ctx, id); len(sessionstest.Snapshots(t, client)) != 1 || sandbox(t, client, id).GetAnnotations()[sessions.AnnSnapshot] == "" {
			t.Errorf("the snapshot went: %+v", s)
		}
	})
}
