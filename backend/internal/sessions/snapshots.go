package sessions

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// GKE Pod Snapshots (podsnapshot.gke.io/v1): a checkpoint of a running gVisor
// pod's memory and root filesystem, kept in Cloud Storage. A pod that is
// created again is restored from the newest ready snapshot of its group; the
// PodSnapshotPolicy in deploy/gke groups by Sandbox, so a session only ever
// restores its own.
var (
	PodSnapshotGVR     = schema.GroupVersionResource{Group: "podsnapshot.gke.io", Version: "v1", Resource: "podsnapshots"}
	SnapshotTriggerGVR = schema.GroupVersionResource{Group: "podsnapshot.gke.io", Version: "v1", Resource: "podsnapshotmanualtriggers"}
	nodeGVR            = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
)

const (
	AnnSnapshot     = "browserjs.dev/snapshot"      // the PodSnapshot a sleeping session wakes from
	AnnSnapshotPool = "browserjs.dev/snapshot-pool" // the node pool it was taken on
	LabelSession    = "browserjs.dev/session"       // on a snapshot trigger: the session's ID

	// LabelPool is the node label naming a node's pool (infra/main). A
	// snapshot only restores on the machine series and CPU it was taken on,
	// so a session with a snapshot is pinned to that pool.
	LabelPool = "browserjs.com/pool"

	// How GKE and the Sandbox controller mark a PodSnapshot's origin: the
	// controller's per-Sandbox pod label, which the snapshot inherits, and
	// the name of the pod, which is the session's ID.
	labelNameHash = "agents.x-k8s.io/sandbox-name-hash"
	annOriginPod  = "podsnapshot.gke.io/origin-pod"
)

// SnapshotOptions turn on sleeping to, and waking from, Pod Snapshots.
type SnapshotOptions struct {
	// Timeout bounds one snapshot, from trigger to ready. A snapshot that
	// takes longer is abandoned and the session sleeps without one.
	Timeout time.Duration
	Poll    time.Duration // how often to look at a snapshot in progress; 1s if unset
}

type snapshotter struct {
	triggers, snapshots dynamic.ResourceInterface
	nodes               dynamic.ResourceInterface
	timeout, poll       time.Duration
}

// EnableSnapshots makes the store snapshot a session before a sleep (for
// being idle, or asked for) and wake it from that snapshot. Without it (a cluster with no Pod
// Snapshots, such as kind) a session sleeps and wakes cold, and the store
// asks the cluster for nothing but Sandboxes.
func (s *Store) EnableSnapshots(client dynamic.Interface, namespace string, o SnapshotOptions) {
	if o.Poll <= 0 {
		o.Poll = time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = 2 * time.Minute
	}
	s.snap = &snapshotter{
		triggers:  client.Resource(SnapshotTriggerGVR).Namespace(namespace),
		snapshots: client.Resource(PodSnapshotGVR).Namespace(namespace),
		nodes:     client.Resource(nodeGVR),
		timeout:   o.Timeout,
		poll:      o.Poll,
	}
}

// nameHash is the value of the Sandbox controller's sandbox-name-hash label
// for a Sandbox name: 32-bit FNV-1a, in hex.
func nameHash(name string) string {
	h := fnv.New32a()
	h.Write([]byte(name))
	return fmt.Sprintf("%08x", h.Sum32())
}

// snapshot is a ready PodSnapshot and where it can be restored.
type snapshot struct{ name, pool string }

// until calls check every poll until it reports done, fails, or ctx ends.
func (n *snapshotter) until(ctx context.Context, check func() (bool, error)) error {
	for {
		done, err := check()
		if err != nil || done {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(n.poll):
		}
	}
}

