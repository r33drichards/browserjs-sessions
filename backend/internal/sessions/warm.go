package sessions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The warm pool: Agent Sandbox keeps a few session Sandboxes started ahead
// of time (a SandboxWarmPool over a SandboxTemplate, deploy/gke/warmpool.yaml),
// and a SandboxClaim takes one of them. The Sandbox keeps the name the pool
// gave it, its pod and its disk, and from then on belongs to the claim rather
// than the pool. So a new session can be a Sandbox that is already running:
// the claim is made, the Sandbox it was bound to is given its owner, and that
// Sandbox's name is the session's ID. Everything after creation (sleep, wake,
// rename) is the same as for a Sandbox the backend made itself.

var ClaimGVR = schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxclaims"}

// AnnCreated is when a pooled Sandbox became a session. Its own creation
// time is when the pool started it, which may be days earlier.
const AnnCreated = "browserjs.dev/created"

// How often a new claim is read while waiting for its Sandbox.
const claimPoll = 50 * time.Millisecond

// EnableWarmPool makes Create take new sessions from the SandboxWarmPool
// named pool, waiting up to wait for the claim to be bound before starting
// the session cold instead. An empty pool name turns it off again.
//
// Session IDs are Sandbox names, and the pool names its Sandboxes
// "<pool>-<5 characters>": only a pool called "s" makes names that are
// session IDs (see ValidID). A Sandbox from any other pool is not used.
func (s *Store) EnableWarmPool(pool string, wait time.Duration) {
	s.warmPool, s.warmWait = pool, wait
}

// createWarm makes a session out of a Sandbox from the warm pool. It leaves
// nothing behind when it fails, so the caller can start the session cold.
func (s *Store) createWarm(ctx context.Context, name, owner string, policy *PolicySpec) (Session, error) {
	// The claim carries the owner from the start: if this process dies
	// before the Sandbox has it, RecoverClaims can finish the job. And the
	// policy, so that the job is not finished with another one.
	annotations := map[string]any{AnnName: name, AnnOwner: owner}
	if policy != nil {
		for k, v := range claimPolicyAnnotations(*policy) {
			annotations[k] = v
		}
	}
	claim, err := s.claims.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": ClaimGVR.GroupVersion().String(),
		"kind":       "SandboxClaim",
		"metadata": map[string]any{
			"name":        newID(),
			"labels":      map[string]any{LabelOwner: OwnerLabel(owner)},
			"annotations": annotations,
		},
		"spec": map[string]any{"warmPoolRef": map[string]any{"name": s.warmPool}},
	}}, metav1.CreateOptions{})
	if err != nil {
		return Session{}, fmt.Errorf("create claim: %w", err)
	}
	session, err := s.adopt(ctx, claim.GetName(), s.warmWait)
	if err != nil {
		// Without its claim the Sandbox goes too, and the pool makes another.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if derr := s.claims.Delete(cleanup, claim.GetName(), metav1.DeleteOptions{}); derr != nil && !apierrors.IsNotFound(derr) {
			slog.Error("could not delete a failed claim; delete it by hand", "claim", claim.GetName(), "err", derr)
		}
		return Session{}, err
	}
	return session, nil
}

