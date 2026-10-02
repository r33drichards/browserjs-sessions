package leader_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/r33drichards/computer-use/backend/internal/leader"
)

const namespace = "browserjs-sessions"

// Quick enough for a test. A Lease counts its duration in whole seconds.
var timing = leader.Timing{Duration: 2 * time.Second, Deadline: 1500 * time.Millisecond, Retry: 100 * time.Millisecond}

var leases = schema.GroupVersionResource{Group: "coordination.k8s.io", Version: "v1", Resource: "leases"}

// cluster is a fake cluster whose Leases are written as the API server
// writes them: an update goes through only if it carries the version it
// read. The election rests on that, and client-go's fake does not do it.
func cluster() *fake.Clientset {
	client := fake.NewClientset()
	var mu sync.Mutex
	version := 0
	client.PrependReactor("update", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		lease := action.(k8stesting.UpdateAction).GetObject().(*coordinationv1.Lease).DeepCopy()
		stored, err := client.Tracker().Get(leases, lease.Namespace, lease.Name)
		if err != nil {
			return true, nil, err
		}
		if stored.(*coordinationv1.Lease).ResourceVersion != lease.ResourceVersion {
			return true, nil, apierrors.NewConflict(leases.GroupResource(), lease.Name, errors.New("the Lease was written since it was read"))
		}
		version++
		lease.ResourceVersion = strconv.Itoa(version)
		return true, lease, client.Tracker().Update(leases, lease, lease.Namespace)
	})
	return client
}

// leading counts who is running the passes.
type leading struct {
	mu   sync.Mutex
	now  map[string]bool
	most int
	ever map[string]int
}

func (l *leading) lead(name string) func(ctx context.Context) {
	return func(ctx context.Context) {
		l.mu.Lock()
		l.now[name] = true
		l.ever[name]++
		l.most = max(l.most, len(l.now))
		l.mu.Unlock()
		<-ctx.Done()
		l.mu.Lock()
		delete(l.now, name)
		l.mu.Unlock()
	}
}

func (l *leading) who() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var names []string
	for name := range l.now {
		names = append(names, name)
	}
	return names
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A replica that a release is still checking does not campaign, takes over
// when it is made active, and gives the Lease up when it no longer is: with
// no restart, the label file is read each time.
func TestOnlyAnActiveReplicaLeads(t *testing.T) {
	client := cluster()
	l := &leading{now: map[string]bool{}, ever: map[string]int{}}
	file := filepath.Join(t.TempDir(), "labels")
	label := func(role string) {
		t.Helper()
		if err := os.WriteFile(file, []byte("app=\"backend\"\nbrowserjs.dev/role=\""+role+"\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	label("preview")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		leader.Run(ctx, client.CoordinationV1(), namespace, "new", timing, leader.Active(file), l.lead("new"))
	}()
	t.Cleanup(func() { cancel(); <-done })

	time.Sleep(timing.Duration)
	if got := l.who(); len(got) != 0 {
		t.Fatalf("a replica under check leads: %v", got)
	}
	label("active")
	eventually(t, "the replica leads once it is active", func() bool { return len(l.who()) == 1 })
	label("preview")
	eventually(t, "the replica stops leading once it is not", func() bool { return len(l.who()) == 0 })

	if leader.Active("") != nil {
		t.Error("with no file a replica should always be eligible")
	}
	if leader.Active(filepath.Join(t.TempDir(), "missing"))() {
		t.Error("a labels file that cannot be read makes the replica eligible")
	}
}

// Of the replicas, one runs the passes; when it goes, another does.
func TestOneReplicaLeadsAtATime(t *testing.T) {
	client := cluster()
	l := &leading{now: map[string]bool{}, ever: map[string]int{}}
	stops := map[string]context.CancelFunc{}
	var wg sync.WaitGroup
	for _, name := range []string{"a", "b", "c"} {
		ctx, cancel := context.WithCancel(t.Context())
		stops[name] = cancel
		wg.Go(func() { leader.Run(ctx, client.CoordinationV1(), namespace, name, timing, nil, l.lead(name)) })
	}
	t.Cleanup(func() {
		for _, stop := range stops {
			stop()
		}
		wg.Wait()
	})

	eventually(t, "a replica leads", func() bool { return len(l.who()) == 1 })
	first := l.who()[0]
	lease, err := client.CoordinationV1().Leases(namespace).Get(t.Context(), leader.Lease, metav1.GetOptions{})
	if err != nil || lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != first {
		t.Fatalf("the Lease: %+v, %v; want held by %s", lease, err, first)
	}
	// It stays the only one, past the Lease's duration.
	time.Sleep(timing.Duration + timing.Duration/2)
	if got := l.who(); len(got) != 1 || got[0] != first {
		t.Fatalf("leading after a while: %v, want only %s", got, first)
	}

	// The leader shuts down: it gives the Lease up, and another takes over
	// without waiting for it to run out.
	stops[first]()
	eventually(t, "another replica leads", func() bool {
		got := l.who()
		return len(got) == 1 && got[0] != first
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.most != 1 {
		t.Errorf("%d replicas led at once", l.most)
	}
}
