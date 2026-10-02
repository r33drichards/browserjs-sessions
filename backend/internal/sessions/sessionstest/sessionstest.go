// Package sessionstest builds a sessions.Store backed by a fake cluster.
package sessionstest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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

	"github.com/r33drichards/computer-use/backend/internal/sessions"
)

const Namespace = "browserjs-sessions"

// Where the fake deployment lives: the app on one host and the sessions on
// another, each under its ID. LegacyURLTemplate is where they used to be, a
// host each, and still answer.
const (
	PublicURL         = "https://app.example.com"
	URLTemplate       = "https://sessions.example.com/{id}"
	LegacyURLTemplate = "https://{id}.sessions.example.com"
)

func parsed(template string) *sessions.URLTemplate {
	urls, err := sessions.ParseURLTemplate(template)
	if err != nil {
		panic(err)
	}
	return urls
}

// URLs is URLTemplate, parsed.
func URLs() *sessions.URLTemplate { return parsed(URLTemplate) }

// LegacyURLs is LegacyURLTemplate, parsed.
func LegacyURLs() *sessions.URLTemplate { return parsed(LegacyURLTemplate) }

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
	return newStore(t, Blueprint)
}

// OldDigest is the digest both images of NewPinned's blueprint are named by.
const OldDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// NewPinned is New with a blueprint that names its images by digest, as a
// deployment's does: registry.test/browser@OldDigest and
// registry.test/mcp-js@OldDigest.
func NewPinned(t *testing.T) (*sessions.Store, dynamic.Interface) {
	t.Helper()
	blueprint := strings.NewReplacer(
		"image: browser:test", "image: registry.test/browser@"+OldDigest,
		"image: mcp-js:test", "image: registry.test/mcp-js@"+OldDigest,
	).Replace(Blueprint)
	return newStore(t, blueprint)
}

func newStore(t *testing.T, blueprint string) (*sessions.Store, dynamic.Interface) {
	t.Helper()
	client := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			sessions.SandboxGVR:         "SandboxList",
			sessions.PodSnapshotGVR:     "PodSnapshotList",
			sessions.SnapshotTriggerGVR: "PodSnapshotManualTriggerList",
			sessions.ClaimGVR:           "SandboxClaimList",
			sessions.PolicyGVR:          "SessionPolicyList",
		})
	emulateAPIServer(client)
	emulatePolicies(client)
	store, err := sessions.NewStore(contextAware{client}, Namespace, blueprint, PublicURL, URLs())
	if err != nil {
		t.Fatal(err)
	}
	return store, client
}

// Node and Pool are where every fake session's pod runs (see Ready).
const (
	Node = "gke-test-sessions-n2d-1"
	Pool = "sessions-n2d-standard-4"
)

// GKE plays the Pod Snapshot controller of a fake cluster: it answers each
// snapshot trigger with a ready PodSnapshot of the target pod. Its fields
// make it misbehave; set them before the store is used.
type GKE struct {
	Hang     bool // triggers are never answered
	Fail     bool // the checkpoint fails
	NotReady bool // snapshots are made but never become ready
	// OnTrigger, if set, runs while a snapshot is being taken.
	OnTrigger func()

	client *dynfake.FakeDynamicClient
	mu     sync.Mutex
	n      int
}

// NewWithSnapshots is New with Pod Snapshots enabled on the store, a node
// (Node, in pool Pool) for sessions to run on, and a GKE to take snapshots.
func NewWithSnapshots(t *testing.T, o sessions.SnapshotOptions) (*sessions.Store, dynamic.Interface, *GKE) {
	t.Helper()
	store, client := New(t)
	fake := client.(*dynfake.FakeDynamicClient)
	node := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Node",
		"metadata": map[string]any{"name": Node, "labels": map[string]any{sessions.LabelPool: Pool}},
	}}
	if err := fake.Tracker().Add(node); err != nil {
		t.Fatal(err)
	}
	gke := &GKE{client: fake}
	fake.PrependReactor("create", sessions.SnapshotTriggerGVR.Resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		trigger := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		gke.answer(trigger)
		return false, nil, nil // stored as answered
	})
	if o.Poll == 0 {
		o.Poll = time.Millisecond
	}
	store.EnableSnapshots(contextAware{client}, Namespace, o)
	return store, client, gke
}

func (g *GKE) answer(trigger *unstructured.Unstructured) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.OnTrigger != nil {
		g.OnTrigger()
	}
	switch {
	case g.Hang:
		return
	case g.Fail:
		_ = unstructured.SetNestedSlice(trigger.Object, []any{map[string]any{
			"type": "Triggered", "status": "False", "reason": "Failed", "message": "sandbox would not checkpoint",
		}}, "status", "conditions")
		return
	}
	g.n++
	pod, _, _ := unstructured.NestedString(trigger.Object, "spec", "targetPod")
	name := fmt.Sprintf("snap-%s-%d", pod, g.n)
	ready := "True"
	if g.NotReady {
		ready = "False"
	}
	_ = g.client.Tracker().Add(Snapshot(name, pod, ready))
	_ = unstructured.SetNestedSlice(trigger.Object, []any{map[string]any{
		"type": "Triggered", "status": "True", "reason": "Complete",
	}}, "status", "conditions")
	_ = unstructured.SetNestedField(trigger.Object, name, "status", "snapshotCreated", "name")
}

