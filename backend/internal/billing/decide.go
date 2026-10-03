package billing

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// Mode is BILLING (enforcement.md, "Modes").
type Mode string

const (
	Off     Mode = "off"
	Meter   Mode = "meter"   // accounts and the read routes; nothing refused, nothing stopped
	Enforce Mode = "enforce" // the decision table applies
)

// ParseMode reads BILLING. Unset is off.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case "", Off:
		return Off, nil
	case Meter, Enforce:
		return Mode(s), nil
	}
	return Off, fmt.Errorf("must be off, meter or enforce, got %q", s)
}

// State is an account's state: derived, not stored.
type State string

const (
	StateBlocked State = "blocked"
	StateExempt  State = "exempt"
	StateTerms   State = "terms"
	StateNoCard  State = "no_card"
	StateActive  State = "active"
)

// StateOf derives an account's state: the first that matches. exempt is
// whether the owner is in BILLING_EXEMPT_EMAILS; termsVersion the version
// that must have been accepted, "" for none.
func StateOf(spec AccountSpec, exempt bool, termsVersion string) State {
	switch {
	case spec.Blocked != nil || spec.DeletedAt != nil:
		return StateBlocked
	case spec.Exempt || exempt:
		return StateExempt
	case termsVersion != "" && (spec.TermsAcceptedAt == nil || spec.TermsVersion != termsVersion):
		return StateTerms
	case spec.PaymentMethod == nil || !spec.PaymentMethod.Present:
		return StateNoCard
	}
	return StateActive
}

// Op is what is being decided: the columns of the decision table.
type Op int

const (
	OpCreate  Op = iota // POST /api/sessions
	OpStart             // resume, or wake on a call
	OpRunning           // a session that is already running (the sweep)
)

func (o Op) String() string { return [...]string{"create", "start", "running"}[o] }

// The refusals (backend-api.yaml, Error.code).
const (
	CodePaymentMethodRequired = "payment_method_required"
	CodeOutOfCredit           = "out_of_credit"
	CodeSessionLimit          = "session_limit"
	CodeAwakeLimit            = "awake_limit"
	CodeAtCapacity            = "at_capacity"
	CodeSizeNotIncluded       = "size_not_included"
	CodeRateLimited           = "rate_limited"
	CodeMeteringUnavailable   = "metering_unavailable"
	CodeAccountBlocked        = "account_blocked"
	CodeTermsRequired         = "terms_required"
	CodeUIOnly                = "ui_only"
	CodeStripeUnavailable     = "stripe_unavailable"
)

// Action is what the table says of a session that is already running.
type Action int

const (
	Leave        Action = iota
	SleepNow            // the stop sequence, with no grace
	StopSequence        // the stop sequence, from exhaustedAt + BILLING_GRACE
)

// Inputs is everything a decision is made from (enforcement.md, "The inputs
// of a decision"). The account is always the session's owner's.
type Inputs struct {
	Mode  Mode
	State State
	// Exhausted is spec.credit.exhausted; an Account with no spec.credit
	// counts as exhausted.
	Exhausted bool
	Tier      Tier
	// Mine is how many sessions the owner has; Awake how many of them are
	// running or starting; ClusterAwake how many of everyone's are.
	Mine, Awake, ClusterAwake int
	// Starts is how many sessions this account started in the last hour.
	Starts int

	WakesPerHour     int // WAKES_PER_HOUR
	MaxAwakeSessions int // MAX_AWAKE_SESSIONS
}

// Decision is the first row of the table that matched, and what it says
// for the Op asked about: Refuse is a code, "" to allow (create, start);
// Action is for a running session.
type Decision struct {
	Row    int
	Refuse string
	Action Action
	// Limit is the N of session_limit and awake_limit.
	Limit int
}

// lastAccountRow is the last row that is decided from the account alone:
// the rows after it count sessions.
const lastAccountRow = 8

// Decide is the decision table of enforcement.md, top to bottom.
func Decide(op Op, in Inputs) Decision {
	refuse := func(row int, code string, running Action) Decision {
		if op == OpRunning {
			return Decision{Row: row, Action: running}
		}
		return Decision{Row: row, Refuse: code}
	}
	allow := func(row int) Decision { return Decision{Row: row} }
	switch {
	case in.Mode != Enforce:
		return allow(1)
	case in.State == StateBlocked:
		return refuse(2, CodeAccountBlocked, SleepNow)
	case in.State == StateExempt:
		return allow(3)
	case in.State == StateTerms:
		return refuse(4, CodeTermsRequired, Leave)
	case in.State == StateNoCard:
		return refuse(5, CodePaymentMethodRequired, SleepNow)
	// Rows 6 and 7 are gone: there is no stale ledger to decide on.
	case in.Exhausted:
		return refuse(8, CodeOutOfCredit, StopSequence)
	case op == OpCreate && in.Mine >= in.Tier.MaxSessions:
		return Decision{Row: 9, Refuse: CodeSessionLimit, Limit: in.Tier.MaxSessions}
	case in.Awake >= in.Tier.MaxAwake:
		d := refuse(10, CodeAwakeLimit, Leave)
		d.Limit = in.Tier.MaxAwake
		return d
	case in.WakesPerHour > 0 && in.Starts >= in.WakesPerHour:
		return refuse(11, CodeRateLimited, Leave)
	case in.MaxAwakeSessions > 0 && in.ClusterAwake >= in.MaxAwakeSessions:
		return refuse(12, CodeAtCapacity, Leave)
	}
	return allow(13)
}

