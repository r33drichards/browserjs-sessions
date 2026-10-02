package sessions

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"text/template"
	"time"
	"unicode"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"
)

var (
	ErrNotFound      = errors.New("session not found")
	ErrInvalidName   = errors.New("name must be 1–63 characters, with no control characters")
	ErrInvalidAction = errors.New(`action must be "stop" or "resume"`)
	ErrOwnerRequired = errors.New("owner is required")
	// ErrStateChanged is returned by a conditional change (an idle suspend, a
	// wake) that no longer applies because the user got there first.
	ErrStateChanged = errors.New("session was stopped by its user")
)

type Store struct {
	client    dynamic.ResourceInterface
	blueprint *template.Template
	publicURL string
	urls      *URLTemplate
	snap      *snapshotter // nil: no Pod Snapshots (see EnableSnapshots)

	// See sizes.go. small is the blueprint's own Sandbox spec; sizes is
	// never nil, and holds small alone until EnableSizes.
	small map[string]any
	sizes *sizes

	// See warm.go. warmPool is "" when new sessions do not come from a pool.
	claims   dynamic.ResourceInterface
	warmPool string
	warmWait time.Duration

	// See policy.go. policies is nil when sessions get no SessionPolicy.
	policies      dynamic.ResourceInterface
	defaultPolicy PolicySpec
}

// NewStore parses blueprint (see deploy/base/blueprint.yaml for the format).
// The blueprint is a template over .ID (the session's ID), .SessionURL (the
// session's own base URL, from urls) and .PublicURL (the app's base URL).
func NewStore(client dynamic.Interface, namespace, blueprint, publicURL string, urls *URLTemplate) (*Store, error) {
	if urls == nil {
		return nil, errors.New("sessions: a session URL template is required")
	}
	tmpl, err := template.New("blueprint").Option("missingkey=error").Parse(blueprint)
	if err != nil {
		return nil, fmt.Errorf("blueprint: %w", err)
	}
	s := &Store{
		client:    client.Resource(SandboxGVR).Namespace(namespace),
		claims:    client.Resource(ClaimGVR).Namespace(namespace),
		blueprint: tmpl,
		publicURL: publicURL,
		urls:      urls,
	}
	// What small is: the blueprint's resources do not depend on the session.
	// A blueprint that does not render is reported when a session is made.
	if s.small, err = s.render("s-aaaaaaaaaa"); err != nil {
		s.small = map[string]any{}
	}
	if s.sizes, err = newSizes(s.small, SizesFile{}); err != nil {
		return nil, err
	}
	return s, nil
}

// render is the blueprint as the Sandbox spec of the session id.
func (s *Store) render(id string) (map[string]any, error) {
	var rendered bytes.Buffer
	if err := s.blueprint.Execute(&rendered, map[string]string{
		"ID": id, "SessionURL": s.urls.Base(id), "PublicURL": s.publicURL,
	}); err != nil {
		return nil, fmt.Errorf("render blueprint: %w", err)
	}
	spec := map[string]any{}
	if err := yaml.Unmarshal(rendered.Bytes(), &spec); err != nil {
		return nil, fmt.Errorf("parse blueprint: %w", err)
	}
	return spec, nil
}

func newID() string {
	b := make([]byte, 7)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "s-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))[:10]
}

func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 63 || strings.ContainsFunc(name, unicode.IsControl) {
		return "", ErrInvalidName
	}
	return name, nil
}

// CheckName reports ErrInvalidName for a name a session cannot have.
func CheckName(name string) error {
	_, err := cleanName(name)
	return err
}

func (s *Store) Create(ctx context.Context, name, owner string) (Session, error) {
	return s.CreateWithPolicy(ctx, name, owner, nil)
}

// CreateWithPolicy is Create for a session that is to have the policy asked
// (nil: the unrestricted one). The policy is taken to be valid; whoever asks
// has had the operator check it. With policies not enabled, only nil can be
// asked for.
func (s *Store) CreateWithPolicy(ctx context.Context, name, owner string, asked *PolicySpec) (Session, error) {
	return s.CreateSized(ctx, name, owner, "", asked)
}

