// Package sessionstest builds a sessions.Store backed by a fake cluster.
package sessionstest

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"

	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

const Namespace = "browserjs-sessions"

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
            value: "{{ .PublicURL }}/s/{{ .ID }}"
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
// can play the controller's part (set status).
func New(t *testing.T) (*sessions.Store, dynamic.Interface) {
	t.Helper()
	client := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{sessions.SandboxGVR: "SandboxList"})
	store, err := sessions.NewStore(client, Namespace, Blueprint, "https://sessions.example.com")
	if err != nil {
		t.Fatal(err)
	}
	return store, client
}

// SetStatus overwrites a Sandbox's status, as the controller would.
func SetStatus(t *testing.T, client dynamic.Interface, id string, status map[string]any) {
	t.Helper()
	ctx := context.Background()
	res := client.Resource(sessions.SandboxGVR).Namespace(Namespace)
	obj, err := res.Get(ctx, id, v1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedMap(obj.Object, status, "status"); err != nil {
		t.Fatal(err)
	}
	if _, err := res.Update(ctx, obj, v1.UpdateOptions{}); err != nil {
		t.Fatal(err)
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
