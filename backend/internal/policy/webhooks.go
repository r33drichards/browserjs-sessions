package policy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/auth"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// Webhook settings live alongside a session's policy but do not change
// enforcement. Secrets are never returned by reads.
func (h *Handlers) GetWebhook(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := caller(w, r, auth.ScopeSessionsRead, id); !ok || !h.capable(w, r, id) {
		return
	}
	obj, err := h.policies.Get(r.Context(), id, metav1.GetOptions{})
	if err != nil {
		clusterError(w, err)
		return
	}
	doc, found, _ := unstructured.NestedMap(obj.Object, "spec", "webhook")
	if !found {
		writeJSON(w, http.StatusOK, nil)
		return
	}
	secret, _ := doc["signing_secret"].(string)
	delete(doc, "signing_secret")
	doc["has_signing_secret"] = secret != ""
	writeJSON(w, http.StatusOK, doc)
}

func (h *Handlers) PutWebhook(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := caller(w, r, auth.ScopeSessionsWrite, id); !ok {
		return
	}
	if !h.capable(w, r, id) {
		return
	}
	var doc map[string]any
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBody))
	if err := dec.Decode(&doc); err != nil || doc == nil {
		writeError(w, http.StatusBadRequest, "the body must be a webhook object")
		return
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "the body must contain one webhook object")
		return
	}
	obj, err := h.policies.Get(r.Context(), id, metav1.GetOptions{})
	if err != nil {
		clusterError(w, err)
		return
	}
	// Omission preserves the current secret; an explicit empty string clears it.
	if _, given := doc["signing_secret"]; !given {
		secret, _, _ := unstructured.NestedString(obj.Object, "spec", "webhook", "signing_secret")
		if secret != "" {
			doc["signing_secret"] = secret
		}
	}
	body, _ := json.Marshal(doc)
	answer, err := h.operator.do(r.Context(), http.MethodPost, "/v1/webhooks/validate", body)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "webhooks cannot be validated right now")
		return
	}
	if answer.status >= 500 {
		writeError(w, http.StatusServiceUnavailable, "webhooks cannot be validated right now")
		return
	}
	if answer.status != http.StatusOK {
		writeError(w, http.StatusBadRequest, "invalid webhook settings: use a public HTTPS URL on port 443, batch size 1–500 and interval 1–60 seconds")
		return
	}
	var v Validation
	if err := json.Unmarshal(answer.body, &v); err != nil {
		writeError(w, http.StatusServiceUnavailable, "invalid validation response")
		return
	}
	if !v.OK {
		message := "the webhook filter does not validate"
		if len(v.Errors) > 0 {
			message += ": " + v.Errors[0].Message
		}
		writeJSON(w, http.StatusUnprocessableEntity, Error{Error: message, Errors: v.Errors})
		return
	}
	// ResourceVersion prevents a concurrent write from restoring an old secret.
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"resourceVersion": obj.GetResourceVersion()}, "spec": map[string]any{"webhook": doc}})
	if _, err := h.policies.Patch(r.Context(), id, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		clusterError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) DeleteWebhook(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := caller(w, r, auth.ScopeSessionsWrite, id); !ok {
		return
	}
	if !h.capable(w, r, id) {
		return
	}
	if _, err := h.policies.Patch(r.Context(), id, types.MergePatchType, []byte(`{"spec":{"webhook":null}}`), metav1.PatchOptions{}); err != nil {
		clusterError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RecordToolEvents is called by a proxy's bounded background uploader.
func (h *Handlers) RecordToolEvents(events []map[string]any) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body, _ := json.Marshal(events)
	a, err := h.operator.do(ctx, http.MethodPost, "/v1/tool-events", body)
	if err != nil || a.status != http.StatusNoContent {
		slog.Warn("tool event ingestion failed; request events omitted", "status", a.status)
		return
	} // best effort, independent of execution
}
