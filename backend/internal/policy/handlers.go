package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

// Handlers serves the policy part of the API. The routes are registered by
// internal/api, which has checked that the caller may use the session
// before a handler that takes one is called.
type Handlers struct {
	store    *sessions.Store
	policies dynamic.ResourceInterface
	operator *Operator

	// Wait is how long a write waits for the policy to be in force before
	// answering that it is saved and still loading. Poll is how often it,
	// and Watch, look.
	Wait time.Duration
	Poll time.Duration
	// NewWait is how long Watch waits for a new session's policy.
	NewWait time.Duration
}

// New serves the policies of store's sessions, with operator as the judge
// of what is valid. It returns nil, which is "no policies" to internal/api,
// for a store that has none enabled (sessions.Store.EnablePolicies).
func New(store *sessions.Store, operator *Operator) *Handlers {
	if store.Policies() == nil {
		return nil
	}
	return &Handlers{
		store: store, policies: store.Policies(), operator: operator,
		Wait: 10 * time.Second, Poll: 250 * time.Millisecond, NewWait: 2 * time.Minute,
	}
}

// Input is a policy as a request states it.
type Input struct {
	Kind       string      `json:"kind"`
	Source     string      `json:"source"`
	Management *Management `json:"management,omitempty"`
}

// Error is the refusal of a policy that does not validate.
type Error struct {
	Error    string       `json:"error"`
	Errors   []Diagnostic `json:"errors"`
	Warnings []Diagnostic `json:"warnings,omitempty"`
}

const (
	msgUnsupported = "this session was created before policies; recreate it to give it one"
	msgNoPolicy    = "this session has no policy"
	msgInvalid     = "the policy does not validate"
	msgUnavailable = "policies cannot be checked right now; nothing was saved"
	// How large the body of a request that carries a policy may be: the
	// source's limit, with room for its JSON escaping.
	MaxBody = 512 << 10
	// How often a write is tried again after losing a race with another.
	saveAttempts = 5
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// caller resolves who is asking and whether their credential may: a token
// needs scope ("" for none), and must not be for a session other than id
// ("" for a route that names none). The API host checks the same before the
// request gets here; these handlers do not rely on being behind it.
func caller(w http.ResponseWriter, r *http.Request, scope, id string) (auth.User, bool) {
	u, ok := auth.UserFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "not signed in")
		return auth.User{}, false
	}
	if !hasScope(u, scope) {
		writeError(w, http.StatusForbidden, "this token lacks the scope "+scope)
		return auth.User{}, false
	}
	if id != "" && !forSession(u, id) {
		writeError(w, http.StatusForbidden, "this token is for another session")
		return auth.User{}, false
	}
	return u, true
}

// capable answers for a session that cannot have a policy, and reports
// whether id can.
func (h *Handlers) capable(w http.ResponseWriter, r *http.Request, id string) bool {
	s, err := h.store.Get(r.Context(), id)
	switch {
	case errors.Is(err, sessions.ErrNotFound):
		writeError(w, http.StatusNotFound, "session not found")
	case err != nil:
		clusterError(w, err)
	case !s.PolicyCapable:
		writeError(w, http.StatusConflict, msgUnsupported)
	default:
		return true
	}
	return false
}

func clusterError(w http.ResponseWriter, err error) {
	slog.Error("cluster request failed", "err", err)
	writeError(w, http.StatusInternalServerError, "cluster request failed")
}

// checkInput checks what can be checked of a policy without the operator.
// It answers for one that fails.
func checkInput(w http.ResponseWriter, in *Input) bool {
	// Rego is the only kind; a policy that names none is Rego.
	if in.Kind == "" {
		in.Kind = KindRego
	}
	if in.Kind != KindRego {
		writeError(w, http.StatusBadRequest, `kind must be "rego"`)
		return false
	}
	if in.Management != nil {
		m, ok := cleanManagement(*in.Management)
		if !ok {
			writeError(w, http.StatusBadRequest, badManagement)
			return false
		}
		in.Management = &m
	}
	if len(in.Source) == 0 || len(in.Source) > MaxSource {
		writeJSON(w, http.StatusUnprocessableEntity, Error{Error: msgInvalid, Errors: []Diagnostic{{
			Code: "size_error", Message: fmt.Sprintf("a policy is 1 to %d bytes long; this one is %d", MaxSource, len(in.Source)),
		}}})
		return false
	}
	return true
}

