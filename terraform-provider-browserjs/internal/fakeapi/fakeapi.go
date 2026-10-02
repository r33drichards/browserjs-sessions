// Package fakeapi is an in-memory stand-in for the backend's API host, written
// from docs/contracts/policy/backend-api.yaml. It is what the provider is
// tested against, and it checks policies only roughly: enough to answer
// with errors that have a row and a column, and with the warnings about
// tools that undo each other's rules. Policies are Rego only.
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

// Unrestricted is the policy a new session has, and a reset one returns to:
// docs/contracts/policy/examples/unrestricted.rego, byte for byte.
const Unrestricted = `# No restrictions: every operation in the browser, full control of the
# desktop, and any shell command.
package browserjs.policy

import rego.v1

# The platform asks a policy only about the tools it knows: browser_execute
# and desktop_execute on server "browser"; exec, stream_logs and search_logs
# on server "exec". This allows all of them, with any arguments.
allow_tool_call := true
`

// kindRego is the only kind of policy; a request may leave it out.
const kindRego = "rego"

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
	// ReportKind, when set, is the kind a read of a policy reports instead of
	// rego: an API that has a kind the provider does not know.
	ReportKind string

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
	source, rego, hash string
	version            int64
	mode, managedURL   string
	loadingLeft        int
	invalid            []client.Diagnostic
	updated, updatedBy string
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
func (s *Server) UIEdit(id, source string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := Validate(source)
	if !v.OK {
		return errors.New(v.Errors[0].Message)
	}
	p := &s.sessions[id].policy
	p.mode, p.managedURL = client.ModeEditor, ""
	p.set(source, v, "ui")
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
	se.policy.set(Unrestricted, Validate(Unrestricted), "ui")
	s.sessions[id] = se
	s.order = append(s.order, id)
	return se
}

func (p *policy) set(source string, v client.Validation, by string) {
	p.source, p.rego, p.hash = source, v.Rego, v.Hash
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
		Kind: kindRego, Version: p.version, Hash: p.hash, State: p.state(),
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

// badKind answers 400 for a kind that is not rego, as the API does.
func badKind(w http.ResponseWriter, kind string) bool {
	if kind == "" || kind == kindRego {
		return false
	}
	writeError(w, http.StatusBadRequest, "kind must be rego")
	return true
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
		if badKind(w, body.Policy.Kind) {
			return
		}
		if v = Validate(body.Policy.Source); !v.OK {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "the policy does not validate", "errors": v.Errors, "warnings": v.Warnings})
			return
		}
	}
	se := s.add(body.Name)
	se.startingLeft = s.SessionStartingReads
	if body.Policy != nil {
		se.policy.set(body.Policy.Source, v, "token:fake")
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
	if s.ReportKind != "" {
		v.Kind = s.ReportKind
	}
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
	if badKind(w, in.Kind) {
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
	v := Validate(in.Source)
	if !v.OK {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "the policy does not validate", "errors": v.Errors, "warnings": v.Warnings})
		return
	}
	mode, managedURL := p.mode, p.managedURL
	if in.Management != nil {
		mode, managedURL = in.Management.Mode, in.Management.ManagedURL
	}
	if in.Source == p.source && mode == p.mode && managedURL == p.managedURL {
		out := p.view()
		out.Warnings = v.Warnings
		writeJSON(w, http.StatusOK, out)
		return
	}
	p.mode, p.managedURL = mode, managedURL
	p.set(in.Source, v, "token:fake")
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
	p.set(Unrestricted, Validate(Unrestricted), "token:fake")
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
	if badKind(w, in.Kind) {
		return
	}
	if s.ValidateUnavailable {
		writeError(w, http.StatusServiceUnavailable, "the policy operator could not be reached")
		return
	}
	writeJSON(w, http.StatusOK, Validate(in.Source))
}

// maxSource is the most bytes a policy may have.
const maxSource = 65536

// guard is the code of the checks a tenant's module must pass.
const guard = "policy_guard_error"

var (
	packageLine = regexp.MustCompile(`(?m)^package[ \t]+(\S+)[ \t]*$`)
	// A rule that defines allow_tool_call, at the start of a line.
	allowRule = regexp.MustCompile(`(?m)^allow_tool_call\b`)
	// What a module that looks inside a call's arguments mentions: the
	// operations of browser_execute, the command of exec.
	readsOperations = regexp.MustCompile(`\boperations\b`)
	readsCommand    = regexp.MustCompile(`\bcmd\b`)
	namesDesktop    = regexp.MustCompile(`"desktop_execute"`)
	namesShell      = regexp.MustCompile(`"exec"`)
)

