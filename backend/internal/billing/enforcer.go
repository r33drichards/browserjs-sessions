package billing

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/sessions"
)

// Config is the backend's billing settings (deploy.md).
type Config struct {
	Mode      Mode
	PublicURL string // the app's base URL; the billing page is under it

	Grace        time.Duration // BILLING_GRACE: from exhaustedAt to the stop sequence
	DrainTimeout time.Duration // BILLING_DRAIN_TIMEOUT: the longest a drain waits for calls
	BalancePass  time.Duration // BILLING_BALANCE_PASS: how often Metronome is read for accounts in use

	ExemptEmails     []string // BILLING_EXEMPT_EMAILS
	MaxAwakeSessions int      // MAX_AWAKE_SESSIONS, 0 for no cluster cap
	WakesPerHour     int      // WAKES_PER_HOUR, 0 for no limit

	ZeroBalanceDelete      bool          // ZERO_BALANCE_DELETE
	ZeroBalanceDeleteAfter time.Duration // ZERO_BALANCE_DELETE_AFTER

	SignupCredit bool   // SIGNUP_CREDIT
	AutoRecharge bool   // AUTO_RECHARGE
	TermsVersion string // TERMS_VERSION, "" for none to accept
	// Payments is what the read route says of Stripe: "off", "test", "live".
	Payments string
}

// Defaults fills in what deploy.md gives a default.
func (c Config) Defaults() Config {
	or := func(d *time.Duration, def time.Duration) {
		if *d <= 0 {
			*d = def
		}
	}
	or(&c.Grace, 5*time.Minute)
	or(&c.DrainTimeout, 10*time.Minute)
	or(&c.BalancePass, 5*time.Minute)
	or(&c.ZeroBalanceDeleteAfter, 14*24*time.Hour)
	if c.Payments == "" {
		c.Payments = "off"
	}
	return c
}

// BillingURL is where a refusal sends its reader.
func (c Config) BillingURL() string { return strings.TrimRight(c.PublicURL, "/") + "/billing" }

// Enforcer makes the decisions of enforcement.md at the doors (create,
// resume, wake) and carries them out on what is running (sweep.go). In
// meter mode it decides the same and only logs what it would have done.
type Enforcer struct {
	cfg       Config
	accounts  Accounts
	ledger    Ledger
	sessions  Sessions
	clock     Clock
	catalogue CatalogueSource
	exempt    map[string]bool

	mu     sync.Mutex
	starts map[string][]time.Time // by account name: starts in the last hour
}

func NewEnforcer(cfg Config, accounts Accounts, ledger Ledger, store Sessions, clock Clock, catalogue CatalogueSource) *Enforcer {
	e := &Enforcer{cfg: cfg.Defaults(), accounts: accounts, ledger: ledger, sessions: store, clock: clock,
		catalogue: catalogue, exempt: map[string]bool{}, starts: map[string][]time.Time{}}
	for _, email := range cfg.ExemptEmails {
		if email = strings.ToLower(strings.TrimSpace(email)); email != "" {
			e.exempt[email] = true
		}
	}
	return e
}

// Config is the settings the Enforcer was made with, defaults filled in.
func (e *Enforcer) Config() Config { return e.cfg }

// Enforcing reports whether refusals are made (BILLING=enforce). The plan's
// session limit then stands in for MAX_SESSIONS_PER_USER.
func (e *Enforcer) Enforcing() bool { return e.cfg.Mode == Enforce }

// standing is an account as a decision sees it: the Account as the
// cluster has it at this moment, and nothing else. No decision calls
// Stripe or Metronome.
type standing struct {
	account Account
	state   State
	tier    Tier
}

// exhausted is spec.credit.exhausted; no spec.credit counts as exhausted.
func (st standing) exhausted() bool {
	return st.account.Spec.Credit == nil || st.account.Spec.Credit.Exhausted
}

// exhaustedAt is when the account's credit ran out, nil while it has some
// (or was never counted).
func (st standing) exhaustedAt() *time.Time {
	if c := st.account.Spec.Credit; c != nil && c.Exhausted {
		return c.ExhaustedAt
	}
	return nil
}

func (e *Enforcer) standing(ctx context.Context, owner string) (standing, error) {
	acc, err := e.accounts.Ensure(ctx, owner)
	if err != nil {
		return standing{}, err
	}
	// An Account is made with its Metronome customer. Metronome being
	// down must refuse nobody: it is tried again at the next sight and by
	// the balance pass.
	if acc.Spec.MetronomeCustomerID == "" && e.ledger != nil {
		if id, err := e.ledger.EnsureCustomer(ctx, acc.Name); err != nil {
			slog.Warn("billing: Metronome customer not made yet", "account", acc.Name, "err", err)
		} else {
			acc.Spec.MetronomeCustomerID = id
		}
	}
	return e.standingOf(acc), nil
}

func (e *Enforcer) standingOf(acc Account) standing {
	return standing{
		account: acc,
		state:   StateOf(acc.Spec, e.exempt[strings.ToLower(acc.Spec.Owner)], e.cfg.TermsVersion),
		tier:    e.catalogue.Catalogue().Tier(PlanOf(acc.Spec, e.catalogue.Catalogue())),
	}
}

// PlanOf is the key of the plan an account is on: its subscription's while
// that is active, trialing or past due; otherwise pay as you go.
func PlanOf(spec AccountSpec, cat Catalogue) string {
	if sub := spec.Subscription; sub != nil && (sub.Status == "active" || sub.Status == "trialing" || sub.Status == "past_due") {
		for _, p := range cat.Plans {
			if p.LookupKey == sub.PriceLookupKey {
				return p.Key
			}
		}
	}
	return PlanPayg
}