// CreateSized is CreateWithPolicy for a session of a size ("" is small).
// Only a small one can come from the warm pool; any other starts cold. One
// that no session node has room for, and no new node could take, is not
// made: ErrNoCapacity.
func (s *Store) CreateSized(ctx context.Context, name, owner, size string, asked *PolicySpec) (Session, error) {
	size, err := s.size(size)
	if err != nil {
		return Session{}, err
	}
	name, err = cleanName(name)
	if err != nil {
		return Session{}, err
	}
	if owner == "" {
		return Session{}, ErrOwnerRequired
	}
	var policy *PolicySpec // nil: none is made
	switch {
	case s.policies == nil && asked != nil:
		return Session{}, ErrPolicyUnsupported
	case s.policies != nil && asked == nil:
		policy = &s.defaultPolicy
	default:
		policy = asked
	}
	// A canary session has images no warm pod runs: it starts cold.
	canary := imageDigests(ctx)
	if canary != nil {
		if err := CheckImageDigests(canary); err != nil {
			return Session{}, err
		}
	}
	if s.warmPool != "" && canary == nil && size == DefaultSize {
		warm, err := s.createWarm(ctx, name, owner, policy)
		if err == nil {
			return warm, nil
		}
		slog.Warn("no session from the warm pool; starting one cold", "err", err)
	}
	id := newID()

	spec, err := s.render(id)
	if err != nil {
		return Session{}, err
	}
	annotations := map[string]any{AnnName: name, AnnOwner: owner}
	if size != DefaultSize {
		if err := s.sizes.apply(spec, size); err != nil {
			return Session{}, err
		}
		annotations[AnnSize] = size
	}
	if err := s.room(ctx, size, spec, ""); err != nil {
		return Session{}, err
	}
	spec["operatingMode"] = "Running"
	// Its idle period starts now (activity.go).
	annotations[AnnLastActive] = time.Now().UTC().Format(timeLayout)
	if canary != nil {
		if err := applyImageDigests(spec, canary); err != nil {
			return Session{}, err
		}
		annotations[AnnCanary] = canaryAnnotation(canary)
	}
	// Stamp the owner on the pod as well as the Sandbox.
	if err := unstructured.SetNestedField(spec, OwnerLabel(owner), "podTemplate", "metadata", "labels", LabelOwner); err != nil {
		return Session{}, fmt.Errorf("blueprint podTemplate.metadata.labels: %w", err)
	}
	if policy != nil && !policyCapableSpec(spec) {
		// A blueprint from before policies: its sessions never ask OPA. One
		// that was asked to be restricted must not be made unrestricted.
		if asked != nil {
			return Session{}, fmt.Errorf("the blueprint's mcp-js does not ask OPA for decisions: %w", ErrPolicyUnsupported)
		}
		policy = nil
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": SandboxGVR.GroupVersion().String(),
		"kind":       "Sandbox",
		"metadata": map[string]any{
			"name":        id,
			"labels":      map[string]any{LabelOwner: OwnerLabel(owner)},
			"annotations": annotations,
		},
		"spec": spec,
	}}
	created, err := s.client.Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return Session{}, err
	}
	if policy != nil {
		if err := s.ensurePolicy(ctx, created, owner, *policy); err != nil {
			// A session without its policy is denied everything.
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if derr := s.client.Delete(cleanup, id, metav1.DeleteOptions{}); derr != nil && !apierrors.IsNotFound(derr) {
				slog.Error("could not delete a session whose policy could not be made; delete it by hand", "session", id, "err", derr)
			}
			return Session{}, fmt.Errorf("create policy: %w", err)
		}
	}
	return FromSandbox(created), nil
}

func (s *Store) Get(ctx context.Context, id string) (Session, error) {
	obj, err := s.client.Get(ctx, id, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	return FromSandbox(obj), nil
}

// List returns owner's sessions. For everyone's, ask ListAll: an empty owner
// is an error, so a caller that lost track of who is asking gets nothing.
func (s *Store) List(ctx context.Context, owner string) ([]Session, error) {
	if owner == "" {
		return nil, ErrOwnerRequired
	}
	return s.list(ctx, labels.SelectorFromSet(labels.Set{LabelOwner: OwnerLabel(owner)}).String())
}

// ListAll returns every user's sessions.
func (s *Store) ListAll(ctx context.Context) ([]Session, error) {
	return s.list(ctx, LabelOwner)
}

func (s *Store) list(ctx context.Context, selector string) ([]Session, error) {
	list, err := s.client.List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, FromSandbox(&list.Items[i]))
	}
	return out, nil
}

// What a user can ask Update to do to a session.
const (
	ActionStop   = "stop"   // remove the pod, keep the disk, stay stopped
	ActionResume = "resume" // start it again
)

