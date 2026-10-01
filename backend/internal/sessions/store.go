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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"
)

var (
	ErrNotFound    = errors.New("session not found")
	ErrInvalidName = errors.New("name must be 1–63 characters")
)

type Store struct {
	client    dynamic.ResourceInterface
	blueprint *template.Template
	publicURL string
}

// NewStore parses blueprint (see deploy/base/blueprint.yaml for the format).
func NewStore(client dynamic.Interface, namespace, blueprint, publicURL string) (*Store, error) {
	tmpl, err := template.New("blueprint").Option("missingkey=error").Parse(blueprint)
	if err != nil {
		return nil, fmt.Errorf("blueprint: %w", err)
	}
	return &Store{
		client:    client.Resource(SandboxGVR).Namespace(namespace),
		blueprint: tmpl,
		publicURL: publicURL,
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
	if name == "" || len(name) > 63 {
		return "", ErrInvalidName
	}
	return name, nil
}

func (s *Store) Create(ctx context.Context, name, owner string) (Session, error) {
	name, err := cleanName(name)
	if err != nil {
		return Session{}, err
	}
	id := newID()

	var rendered bytes.Buffer
	if err := s.blueprint.Execute(&rendered, map[string]string{"ID": id, "PublicURL": s.publicURL}); err != nil {
		return Session{}, fmt.Errorf("render blueprint: %w", err)
	}
	spec := map[string]any{}
	if err := yaml.Unmarshal(rendered.Bytes(), &spec); err != nil {
		return Session{}, fmt.Errorf("parse blueprint: %w", err)
	}
	spec["operatingMode"] = "Running"
	// Stamp the owner on the pod as well as the Sandbox.
	if err := unstructured.SetNestedField(spec, owner, "podTemplate", "metadata", "labels", LabelOwner); err != nil {
		return Session{}, fmt.Errorf("blueprint podTemplate.metadata.labels: %w", err)
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": SandboxGVR.GroupVersion().String(),
		"kind":       "Sandbox",
		"metadata": map[string]any{
			"name":        id,
			"labels":      map[string]any{LabelOwner: owner},
			"annotations": map[string]any{AnnName: name},
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

// List returns owner's sessions, or everyone's when owner is "".
func (s *Store) List(ctx context.Context, owner string) ([]Session, error) {
	opts := metav1.ListOptions{LabelSelector: LabelOwner}
	if owner != "" {
		opts.LabelSelector = LabelOwner + "=" + owner
	}
	list, err := s.client.List(ctx, opts)
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, FromSandbox(&list.Items[i]))
	}
	return out, nil
}

func (s *Store) patch(ctx context.Context, id string, patch map[string]any) error {
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
	name, err := cleanName(name)
	if err != nil {
		return err
	}
	return s.patch(ctx, id, map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{AnnName: name}},
	})
}

// Suspend removes the session's pod and keeps its disk. by is StoppedByUser
// or StoppedByIdle.
func (s *Store) Suspend(ctx context.Context, id, by string) error {
	return s.patch(ctx, id, map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{AnnStoppedBy: by}},
		"spec":     map[string]any{"operatingMode": "Suspended"},
	})
}

func (s *Store) Resume(ctx context.Context, id string) error {
	return s.patch(ctx, id, map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{AnnStoppedBy: nil}},
		"spec":     map[string]any{"operatingMode": "Running"},
	})
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
