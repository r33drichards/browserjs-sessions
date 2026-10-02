// Package fakeapi is an in-memory stand-in for the backend's API host, written
// from docs/contracts/policy/backend-api.yaml. It is what the provider is
// tested against until the real API exists, and it validates policies only
// roughly: enough to answer with errors that have a row and a column.
package fakeapi

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/r33drichards/browserjs-sessions/terraform-provider-browserjs/internal/client"
)

// Unrestricted is the policy a new session has, and a reset one returns to.
const Unrestricted = `{
  "version": 1,
  "description": "No restrictions: every browser operation, with any parameters.",
  "allow": { "operations": ["*"] }
}
`

// Request is one request the fake received.
type Request struct {
	Method, Path, UserAgent, Body string
	Authorized                    bool
}

// Server is the fake. Its exported fields are settings; change them through
// Configure once it is serving.
type Server struct {
	mu sync.Mutex

	// Token is the one API token accepted, with every scope.
	Token string
	// Owner is who the token belongs to.
	Owner string
	// BaseMCPURL is what session MCP URLs start with.
	BaseMCPURL string
	// Limit is how many sessions the owner may have.
	Limit int
	// SessionStartingReads is how many reads of a new session report it
	// `starting`, its policy `loading`.
	SessionStartingReads int
	// PolicyLoadingReads, when positive, makes a policy write answer 202 and
	// that many reads of the policy report `loading`.
	PolicyLoadingReads int
	// CompileErrors, when set, makes a policy written with 202 turn `invalid`
	// with these errors instead of `ready`.
	CompileErrors []client.Diagnostic
	// ValidateUnavailable makes validation answer 503.
	ValidateUnavailable bool

	sessions map[string]*session
	order    []string
	requests []Request
	named    int
}

type session struct {
	id, name     string
	legacy       bool // predates policies
	startingLeft int
	policy       policy
}

type policy struct {
	kind, source, rego, hash string
	version                  int64
	mode, managedURL         string
	loadingLeft              int
	invalid                  []client.Diagnostic
	updated, updatedBy       string
}

// New makes a fake that accepts token.
func New(token string) *Server {
	return &Server{
		Token:      token,
		Owner:      "dev@example.com",
		BaseMCPURL: "https://sessions.example.test",
		Limit:      20,
		sessions:   map[string]*session{},
	}
}

// Configure changes settings while the fake is serving.
func (s *Server) Configure(f func(*Server)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

// Requests is every request so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Count is how many requests had this method and path.
func (s *Server) Count(method, path string) int {
	n := 0
	for _, r := range s.Requests() {
		if r.Method == method && r.Path == path {
			n++
		}
	}
	return n
}

// AddLegacySession adds a session that predates policies and returns its ID.
func (s *Server) AddLegacySession(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	se := s.add(name)
	se.legacy = true
	return se.id
}

// AddSession adds a running session with the unrestricted policy, as if made
// in the UI, and returns its ID.
func (s *Server) AddSession(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.add(name).id
}

// UIManageHere is "Manage here instead" in the UI: the mode becomes editor.
func (s *Server) UIManageHere(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &s.sessions[id].policy
	p.mode, p.managedURL = client.ModeEditor, ""
	p.version++
}

// UIEdit is an edit made in the UI's editor: the mode becomes editor and the
// source changes.
func (s *Server) UIEdit(id, kind, source string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := Validate(kind, source)
	if !v.OK {
		return errors.New(v.Errors[0].Message)
	}
	p := &s.sessions[id].policy
	p.mode, p.managedURL = client.ModeEditor, ""
	p.set(kind, source, v, "ui")
	return nil
}

// PolicyOf is the policy of a session as the API would show it, without
// counting as a read.
func (s *Server) PolicyOf(id string) client.Policy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id].policy.view()
}

// IDByName is the ID of the first session with this name, or "".
func (s *Server) IDByName(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.order {
		if se, ok := s.sessions[id]; ok && se.name == name {
			return id
		}
	}
	return ""
}

// Has reports whether the session exists.
func (s *Server) Has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.sessions[id]
	return ok
}

func (s *Server) add(name string) *session {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	id := "s-" + strings.ToLower(base32.StdEncoding.EncodeToString(b))[:10]
	if strings.TrimSpace(name) == "" {
		s.named++
		name = fmt.Sprintf("session-%d", s.named)
	}
	se := &session{id: id, name: name}
	se.policy.mode = client.ModeEditor
	se.policy.set("json", Unrestricted, Validate("json", Unrestricted), "ui")
	s.sessions[id] = se
	s.order = append(s.order, id)
	return se
}

func (p *policy) set(kind, source string, v client.Validation, by string) {
	p.kind, p.source, p.rego, p.hash = kind, source, v.Rego, v.Hash
	p.version++
	p.invalid = nil
	p.updated = time.Now().UTC().Format(time.RFC3339)
	p.updatedBy = by
}