// Update renames a session (name non-nil) and applies action (which may be
// empty) in a single write, so the request takes effect entirely or not at
// all. Both are validated before anything is written.
func (s *Store) Update(ctx context.Context, id string, name *string, action string) error {
	annotations := map[string]any{}
	patch := map[string]any{}
	if name != nil {
		clean, err := cleanName(*name)
		if err != nil {
			return err
		}
		annotations[AnnName] = clean
	}
	switch action {
	case "":
	case ActionStop:
		annotations[AnnStoppedBy] = StoppedByUser
		annotations[AnnLastActive] = nil
		patch["spec"] = map[string]any{"operatingMode": "Suspended"}
	case ActionResume:
		annotations[AnnStoppedBy] = nil
		// Its idle period starts now (activity.go).
		annotations[AnnLastActive] = time.Now().UTC().Format(timeLayout)
		patch["spec"] = map[string]any{"operatingMode": "Running"}
	default:
		return ErrInvalidAction
	}
	if len(annotations) == 0 {
		return nil
	}
	if action == ActionResume {
		// A resize that was waiting for this start, and room to start in.
		if _, err := s.start(ctx, id); err != nil {
			return err
		}
	}
	if s.snap != nil && action != "" {
		// A user's stop takes no snapshot, and an older one must not be
		// restored over what the session did since: forget it. A resume
		// keeps a snapshot that is still good (the session was asleep).
		obj, err := s.client.Get(ctx, id, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		ann := obj.GetAnnotations()
		if action == ActionStop || ann[AnnSnapshot] == "" || !s.snap.ready(ctx, ann[AnnSnapshot]) {
			annotations[AnnSnapshot], annotations[AnnSnapshotPool] = nil, nil
			if _, pinned := ann[AnnSnapshotPool]; pinned {
				spec, _ := patch["spec"].(map[string]any)
				spec["podTemplate"] = map[string]any{"spec": map[string]any{"nodeSelector": map[string]any{LabelPool: nil}}}
			}
		}
	}
	patch["metadata"] = map[string]any{"annotations": annotations}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = s.client.Patch(ctx, id, types.MergePatchType, body, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return ErrNotFound
	}
	if err == nil && s.snap != nil && action == ActionStop {
		s.snap.pruneLogged(ctx, id, "")
	}
	if err == nil && action == ActionStop {
		s.settle(ctx, id)
	}
	return err
}

// settle applies a resize that was waiting to a session that has just been
// suspended. One that is missed here is applied when the session starts.
func (s *Store) settle(ctx context.Context, id string) {
	resized := false
	err := s.modify(ctx, id, func(obj *unstructured.Unstructured) (bool, error) {
		resized = false
		if operatingMode(obj) != "Suspended" || obj.GetAnnotations()[AnnResizeTo] == "" {
			return false, nil
		}
		var err error
		resized, err = s.applyResize(obj)
		return true, err
	})
	if err != nil {
		slog.Warn("resize not applied at the stop; it is at the next start", "session", id, "err", err)
		return
	}
	if resized && s.snap != nil {
		s.snap.pruneLogged(ctx, id, "")
	}
}

func (s *Store) Rename(ctx context.Context, id, name string) error {
	return s.Update(ctx, id, &name, "")
}

// Suspend removes the session's pod and keeps its disk. by is StoppedByUser
// or StoppedByIdle; an idle suspend is a Sleep.
//
// A user's stop always applies. An idle suspend applies only to a session
// that is not suspended already: it returns ErrStateChanged rather than take
// over a stop the user asked for, which would let the next request wake it.
func (s *Store) Suspend(ctx context.Context, id, by string) error {
	switch by {
	case StoppedByUser:
		return s.Update(ctx, id, nil, ActionStop)
	case StoppedByIdle:
		return s.Sleep(ctx, id, StoppedByIdle, nil)
	default:
		return fmt.Errorf("suspend: unknown reason %q", by)
	}
}

// Resume starts a session again on its user's request, however it stopped.
func (s *Store) Resume(ctx context.Context, id string) error {
	return s.Update(ctx, id, nil, ActionResume)
}

// Wake resumes a session that was put to sleep for being idle (or by its
// user, or for its owner's credit or payment method: whether the account now allows it is
// asked before Wake, not here); its pod is restored from the snapshot taken
// then, if there is one. It does nothing to one that is already awake, and
// returns ErrStateChanged for one its user stopped, including a stop that
// lands while Wake is in progress.
//
// It is safe from any number of replicas at once: the write is conditional
// on what was read, so one of them resumes the session and the others, on
// reading again, find it awake and do nothing.
func (s *Store) Wake(ctx context.Context, id string) error {
	resized := false
	err := s.modify(ctx, id, func(obj *unstructured.Unstructured) (bool, error) {
		resized = false
		if operatingMode(obj) != "Suspended" {
			return false, nil
		}
		if !wakes(obj.GetAnnotations()[AnnStoppedBy]) {
			return false, ErrStateChanged
		}
		setAnnotation(obj, AnnStoppedBy, "")
		// A resize that was waiting for this start, and room to start in.
		var err error
		if resized, err = s.applyResize(obj); err != nil {
			return false, err
		}
		if err := s.roomToStart(ctx, obj); err != nil {
			return false, err
		}
		startClock(obj)
		if err := s.keepOrDropSnapshot(ctx, obj); err != nil {
			return false, err
		}
		return true, unstructured.SetNestedField(obj.Object, "Running", "spec", "operatingMode")
	})
	if err == nil && resized && s.snap != nil {
		s.snap.pruneLogged(ctx, id, "")
	}
	return err
}

func operatingMode(obj *unstructured.Unstructured) string {
	mode, _, _ := unstructured.NestedString(obj.Object, "spec", "operatingMode")
	return mode
}

// setAnnotation sets an annotation, or removes it when value is "".
func setAnnotation(obj *unstructured.Unstructured, key, value string) {
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	if value == "" {
		delete(ann, key)
	} else {
		ann[key] = value
	}
	obj.SetAnnotations(ann)
}

// How often modify re-reads after losing a race before giving up.
const modifyAttempts = 5

// modify is a compare-and-swap on a Sandbox: change inspects and edits a
// fresh read, and the write goes through only if nobody else wrote in the
// meantime (the update carries the read's resourceVersion). On a conflict
// change runs again on a new read, so its decision is always made on the
// state it is about to replace. change returns false to write nothing.
func (s *Store) modify(ctx context.Context, id string, change func(obj *unstructured.Unstructured) (bool, error)) error {
	var err error
	for range modifyAttempts {
		var obj *unstructured.Unstructured
		if obj, err = s.client.Get(ctx, id, metav1.GetOptions{}); err != nil {
			break
		}
		var write bool
		if write, err = change(obj); err != nil || !write {
			return err
		}
		if _, err = s.client.Update(ctx, obj, metav1.UpdateOptions{}); !apierrors.IsConflict(err) {
			break
		}
	}
	if apierrors.IsNotFound(err) {
		return ErrNotFound
	}
	return err
}

// Delete removes the session. Its disk goes with it (the PVC is owned by the
// Sandbox — confirmed against a real cluster in Task 20), and so do its
// snapshots: first, so that a failure leaves a session to delete again
// rather than a snapshot nobody knows of.
func (s *Store) Delete(ctx context.Context, id string) error {
	if s.snap != nil {
		if _, err := s.snap.prune(ctx, id, ""); err != nil {
			return err
		}
	}
	err := s.releaseClaim(ctx, id)
	if err == nil {
		err = s.client.Delete(ctx, id, metav1.DeleteOptions{})
	}
	if apierrors.IsNotFound(err) {
		return ErrNotFound
	}
	if err == nil {
		s.deletePolicy(ctx, id)
	}
	return err
}

// SetDraining marks a running session as draining for reason (one of
// billing's sleep reasons), or removes the mark when reason is "". A session
// that is suspended has nothing to drain: ErrStateChanged.
func (s *Store) SetDraining(ctx context.Context, id, reason string) error {
	if reason != "" && (!sleepReason(reason) || reason == StoppedByIdle) {
		return fmt.Errorf("draining: unknown reason %q", reason)
	}
	return s.modify(ctx, id, func(obj *unstructured.Unstructured) (bool, error) {
		ann := obj.GetAnnotations()
		if reason == "" {
			if ann[AnnDraining] == "" && ann[AnnDrainingSince] == "" {
				return false, nil
			}
			setAnnotation(obj, AnnDraining, "")
			setAnnotation(obj, AnnDrainingSince, "")
			return true, nil
		}
		if operatingMode(obj) == "Suspended" || obj.GetDeletionTimestamp() != nil {
			return false, ErrStateChanged
		}
		if ann[AnnDraining] == reason {
			return false, nil // the mark keeps its time
		}
		setAnnotation(obj, AnnDraining, reason)
		setAnnotation(obj, AnnDrainingSince, time.Now().UTC().Format(time.RFC3339))
		return true, nil
	})
}