// validate has the operator check a policy, and answers for one that does
// not pass or could not be checked.
func (h *Handlers) validate(w http.ResponseWriter, r *http.Request, kind, source string) bool {
	v, err := h.operator.Validate(r.Context(), kind, source)
	if err != nil {
		slog.Error("policy not validated", "err", err)
		writeError(w, http.StatusServiceUnavailable, msgUnavailable)
		return false
	}
	if !v.OK {
		writeJSON(w, http.StatusUnprocessableEntity, Error{Error: msgInvalid, Errors: v.Errors, Warnings: v.Warnings})
		return false
	}
	return true
}

// ForCreate checks the policy a new session is asked to have (nil: none,
// which is the unrestricted one and needs no check). It answers the request
// itself when the session must not be created, and reports whether it may.
func (h *Handlers) ForCreate(w http.ResponseWriter, r *http.Request, u auth.User, in *Input) (*sessions.PolicySpec, bool) {
	if in == nil {
		return nil, true
	}
	if !hasScope(u, ScopeWrite) {
		writeError(w, http.StatusForbidden, "this token lacks the scope "+ScopeWrite)
		return nil, false
	}
	if u.Token != nil && u.Token.Session != "" {
		writeError(w, http.StatusForbidden, "this token is for one session")
		return nil, false
	}
	if !checkInput(w, in) || !h.validate(w, r, in.Kind, in.Source) {
		return nil, false
	}
	spec := &sessions.PolicySpec{Kind: in.Kind, Source: in.Source, Mode: sessions.PolicyModeEditor, UpdatedBy: updatedBy(u)}
	if in.Management != nil {
		spec.Mode, spec.ManagedURL = in.Management.Mode, in.Management.ManagedURL
	}
	return spec, true
}

