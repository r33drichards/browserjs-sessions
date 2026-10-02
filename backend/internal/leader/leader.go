// Package leader has one replica of the backend at a time run the periodic
// passes (the idle sweep, billing's sweep and balance pass, Stripe's
// reconciles). Every replica serves requests; they hold an election over a
// Lease, and the one that holds it runs the passes.
//
// The election is there so that N replicas do not each list every session
// every minute and each try the same snapshot. It is not what makes the
// passes correct: every step of a pass is a write conditional on what was
// just read from the cluster, so two replicas that both believe they lead
// (which a Lease allows for a moment, when a leader stalls) do the work
// twice and nothing else.
package leader

import (
	"context"
	"log/slog"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationv1 "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Lease is the name of the Lease the replicas hold their election over, in
// the backend's namespace. deploy/base/backend.yaml grants it by this name.
const Lease = "backend-leader"

// Timing is the election's: a leader that stops renewing is replaced after
// Duration; it gives up leading if it could not renew for Deadline; every
// replica looks at the Lease each Retry (the leader to renew it, the others
// to see whether it is still held).
type Timing struct{ Duration, Deadline, Retry time.Duration }

// DefaultTiming replaces a dead leader within about a minute and costs the
// API server four writes a minute, and four reads per other replica. The
// passes run once or twice a minute, so nothing is gained by being quicker;
// a replica that shuts down gives the Lease up at once.
var DefaultTiming = Timing{Duration: 60 * time.Second, Deadline: 45 * time.Second, Retry: 15 * time.Second}

// Run campaigns until ctx is done. Each time this replica is elected, lead
// is called with a context that ends when it stops leading (or ctx ends).
// lead is to return when that context ends, and only then does the replica
// campaign again. identity names the replica in the Lease.
func Run(ctx context.Context, leases coordinationv1.LeasesGetter, namespace, identity string, timing Timing, lead func(ctx context.Context)) {
	check(ctx, leases, namespace)
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Namespace: namespace, Name: Lease},
		Client:     leases,
		LockConfig: resourcelock.ResourceLockConfig{Identity: identity},
	}
	for ctx.Err() == nil {
		var leading sync.WaitGroup
		elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
			Lock:            lock,
			LeaseDuration:   timing.Duration,
			RenewDeadline:   timing.Deadline,
			RetryPeriod:     timing.Retry,
			ReleaseOnCancel: true,
			Name:            Lease,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(ctx context.Context) {
					leading.Add(1)
					defer leading.Done()
					slog.Info("leading: this replica runs the periodic passes", "identity", identity)
					lead(ctx)
				},
				OnStoppedLeading: func() {
					slog.Info("no longer leading", "identity", identity)
				},
			},
		})
		if err != nil {
			slog.Error("leader election not started; no replica of this configuration runs the periodic passes", "err", err)
			return
		}
		elector.Run(ctx) // returns when the Lease is lost, or ctx is done
		leading.Wait()
	}
}

// check says, once and loudly, when this replica may not read the Lease:
// then it never leads, and if that is true of every replica no session is
// ever put to sleep.
func check(ctx context.Context, leases coordinationv1.LeasesGetter, namespace string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := leases.Leases(namespace).Get(ctx, Lease, metav1.GetOptions{}); apierrors.IsForbidden(err) {
		slog.Error("this replica may not read its election Lease and will never run the periodic passes (idle sweep, billing): give the backend's Role get, update and create on leases (deploy/base/backend.yaml)",
			"lease", Lease, "err", err)
	}
}