// adopt waits up to wait for the claim to be bound to a Sandbox, and makes
// that Sandbox the session of the user the claim names. Doing it again
// changes nothing.
func (s *Store) adopt(ctx context.Context, claimName string, wait time.Duration) (Session, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var claim *unstructured.Unstructured
	var id string
	for {
		var err error
		if claim, err = s.claims.Get(ctx, claimName, metav1.GetOptions{}); err != nil {
			return Session{}, fmt.Errorf("claim %s: %w", claimName, err)
		}
		if id, _, _ = unstructured.NestedString(claim.Object, "status", "sandbox", "name"); id != "" {
			break
		}
		select {
		case <-ctx.Done():
			return Session{}, fmt.Errorf("claim %s was not bound to a Sandbox within %s", claimName, wait)
		case <-time.After(claimPoll):
		}
	}
	// The name becomes a host name and is what every later request is
	// checked against.
	if !ValidID(id) {
		return Session{}, fmt.Errorf("claim %s was bound to Sandbox %q, which is not a session ID: the warm pool must be named \"s\"", claimName, id)
	}
	owner, name := claim.GetAnnotations()[AnnOwner], claim.GetAnnotations()[AnnName]
	if owner == "" {
		return Session{}, fmt.Errorf("claim %s names no owner", claimName)
	}
	if err := s.refuseRestored(ctx, claimName, id); err != nil {
		return Session{}, err
	}
	// Before the owner: until it has one, nobody can use the Sandbox.
	if err := s.adoptPolicy(ctx, claim, id, owner); err != nil {
		return Session{}, err
	}
	var adopted *unstructured.Unstructured
	err := s.modify(ctx, id, func(obj *unstructured.Unstructured) (bool, error) {
		adopted = obj
		if ref := metav1.GetControllerOf(obj); ref == nil || ref.Kind != "SandboxClaim" || ref.UID != claim.GetUID() {
			return false, fmt.Errorf("Sandbox %s does not belong to claim %s", id, claimName)
		}
		switch obj.GetAnnotations()[AnnOwner] {
		case owner:
			return false, nil
		case "":
		default:
			// Never hand one user a Sandbox another has used.
			return false, fmt.Errorf("Sandbox %s already has an owner", id)
		}
		labels := obj.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[LabelOwner] = OwnerLabel(owner)
		obj.SetLabels(labels)
		setAnnotation(obj, AnnOwner, owner)
		setAnnotation(obj, AnnName, name)
		setAnnotation(obj, AnnCreated, time.Now().UTC().Format(time.RFC3339))
		return true, unstructured.SetNestedField(obj.Object, "Running", "spec", "operatingMode")
	})
	if err != nil {
		return Session{}, err
	}
	return FromSandbox(adopted), nil
}

// refuseRestored keeps a pooled Sandbox that has snapshots from becoming a
// session. A pod is restored from the newest snapshot of its Sandbox's name
// (deploy/gke/snapshots.yaml), and the pool's names are short enough to come
// round again: a Sandbox named like a session that was removed without its
// snapshots (by hand, not by Delete) may be running that session's memory.
// The claim, and with it the Sandbox, is deleted before the snapshots are, so
// that the pool's next Sandbox of that name starts clean.
func (s *Store) refuseRestored(ctx context.Context, claimName, id string) error {
	if s.snap == nil {
		return nil
	}
	obj, err := s.client.Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if obj.GetAnnotations()[AnnOwner] != "" {
		return nil // a session already: its snapshots are its own
	}
	stale, err := s.snap.of(ctx, id)
	if err != nil {
		return fmt.Errorf("list snapshots: %w", err)
	}
	if len(stale) == 0 {
		return nil
	}
	if err := s.claims.Delete(ctx, claimName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("Sandbox %s has snapshots %v from an earlier session and its claim could not be deleted: %w", id, stale, err)
	}
	s.snap.pruneLogged(ctx, id, "")
	return fmt.Errorf("Sandbox %s had snapshots %v from an earlier session of that name; it was deleted", id, stale)
}

// RecoverClaims finishes what a backend that died in the middle of Create
// left undone: a claim bound to a Sandbox that was never given its owner,
// which runs where no user and no idle sweep can see it. Run it at startup.
func (s *Store) RecoverClaims(ctx context.Context) error {
	list, err := s.claims.List(ctx, metav1.ListOptions{LabelSelector: LabelOwner})
	if err != nil {
		return err
	}
	var errs []error
	for i := range list.Items {
		if _, err := s.adopt(ctx, list.Items[i].GetName(), time.Second); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// releaseClaim deletes the claim a session's Sandbox came from, if it came
// from one. The claim has to go first: while it exists, the claim controller
// answers the loss of its Sandbox by binding it to another.
func (s *Store) releaseClaim(ctx context.Context, id string) error {
	obj, err := s.client.Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return err
	}
	ref := metav1.GetControllerOf(obj)
	if ref == nil || ref.Kind != "SandboxClaim" {
		return nil
	}
	if err := s.claims.Delete(ctx, ref.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
