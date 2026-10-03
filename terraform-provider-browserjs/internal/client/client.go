// Package client is the provider's view of the API of
// docs/contracts/policy/backend-api.yaml: the types the resources work with,
// and the calls they make.
//
// The calls are made by the Computer Use SDK (sdk/go, the Rust client behind
// UniFFI bindings), not by HTTP code of the provider's own. This package
// turns the SDK's records and errors into the provider's, and gives each
// blocking SDK call the context of the Terraform operation it is part of.
package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/r33drichards/computer-use/sdk/go/computeruse"
)

// Policy states, as PolicyState in the API.
const (
	StateReady       = "ready"
	StateLoading     = "loading"
	StateInvalid     = "invalid"
	StateUnsupported = "unsupported"
)

// KindRego is the only kind of policy.
const KindRego = "rego"

// Management modes.
const (
	ModeEditor = "editor"
	ModeIaC    = "iac"
)

// Session is a session as the API shows it.
type Session struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Owner  string         `json:"owner"`
	State  string         `json:"state"`
	MCPURL string         `json:"mcp_url"`
	Policy *PolicySummary `json:"policy,omitempty"`
	// Size is the size the session runs at. PendingSize is the size asked
	// for while it was awake, which it takes at its next start; empty when
	// no resize is waiting.
	Size        string `json:"size,omitempty"`
	PendingSize string `json:"pendingSize,omitempty"`
}

// Management says who manages a policy.
type Management struct {
	Mode       string `json:"mode"`
	ManagedURL string `json:"managed_url,omitempty"`
}

// PolicySummary is what a session carries about its policy.
type PolicySummary struct {
	Kind       string      `json:"kind,omitempty"`
	Version    int64       `json:"version,omitempty"`
	Hash       string      `json:"hash,omitempty"`
	State      string      `json:"state"`
	Management *Management `json:"management,omitempty"`
}

