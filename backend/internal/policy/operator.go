package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrOperatorUnavailable is the answer when the policy operator could not be
// asked, or answered something other than its API says it does. Nothing is
// saved on the strength of a check that was not made.
var ErrOperatorUnavailable = errors.New("the policy operator is unavailable")

// The operator refuses bodies over 128 KiB (validate) and inputs over 1 MiB
// (evaluate); the backend refuses them first.
const (
	maxValidateBody = 128 << 10
	maxEvaluateBody = (1 << 20) + maxValidateBody
	maxOperatorBody = 4 << 20 // what is read of an answer
)

// Operator is a client of the policy operator's HTTP API
// (docs/contracts/policy/operator-api.yaml).
type Operator struct {
	base   string
	token  string
	client *http.Client
}

// NewOperator talks to the operator at baseURL, as OPERATOR_API_TOKEN.
func NewOperator(baseURL, token string) *Operator {
	return &Operator{
		base:   strings.TrimRight(baseURL, "/"),
		token:  token,
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

// Validation is the operator's verdict on a policy.
type Validation struct {
	OK       bool         `json:"ok"`
	Rego     string       `json:"rego,omitempty"`
	Hash     string       `json:"hash,omitempty"`
	Errors   []Diagnostic `json:"errors"`
	Warnings []Diagnostic `json:"warnings"`
}

// answer is what the operator said, for passing on.
type answer struct {
	status      int
	contentType string
	body        []byte
}

func (o *Operator) do(ctx context.Context, method, path string, body []byte) (answer, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, o.base+path, reader)
	if err != nil {
		return answer{}, fmt.Errorf("%w: %v", ErrOperatorUnavailable, err)
	}
	req.Header.Set("Authorization", "Bearer "+o.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return answer{}, fmt.Errorf("%w: %v", ErrOperatorUnavailable, err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(io.LimitReader(resp.Body, maxOperatorBody+1))
	if err != nil || len(got) > maxOperatorBody {
		return answer{}, fmt.Errorf("%w: reading its answer to %s: %v", ErrOperatorUnavailable, path, err)
	}
	return answer{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: got}, nil
}

// Validate asks the operator whether a policy is valid. An invalid policy
// is a Validation with OK false, not an error.
func (o *Operator) Validate(ctx context.Context, kind, source string) (Validation, error) {
	body, err := json.Marshal(map[string]string{"kind": kind, "source": source})
	if err != nil {
		return Validation{}, err
	}
	a, err := o.do(ctx, http.MethodPost, "/v1/validate", body)
	if err != nil {
		return Validation{}, err
	}
	if a.status != http.StatusOK {
		return Validation{}, fmt.Errorf("%w: validate answered %d", ErrOperatorUnavailable, a.status)
	}
	var v Validation
	if err := json.Unmarshal(a.body, &v); err != nil {
		return Validation{}, fmt.Errorf("%w: validate answered what is not JSON: %v", ErrOperatorUnavailable, err)
	}
	if !v.OK && len(v.Errors) == 0 {
		// A refusal has to say why; without it, it cannot be told from an
		// answer of some other shape.
		return Validation{}, fmt.Errorf("%w: validate refused a policy without saying why", ErrOperatorUnavailable)
	}
	return v, nil
}