// Refusal is a refused create, resume or wake, and how it is said
// (enforcement.md, "Answers"). It is an error so that it can travel up
// through the waker.
type Refusal struct {
	Code       string
	Status     int
	Message    string
	RetryAfter int    // seconds; 0 for none
	Limit      int    // the N of session_limit and awake_limit
	BillingURL string // <PUBLIC_URL>/billing
}

func (r *Refusal) Error() string { return r.Code + ": " + r.Message }

// AsRefusal finds a Refusal in err.
func AsRefusal(err error) (*Refusal, bool) {
	var r *Refusal
	ok := errors.As(err, &r)
	return r, ok
}

// rateLimitRetry is the Retry-After of rate_limited: starts are counted
// over an hour, so one becomes free within it; a client that waits ten
// minutes has not hammered.
const rateLimitRetry = 600

// NewRefusal is the answer for a code of the decision table.
func NewRefusal(code string, limit int, billingURL string) *Refusal {
	r := &Refusal{Code: code, Limit: limit, BillingURL: billingURL}
	switch code {
	case CodePaymentMethodRequired:
		r.Status, r.Message = http.StatusPaymentRequired, "Add a payment method to create or wake sessions."
	case CodeOutOfCredit:
		r.Status, r.Message = http.StatusPaymentRequired, "You are out of credit. Add credit or change plan to continue."
	case CodeSessionLimit:
		r.Status, r.Message = http.StatusConflict, fmt.Sprintf("Your plan allows %d sessions. Delete one, or change plan.", limit)
	case CodeAwakeLimit:
		r.Status, r.Message = http.StatusConflict, fmt.Sprintf("Your plan runs %d sessions at once. Stop one, or change plan.", limit)
	case CodeAtCapacity:
		r.Status, r.Message, r.RetryAfter = http.StatusServiceUnavailable, "Every desktop is in use right now. Try again in a few minutes.", 120
	case CodeRateLimited:
		r.Status, r.Message, r.RetryAfter = http.StatusTooManyRequests, "Too many starts in the last hour. Try again later.", rateLimitRetry
	case CodeMeteringUnavailable:
		r.Status, r.Message, r.RetryAfter = http.StatusServiceUnavailable, "Billing is unavailable right now. Try again in a few minutes.", 120
	case CodeAccountBlocked:
		r.Status, r.Message = http.StatusForbidden, "This account is suspended. Contact support."
	case CodeTermsRequired:
		r.Status, r.Message = http.StatusForbidden, "Accept the terms to continue."
	case CodeSizeNotIncluded:
		r.Status, r.Message = http.StatusForbidden, "Your plan does not include sessions of this size. Pick a smaller size, or change plan."
	default:
		r.Status, r.Message = http.StatusForbidden, "Refused."
	}
	return r
}

// ErrorBody is Error of backend-api.yaml.
type ErrorBody struct {
	Error      string `json:"error"`
	Code       string `json:"code"`
	BillingURL string `json:"billingUrl,omitempty"`
	Limit      int    `json:"limit,omitempty"`
}

func (r *Refusal) header(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(r.RetryAfter))
	}
	w.WriteHeader(r.Status)
}

// WriteHTTP answers a request of the app or the API host.
func (r *Refusal) WriteHTTP(w http.ResponseWriter) {
	r.header(w)
	_ = json.NewEncoder(w).Encode(ErrorBody{Error: r.Message, Code: r.Code, BillingURL: r.BillingURL, Limit: r.Limit})
}

// mcpRefusal is the JSON-RPC error code of every refusal.
const mcpRefusal = -32002

// MCPMessage is what a model can relay to its user.
func (r *Refusal) MCPMessage() string {
	switch r.Code {
	case CodeOutOfCredit:
		return "This session is asleep because its owner is out of credit. It is kept as it was. Add credit at " + r.BillingURL + " and call again."
	case CodePaymentMethodRequired:
		return "This session is asleep because its owner has no payment method. It is kept as it was. Add one at " + r.BillingURL + " and call again."
	}
	return "This session cannot be used right now: " + r.Message + " See " + r.BillingURL + "."
}

// WriteMCP answers a request to a session's MCP endpoint: the same status,
// and a JSON-RPC error. The request's id is not read (the body may be a
// stream), so it is null.
func (r *Refusal) WriteMCP(w http.ResponseWriter) {
	r.header(w)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      nil,
		"error":   map[string]any{"code": mcpRefusal, "message": r.MCPMessage()},
	})
}

// drainCode is the refusal a new request to a draining session gets, by
// the reason of the drain.
func drainCode(reason string) string {
	switch reason {
	case "credit":
		return CodeOutOfCredit
	case "payment-method":
		return CodePaymentMethodRequired
	}
	return CodeAccountBlocked
}