// take snapshots the session's running pod and waits until the snapshot can
// be restored from. The pod keeps running. On any failure, including the
// timeout, nothing is left behind.
func (n *snapshotter) take(ctx context.Context, sandbox *unstructured.Unstructured) (*snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	id := sandbox.GetName()

	nodeName, _, _ := unstructured.NestedString(sandbox.Object, "status", "nodeName")
	if nodeName == "" {
		return nil, errors.New("the session's pod is on no node")
	}
	node, err := n.nodes.Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("node %s: %w", nodeName, err)
	}
	pool := node.GetLabels()[LabelPool]
	if pool == "" {
		return nil, fmt.Errorf("node %s has no %s label to restore on", nodeName, LabelPool)
	}

	// A trigger is used once; each sleep needs a new name.
	trigger := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": SnapshotTriggerGVR.GroupVersion().String(),
		"kind":       "PodSnapshotManualTrigger",
		"metadata": map[string]any{
			"name":   id + "-" + strconv.FormatInt(time.Now().UnixMilli(), 36),
			"labels": map[string]any{LabelSession: id},
			// Goes with the session if this process dies before removing it.
			"ownerReferences": []any{map[string]any{
				"apiVersion": sandbox.GetAPIVersion(), "kind": "Sandbox",
				"name": id, "uid": string(sandbox.GetUID()),
			}},
		},
		// The pod has its Sandbox's name.
		"spec": map[string]any{"targetPod": id},
	}}
	if _, err := n.triggers.Create(ctx, trigger, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("trigger: %w", err)
	}
	defer func() {
		ctx, cancel := cleanupContext(ctx)
		defer cancel()
		if err := n.triggers.Delete(ctx, trigger.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			slog.Warn("snapshot trigger not removed", "trigger", trigger.GetName(), "err", err)
		}
	}()

	// The trigger is done when the checkpoint is written; the snapshot is
	// only restored from once it is Ready (uploaded).
	var name string
	err = n.until(ctx, func() (bool, error) {
		obj, err := n.triggers.Get(ctx, trigger.GetName(), metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		name, _, _ = unstructured.NestedString(obj.Object, "status", "snapshotCreated", "name")
		switch c := conditions(obj)["Triggered"]; {
		case c.status == "True" && name != "":
			return true, nil
		case c.status == "False" && (c.reason == "Failed" || c.reason == "Error"):
			return false, fmt.Errorf("checkpoint failed: %s", c.message)
		}
		return false, nil
	})
	if err == nil {
		err = n.until(ctx, func() (bool, error) {
			obj, err := n.snapshots.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			return conditions(obj)["Ready"].status == "True", nil
		})
	}
	if err != nil {
		if name != "" {
			n.discard(ctx, name)
		}
		return nil, err
	}
	return &snapshot{name: name, pool: pool}, nil
}

// cleanupContext is for removing what an operation made after the operation
// itself was cancelled or timed out.
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
}

// discard deletes one snapshot; GKE removes its objects from the bucket.
func (n *snapshotter) discard(ctx context.Context, name string) {
	ctx, cancel := cleanupContext(ctx)
	defer cancel()
	if err := n.snapshots.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		slog.Error("snapshot not deleted", "snapshot", name, "err", err)
	}
}

// ready reports whether the named snapshot exists and can be restored from.
func (n *snapshotter) ready(ctx context.Context, name string) bool {
	obj, err := n.snapshots.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			slog.Warn("snapshot not readable; waking cold", "snapshot", name, "err", err)
		}
		return false
	}
	return obj.GetDeletionTimestamp() == nil && conditions(obj)["Ready"].status == "True"
}

// of lists the session's snapshots, whether or not the store recorded them.
func (n *snapshotter) of(ctx context.Context, id string) ([]string, error) {
	list, err := n.snapshots.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	hash := nameHash(id)
	var names []string
	for _, item := range list.Items {
		if item.GetAnnotations()[annOriginPod] == id || item.GetLabels()[labelNameHash] == hash {
			names = append(names, item.GetName())
		}
	}
	return names, nil
}

// prune deletes the session's snapshots, all but keep. A snapshot left
// behind would be restored the next time the session's pod is created, over
// a disk that has moved on since.
func (n *snapshotter) prune(ctx context.Context, id, keep string) (int, error) {
	names, err := n.of(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("list snapshots: %w", err)
	}
	deleted := 0
	for _, name := range names {
		if name == keep {
			continue
		}
		if err := n.snapshots.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return deleted, fmt.Errorf("delete snapshot %s: %w", name, err)
		}
		deleted++
	}
	return deleted, nil
}

// pruneLogged is prune for after the session's own state is already written.
func (n *snapshotter) pruneLogged(ctx context.Context, id, keep string) {
	ctx, cancel := cleanupContext(ctx)
	defer cancel()
	if _, err := n.prune(ctx, id, keep); err != nil {
		slog.Error("stale snapshots not deleted", "session", id, "err", err)
	}
}

// setSnapshot records on the Sandbox the snapshot its next pod restores
// from, and pins that pod to the snapshot's node pool; nil removes both.
// Only a pin this function made is removed.
func setSnapshot(obj *unstructured.Unstructured, snap *snapshot) error {
	selector := []string{"spec", "podTemplate", "spec", "nodeSelector"}
	if snap == nil {
		_, pinned := obj.GetAnnotations()[AnnSnapshotPool]
		setAnnotation(obj, AnnSnapshot, "")
		setAnnotation(obj, AnnSnapshotPool, "")
		if pinned {
			unstructured.RemoveNestedField(obj.Object, append(selector, LabelPool)...)
		}
		return nil
	}
	setAnnotation(obj, AnnSnapshot, snap.name)
	setAnnotation(obj, AnnSnapshotPool, snap.pool)
	return unstructured.SetNestedField(obj.Object, snap.pool, append(selector, LabelPool)...)
}

// keepOrDropSnapshot is called on a Sandbox about to run again: its recorded
// snapshot stays (and with it the pin) if it can be restored from, and is
// forgotten otherwise, so the pod cold starts on any pool.
func (s *Store) keepOrDropSnapshot(ctx context.Context, obj *unstructured.Unstructured) error {
	if s.snap == nil {
		return nil
	}
	if name := obj.GetAnnotations()[AnnSnapshot]; name != "" && s.snap.ready(ctx, name) {
		return nil
	}
	return setSnapshot(obj, nil)
}

