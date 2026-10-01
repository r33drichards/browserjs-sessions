// Package sessionstest builds a sessions.Store backed by a fake cluster.
package sessionstest

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

const Namespace = "browserjs-sessions"

// Where the fake deployment lives: the app on one host, each session on its
// own.
const (
	PublicURL   = "https://app.example.com"
	URLTemplate = "https://{id}.sessions.example.com"
)

// URLs is URLTemplate, parsed.
func URLs() *sessions.URLTemplate {
	urls, err := sessions.ParseURLTemplate(URLTemplate)
	if err != nil {
		panic(err)
	}
	return urls
}

const Blueprint = `
podTemplate:
  metadata:
    labels:
      app: browserjs-session
  spec:
    containers:
      - name: browser
        image: browser:test
      - name: mcp-js
        image: mcp-js:test
        env:
          - name: MCP_V8_PUBLIC_URL
            value: "{{ .SessionURL }}"
volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      accessModes: ["ReadWriteOnce"]
      resources:
        requests:
          storage: 5Gi
`

// New returns a store on an empty fake cluster, plus the raw client so tests
// can play the controller's part (set status) and inspect what was written.
func New(t *testing.T) (*sessions.Store, dynamic.Interface) {
	t.Helper()
	client := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{sessions.SandboxGVR: "SandboxList"})
	emulateAPIServer(client)
	store, err := sessions.NewStore(contextAware{client}, Namespace, Blueprint, PublicURL, URLs())
	if err != nil {
		t.Fatal(err)
	}
	return store, client
}

// contextAware makes the store's requests fail once their context is done,
// as requests to a real API server do. (The stock fake ignores the context.)
type contextAware struct{ dynamic.Interface }

func (c contextAware) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return contextAwareResource{c.Interface.Resource(gvr)}
}

type contextAwareResource struct {
	dynamic.NamespaceableResourceInterface
}

func (r contextAwareResource) Namespace(ns string) dynamic.ResourceInterface {
	return contextAwareNamespace{r.NamespaceableResourceInterface.Namespace(ns)}
}

type contextAwareNamespace struct{ dynamic.ResourceInterface }

func (r contextAwareNamespace) Create(ctx context.Context, obj *unstructured.Unstructured, o metav1.CreateOptions, sub ...string) (*unstructured.Unstructured, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.ResourceInterface.Create(ctx, obj, o, sub...)
}

func (r contextAwareNamespace) Update(ctx context.Context, obj *unstructured.Unstructured, o metav1.UpdateOptions, sub ...string) (*unstructured.Unstructured, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.ResourceInterface.Update(ctx, obj, o, sub...)
}

func (r contextAwareNamespace) Delete(ctx context.Context, name string, o metav1.DeleteOptions, sub ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.ResourceInterface.Delete(ctx, name, o, sub...)
}

func (r contextAwareNamespace) Get(ctx context.Context, name string, o metav1.GetOptions, sub ...string) (*unstructured.Unstructured, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.ResourceInterface.Get(ctx, name, o, sub...)
}

func (r contextAwareNamespace) List(ctx context.Context, o metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.ResourceInterface.List(ctx, o)
}

func (r contextAwareNamespace) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, o metav1.PatchOptions, sub ...string) (*unstructured.Unstructured, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.ResourceInterface.Patch(ctx, name, pt, data, o, sub...)
}