func awake(s sessions.Session) bool {
	return s.State == sessions.Running || s.State == sessions.Starting
}

func countAwake(list []sessions.Session) (n int) {
	for _, s := range list {
		if awake(s) {
			n++
		}
	}
	return n
}

// inputs is the decision's inputs for an account, without the counts.
func (e *Enforcer) inputs(st standing) Inputs {
	return Inputs{
		// The table is always worked out as if enforcing: meter mode differs
		// in what is done with the answer.
		Mode: Enforce, State: st.state, Exhausted: st.exhausted(), Tier: st.tier,
		WakesPerHour: e.cfg.WakesPerHour, MaxAwakeSessions: e.cfg.MaxAwakeSessions,
	}
}

// startsOf counts the account's starts in the last hour.
func (e *Enforcer) startsOf(account string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	cutoff := e.clock.Now().Add(-time.Hour)
	kept := e.starts[account][:0]
	for _, t := range e.starts[account] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(e.starts, account)
	} else {
		e.starts[account] = kept
	}
	return len(kept)
}

func (e *Enforcer) started(account string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.starts[account] = append(e.starts[account], e.clock.Now())
}

// judge decides a create or a start for owner. mine is the owner's
// sessions if the caller has them already.
func (e *Enforcer) judge(ctx context.Context, op Op, owner string, mine []sessions.Session, haveMine bool) (Decision, standing, error) {
	st, err := e.standing(ctx, owner)
	if err != nil {
		return Decision{}, st, err
	}
	in := e.inputs(st)
	// Rows 1 to 8 are the account's alone; only the rest count sessions.
	if d := Decide(op, in); d.Row <= lastAccountRow {
		return d, st, nil
	}
	if !haveMine {
		if mine, err = e.sessions.List(ctx, owner); err != nil {
			return Decision{}, st, err
		}
	}
	in.Mine, in.Awake = len(mine), countAwake(mine)
	in.Starts = e.startsOf(st.account.Name)
	if e.cfg.MaxAwakeSessions > 0 {
		all, err := e.sessions.ListAll(ctx)
		if err != nil {
			return Decision{}, st, err
		}
		in.ClusterAwake = countAwake(all)
	}
	return Decide(op, in), st, nil
}

// check is judge, turned into what the caller does: nil to go ahead, a
// *Refusal, or the error that kept the question from being answered.
func (e *Enforcer) check(ctx context.Context, op Op, owner, session string, mine []sessions.Session, haveMine bool) error {
	if e.cfg.Mode == Off {
		return nil
	}
	d, st, err := e.judge(ctx, op, owner, mine, haveMine)
	if err != nil {
		if e.cfg.Mode != Enforce {
			slog.Error("billing: decision not made; allowed (not enforcing)", "op", op.String(), "err", err)
			return nil
		}
		return err
	}
	if d.Refuse == "" {
		e.started(st.account.Name)
		return nil
	}
	if e.cfg.Mode != Enforce {
		slog.Info("would_refuse", "op", op.String(), "account", st.account.Name, "session", session,
			"code", d.Refuse, "row", d.Row, "state", string(st.state))
		e.started(st.account.Name)
		return nil
	}
	slog.Info("billing: refused", "op", op.String(), "account", st.account.Name, "session", session, "code", d.Refuse, "row", d.Row)
	return NewRefusal(d.Refuse, d.Limit, e.cfg.BillingURL())
}

// Create judges a new session for owner, whose sessions are mine. It is
// asked before anything is made, and so before any warm-pool claim.
func (e *Enforcer) Create(ctx context.Context, owner string, mine []sessions.Session) error {
	return e.check(ctx, OpCreate, owner, "", mine, true)
}

// Start judges making s awake: a resume, or a wake on a call. The account
// is the session's owner's, whoever asks.
func (e *Enforcer) Start(ctx context.Context, s sessions.Session) error {
	if s.Owner == "" {
		return nil // not a user's session; nothing to charge
	}
	return e.check(ctx, OpStart, s.Owner, s.ID, nil, false)
}

// Draining is the refusal a new request to s gets while s is being
// drained, nil if it is not (or nothing is enforced).
func (e *Enforcer) Draining(s sessions.Session) error {
	if e.cfg.Mode != Enforce || s.Draining == "" {
		return nil
	}
	return NewRefusal(drainCode(s.Draining), 0, e.cfg.BillingURL())
}

// View is billing's part of the session view. owner's deletion date is
// looked up once per owner by the caller's cache, if it keeps one.
func (e *Enforcer) View(ctx context.Context, s sessions.Session) SessionView {
	v := SessionView{StoppedBy: s.StoppedBy, Draining: s.Draining}
	if !e.cfg.ZeroBalanceDelete || e.cfg.Mode != Enforce || s.Owner == "" {
		return v
	}
	acc, err := e.accounts.Get(ctx, AccountName(s.Owner))
	if err != nil {
		return v
	}
	v.DeleteAfter = e.deleteAt(e.standingOf(acc))
	return v
}

// deleteAt is when the account's sessions are deleted for having been at
// zero (enforcement.md, "Disks at zero"), nil if they are not going to be.
func (e *Enforcer) deleteAt(st standing) *time.Time {
	if !e.cfg.ZeroBalanceDelete || e.cfg.Mode != Enforce || st.state == StateExempt || st.exhaustedAt() == nil {
		return nil
	}
	from := *st.exhaustedAt()
	// With no card the clock runs from when the card went, if that is later.
	if pm := st.account.Spec.PaymentMethod; st.state == StateNoCard && pm != nil && pm.RemovedAt != nil && pm.RemovedAt.After(from) {
		from = *pm.RemovedAt
	}
	at := from.Add(e.cfg.ZeroBalanceDeleteAfter)
	return &at
}
