package billing

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// base is an account nothing stands in the way of: row 13.
func base() Inputs {
	return Inputs{
		Mode: Enforce, State: StateActive,
		Tier: Tier{MaxSessions: 3, MaxAwake: 2}, Mine: 1, Awake: 1, ClusterAwake: 4,
		Starts: 5, WakesPerHour: 30, MaxAwakeSessions: 10,
	}
}

// One case per row of the decision table of enforcement.md, each asked in
// every column: create, resume or wake, and already running.
func TestDecisionTable(t *testing.T) {
	for _, c := range []struct {
		name    string
		with    func(*Inputs)
		row     int
		create  string // the refusal, "" to allow
		start   string
		running Action
	}{
		{"1 not enforcing", func(in *Inputs) { in.Mode = Meter; in.State = StateNoCard }, 1, "", "", Leave},
		{"1 off", func(in *Inputs) { in.Mode = Off; in.State = StateBlocked }, 1, "", "", Leave},
		{"2 blocked", func(in *Inputs) { in.State = StateBlocked }, 2, CodeAccountBlocked, CodeAccountBlocked, SleepNow},
		{"3 exempt", func(in *Inputs) { in.State = StateExempt; in.Exhausted = true }, 3, "", "", Leave},
		{"4 terms", func(in *Inputs) { in.State = StateTerms }, 4, CodeTermsRequired, CodeTermsRequired, Leave},
		{"5 no card", func(in *Inputs) { in.State = StateNoCard }, 5, CodePaymentMethodRequired, CodePaymentMethodRequired, SleepNow},
		{"8 out of credit", func(in *Inputs) { in.Exhausted = true }, 8, CodeOutOfCredit, CodeOutOfCredit, StopSequence},
		{"10 awake limit", func(in *Inputs) { in.Awake = 2 }, 10, CodeAwakeLimit, CodeAwakeLimit, Leave},
		{"11 rate limited", func(in *Inputs) { in.Starts = 30 }, 11, CodeRateLimited, CodeRateLimited, Leave},
		{"12 at capacity", func(in *Inputs) { in.ClusterAwake = 10 }, 12, CodeAtCapacity, CodeAtCapacity, Leave},
		{"13 otherwise", func(*Inputs) {}, 13, "", "", Leave},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := base()
			c.with(&in)
			for op, want := range map[Op]string{OpCreate: c.create, OpStart: c.start} {
				if d := Decide(op, in); d.Row != c.row || d.Refuse != want || d.Action != Leave {
					t.Errorf("%s: row %d, refuse %q, action %d; want row %d, refuse %q", op, d.Row, d.Refuse, d.Action, c.row, want)
				}
			}
			if d := Decide(OpRunning, in); d.Row != c.row || d.Action != c.running || d.Refuse != "" {
				t.Errorf("running: row %d, action %d, refuse %q; want row %d, action %d", d.Row, d.Action, d.Refuse, c.row, c.running)
			}
		})
	}

	// Row 9 is create's alone: a resume or a wake, and a running session,
	// pass it by.
	t.Run("9 session limit", func(t *testing.T) {
		in := base()
		in.Mine = 3
		if d := Decide(OpCreate, in); d.Row != 9 || d.Refuse != CodeSessionLimit || d.Limit != 3 {
			t.Errorf("create: %+v", d)
		}
		if d := Decide(OpStart, in); d.Row != 13 || d.Refuse != "" {
			t.Errorf("start: %+v", d)
		}
		if d := Decide(OpRunning, in); d.Row != 13 || d.Action != Leave {
			t.Errorf("running: %+v", d)
		}
	})
	t.Run("the limits carry their N", func(t *testing.T) {
		in := base()
		in.Awake = 2
		if d := Decide(OpStart, in); d.Limit != 2 {
			t.Errorf("awake_limit: limit %d, want 2", d.Limit)
		}
	})
	t.Run("a limit of zero is no limit", func(t *testing.T) {
		in := base()
		in.Starts, in.ClusterAwake, in.WakesPerHour, in.MaxAwakeSessions = 1000, 1000, 0, 0
		if d := Decide(OpCreate, in); d.Row != 13 {
			t.Errorf("%+v", d)
		}
	})
}

// The first row that matches answers: an account to which every row
// applies gets row 2, and each row taken away uncovers the next.
func TestDecisionOrder(t *testing.T) {
	in := Inputs{
		Mode: Enforce, State: StateBlocked, Exhausted: true,
		Tier: Tier{MaxSessions: 3, MaxAwake: 2}, Mine: 3, Awake: 2, ClusterAwake: 10,
		Starts: 30, WakesPerHour: 30, MaxAwakeSessions: 10,
	}
	steps := []struct {
		row  int
		next func(*Inputs) // makes this row no longer apply
	}{
		{2, func(in *Inputs) { in.State = StateTerms }},
		{4, func(in *Inputs) { in.State = StateNoCard }},
		{5, func(in *Inputs) { in.State = StateActive }},
		{8, func(in *Inputs) { in.Exhausted = false }},
		{9, func(in *Inputs) { in.Mine = 2 }},
		{10, func(in *Inputs) { in.Awake = 1 }},
		{11, func(in *Inputs) { in.Starts = 29 }},
		{12, func(in *Inputs) { in.ClusterAwake = 9 }},
		{13, func(*Inputs) {}},
	}
	for _, s := range steps {
		if d := Decide(OpCreate, in); d.Row != s.row {
			t.Fatalf("row %d answered, want %d (inputs %+v)", d.Row, s.row, in)
		}
		s.next(&in)
	}
	// Exempt comes before everything but blocked. Rows 6 and 7 (the stale
	// ledger) are gone: nothing answers with them.
	in.State, in.Exhausted = StateExempt, true
	if d := Decide(OpCreate, in); d.Row != 3 {
		t.Errorf("exempt: row %d", d.Row)
	}
}