func (p *policy) state() string {
	switch {
	case p.loadingLeft > 0:
		return client.StateLoading
	case p.invalid != nil:
		return client.StateInvalid
	}
	return client.StateReady
}

func (p *policy) summary() *client.PolicySummary {
	return &client.PolicySummary{
		Kind: p.kind, Version: p.version, Hash: p.hash, State: p.state(),
		Management: &client.Management{Mode: p.mode, ManagedURL: p.managedURL},
	}
}

func (p *policy) view() client.Policy {
	v := client.Policy{PolicySummary: *p.summary(), Source: p.source, Rego: p.rego, Updated: p.updated, UpdatedBy: p.updatedBy}
	v.Loaded = &client.Loaded{Replicas: 2, Total: 2}
	switch v.State {
	case client.StateLoading:
		v.Loaded.Replicas = 0
	case client.StateInvalid:
		v.Errors = p.invalid
	}
	return v
}

func (s *Server) sessionView(se *session) client.Session {
	v := client.Session{ID: se.id, Name: se.name, Owner: s.Owner, State: "running", MCPURL: s.BaseMCPURL + "/" + se.id + "/mcp"}
	if se.legacy {
		v.Policy = &client.PolicySummary{State: client.StateUnsupported}
		return v
	}
	v.Policy = se.policy.summary()
	if se.startingLeft > 0 {
		v.State = "starting"
		v.Policy.State = client.StateLoading
	}
	return v
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// Handler serves the API under /v1, as the API host does.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sessions", s.createSession)
	mux.HandleFunc("GET /v1/sessions", s.listSessions)
	mux.HandleFunc("GET /v1/sessions/{id}", s.session(s.getSession))
	mux.HandleFunc("PATCH /v1/sessions/{id}", s.session(s.patchSession))
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.deleteSession)
	mux.HandleFunc("GET /v1/sessions/{id}/policy", s.session(s.getPolicy))
	mux.HandleFunc("PUT /v1/sessions/{id}/policy", s.session(s.putPolicy))
	mux.HandleFunc("DELETE /v1/sessions/{id}/policy", s.session(s.resetPolicy))
	mux.HandleFunc("PUT /v1/sessions/{id}/policy/management", s.session(s.putManagement))
	mux.HandleFunc("POST /v1/policies/validate", s.validate)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body bytes.Buffer
		_, _ = body.ReadFrom(http.MaxBytesReader(w, r.Body, 1<<20))
		r.Body = http.NoBody
		ok := r.Header.Get("Authorization") == "Bearer "+s.Token
		s.mu.Lock()
		defer s.mu.Unlock()
		s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, UserAgent: r.UserAgent(), Body: body.String(), Authorized: ok})
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		r = r.WithContext(withBody(r.Context(), body.Bytes()))
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) session(h func(http.ResponseWriter, *http.Request, *session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		se, ok := s.sessions[r.PathValue("id")]
		if !ok {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		h(w, r, se)
	}
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string              `json:"name"`
		Policy *client.PolicyInput `json:"policy"`
	}
	if b := bodyOf(r); len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &body); err != nil {
			writeError(w, http.StatusBadRequest, "body must be JSON, optionally with a name")
			return
		}
	}
	if len(s.sessions) >= s.Limit {
		writeError(w, http.StatusConflict, "session limit reached; delete one first")
		return
	}
	var v client.Validation
	if body.Policy != nil {
		if v = Validate(body.Policy.Kind, body.Policy.Source); !v.OK {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "the policy does not validate", "errors": v.Errors, "warnings": v.Warnings})
			return
		}
	}
	se := s.add(body.Name)
	se.startingLeft = s.SessionStartingReads
	if body.Policy != nil {
		se.policy.set(body.Policy.Kind, body.Policy.Source, v, "token:fake")
		se.policy.version = 1
		if m := body.Policy.Management; m != nil {
			se.policy.mode, se.policy.managedURL = m.Mode, m.ManagedURL
		}
	}
	writeJSON(w, http.StatusCreated, s.sessionView(se))
}