// The warnings of docs/contracts/policy/rego-contract.md, about the policy
// as a whole: they have no row and no column.
var bypass = map[string]string{
	"browser_bypass_desktop": "the policy refuses some browser_execute calls but allows desktop_execute to click or type: " +
		"with the mouse and keyboard an agent can use the address bar and DevTools, so the rules on " +
		"browser_execute can be walked around. Deny desktop_execute, or allow only its screen operations",
	"browser_bypass_shell": "the policy refuses some browser_execute calls but allows exec to run arbitrary commands: a command " +
		"can reach the browser's own control ports on 127.0.0.1 (8081, 9222), so the rules on browser_execute " +
		"can be walked around. Deny exec, or allow only whole commands that cannot make requests or start programs",
	"shell_bypass_desktop": "the policy refuses some exec commands but allows desktop_execute to click or type: an agent can open " +
		"a terminal on the desktop and type any command. Deny desktop_execute, or allow only its screen operations",
}

// blank is source with its comments, and when literals is set the insides
// of its strings too, replaced by spaces. Offsets and line breaks are kept,
// so a position in the result is a position in source.
func blank(source string, literals bool) string {
	out := []byte(source)
	var in byte // 0 in code, else '#', '"' or '`'
	for i := 0; i < len(out); i++ {
		c := out[i]
		switch {
		case in == 0:
			switch c {
			case '#':
				in, out[i] = c, ' '
			case '"', '`':
				in = c
			}
		case c == '\n':
			if in != '`' {
				in = 0
			}
		case in == '#':
			out[i] = ' '
		case c == in:
			in = 0
		default:
			escape := in == '"' && c == '\\' && i+1 < len(out) && out[i+1] != '\n'
			if literals {
				out[i] = ' '
				if escape {
					out[i+1] = ' '
				}
			}
			if escape {
				i++
			}
		}
	}
	return string(out)
}

// Validate is the fake's rough check of a Rego policy. The real one is the
// policy operator's, which parses, compiles and evaluates the module; this
// one reads the text. It knows the size limit, the package line, brace
// balance outside comments and strings, and that allow_tool_call must be
// defined. Its warnings are a guess from what the module mentions: one that
// looks at the operations of browser_execute restricts the browser, one that
// looks at the command of exec restricts the shell, and naming
// "desktop_execute" or "exec" outside a comment allows it.
func Validate(source string) client.Validation {
	v := client.Validation{Errors: []client.Diagnostic{}, Warnings: []client.Diagnostic{}}
	fail := func(row, col int, code, msg string) client.Validation {
		v.Errors = append(v.Errors, client.Diagnostic{Row: row, Col: col, Code: code, Message: msg})
		return v
	}
	if source == "" {
		return fail(0, 0, "size_error", "the policy is empty")
	}
	if len(source) > maxSource {
		return fail(0, 0, "size_error", fmt.Sprintf("the policy is larger than %d bytes", maxSource))
	}
	code := blank(source, false)
	bare := blank(source, true)

	m := packageLine.FindStringSubmatchIndex(bare)
	if m == nil {
		return fail(1, 1, "rego_parse_error", "package expected")
	}
	if name := source[m[2]:m[3]]; name != "browserjs.policy" {
		row, col := position(source, m[2])
		return fail(row, col, guard, fmt.Sprintf("the package must be browserjs.policy, not %s", name))
	}
	depth := 0
	for i, c := range bare {
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
	if !allowRule.MatchString(bare) {
		return fail(0, 0, guard, "the policy must define allow_tool_call")
	}

	warn := func(code string) {
		v.Warnings = append(v.Warnings, client.Diagnostic{Code: code, Message: bypass[code]})
	}
	browser, shell := readsOperations.MatchString(bare), readsCommand.MatchString(bare)
	desktop := namesDesktop.MatchString(code)
	if browser && desktop {
		warn("browser_bypass_desktop")
	}
	if browser && namesShell.MatchString(code) && !shell {
		warn("browser_bypass_shell")
	}
	if shell && desktop {
		warn("shell_bypass_desktop")
	}

	v.Rego = source
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