// Watch follows a new session until the policy it was created with is in
// force. If the operator refuses that policy after all (the backend had it
// checked before creating anything), the session is deleted: it would never
// be allowed anything. A policy that has been written to since is its
// owner's business, and is left alone.
func (h *Handlers) Watch(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(ctx, h.NewWait)
	defer cancel()
	for {
		obj, err := h.policies.Get(ctx, id, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			return
		case err == nil:
			s := summaryOf(obj)
			if s.inForce || s.Version != 1 {
				return
			}
			if s.State == Invalid {
				slog.Error("the operator refused a new session's policy; deleting the session", "session", id, "errors", diagnostics(obj, "errors"))
				if err := h.store.Delete(ctx, id); err != nil && !errors.Is(err, sessions.ErrNotFound) {
					slog.Error("could not delete a session whose policy was refused", "session", id, "err", err)
				}
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(h.Poll):
		}
	}
}

// Summary is the policy of s as a session carries it.
func (h *Handlers) Summary(ctx context.Context, s sessions.Session) (Summary, error) {
	if !s.PolicyCapable {
		return Summary{State: Unsupported}, nil
	}
	obj, err := h.policies.Get(ctx, s.ID, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// Removed behind the backend. Nothing is in force.
		return Summary{State: Loading}, nil
	}
	if err != nil {
		return Summary{}, err
	}
	return summaryOf(obj), nil
}

// Summaries is Summary for each of list, by session ID, from one read of
// the cluster: owner's policies, or everyone's when owner is "".
func (h *Handlers) Summaries(ctx context.Context, list []sessions.Session, owner string) (map[string]Summary, error) {
	selector := sessions.LabelOwner
	if owner != "" {
		selector = labels.SelectorFromSet(labels.Set{sessions.LabelOwner: sessions.OwnerLabel(owner)}).String()
	}
	objs, err := h.policies.List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	found := make(map[string]Summary, len(objs.Items))
	for i := range objs.Items {
		found[objs.Items[i].GetName()] = summaryOf(&objs.Items[i])
	}
	out := make(map[string]Summary, len(list))
	for _, s := range list {
		summary, ok := found[s.ID]
		switch {
		case !s.PolicyCapable:
			summary = Summary{State: Unsupported}
		case !ok:
			summary = Summary{State: Loading}
		}
		out[s.ID] = summary
	}
	return out, nil
}

func etag(version int64) string { return `"` + strconv.FormatInt(version, 10) + `"` }

// ifMatch reports whether an If-Match header lets a write to the policy at
// version go ahead. exists is whether there is a policy at all.
func ifMatch(header string, version int64, exists bool) bool {
	if strings.TrimSpace(header) == "" {
		return true
	}
	if !exists {
		return false
	}
	for _, tag := range strings.Split(header, ",") {
		tag = strings.TrimSpace(tag)
		if tag == "*" || strings.Trim(strings.TrimPrefix(tag, "W/"), `"`) == strconv.FormatInt(version, 10) {
			return true
		}
	}
	return false
}

func respond(w http.ResponseWriter, status int, p Policy) {
	if p.Version > 0 {
		w.Header().Set("ETag", etag(p.Version))
	}
	writeJSON(w, status, p)
}

// Get serves GET /sessions/{id}/policy.
func (h *Handlers) Get(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := caller(w, r, ScopeRead, id); !ok || !h.capable(w, r, id) {
		return
	}
	obj, err := h.policies.Get(r.Context(), id, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		writeError(w, http.StatusNotFound, msgNoPolicy)
		return
	}
	if err != nil {
		clusterError(w, err)
		return
	}
	respond(w, http.StatusOK, FromObject(obj))
}

// Put serves PUT /sessions/{id}/policy.
func (h *Handlers) Put(w http.ResponseWriter, r *http.Request, id string) {
	u, ok := caller(w, r, ScopeWrite, id)
	if !ok || !h.capable(w, r, id) {
		return
	}
	var in Input
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBody)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON with a kind and a source")
		return
	}
	if !checkInput(w, &in) {
		return
	}
	h.save(w, r, u, id, in, true)
}

// Delete serves DELETE /sessions/{id}/policy: the policy is reset, not
// removed. A session with none would be denied everything.
func (h *Handlers) Delete(w http.ResponseWriter, r *http.Request, id string) {
	u, ok := caller(w, r, ScopeWrite, id)
	if !ok || !h.capable(w, r, id) {
		return
	}
	reset := Unrestricted()
	h.save(w, r, u, id, Input{Kind: reset.Kind, Source: reset.Source}, false)
}

