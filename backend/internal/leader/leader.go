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
	"os"
	"strings"
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
//
// eligible, if not nil, is asked every Retry: a replica campaigns only
// while it says yes, and one that is leading when it says no gives the
// Lease up. A replica that is being checked before a release serves the
// requests it is sent and runs no pass (Active).
func Run(ctx context.Context, leases coordinationv1.LeasesGetter, namespace, identity string, timing Timing, eligible func() bool, lead func(ctx context.Context)) {
	check(ctx, leases, namespace)
	if eligible == nil {
		eligible = func() bool { return true }
	}
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Namespace: namespace, Name: Lease},
		Client:     leases,
		LockConfig: resourcelock.ResourceLockConfig{Identity: identity},
	}
	for ctx.Err() == nil {
		if !eligible() {
			select {
			case <-ctx.Done():
			case <-time.After(timing.Retry):
			}
			continue
		}
		// The campaign ends with ctx, or when the replica stops being
		// eligible.
		ctx, stop := context.WithCancel(ctx)
		go func() {
			tick := time.NewTicker(timing.Retry)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					if !eligible() {
						slog.Info("no longer eligible to lead; giving the Lease up", "identity", identity)
						stop()
						return
					}
				}
			}
		}()
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
			stop()
			return
		}
		elector.Run(ctx) // returns when the Lease is lost, or ctx is done
		stop()
		leading.Wait()
	}
}

// ActiveLabel is the line of a pod's labels file (the downward API's
// metadata.labels) that makes the pod eligible to lead: the label a release
// puts on the backend that serves users, and not on one it is still
// checking.
const ActiveLabel = `browserjs.dev/role="active"`

// Active is the eligibility of a replica whose labels are in file: it is
// eligible while the file has the line ActiveLabel. The file is read each
// time, so a label changed on a running pod is seen without a restart.
// With no file (the empty name) every replica is eligible.
func Active(file string) func() bool {
	if file == "" {
		return nil
	}
	return func() bool {
		data, err := os.ReadFile(file)
		if err != nil {
			return false
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == ActiveLabel {
				return true
			}
		}
		return false
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
