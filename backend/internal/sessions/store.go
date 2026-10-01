package sessions

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/template"
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
	return &Store{
		client:    client.Resource(SandboxGVR).Namespace(namespace),
		blueprint: tmpl,
		publicURL: publicURL,
		urls:      urls,
	}, nil
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

func (s *Store) Create(ctx context.Context, name, owner string) (Session, error) {
	name, err := cleanName(name)
	if err != nil {
		return Session{}, err
	}
	if owner == "" {
		return Session{}, ErrOwnerRequired
	}
	id := newID()

	var rendered bytes.Buffer
	if err := s.blueprint.Execute(&rendered, map[string]string{
		"ID": id, "SessionURL": s.urls.Base(id), "PublicURL": s.publicURL,
	}); err != nil {
		return Session{}, fmt.Errorf("render blueprint: %w", err)
	}
	spec := map[string]any{}
	if err := yaml.Unmarshal(rendered.Bytes(), &spec); err != nil {
		return Session{}, fmt.Errorf("parse blueprint: %w", err)
	}
	spec["operatingMode"] = "Running"
	// Stamp the owner on the pod as well as the Sandbox.
	if err := unstructured.SetNestedField(spec, OwnerLabel(owner), "podTemplate", "metadata", "labels", LabelOwner); err != nil {
		return Session{}, fmt.Errorf("blueprint podTemplate.metadata.labels: %w", err)
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": SandboxGVR.GroupVersion().String(),
		"kind":       "Sandbox",
		"metadata": map[string]any{
			"name":        id,
			"labels":      map[string]any{LabelOwner: OwnerLabel(owner)},
			"annotations": map[string]any{AnnName: name, AnnOwner: owner},
		},
		"spec": spec,
	}}
	created, err := s.client.Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return Session{}, err
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
		patch["spec"] = map[string]any{"operatingMode": "Suspended"}
	case ActionResume:
		annotations[AnnStoppedBy] = nil
		patch["spec"] = map[string]any{"operatingMode": "Running"}
	default:
		return ErrInvalidAction
	}
	if len(annotations) == 0 {
		return nil
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
	return err
}

func (s *Store) Rename(ctx context.Context, id, name string) error {
	return s.Update(ctx, id, &name, "")
}

// Suspend removes the session's pod and keeps its disk. by is StoppedByUser
// or StoppedByIdle.
//
// A user's stop always applies. An idle suspend applies only to a session
// that is not suspended already: it returns ErrStateChanged rather than take
// over a stop the user asked for, which would let the next request wake it.
func (s *Store) Suspend(ctx context.Context, id, by string) error {
	switch by {
	case StoppedByUser:
		return s.Update(ctx, id, nil, ActionStop)
	case StoppedByIdle:
		return s.modify(ctx, id, func(obj *unstructured.Unstructured) (bool, error) {
			if operatingMode(obj) == "Suspended" || obj.GetDeletionTimestamp() != nil {
				return false, ErrStateChanged
			}
			setAnnotation(obj, AnnStoppedBy, StoppedByIdle)
			return true, unstructured.SetNestedField(obj.Object, "Suspended", "spec", "operatingMode")
		})
	default:
		return fmt.Errorf("suspend: unknown reason %q", by)
	}
}

// Resume starts a session again on its user's request, however it stopped.
func (s *Store) Resume(ctx context.Context, id string) error {
	return s.Update(ctx, id, nil, ActionResume)
}

// Wake resumes a session that was put to sleep for being idle. It does
// nothing to one that is already awake, and returns ErrStateChanged for one
// its user stopped, including a stop that lands while Wake is in progress.
func (s *Store) Wake(ctx context.Context, id string) error {
	return s.modify(ctx, id, func(obj *unstructured.Unstructured) (bool, error) {
		if operatingMode(obj) != "Suspended" {
			return false, nil
		}
		if obj.GetAnnotations()[AnnStoppedBy] != StoppedByIdle {
			return false, ErrStateChanged
		}
		setAnnotation(obj, AnnStoppedBy, "")
		return true, unstructured.SetNestedField(obj.Object, "Running", "spec", "operatingMode")
	})
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
// Sandbox — confirmed against a real cluster in Task 20).
func (s *Store) Delete(ctx context.Context, id string) error {
	err := s.client.Delete(ctx, id, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return ErrNotFound
	}
	return err
}