// Diagnostic is one error or warning about a policy's source. Row and Col
// are 1-based, and zero when the API gave none: the warnings about a tool
// that undoes another's rules are about the policy as a whole.
type Diagnostic struct {
	Row     int    `json:"row,omitempty"`
	Col     int    `json:"col,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Loaded says how many OPA replicas have the policy.
type Loaded struct {
	Replicas int `json:"replicas"`
	Total    int `json:"total"`
}

// Policy is a session's policy, with its source.
type Policy struct {
	PolicySummary
	Source    string       `json:"source,omitempty"`
	Rego      string       `json:"rego,omitempty"`
	Errors    []Diagnostic `json:"errors,omitempty"`
	Warnings  []Diagnostic `json:"warnings,omitempty"`
	Loaded    *Loaded      `json:"loaded,omitempty"`
	Updated   string       `json:"updated,omitempty"`
	UpdatedBy string       `json:"updated_by,omitempty"`
}

// PolicyInput is the body of a policy write.
type PolicyInput struct {
	Kind       string      `json:"kind"`
	Source     string      `json:"source"`
	Management *Management `json:"management,omitempty"`
}

// Validation is the verdict of POST /policies/validate.
type Validation struct {
	OK       bool         `json:"ok"`
	Rego     string       `json:"rego,omitempty"`
	Hash     string       `json:"hash,omitempty"`
	Errors   []Diagnostic `json:"errors"`
	Warnings []Diagnostic `json:"warnings"`
}

// APIError is any answer that is not a success.
type APIError struct {
	Status     int
	Message    string
	Errors     []Diagnostic
	Warnings   []Diagnostic
	ManagedURL string
	// Code and BillingURL are set when billing refused the request (a 402
	// for want of a payment method or of credit, a plan's limit): what was
	// refused, and where its owner puts it right.
	Code       string
	BillingURL string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("the API answered %d %s", e.Status, http.StatusText(e.Status))
	}
	if e.BillingURL != "" {
		// What a person running terraform needs: the sentence, and the link.
		return fmt.Sprintf("the API answered %d: %s See %s", e.Status, e.Message, e.BillingURL)
	}
	return fmt.Sprintf("the API answered %d: %s", e.Status, e.Message)
}

// StatusOf is the HTTP status of err when it is an APIError, and 0 otherwise.
func StatusOf(err error) int {
	var e *APIError
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// IsNotFound reports a 404: no such session, or not the caller's.
func IsNotFound(err error) bool { return StatusOf(err) == http.StatusNotFound }

// Client is an API client. The token is sent and never printed.
type Client struct {
	sdk *computeruse.Client
}

// New makes a client for the API host at endpoint (without /v1).
func New(endpoint, token, version string) (*Client, error) {
	if token == "" {
		return nil, errors.New("token is empty")
	}
	userAgent := "terraform-provider-browserjs/" + version
	// The token is sent as it is, as the provider always has: one request
	// per call, and nothing to refresh during a long apply. An http endpoint
	// is the provider's to warn about (provider.go), not the SDK's to refuse.
	no, yes := false, true
	sdk, err := computeruse.NewClient(computeruse.ClientOptions{
		ApiToken:          token,
		BaseUrl:           &endpoint,
		ExchangeToken:     &no,
		AllowInsecureHttp: &yes,
		UserAgent:         &userAgent,
	})
	if err != nil {
		var bad *computeruse.ComputerUseErrorConfiguration
		if errors.As(err, &bad) {
			return nil, fmt.Errorf("endpoint must be an http(s) URL such as https://api.computeruse.site: %s", bad.Reason)
		}
		return nil, convert(err)
	}
	return &Client{sdk: sdk}, nil
}

// call runs one blocking SDK call and gives up waiting for it when ctx ends
// (a Terraform timeout, an interrupt). The SDK's own time limit then ends the
// call itself.
func call[T any](ctx context.Context, f func() (T, error)) (T, error) {
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1)
	go func() {
		value, err := f()
		done <- result{value, err}
	}()
	select {
	case r := <-done:
		return r.value, convert(r.err)
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// convert turns an SDK error into the provider's: an *APIError for an answer
// of the API, and the SDK's sentence for anything else. No SDK error carries
// the token.
func convert(err error) error {
	if err == nil {
		return nil
	}
	str := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	var (
		unauthorized *computeruse.ComputerUseErrorUnauthorized
		payment      *computeruse.ComputerUseErrorPaymentRequired
		forbidden    *computeruse.ComputerUseErrorForbidden
		notFound     *computeruse.ComputerUseErrorNotFound
		conflict     *computeruse.ComputerUseErrorConflict
		invalid      *computeruse.ComputerUseErrorInvalidPolicy
		limited      *computeruse.ComputerUseErrorRateLimited
		other        *computeruse.ComputerUseErrorApi
		transport    *computeruse.ComputerUseErrorTransport
		timeout      *computeruse.ComputerUseErrorTimeout
		decode       *computeruse.ComputerUseErrorDecode
		config       *computeruse.ComputerUseErrorConfiguration
	)
	switch {
	case errors.As(err, &unauthorized):
		return &APIError{Status: http.StatusUnauthorized, Message: unauthorized.Message}
	case errors.As(err, &payment):
		return &APIError{Status: http.StatusPaymentRequired, Message: payment.Message, Code: str(payment.Code), BillingURL: str(payment.BillingUrl)}
	case errors.As(err, &forbidden):
		return &APIError{Status: http.StatusForbidden, Message: forbidden.Message, Code: str(forbidden.Code), BillingURL: str(forbidden.BillingUrl)}
	case errors.As(err, &notFound):
		return &APIError{Status: http.StatusNotFound, Message: notFound.Message}
	case errors.As(err, &conflict):
		return &APIError{Status: http.StatusConflict, Message: conflict.Message, Code: str(conflict.Code),
			ManagedURL: str(conflict.ManagedUrl), BillingURL: str(conflict.BillingUrl)}
	case errors.As(err, &invalid):
		return &APIError{Status: http.StatusUnprocessableEntity, Message: invalid.Message,
			Errors: diagnostics(invalid.Errors), Warnings: diagnostics(invalid.Warnings)}
	case errors.As(err, &limited):
		return &APIError{Status: http.StatusTooManyRequests, Message: limited.Message}
	case errors.As(err, &other):
		return &APIError{Status: int(other.Status), Message: other.Message, Code: str(other.Code)}
	case errors.As(err, &transport):
		return errors.New(transport.Reason)
	case errors.As(err, &timeout):
		return fmt.Errorf("%s: no answer in time", timeout.Operation)
	case errors.As(err, &decode):
		return fmt.Errorf("the answer is not the JSON expected: %s", decode.Reason)
	case errors.As(err, &config):
		return errors.New(config.Reason)
	}
	return err
}

func diagnostics(in []computeruse.Diagnostic) []Diagnostic {
	if len(in) == 0 {
		return nil
	}
	out := make([]Diagnostic, len(in))
	for i, d := range in {
		out[i] = Diagnostic{Code: d.Code, Message: d.Message}
		if d.Row != nil {
			out[i].Row = int(*d.Row)
		}
		if d.Col != nil {
			out[i].Col = int(*d.Col)
		}
	}
	return out
}

var sessionStates = map[computeruse.SessionState]string{
	computeruse.SessionStateStarting: "starting",
	computeruse.SessionStateRunning:  "running",
	computeruse.SessionStateStopping: "stopping",
	computeruse.SessionStateAsleep:   "asleep",
	computeruse.SessionStateStopped:  "stopped",
	computeruse.SessionStateFailed:   "failed",
}

var policyStates = map[computeruse.PolicyState]string{
	computeruse.PolicyStateReady:       StateReady,
	computeruse.PolicyStateLoading:     StateLoading,
	computeruse.PolicyStateInvalid:     StateInvalid,
	computeruse.PolicyStateUnsupported: StateUnsupported,
}

// named is the API's word for an SDK enum value; a value this SDK does not
// know is "unknown".
func named[K comparable](names map[K]string, value K) string {
	if name, ok := names[value]; ok {
		return name
	}
	return "unknown"
}

func management(in *computeruse.Management) *Management {
	if in == nil {
		return nil
	}
	out := &Management{Mode: "unknown"}
	switch in.Mode {
	case computeruse.ManagementModeEditor:
		out.Mode = ModeEditor
	case computeruse.ManagementModeIac:
		out.Mode = ModeIaC
	}
	if in.ManagedUrl != nil {
		out.ManagedURL = *in.ManagedUrl
	}
	return out
}

func deref[T any](p *T) (zero T) {
	if p == nil {
		return zero
	}
	return *p
}

func session(in computeruse.SessionInfo) *Session {
	out := &Session{ID: in.Id, Name: in.Name, Owner: in.Owner, State: named(sessionStates, in.State), MCPURL: deref(in.McpUrl),
		Size: deref(in.Size), PendingSize: deref(in.PendingSize)}
	if p := in.Policy; p != nil {
		out.Policy = &PolicySummary{Kind: deref(p.Kind), Version: deref(p.Version), Hash: deref(p.Hash),
			State: named(policyStates, p.State), Management: management(p.Management)}
	}
	return out
}

func policy(in computeruse.Policy) *Policy {
	out := &Policy{
		PolicySummary: PolicySummary{Kind: deref(in.Kind), Version: deref(in.Version), Hash: deref(in.Hash),
			State: named(policyStates, in.State), Management: management(in.Management)},
		Source: deref(in.Source), Rego: deref(in.Rego),
		Errors: diagnostics(in.Errors), Warnings: diagnostics(in.Warnings),
		Updated: deref(in.Updated), UpdatedBy: deref(in.UpdatedBy),
	}
	if in.Loaded != nil {
		out.Loaded = &Loaded{Replicas: int(in.Loaded.Replicas), Total: int(in.Loaded.Total)}
	}
	return out
}

// handle is the SDK's handle to the session id. What is not a session id
// names no session: the API's own answer to it is a 404.
func (c *Client) handle(id string) (*computeruse.Session, error) {
	h, err := c.sdk.Session(id)
	if err != nil {
		return nil, &APIError{Status: http.StatusNotFound, Message: "session not found"}
	}
	return h, nil
}

// CreateSession makes a session; an empty name lets the server choose one,
// and an empty size is the deployment's default.
func (c *Client) CreateSession(ctx context.Context, name, size string) (*Session, error) {
	request := computeruse.CreateSessionRequest{}
	if name != "" {
		request.Name = &name
	}
	if size != "" {
		request.Size = &size
	}
	return call(ctx, func() (*Session, error) {
		created, err := c.sdk.CreateSession(request)
		if err != nil {
			return nil, err
		}
		info := created.LastInfo()
		if info == nil {
			return nil, errors.New("the SDK created a session and has nothing to say about it")
		}
		return session(*info), nil
	})
}

func (c *Client) GetSession(ctx context.Context, id string) (*Session, error) {
	h, err := c.handle(id)
	if err != nil {
		return nil, err
	}
	return call(ctx, func() (*Session, error) {
		info, err := h.Refresh()
		if err != nil {
			return nil, err
		}
		return session(info), nil
	})
}

func (c *Client) ListSessions(ctx context.Context) ([]Session, error) {
	return call(ctx, func() ([]Session, error) {
		list, err := c.sdk.ListSessions()
		if err != nil {
			return nil, err
		}
		out := make([]Session, len(list))
		for i, info := range list {
			out[i] = *session(info)
		}
		return out, nil
	})
}

func (c *Client) RenameSession(ctx context.Context, id, name string) (*Session, error) {
	h, err := c.handle(id)
	if err != nil {
		return nil, err
	}
	return call(ctx, func() (*Session, error) {
		info, err := h.Rename(name)
		if err != nil {
			return nil, err
		}
		return session(info), nil
	})
}

// ResizeSession changes a session's size. An awake session takes it at its
// next start; either way that start is fresh, without its snapshot.
func (c *Client) ResizeSession(ctx context.Context, id, size string) (*Session, error) {
	h, err := c.handle(id)
	if err != nil {
		return nil, err
	}
	return call(ctx, func() (*Session, error) {
		info, err := h.Resize(size)
		if err != nil {
			return nil, err
		}
		return session(info), nil
	})
}

// DeleteSession deletes a session, its disk and its browser's logins.
func (c *Client) DeleteSession(ctx context.Context, id string) error {
	h, err := c.handle(id)
	if err != nil {
		return err
	}
	_, err = call(ctx, func() (struct{}, error) { return struct{}{}, h.Delete() })
	return err
}

func (c *Client) GetPolicy(ctx context.Context, id string) (*Policy, error) {
	h, err := c.handle(id)
	if err != nil {
		return nil, err
	}
	return call(ctx, func() (*Policy, error) {
		p, err := h.Policy()
		if err != nil {
			return nil, err
		}
		return policy(p), nil
	})
}

// PutPolicy replaces the policy. loading is true when it is saved and not
// yet in force everywhere (the API's 202).
func (c *Client) PutPolicy(ctx context.Context, id string, in PolicyInput) (p *Policy, loading bool, err error) {
	h, err := c.handle(id)
	if err != nil {
		return nil, false, err
	}
	// Rego is the only kind, and the SDK says so itself.
	input := computeruse.PolicyInput{Source: in.Source}
	if m := in.Management; m != nil {
		input.Management = &computeruse.Management{Mode: computeruse.ManagementModeEditor}
		if m.Mode == ModeIaC {
			input.Management.Mode = computeruse.ManagementModeIac
		}
		if m.ManagedURL != "" {
			input.Management.ManagedUrl = &m.ManagedURL
		}
	}
	p, err = call(ctx, func() (*Policy, error) {
		saved, err := h.PutPolicy(input)
		if err != nil {
			return nil, err
		}
		return policy(saved), nil
	})
	if err != nil {
		return &Policy{}, false, err
	}
	return p, p.State == StateLoading, nil
}

// ResetPolicy puts back the unrestricted policy, in editor mode.
func (c *Client) ResetPolicy(ctx context.Context, id string) error {
	h, err := c.handle(id)
	if err != nil {
		return err
	}
	_, err = call(ctx, func() (struct{}, error) {
		_, err := h.ResetPolicy()
		return struct{}{}, err
	})
	return err
}

// ValidatePolicy checks a policy without saving it.
func (c *Client) ValidatePolicy(ctx context.Context, source string) (*Validation, error) {
	return call(ctx, func() (*Validation, error) {
		v, err := c.sdk.ValidatePolicy(source)
		if err != nil {
			return nil, err
		}
		return &Validation{OK: v.Ok, Rego: deref(v.Rego), Hash: deref(v.Hash),
			Errors: diagnostics(v.Errors), Warnings: diagnostics(v.Warnings)}, nil
	})
}