func TestStateOf(t *testing.T) {
	now := time.Now()
	card := &PaymentMethods{Present: true}
	for _, c := range []struct {
		name   string
		spec   AccountSpec
		exempt bool
		terms  string
		want   State
	}{
		{"new", AccountSpec{}, false, "", StateNoCard},
		{"card removed", AccountSpec{PaymentMethod: &PaymentMethods{Present: false, RemovedAt: &now}}, false, "", StateNoCard},
		{"card", AccountSpec{PaymentMethod: card}, false, "", StateActive},
		{"exempt by the list", AccountSpec{}, true, "", StateExempt},
		{"exempt by its spec", AccountSpec{Exempt: true}, false, "", StateExempt},
		{"blocked comes before exempt", AccountSpec{Exempt: true, Blocked: &Blocked{Reason: "abuse"}}, true, "", StateBlocked},
		{"deleted", AccountSpec{PaymentMethod: card, DeletedAt: &now}, false, "", StateBlocked},
		{"terms not accepted", AccountSpec{PaymentMethod: card}, false, "v2", StateTerms},
		{"an older version accepted", AccountSpec{PaymentMethod: card, TermsAcceptedAt: &now, TermsVersion: "v1"}, false, "v2", StateTerms},
		{"terms accepted", AccountSpec{PaymentMethod: card, TermsAcceptedAt: &now, TermsVersion: "v2"}, false, "v2", StateActive},
		{"exempt comes before terms", AccountSpec{}, true, "v2", StateExempt},
		{"terms come before the card", AccountSpec{}, false, "v2", StateTerms},
	} {
		if got := StateOf(c.spec, c.exempt, c.terms); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// The answers of enforcement.md: status, message, Retry-After.
func TestAnswers(t *testing.T) {
	const url = "https://app.example.test/billing"
	for _, c := range []struct {
		code       string
		limit      int
		status     int
		message    string
		retryAfter string
	}{
		{CodePaymentMethodRequired, 0, 402, "Add a payment method to create or wake sessions.", ""},
		{CodeOutOfCredit, 0, 402, "You are out of credit. Add credit or change plan to continue.", ""},
		{CodeSessionLimit, 3, 409, "Your plan allows 3 sessions. Delete one, or change plan.", ""},
		{CodeAwakeLimit, 2, 409, "Your plan runs 2 sessions at once. Stop one, or change plan.", ""},
		{CodeAtCapacity, 0, 503, "Every desktop is in use right now. Try again in a few minutes.", "120"},
		{CodeRateLimited, 0, 429, "Too many starts in the last hour. Try again later.", "600"},
		{CodeMeteringUnavailable, 0, 503, "Billing is unavailable right now. Try again in a few minutes.", "120"},
		{CodeAccountBlocked, 0, 403, "This account is suspended. Contact support.", ""},
		{CodeTermsRequired, 0, 403, "Accept the terms to continue.", ""},
	} {
		t.Run(c.code, func(t *testing.T) {
			ref := NewRefusal(c.code, c.limit, url)
			rec := httptest.NewRecorder()
			ref.WriteHTTP(rec)
			var body ErrorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if rec.Code != c.status || body.Error != c.message || body.Code != c.code || body.BillingURL != url || body.Limit != c.limit {
				t.Errorf("%d %s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get("Retry-After"); got != c.retryAfter {
				t.Errorf("Retry-After = %q, want %q", got, c.retryAfter)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q", got)
			}

			rec = httptest.NewRecorder()
			ref.WriteMCP(rec)
			var rpc struct {
				JSONRPC string           `json:"jsonrpc"`
				ID      *json.RawMessage `json:"id"`
				Error   struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &rpc); err != nil {
				t.Fatal(err)
			}
			if rec.Code != c.status || rpc.JSONRPC != "2.0" || rpc.Error.Code != -32002 || !strings.Contains(rpc.Error.Message, url) {
				t.Errorf("MCP: %d %s", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), `"id":null`) {
				t.Errorf("MCP: the id is not null: %s", rec.Body)
			}
			if got := rec.Header().Get("Retry-After"); got != c.retryAfter {
				t.Errorf("MCP: Retry-After = %q, want %q", got, c.retryAfter)
			}
		})
	}
	// The two messages of the contract, word for word.
	for code, want := range map[string]string{
		CodeOutOfCredit:           "This session is asleep because its owner is out of credit. It is kept as it was. Add credit at https://app.example.test/billing and call again.",
		CodePaymentMethodRequired: "This session is asleep because its owner has no payment method. It is kept as it was. Add one at https://app.example.test/billing and call again.",
	} {
		if got := NewRefusal(code, 0, url).MCPMessage(); got != want {
			t.Errorf("%s: %q", code, got)
		}
	}
	// A refusal travels as an error.
	var err error = NewRefusal(CodeOutOfCredit, 0, url)
	if ref, ok := AsRefusal(err); !ok || ref.Status != http.StatusPaymentRequired {
		t.Errorf("AsRefusal(%v) = %v, %v", err, ref, ok)
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": Off, "off": Off, "meter": Meter, "enforce": Enforce} {
		if got, err := ParseMode(in); err != nil || got != want {
			t.Errorf("ParseMode(%q) = %s, %v", in, got, err)
		}
	}
	if _, err := ParseMode("on"); err == nil {
		t.Error(`"on" is not a mode`)
	}
}