// emulateAPIServer adds the two API server behaviours the stock fake leaves
// out and the store depends on: label values are validated on create, and
// every write bumps metadata.resourceVersion, with an update that carries a
// stale one refused as a conflict.
func emulateAPIServer(client *dynfake.FakeDynamicClient) {
	tracker := client.Tracker()
	apply := k8stesting.ObjectReaction(tracker)
	gr := sessions.SandboxGVR.GroupResource()
	client.PrependReactor("*", sessions.SandboxGVR.Resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		switch action.GetVerb() {
		case "create":
			obj := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
			podLabels, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "podTemplate", "metadata", "labels")
			for _, set := range []map[string]string{obj.GetLabels(), podLabels} {
				for k, v := range set {
					if problems := validation.IsValidLabelValue(v); len(problems) > 0 {
						return true, nil, apierrors.NewBadRequest(fmt.Sprintf("label %s=%q: %s", k, v, strings.Join(problems, "; ")))
					}
				}
			}
		case "update":
			obj := action.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured)
			cur, err := tracker.Get(sessions.SandboxGVR, action.GetNamespace(), obj.GetName())
			if err != nil {
				return true, nil, err
			}
			if rv := obj.GetResourceVersion(); rv != "" && rv != cur.(*unstructured.Unstructured).GetResourceVersion() {
				return true, nil, apierrors.NewConflict(gr, obj.GetName(), errors.New("the object has been modified"))
			}
		case "patch":
		default:
			return false, nil, nil
		}
		_, out, err := apply(action)
		if err != nil {
			return true, nil, err
		}
		obj := out.(*unstructured.Unstructured)
		bumpResourceVersion(obj)
		if err := tracker.Update(sessions.SandboxGVR, obj, action.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, obj, nil
	})
}

func bumpResourceVersion(obj *unstructured.Unstructured) {
	rv, _ := strconv.Atoi(obj.GetResourceVersion())
	obj.SetResourceVersion(strconv.Itoa(rv + 1))
}

// RaceNextGet makes the next read of Sandbox id lose a race: the reader is
// served the object as it was, and mutate is applied to the stored copy
// before the reader can act on what it saw.
func RaceNextGet(t *testing.T, client dynamic.Interface, id string, mutate func(obj *unstructured.Unstructured)) {
	t.Helper()
	fake := client.(*dynfake.FakeDynamicClient)
	tracker := fake.Tracker()
	var once sync.Once
	fake.PrependReactor("get", sessions.SandboxGVR.Resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.(k8stesting.GetAction).GetName() != id {
			return false, nil, nil
		}
		var seen runtime.Object
		var err error
		raced := false
		once.Do(func() {
			raced = true
			if seen, err = tracker.Get(sessions.SandboxGVR, Namespace, id); err != nil {
				return
			}
			changed := seen.DeepCopyObject().(*unstructured.Unstructured)
			mutate(changed)
			bumpResourceVersion(changed)
			err = tracker.Update(sessions.SandboxGVR, changed, Namespace)
		})
		if !raced {
			return false, nil, nil
		}
		return true, seen, err
	})
}

// UserStop edits a Sandbox the way a user's stop does.
func UserStop(obj *unstructured.Unstructured) {
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[sessions.AnnStoppedBy] = sessions.StoppedByUser
	obj.SetAnnotations(ann)
	_ = unstructured.SetNestedField(obj.Object, "Suspended", "spec", "operatingMode")
}

// SetStatus overwrites a Sandbox's status, as the controller would.
func SetStatus(t *testing.T, client dynamic.Interface, id string, status map[string]any) {
	t.Helper()
	if err := TrySetStatus(client, id, status); err != nil {
		t.Fatal(err)
	}
}

// TrySetStatus is SetStatus for use off the test goroutine.
func TrySetStatus(client dynamic.Interface, id string, status map[string]any) error {
	ctx := context.Background()
	res := client.Resource(sessions.SandboxGVR).Namespace(Namespace)
	for {
		obj, err := res.Get(ctx, id, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := unstructured.SetNestedMap(obj.Object, status, "status"); err != nil {
			return err
		}
		if _, err = res.Update(ctx, obj, metav1.UpdateOptions{}); !apierrors.IsConflict(err) {
			return err
		}
	}
}

// Ready is the status of a running session with a pod IP.
func Ready(podIP string) map[string]any {
	return map[string]any{
		"podIPs":     []any{podIP},
		"conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "DependenciesReady"}},
	}
}

// Suspended is the status of a session whose pod has been removed.
func Suspended() map[string]any {
	return map[string]any{
		"conditions": []any{map[string]any{"type": "Suspended", "status": "True", "reason": "PodTerminated"}},
	}
}