// Snapshot is a PodSnapshot of the pod of session id, as GKE makes it.
func Snapshot(name, id, ready string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": sessions.PodSnapshotGVR.GroupVersion().String(), "kind": "PodSnapshot",
		"metadata": map[string]any{
			"name": name, "namespace": Namespace,
			"annotations": map[string]any{"podsnapshot.gke.io/origin-pod": id},
		},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": ready}}},
	}}
}

// Snapshots lists the names of the PodSnapshots in the fake cluster.
func Snapshots(t *testing.T, client dynamic.Interface) []string {
	t.Helper()
	list, err := client.Resource(sessions.PodSnapshotGVR).Namespace(Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, item := range list.Items {
		names = append(names, item.GetName())
	}
	sort.Strings(names)
	return names
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
			obj.SetUID(types.UID("uid-" + obj.GetName()))
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
		"nodeName":   Node,
		"conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "DependenciesReady"}},
	}
}

// Suspended is the status of a session whose pod has been removed.
func Suspended() map[string]any {
	return map[string]any{
		"conditions": []any{map[string]any{"type": "Suspended", "status": "True", "reason": "PodTerminated"}},
	}
}

// WarmPoolName is the SandboxWarmPool the fake claim controller serves.
const WarmPoolName = "s"

// PlayClaimController answers every new SandboxClaim the way Agent Sandbox's
// claim controller does. A claim is bound to the next of warm, a Sandbox the
// pool made earlier and that keeps its own name; once warm runs out, to a new
// Sandbox named after the claim (a cold start from the template). The claim
// becomes the Sandbox's controlling owner and names it in its status.
func PlayClaimController(t *testing.T, client dynamic.Interface, warm ...string) {
	t.Helper()
	playClaimController(t, client, func() map[string]any { return map[string]any{} }, warm)
}

func playClaimController(t *testing.T, client dynamic.Interface, spec func() map[string]any, warm []string) {
	t.Helper()
	fake := client.(*dynfake.FakeDynamicClient)
	tracker := fake.Tracker()
	for _, name := range warm {
		pooled := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": sessions.SandboxGVR.GroupVersion().String(),
			"kind":       "Sandbox",
			"metadata": map[string]any{
				"name": name, "namespace": Namespace,
				"uid":               "uid-" + name,
				"creationTimestamp": "2026-10-01T00:00:00Z",
				"labels":            map[string]any{"agents.x-k8s.io/warm-pool-sandbox": "pool"},
			},
			"spec": spec(),
		}}
		if err := tracker.Create(sessions.SandboxGVR, pooled, Namespace); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	fake.PrependReactor("create", sessions.ClaimGVR.Resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		claim := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		claim.SetNamespace(Namespace)
		claim.SetUID(types.UID("uid-" + claim.GetName()))
		name := claim.GetName()
		if len(warm) > 0 {
			name, warm = warm[0], warm[1:]
		} else {
			cold := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": sessions.SandboxGVR.GroupVersion().String(),
				"kind":       "Sandbox",
				"metadata":   map[string]any{"name": name, "namespace": Namespace, "uid": "uid-" + name},
				"spec":       spec(),
			}}
			if err := tracker.Create(sessions.SandboxGVR, cold, Namespace); err != nil {
				return true, nil, err
			}
		}
		obj, err := tracker.Get(sessions.SandboxGVR, Namespace, name)
		if err != nil {
			return true, nil, err
		}
		sandbox := obj.(*unstructured.Unstructured).DeepCopy()
		controller := true
		sandbox.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: sessions.ClaimGVR.GroupVersion().String(), Kind: "SandboxClaim",
			Name: claim.GetName(), UID: claim.GetUID(), Controller: &controller,
		}})
		labels := sandbox.GetLabels()
		delete(labels, "agents.x-k8s.io/warm-pool-sandbox")
		sandbox.SetLabels(labels)
		bumpResourceVersion(sandbox)
		if err := tracker.Update(sessions.SandboxGVR, sandbox, Namespace); err != nil {
			return true, nil, err
		}
		if err := unstructured.SetNestedField(claim.Object, name, "status", "sandbox", "name"); err != nil {
			return true, nil, err
		}
		if err := tracker.Create(sessions.ClaimGVR, claim, Namespace); err != nil {
			return true, nil, err
		}
		return true, claim, nil
	})
}

// Claims lists the SandboxClaims in the fake cluster.
func Claims(t *testing.T, client dynamic.Interface) []unstructured.Unstructured {
	t.Helper()
	list, err := client.Resource(sessions.ClaimGVR).Namespace(Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return list.Items
}