// save writes a policy and answers the request. With check, in is what the
// caller asked for: it goes to the operator first, and keeps the policy's
// management unless it states one. Without, it is the reset: the known-good
// unrestricted policy, in editor mode.
func (h *Handlers) save(w http.ResponseWriter, r *http.Request, u auth.User, id string, in Input, check bool) {
	ctx := r.Context()
	validated := !check
	for range saveAttempts {
		obj, err := h.policies.Get(ctx, id, metav1.GetOptions{})
		exists := !apierrors.IsNotFound(err)
		if err != nil && exists {
			clusterError(w, err)
			return
		}
		// A session whose policy was removed behind the backend gets the
		// one written now; nobody manages what is not there.
		current := Policy{Summary: Summary{Management: &Management{Mode: sessions.PolicyModeEditor}}}
		if exists {
			current = FromObject(obj)
		}
		if refusal := mayWrite(*current.Management, u, in.Management); refusal != nil {
			writeJSON(w, http.StatusConflict, refusal)
			return
		}
		if !ifMatch(r.Header.Get("If-Match"), current.Version, exists) {
			writeError(w, http.StatusPreconditionFailed, "the policy has changed since it was read; read it again")
			return
		}
		management := Management{Mode: sessions.PolicyModeEditor}
		switch {
		case in.Management != nil:
			management = *in.Management
		case check:
			management = *current.Management
		}
		if exists && current.Kind == in.Kind && current.Source == in.Source && *current.Management == management {
			respond(w, http.StatusOK, current)
			return
		}
		if !validated {
			if !h.validate(w, r, in.Kind, in.Source) {
				return
			}
			validated = true
		}

		if !exists {
			err = h.store.EnsurePolicy(ctx, id, sessions.PolicySpec{
				Kind: in.Kind, Source: in.Source, Mode: management.Mode, ManagedURL: management.ManagedURL, UpdatedBy: updatedBy(u),
			})
			if err == nil {
				obj, err = h.policies.Get(ctx, id, metav1.GetOptions{})
			}
		} else {
			spec := map[string]any{"mode": management.Mode}
			if management.Mode == sessions.PolicyModeIaC {
				spec["managedURL"] = management.ManagedURL
			}
			_ = unstructured.SetNestedField(obj.Object, in.Kind, "spec", "kind")
			_ = unstructured.SetNestedField(obj.Object, in.Source, "spec", "source")
			_ = unstructured.SetNestedMap(obj.Object, spec, "spec", "management")
			annotations := obj.GetAnnotations()
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[sessions.AnnUpdatedBy] = updatedBy(u)
			obj.SetAnnotations(annotations)
			// The update carries the resourceVersion that was read: it goes
			// through only if this is still the policy that was checked.
			obj, err = h.policies.Update(ctx, obj, metav1.UpdateOptions{})
		}
		switch {
		case apierrors.IsConflict(err), apierrors.IsNotFound(err):
			continue
		case errors.Is(err, sessions.ErrNotFound):
			writeError(w, http.StatusNotFound, "session not found")
			return
		case errors.Is(err, sessions.ErrPolicyUnsupported):
			writeError(w, http.StatusConflict, msgUnsupported)
			return
		case err != nil:
			clusterError(w, err)
			return
		}
		saved := h.await(ctx, obj)
		status := http.StatusAccepted
		if saved.State == Ready {
			status = http.StatusOK
		}
		respond(w, status, saved)
		return
	}
	writeError(w, http.StatusServiceUnavailable, "the policy kept changing while it was being saved; try again")
}

// await waits up to Wait for the operator to say what became of the policy
// just written (written is what the write returned), and returns the policy
// as it then is.
func (h *Handlers) await(ctx context.Context, written *unstructured.Unstructured) Policy {
	latest := FromObject(written)
	generation := latest.Version
	deadline := time.NewTimer(h.Wait)
	defer deadline.Stop()
	for {
		// Not Loading: it is in force, or it was refused. Another
		// generation: someone has written since, and theirs is not ours to
		// wait for.
		if latest.State != Loading || latest.Version != generation {
			return latest
		}
		select {
		case <-ctx.Done():
			return latest
		case <-deadline.C:
			return latest
		case <-time.After(h.Poll):
		}
		if obj, err := h.policies.Get(ctx, written.GetName(), metav1.GetOptions{}); err == nil {
			latest = FromObject(obj)
		}
	}
}

// PutManagement serves PUT /sessions/{id}/policy/management. Either
// credential may, in either mode: it is how a policy changes hands.
func (h *Handlers) PutManagement(w http.ResponseWriter, r *http.Request, id string) {
	u, ok := caller(w, r, ScopeWrite, id)
	if !ok || !h.capable(w, r, id) {
		return
	}
	var in Management
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON with a mode")
		return
	}
	management, ok := cleanManagement(in)
	if !ok {
		writeError(w, http.StatusBadRequest, badManagement)
		return
	}
	ctx := r.Context()
	obj, err := h.policies.Get(ctx, id, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		writeError(w, http.StatusNotFound, msgNoPolicy)
		return
	}
	if err != nil {
		clusterError(w, err)
		return
	}
	if current := FromObject(obj); *current.Management == management {
		respond(w, http.StatusOK, current)
		return
	}
	// A merge patch: only the management changes, whatever else is being
	// written at the same time. null removes the link.
	spec := map[string]any{"mode": management.Mode, "managedURL": nil}
	if management.Mode == sessions.PolicyModeIaC {
		spec["managedURL"] = management.ManagedURL
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{sessions.AnnUpdatedBy: updatedBy(u)}},
		"spec":     map[string]any{"management": spec},
	})
	if err != nil {
		clusterError(w, err)
		return
	}
	obj, err = h.policies.Patch(ctx, id, types.MergePatchType, patch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		writeError(w, http.StatusNotFound, msgNoPolicy)
		return
	}
	if err != nil {
		clusterError(w, err)
		return
	}
	respond(w, http.StatusOK, FromObject(obj))
}

