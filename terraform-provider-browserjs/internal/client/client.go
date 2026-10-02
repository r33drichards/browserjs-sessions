// Package client speaks the API of docs/contracts/policy/backend-api.yaml on
// the API host, with an API token.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Policy states, as PolicyState in the API.
const (
	StateReady       = "ready"
	StateLoading     = "loading"
	StateInvalid     = "invalid"
	StateUnsupported = "unsupported"
)

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
// are 1-based, and zero when the API gave none.
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
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("the API answered %d %s", e.Status, http.StatusText(e.Status))
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
	base      string
	token     string
	userAgent string
	http      *http.Client
}

// New makes a client for the API host at endpoint (without /v1).
func New(endpoint, token, version string) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("endpoint must be an http(s) URL such as https://api.computeruse.site")
	}
	if token == "" {
		return nil, errors.New("token is empty")
	}
	return &Client{
		base:      strings.TrimRight(endpoint, "/") + "/v1",
		token:     token,
		userAgent: "terraform-provider-browserjs/" + version,
		http:      &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// do sends one request. out, when not nil, receives a 2xx body. The status
// is returned so callers can tell 200 from 202.
func (c *Client) do(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// A *url.Error names the method and URL, never a header.
		return 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("%s %s: reading the answer: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error      string       `json:"error"`
			Errors     []Diagnostic `json:"errors"`
			Warnings   []Diagnostic `json:"warnings"`
			ManagedURL string       `json:"managed_url"`
		}
		_ = json.Unmarshal(raw, &e)
		return resp.StatusCode, &APIError{Status: resp.StatusCode, Message: e.Error, Errors: e.Errors, Warnings: e.Warnings, ManagedURL: e.ManagedURL}
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: the answer is not the JSON expected: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

func sessionPath(id string) string { return "/sessions/" + url.PathEscape(id) }

// CreateSession makes a session; an empty name lets the server choose one.
func (c *Client) CreateSession(ctx context.Context, name string) (*Session, error) {
	in := map[string]string{}
	if name != "" {
		in["name"] = name
	}
	var s Session
	_, err := c.do(ctx, http.MethodPost, "/sessions", in, &s)
	return &s, err
}

func (c *Client) GetSession(ctx context.Context, id string) (*Session, error) {
	var s Session
	_, err := c.do(ctx, http.MethodGet, sessionPath(id), nil, &s)
	return &s, err
}

func (c *Client) ListSessions(ctx context.Context) ([]Session, error) {
	var l []Session
	_, err := c.do(ctx, http.MethodGet, "/sessions", nil, &l)
	return l, err
}

func (c *Client) RenameSession(ctx context.Context, id, name string) (*Session, error) {
	var s Session
	_, err := c.do(ctx, http.MethodPatch, sessionPath(id), map[string]string{"name": name}, &s)
	return &s, err
}

// DeleteSession deletes a session, its disk and its browser's logins.
func (c *Client) DeleteSession(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, sessionPath(id), nil, nil)
	return err
}

func (c *Client) GetPolicy(ctx context.Context, id string) (*Policy, error) {
	var p Policy
	_, err := c.do(ctx, http.MethodGet, sessionPath(id)+"/policy", nil, &p)
	return &p, err
}

// PutPolicy replaces the policy. loading is true for 202: saved, not yet in
// force everywhere.
func (c *Client) PutPolicy(ctx context.Context, id string, in PolicyInput) (p *Policy, loading bool, err error) {
	p = &Policy{}
	status, err := c.do(ctx, http.MethodPut, sessionPath(id)+"/policy", in, p)
	return p, status == http.StatusAccepted, err
}

// ResetPolicy puts back the unrestricted policy, in editor mode.
func (c *Client) ResetPolicy(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, sessionPath(id)+"/policy", nil, nil)
	return err
}

// ValidatePolicy checks a policy without saving it.
func (c *Client) ValidatePolicy(ctx context.Context, kind, source string) (*Validation, error) {
	var v Validation
	_, err := c.do(ctx, http.MethodPost, "/policies/validate", PolicyInput{Kind: kind, Source: source}, &v)
	return &v, err
}