// Sleep puts a session to sleep, recording why (by: StoppedByIdle,
// StoppedBySleep for its user asking, or one of billing's reasons): with snapshots enabled it snapshots the pod first, so
// that the session wakes as it was. A snapshot that fails or runs out of
// time does not stop the sleep; the session then wakes cold. A session put
// to sleep for billing before it ever ran has no pod worth a snapshot.
//
// stillWanted, if not nil, is asked once the snapshot is done: a session
// used in the meantime (or whose owner's credit came back) is left running
// (ErrStateChanged). Like an idle Suspend, Sleep never takes over a session
// that is already suspended. The draining mark, if any, goes with the sleep.
func (s *Store) Sleep(ctx context.Context, id, by string, stillWanted func() bool) error {
	if !sleepReason(by) {
		return fmt.Errorf("sleep: unknown reason %q", by)
	}
	var snap *snapshot
	if s.snap != nil {
		obj, err := s.client.Get(ctx, id, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if operatingMode(obj) == "Suspended" || obj.GetDeletionTimestamp() != nil {
			return ErrStateChanged
		}
		if by != StoppedByIdle && FromSandbox(obj).State != Running {
			// Still starting: nothing to snapshot.
		} else if snap, err = s.snap.take(ctx, obj); err != nil {
			slog.Warn("snapshot failed; the session will wake cold", "session", id, "err", err)
		}
		if stillWanted != nil && !stillWanted() {
			if snap != nil {
				s.snap.discard(ctx, snap.name)
			}
			return ErrStateChanged
		}
	}
	err := s.modify(ctx, id, func(obj *unstructured.Unstructured) (bool, error) {
		if operatingMode(obj) == "Suspended" || obj.GetDeletionTimestamp() != nil {
			return false, ErrStateChanged
		}
		setAnnotation(obj, AnnStoppedBy, by)
		setAnnotation(obj, AnnDraining, "")
		setAnnotation(obj, AnnDrainingSince, "")
		if s.snap != nil {
			if err := setSnapshot(obj, snap); err != nil {
				return false, err
			}
		}
		return true, unstructured.SetNestedField(obj.Object, "Suspended", "spec", "operatingMode")
	})
	if s.snap == nil {
		return err
	}
	if err != nil {
		// Stopped or deleted while the snapshot was being taken.
		if snap != nil {
			s.snap.discard(ctx, snap.name)
		}
		return err
	}
	keep := ""
	if snap != nil {
		keep = snap.name
		slog.Info("session snapshotted", "session", id, "snapshot", snap.name, "pool", snap.pool)
	}
	s.snap.pruneLogged(ctx, id, keep)
	return nil
}

// ColdStart gives up on restoring a waking session from its snapshot: the
// snapshot is deleted, the pin to its node pool removed, and the pod made
// again, to start cold on any pool. It reports false, and does nothing, for
// a session that has no snapshot to give up on.
func (s *Store) ColdStart(ctx context.Context, id string) (bool, error) {
	if s.snap == nil {
		return false, nil
	}
	obj, err := s.client.Get(ctx, id, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	deleted, err := s.snap.prune(ctx, id, "")
	if err != nil {
		return false, err
	}
	if deleted == 0 && obj.GetAnnotations()[AnnSnapshot] == "" && obj.GetAnnotations()[AnnSnapshotPool] == "" {
		return false, nil
	}
	slog.Warn("restore from snapshot abandoned; starting cold", "session", id)

	// A pod made while the snapshot still exists would restore from it again.
	err = s.snap.until(ctx, func() (bool, error) {
		names, err := s.snap.of(ctx, id)
		return len(names) == 0, err
	})
	if err != nil {
		return true, fmt.Errorf("snapshots still there: %w", err)
	}
	// The pod template only applies to a new pod: suspend to remove the one
	// that is stuck, then run again.
	err = s.modify(ctx, id, func(obj *unstructured.Unstructured) (bool, error) {
		if operatingMode(obj) == "Suspended" && !wakes(obj.GetAnnotations()[AnnStoppedBy]) {
			return false, ErrStateChanged
		}
		setAnnotation(obj, AnnStoppedBy, StoppedByIdle)
		if err := setSnapshot(obj, nil); err != nil {
			return false, err
		}
		return true, unstructured.SetNestedField(obj.Object, "Suspended", "spec", "operatingMode")
	})
	if err != nil {
		return true, err
	}
	// Wait for the controller to have acted on this suspend: a Suspended
	// condition can be left over, still true, from the sleep before.
	err = s.snap.until(ctx, func() (bool, error) {
		obj, err := s.client.Get(ctx, id, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		c := conditions(obj)["Suspended"]
		return operatingMode(obj) != "Suspended" ||
			(c.status == "True" && c.observedGeneration >= obj.GetGeneration()), nil
	})
	if apierrors.IsNotFound(err) {
		return true, ErrNotFound
	}
	if err != nil {
		return true, err
	}
	return true, s.Wake(ctx, id)
}