func (s *Server) listSessions(w http.ResponseWriter, _ *http.Request) {
	out := []client.Session{}
	for _, id := range s.order {
		if se, ok := s.sessions[id]; ok {
			out = append(out, s.sessionView(se))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getSession(w http.ResponseWriter, _ *http.Request, se *session) {
	v := s.sessionView(se)
	if se.startingLeft > 0 {
		se.startingLeft--
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) patchSession(w http.ResponseWriter, r *http.Request, se *session) {
	var body struct {
		Name *string `json:"name"`
	}
	if err := json.Unmarshal(bodyOf(r), &body); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON")
		return
	}
	if body.Name != nil {
		if strings.TrimSpace(*body.Name) == "" {
			writeError(w, http.StatusBadRequest, "name must not be empty")
			return
		}
		se.name = *body.Name
	}
	writeJSON(w, http.StatusOK, s.sessionView(se))
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	delete(s.sessions, r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

const predates = "this session predates policies; create a new session to give it one"

func (s *Server) getPolicy(w http.ResponseWriter, _ *http.Request, se *session) {
	if se.legacy {
		writeError(w, http.StatusConflict, predates)
		return
	}
	v := se.policy.view()
	if se.policy.loadingLeft > 0 {
		se.policy.loadingLeft--
	}
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(v.Version, 10)))
	writeJSON(w, http.StatusOK, v)
}

// refuse answers 409 when a token may not write in the policy's mode.
func refuse(w http.ResponseWriter, se *session, takesOver bool) bool {
	if se.legacy {
		writeError(w, http.StatusConflict, predates)
		return true
	}
	if se.policy.mode == client.ModeEditor && !takesOver {
		writeError(w, http.StatusConflict, "this policy is managed in the editor")
		return true
	}
	return false
}

func validManagement(m *client.Management) string {
	switch m.Mode {
	case client.ModeEditor:
		return ""
	case client.ModeIaC:
		if !strings.HasPrefix(m.ManagedURL, "https://") {
			return "managed_url must be an https URL when mode is iac"
		}
		return ""
	}
	return "mode must be editor or iac"
}

func (s *Server) putPolicy(w http.ResponseWriter, r *http.Request, se *session) {
	var in client.PolicyInput
	if err := json.Unmarshal(bodyOf(r), &in); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON")
		return
	}
	if refuse(w, se, in.Management != nil && in.Management.Mode == client.ModeIaC) {
		return
	}
	if in.Management != nil {
		if msg := validManagement(in.Management); msg != "" {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
	}
	p := &se.policy
	if m := r.Header.Get("If-Match"); m != "" && m != strconv.Quote(strconv.FormatInt(p.version, 10)) {
		writeError(w, http.StatusPreconditionFailed, "the policy has changed since it was read")
		return
	}
	if s.ValidateUnavailable {
		writeError(w, http.StatusServiceUnavailable, "the policy operator could not be reached")
		return
	}
	v := Validate(in.Kind, in.Source)
	if !v.OK {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "the policy does not validate", "errors": v.Errors, "warnings": v.Warnings})
		return
	}
	mode, managedURL := p.mode, p.managedURL
	if in.Management != nil {
		mode, managedURL = in.Management.Mode, in.Management.ManagedURL
	}
	if in.Kind == p.kind && in.Source == p.source && mode == p.mode && managedURL == p.managedURL {
		out := p.view()
		out.Warnings = v.Warnings
		writeJSON(w, http.StatusOK, out)
		return
	}
	p.mode, p.managedURL = mode, managedURL
	p.set(in.Kind, in.Source, v, "token:fake")
	status := http.StatusOK
	if s.PolicyLoadingReads > 0 {
		p.loadingLeft = s.PolicyLoadingReads
		p.invalid = s.CompileErrors
		status = http.StatusAccepted
	}
	out := p.view()
	out.Warnings = v.Warnings
	writeJSON(w, status, out)
}

func (s *Server) resetPolicy(w http.ResponseWriter, _ *http.Request, se *session) {
	if refuse(w, se, false) {
		return
	}
	p := &se.policy
	p.mode, p.managedURL = client.ModeEditor, ""
	p.set("json", Unrestricted, Validate("json", Unrestricted), "token:fake")
	writeJSON(w, http.StatusOK, p.view())
}

func (s *Server) putManagement(w http.ResponseWriter, r *http.Request, se *session) {
	var m client.Management
	if err := json.Unmarshal(bodyOf(r), &m); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON")
		return
	}
	if se.legacy {
		writeError(w, http.StatusConflict, predates)
		return
	}
	if msg := validManagement(&m); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if m.Mode == client.ModeEditor {
		m.ManagedURL = ""
	}
	if se.policy.mode != m.Mode || se.policy.managedURL != m.ManagedURL {
		se.policy.mode, se.policy.managedURL = m.Mode, m.ManagedURL
		se.policy.version++
	}
	writeJSON(w, http.StatusOK, se.policy.view())
}

func (s *Server) validate(w http.ResponseWriter, r *http.Request) {
	var in client.PolicyInput
	if err := json.Unmarshal(bodyOf(r), &in); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON")
		return
	}
	if s.ValidateUnavailable {
		writeError(w, http.StatusServiceUnavailable, "the policy operator could not be reached")
		return
	}
	writeJSON(w, http.StatusOK, Validate(in.Kind, in.Source))
}

