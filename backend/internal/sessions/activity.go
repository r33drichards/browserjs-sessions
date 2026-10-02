package sessions

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// What the backend's replicas tell each other about a session is written on
// the session itself, in two kinds of annotation, and nowhere else:
//
//   - AnnLastActive, when it was last used. The idle sweep (internal/idle)
//     decides from it alone.
//   - AnnInFlightPrefix + a replica's ID, until when that replica vouches for
//     a call it is proxying. Billing's drain waits for the marks of the
//     other replicas as it waits for its own calls.
//
// Neither is state a replica needs back: one that dies stops renewing its
// marks, which then run out.

// timeLayout is how a moment is written in these annotations. Read with
// time.RFC3339, which takes the fraction too.
const timeLayout = time.RFC3339Nano

var replicaPattern = regexp.MustCompile(`^[a-z0-9]{1,32}$`)

// ValidReplica reports whether id can name a replica in an annotation key.
func ValidReplica(id string) bool { return replicaPattern.MatchString(id) }

func activityOf(obj *unstructured.Unstructured) (lastActive time.Time, inFlight map[string]time.Time) {
	for key, value := range obj.GetAnnotations() {
		switch replica, isFlight := strings.CutPrefix(key, AnnInFlightPrefix); {
		case key == AnnLastActive:
			lastActive, _ = time.Parse(time.RFC3339, value)
		case isFlight:
			if until, err := time.Parse(time.RFC3339, value); err == nil {
				if inFlight == nil {
					inFlight = map[string]time.Time{}
				}
				inFlight[replica] = until
			}
		}
	}
	return lastActive, inFlight
}

// InFlightElsewhere reports whether a replica other than self vouches, at
// now, for a call in flight to the session. A replica knows its own calls
// better than its mark does, so its own mark is not asked.
func (s Session) InFlightElsewhere(self string, now time.Time) bool {
	for replica, until := range s.InFlight {
		if replica != self && until.After(now) {
			return true
		}
	}
	return false
}

// Activity is what a replica says of a session in one write.
type Activity struct {
	// Active, if not zero, is when the session was last used.
	Active time.Time
	// Replica vouches for a call in flight until InFlightUntil, if that is
	// not zero.
	Replica       string
	InFlightUntil time.Time
}

// Mark writes a onto the session, whatever is there, and returns the session
// as it then is: a caller sees in the answer that the session was put to
// sleep, or is being drained, since it last looked. One request, and no read:
// it is made on the way of a user's call.
//
// Active must be the present moment. A moment in the past could move the
// annotation backwards over what another replica wrote since; that is
// MarkActiveSince's to write.
func (s *Store) Mark(ctx context.Context, id string, a Activity) (Session, error) {
	annotations := map[string]any{}
	if !a.Active.IsZero() {
		annotations[AnnLastActive] = a.Active.UTC().Format(timeLayout)
	}
	if !a.InFlightUntil.IsZero() && ValidReplica(a.Replica) {
		annotations[AnnInFlightPrefix+a.Replica] = a.InFlightUntil.UTC().Format(timeLayout)
	}
	if len(annotations) == 0 {
		return s.Get(ctx, id)
	}
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": annotations}})
	if err != nil {
		return Session{}, err
	}
	obj, err := s.client.Patch(ctx, id, types.MergePatchType, body, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	return FromSandbox(obj), nil
}

// MarkActiveSince records that the session was last used at at, a moment
// that has passed, unless a later one is recorded already: the annotation
// only ever moves forward. A session that is suspended has no use to record
// (waking it starts its clock).
func (s *Store) MarkActiveSince(ctx context.Context, id string, at time.Time) error {
	return s.modify(ctx, id, func(obj *unstructured.Unstructured) (bool, error) {
		if operatingMode(obj) == "Suspended" || obj.GetDeletionTimestamp() != nil {
			return false, nil
		}
		if last, _ := activityOf(obj); !last.Before(at) {
			return false, nil
		}
		setAnnotation(obj, AnnLastActive, at.UTC().Format(timeLayout))
		return true, nil
	})
}

// ForgetInFlight removes the given replicas' in-flight marks from the
// session: marks that ran out long ago, of replicas that are gone.
func (s *Store) ForgetInFlight(ctx context.Context, id string, replicas []string) error {
	annotations := map[string]any{}
	for _, replica := range replicas {
		if ValidReplica(replica) {
			annotations[AnnInFlightPrefix+replica] = nil
		}
	}
	if len(annotations) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": annotations}})
	if err != nil {
		return err
	}
	_, err = s.client.Patch(ctx, id, types.MergePatchType, body, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return ErrNotFound
	}
	return err
}

// startClock begins a session's idle period on an object that is about to
// run (created, woken, resumed): used now, and no call in flight.
func startClock(obj *unstructured.Unstructured) {
	stopClock(obj)
	setAnnotation(obj, AnnLastActive, time.Now().UTC().Format(timeLayout))
}

// stopClock removes what was said of the use of a session that is being
// suspended. Whatever runs next starts afresh.
func stopClock(obj *unstructured.Unstructured) {
	for key := range obj.GetAnnotations() {
		if key == AnnLastActive || strings.HasPrefix(key, AnnInFlightPrefix) {
			setAnnotation(obj, key, "")
		}
	}
}