// readBody reads a request body of at most limit bytes, and answers for one
// that is larger.
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "the request is too large")
		} else {
			writeError(w, http.StatusBadRequest, "the request could not be read")
		}
		return nil, false
	}
	return body, true
}

// source is a policy to check or try: PolicySource, with the input to try
// it on.
type source struct {
	Kind   string          `json:"kind"`
	Source string          `json:"source"`
	Input  json.RawMessage `json:"input,omitempty"`
}

func decodeSource(w http.ResponseWriter, body []byte) (source, bool) {
	var s source
	if err := json.Unmarshal(body, &s); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON with a kind and a source")
		return s, false
	}
	if s.Kind == "" {
		s.Kind = KindRego
	}
	if s.Kind != KindRego {
		writeError(w, http.StatusBadRequest, `kind must be "rego"`)
		return s, false
	}
	return s, true
}

// pass asks the operator and passes its answer on: the verdict as it is
// (the two APIs share its shape), its refusal of a malformed request as
// one, and anything else as the operator being unavailable.
func (h *Handlers) pass(w http.ResponseWriter, r *http.Request, method, path string, body any, contentType string) {
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			writeError(w, http.StatusBadRequest, "the request could not be encoded")
			return
		}
	}
	a, err := h.operator.do(r.Context(), method, path, encoded)
	switch {
	case err == nil && a.status == http.StatusOK && json.Valid(a.body):
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(a.body)
	case err == nil && a.status == http.StatusBadRequest:
		var refusal struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(a.body, &refusal) != nil || refusal.Error == "" {
			refusal.Error = "the policy service refused the request"
		}
		writeError(w, http.StatusBadRequest, refusal.Error)
	case err == nil && a.status == http.StatusRequestEntityTooLarge:
		writeError(w, http.StatusRequestEntityTooLarge, "the request is too large")
	default:
		if err == nil {
			err = fmt.Errorf("%s answered %d", path, a.status)
		}
		slog.Error("policy operator request failed", "err", err)
		writeError(w, http.StatusServiceUnavailable, "the policy service is unavailable")
	}
}

// Validate serves POST /policies/validate: the operator's verdict on a
// policy, saving nothing.
func (h *Handlers) Validate(w http.ResponseWriter, r *http.Request, _ auth.User) {
	if _, ok := caller(w, r, "", ""); !ok {
		return
	}
	body, ok := readBody(w, r, maxValidateBody)
	if !ok {
		return
	}
	s, ok := decodeSource(w, body)
	if !ok {
		return
	}
	h.pass(w, r, http.MethodPost, "/v1/validate", map[string]string{"kind": s.Kind, "source": s.Source}, "application/json")
}

// Evaluate serves POST /policies/evaluate: what a policy would say to one
// call, saving nothing.
func (h *Handlers) Evaluate(w http.ResponseWriter, r *http.Request, _ auth.User) {
	if _, ok := caller(w, r, "", ""); !ok {
		return
	}
	body, ok := readBody(w, r, maxEvaluateBody)
	if !ok {
		return
	}
	s, ok := decodeSource(w, body)
	if !ok {
		return
	}
	if len(s.Input) == 0 {
		writeError(w, http.StatusBadRequest, "an input is required: the call to try the policy on")
		return
	}
	h.pass(w, r, http.MethodPost, "/v1/evaluate", s, "application/json")
}

// Presets serves GET /policy-presets.
func (h *Handlers) PresetList(w http.ResponseWriter, r *http.Request, _ auth.User) {
	if _, ok := caller(w, r, "", ""); !ok {
		return
	}
	writeJSON(w, http.StatusOK, Presets())
}