var operations = map[string]bool{
	"click": true, "evaluate": true, "navigate": true, "press": true, "screenshot": true, "select": true,
	"setContent": true, "setViewport": true, "type": true, "url": true, "wait": true,
}

var packageLine = regexp.MustCompile(`(?m)^package[ \t]+(\S+)[ \t]*$`)

// Validate is the fake's rough check of a policy. The real one is the policy
// operator's; this one knows JSON syntax, the top-level keys, the operation
// names, and a Rego module's package line and brace balance.
func Validate(kind, source string) client.Validation {
	v := client.Validation{Errors: []client.Diagnostic{}, Warnings: []client.Diagnostic{}}
	fail := func(row, col int, code, msg string) client.Validation {
		v.Errors = append(v.Errors, client.Diagnostic{Row: row, Col: col, Code: code, Message: msg})
		return v
	}
	switch kind {
	case "json":
		var doc struct {
			Version     json.RawMessage `json:"version"`
			Description string          `json:"description"`
			Allow       *struct {
				Operations []string          `json:"operations"`
				Rules      []json.RawMessage `json:"rules"`
			} `json:"allow"`
			Deny *struct {
				Operations []string `json:"operations"`
			} `json:"deny"`
		}
		dec := json.NewDecoder(strings.NewReader(source))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&doc); err != nil {
			var syn *json.SyntaxError
			var typ *json.UnmarshalTypeError
			switch {
			case errors.As(err, &syn):
				// Offset counts the bytes read, the offending one included.
				row, col := position(source, int(syn.Offset)-1)
				return fail(row, col, "json_syntax", syn.Error())
			case errors.As(err, &typ):
				row, col := position(source, int(typ.Offset))
				return fail(row, col, "schema", fmt.Sprintf("%s must be %s", typ.Field, typ.Type))
			}
			if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
				row, col := find(source, name)
				return fail(row, col, "schema", "unknown property "+name)
			}
			return fail(1, 1, "json_syntax", err.Error())
		}
		if string(doc.Version) != "1" {
			row, col := find(source, `"version"`)
			return fail(row, col, "schema", "version must be 1")
		}
		var allowed, denied []string
		if doc.Allow != nil {
			allowed = doc.Allow.Operations
		}
		if doc.Deny != nil {
			denied = doc.Deny.Operations
		}
		for _, op := range allowed {
			if op != "*" && !operations[op] {
				row, col := find(source, strconv.Quote(op))
				fail(row, col, "schema", fmt.Sprintf("allow.operations: %q is not an operation", op))
			}
		}
		for _, op := range denied {
			if !operations[op] {
				row, col := find(source, strconv.Quote(op))
				fail(row, col, "schema", fmt.Sprintf("deny.operations: %q is not an operation", op))
			}
		}
		if len(v.Errors) > 0 {
			return v
		}
		for _, op := range allowed {
			if op == "*" && len(denied) == 0 {
				row, col := find(source, `"*"`)
				v.Warnings = append(v.Warnings, client.Diagnostic{Row: row, Col: col, Code: "unrestricted", Message: "this policy allows every operation"})
			}
		}
		var compact bytes.Buffer
		_ = json.Compact(&compact, []byte(source))
		v.Rego = "# Generated from a browserjs JSON policy (version 1). Edit the JSON, not this file.\npackage browserjs.policy\n\n# fake translation of " + compact.String() + "\n"
	case "rego":
		m := packageLine.FindStringSubmatchIndex(source)
		if m == nil {
			return fail(1, 1, "rego_parse_error", "package expected")
		}
		if name := source[m[2]:m[3]]; name != "browserjs.policy" {
			row, col := position(source, m[2])
			return fail(row, col, "package", fmt.Sprintf("the package must be browserjs.policy, not %s", name))
		}
		depth := 0
		for i, c := range source {
			switch c {
			case '{':
				depth++
			case '}':
				depth--
			}
			if depth < 0 {
				row, col := position(source, i)
				return fail(row, col, "rego_parse_error", "unexpected }")
			}
		}
		if depth != 0 {
			row, col := position(source, len(source))
			return fail(row, col, "rego_parse_error", "unexpected end of file: } expected")
		}
		v.Rego = source
	default:
		return fail(0, 0, "kind", "kind must be json or rego")
	}
	sum := sha256.Sum256([]byte(v.Rego))
	v.Hash = hex.EncodeToString(sum[:])
	v.OK = true
	return v
}

// position is the 1-based row and column of a byte offset.
func position(source string, offset int) (int, int) {
	offset = max(0, min(offset, len(source)))
	before := source[:offset]
	row := strings.Count(before, "\n") + 1
	col := offset - strings.LastIndex(before, "\n")
	return row, col
}

func find(source, needle string) (int, int) {
	i := strings.Index(source, needle)
	if i < 0 {
		return 1, 1
	}
	return position(source, i)
}
