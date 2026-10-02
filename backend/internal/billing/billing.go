// Package billing is accounts and enforcement: who has a card and credit,
// what they may start, and what is put to sleep when they have neither.
//
// This file is the interfaces of docs/contracts/billing/testing.md and the
// types they speak in. Every stateful thing billing and enforcement touch
// (accounts, the ledger, the clock, Stripe, the session store, the proxy's
// calls in flight) is one of these, with an in-memory fake in billingtest
// and, for the cluster, an implementation in billing/kube. Nothing in this
// package knows which it has.
package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/sessions"
)

// ErrNotFound is the answer of a lookup (an Account, a Checkout Session, a
// period) that found nothing.
var ErrNotFound = errors.New("billing: not found")

// Clock is the only source of time in billing and enforcement code.
type Clock interface{ Now() time.Time }

// SystemClock is the real time, in UTC to the second (what the cluster's
// objects carry).
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC().Truncate(time.Second) }

// Accounts is the account and payment-method state: Account.spec.
type Accounts interface {
	// Ensure returns the owner's Account, making it if there is none.
	Ensure(ctx context.Context, owner string) (Account, error)
	Get(ctx context.Context, name string) (Account, error)
	ByCustomer(ctx context.Context, stripeCustomerID string) (Account, error)
	// ByPaymentMethod finds the Account whose paymentMethod.ids has id.
	ByPaymentMethod(ctx context.Context, id string) (Account, error)
	// ByMetronomeCustomer finds the Account whose metronomeCustomerId is id
	// (Metronome's alert names nothing else). testing.md does not have it.
	ByMetronomeCustomer(ctx context.Context, id string) (Account, error)
	WithCustomer(ctx context.Context) ([]Account, error)
	// Update applies change to the Account's spec with optimistic
	// concurrency, retrying on conflict. It is the only way spec is written.
	Update(ctx context.Context, name string, change func(*AccountSpec) error) (Account, error)
}

// Ledger is the credit, kept in Metronome (metronome.md). Its production
// implementation (NewLedger) is written over the Metronome interface below
// and holds no state. No decision at create, resume or wake calls it: they
// read Account.spec.credit.
type Ledger interface {
	// EnsureCustomer makes the account's Metronome customer and contract if
	// there are none, and returns the customer's ID.
	EnsureCustomer(ctx context.Context, account string) (customerID string, err error)
	// Balance reads the account's credit from Metronome now: the net
	// balance, and what is left of each credit with its source and end.
	Balance(ctx context.Context, account string) (Balance, error)
	// Usage reads what the account used between from and to, by session
	// and by day.
	Usage(ctx context.Context, account string, from, to time.Time) (Usage, error)
	// EnsureGrant creates the credit whose uniqueness key is g.Key, then
	// runs EnsureCredit. created is false when the key was used: existing
	// is that credit if it is this account's (a replay), and has an empty
	// Account if it is another's.
	EnsureGrant(ctx context.Context, g Grant) (created bool, existing Grant, err error)
	// Revoke archives the credits sel finds, then runs EnsureCredit. With no
	// sel.Account (a refund names a payment, not an account) every account
	// that has a Stripe customer is looked in.
	Revoke(ctx context.Context, sel GrantSelector, reason string) error
	// EnsureCredit reads Balance and writes Account.spec.credit (exhausted,
	// exhaustedAt, balanceMicros, nextExpiryAt, checkedAt). The only writer
	// of that field.
	EnsureCredit(ctx context.Context, account string) (AccountCredit, error)
}

// Metronome is every call the backend makes to Metronome (metronome.md),
// as Stripe is for Stripe. One method per endpoint; no logic.
type Metronome interface {
	CreateCustomer(ctx context.Context, p MetronomeCustomerParams) (id string, err error)
	CustomerByAlias(ctx context.Context, alias string) (id string, found bool, err error)
	CreateContract(ctx context.Context, p MetronomeContractParams) error // a 409 is nil
	// CreateCredit reports conflict (not an error) when the key was used.
	CreateCredit(ctx context.Context, p MetronomeCreditParams) (id string, conflict bool, err error)
	Credits(ctx context.Context, customer string) ([]MetronomeCredit, error) // with balances and custom fields
	ArchiveCredit(ctx context.Context, customer, id string) error
	Usage(ctx context.Context, customer string, from, to time.Time) (MetronomeUsage, error)
}

// Observer is the sign of life of the observer that sends usage to
// Metronome: when it last renewed its Lease. testing.md does not name it;
// nothing is refused on it (it only makes the billing page say "stale").
type Observer interface {
	Renewed(ctx context.Context) (time.Time, error)
}

// Stripe is every call the backend makes to Stripe (stripe.md).
type Stripe interface {
	CreateCustomer(ctx context.Context, p CustomerParams) (id string, err error)
	CreateCheckout(ctx context.Context, p CheckoutParams) (CheckoutSession, error) // setup, subscription or payment
	CreatePortal(ctx context.Context, customer, returnURL string) (url string, err error)
	CreateRecharge(ctx context.Context, p RechargeParams) (PaymentIntent, error)
	Checkout(ctx context.Context, id string) (CheckoutSession, error)
	PaymentMethods(ctx context.Context, customer string) (methods []PaymentMethod, defaultID string, err error)
	Subscription(ctx context.Context, id string) (Subscription, error)
	PaymentIntent(ctx context.Context, id string) (PaymentIntent, error)
	Prices(ctx context.Context, lookupKeys []string) (map[string]string, error)
	CancelSubscription(ctx context.Context, id string) error
	DetachPaymentMethod(ctx context.Context, id string) error
	// The list calls of the reconcile (stripe.md, "Reconcile").
	Subscriptions(ctx context.Context, customer string) ([]Subscription, error)
	Checkouts(ctx context.Context, customer string, since time.Time) ([]CheckoutSession, error)
	PaymentIntents(ctx context.Context, customer string, since time.Time) ([]PaymentIntent, error)
}

// Sessions is what enforcement and the API need of the session store.
type Sessions interface {
	Create(ctx context.Context, name, owner string, policy *sessions.PolicySpec) (sessions.Session, error)
	Get(ctx context.Context, id string) (sessions.Session, error)
	List(ctx context.Context, owner string) ([]sessions.Session, error)
	ListAll(ctx context.Context) ([]sessions.Session, error)
	// Sleep snapshots the session, then suspends it, recording why.
	Sleep(ctx context.Context, id, stoppedBy string, stillWanted func(sessions.Session) bool) error
	Wake(ctx context.Context, id string) error
	Update(ctx context.Context, id string, name *string, action string) error
	Delete(ctx context.Context, id string) error
	SetDraining(ctx context.Context, id, reason string) error // "" clears it
}

// InFlight is one replica's knowledge of work in progress on a session:
// its own. What the other replicas have in flight they say on the session
// (sessions.Session.InFlight), under their names.
type InFlight interface {
	Calls(id string) int    // MCP calls and uploads this replica is proxying now
	CloseStreams(id string) // this replica's VNC viewers and MCP event streams
	Replica() string        // this replica's name among the marks on a session
}

// Store adapts a *sessions.Store to Sessions: the store's Create takes no
// policy, and the one that does is CreateWithPolicy.
type Store struct{ *sessions.Store }

func (s Store) Create(ctx context.Context, name, owner string, policy *sessions.PolicySpec) (sessions.Session, error) {
	return s.Store.CreateWithPolicy(ctx, name, owner, policy)
}

// SessionView is what billing adds to a session as the API shows it
// (backend-api.yaml): why it is asleep or stopped, why it is draining, and
// when it will be deleted for its account being at zero.
type SessionView struct {
	StoppedBy   string
	Draining    string
	DeleteAfter *time.Time
}

// AccountName is the name of owner's Account: acct-<sessions.OwnerLabel>.
func AccountName(owner string) string { return "acct-" + sessions.OwnerLabel(owner) }

// Account is one user of the product (crd-account.yaml). Spec is the
// backend's; the operator's status is read through Ledger.
type Account struct {
	Name    string
	Created time.Time
	Spec    AccountSpec
}

// AccountSpec is Account.spec, field for field.
type AccountSpec struct {
	Owner            string `json:"owner"`
	OwnerHash        string `json:"ownerHash"`
	StripeCustomerID string `json:"stripeCustomerId,omitempty"`
	// MetronomeCustomerID is the Metronome customer, made with the Account.
	MetronomeCustomerID string `json:"metronomeCustomerId,omitempty"`
	// Credit is whether the account has credit left, as last read from
	// Metronome by Ledger.EnsureCredit. Absent counts as exhausted.
	Credit          *AccountCredit     `json:"credit,omitempty"`
	PaymentMethod   *PaymentMethods    `json:"paymentMethod,omitempty"`
	SignupCredit    *SignupCredit      `json:"signupCredit,omitempty"`
	Subscription    *SubscriptionState `json:"subscription,omitempty"`
	AutoRecharge    *AutoRecharge      `json:"autoRecharge,omitempty"`
	Exempt          bool               `json:"exempt,omitempty"`
	Blocked         *Blocked           `json:"blocked,omitempty"`
	TermsAcceptedAt *time.Time         `json:"termsAcceptedAt,omitempty"`
	TermsVersion    string             `json:"termsVersion,omitempty"`
	DeletedAt       *time.Time         `json:"deletedAt,omitempty"`
}

// PaymentMethods is what was last read from Stripe about the customer's
// saved payment methods.
type PaymentMethods struct {
	Present   bool       `json:"present"`
	IDs       []string   `json:"ids,omitempty"`
	Default   string     `json:"default,omitempty"`
	ReadAt    time.Time  `json:"readAt"`
	RemovedAt *time.Time `json:"removedAt,omitempty"`
}

// The outcomes of the sign-up credit, and why it was refused.
const (
	SignupGranted = "granted"
	SignupRefused = "refused"

	RefusedCardUsed      = "card-used"
	RefusedPrepaid       = "prepaid"
	RefusedWallet        = "wallet"
	RefusedNoFingerprint = "no-fingerprint"
)

type SignupCredit struct {
	State  string    `json:"state"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

type SubscriptionState struct {
	ID                 string     `json:"id"`
	Status             string     `json:"status"`
	PriceLookupKey     string     `json:"priceLookupKey"`
	CurrentPeriodStart *time.Time `json:"currentPeriodStart,omitempty"`
	CurrentPeriodEnd   *time.Time `json:"currentPeriodEnd,omitempty"`
	CancelAt           *time.Time `json:"cancelAt,omitempty"`
	ReadAt             *time.Time `json:"readAt,omitempty"`
}

type AutoRecharge struct {
	Enabled         bool          `json:"enabled"`
	ThresholdMicros int64         `json:"thresholdMicros,omitempty"`
	Pack            string        `json:"pack,omitempty"`
	MonthlyCapCents int64         `json:"monthlyCapCents,omitempty"`
	AgreedAt        *time.Time    `json:"agreedAt,omitempty"`
	Month           string        `json:"month,omitempty"`
	Seq             int           `json:"seq,omitempty"`
	ChargedCents    int64         `json:"chargedCents,omitempty"`
	Last            *LastRecharge `json:"last,omitempty"`
	DisabledReason  string        `json:"disabledReason,omitempty"`
}

type LastRecharge struct {
	PaymentIntent string     `json:"paymentIntent,omitempty"`
	Status        string     `json:"status,omitempty"`
	At            *time.Time `json:"at,omitempty"`
}

type Blocked struct {
	Reason string     `json:"reason"`
	Note   string     `json:"note,omitempty"`
	Since  *time.Time `json:"since,omitempty"`
}

// The levels of a ledger.
const (
	LevelOK        = "ok"
	LevelLow       = "low"
	LevelExhausted = "exhausted"
)

// AccountCredit is Account.spec.credit: what a decision reads.
type AccountCredit struct {
	Exhausted     bool       `json:"exhausted"`
	ExhaustedAt   *time.Time `json:"exhaustedAt,omitempty"`
	BalanceMicros int64      `json:"balanceMicros"`
	NextExpiryAt  *time.Time `json:"nextExpiryAt,omitempty"`
	CheckedAt     *time.Time `json:"checkedAt,omitempty"`
}

// Balance is an account's credit as Metronome has it now.
type Balance struct {
	NetMicros int64
	// Credits is every credit that counts now, in the order it is used.
	Credits []CreditBalance
}

type CreditBalance struct {
	Key             string
	Source          string
	AmountMicros    int64
	RemainingMicros int64
	ExpiresAt       *time.Time
}

// SourceBalance is the balance of one source (backend-api.yaml, balances).
type SourceBalance struct {
	Source    string     `json:"source"`
	Micros    int64      `json:"micros"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// Usage is what an account used over a period, with what it cost at the
// catalogue's rates.
type Usage struct {
	Start         time.Time
	End           time.Time
	AwakeSeconds  int64
	DiskGBSeconds int64
	AwakeMicros   int64
	DiskMicros    int64
	Days          []DayUsage
	Sessions      []SessionUsage
}

type DayUsage struct {
	Date         string `json:"date"`
	AwakeSeconds int64  `json:"awakeSeconds,omitempty"`
	AwakeMicros  int64  `json:"awakeMicros,omitempty"`
	DiskMicros   int64  `json:"diskMicros,omitempty"`
}

type SessionUsage struct {
	ID           string `json:"id"`
	AwakeSeconds int64  `json:"awakeSeconds,omitempty"`
	AwakeMicros  int64  `json:"awakeMicros,omitempty"`
	DiskMicros   int64  `json:"diskMicros,omitempty"`
}

// The sources of a Grant.
const (
	SourceSignup   = "signup"
	SourcePlan     = "plan"
	SourcePurchase = "purchase"
	SourceAdmin    = "admin"
)

// Grant is an amount of credit given to one account (crd-grant.yaml). Its
// name is GrantName(Key): what caused it, so it cannot be made twice.
type Grant struct {
	Name         string     `json:"-"`
	Account      string     `json:"account"`
	Source       string     `json:"source"`
	AmountMicros int64      `json:"amountMicros"`
	ValidFrom    time.Time  `json:"validFrom"`
	ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
	Key          string     `json:"key"`
	Item         string     `json:"item,omitempty"`
	Ref          *GrantRef  `json:"ref,omitempty"`
	Revoked      *Revoked   `json:"revoked,omitempty"`
}

type GrantRef struct {
	CheckoutSession string `json:"checkoutSession,omitempty"`
	PaymentIntent   string `json:"paymentIntent,omitempty"`
	Invoice         string `json:"invoice,omitempty"`
	Subscription    string `json:"subscription,omitempty"`
}

type Revoked struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// GrantName is the name of the Grant made for key: "g-" and the first 40
// hex characters of the key's SHA-256 (stripe.md, "Grant keys").
func GrantName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "g-" + hex.EncodeToString(sum[:])[:40]
}

// GrantSelector picks the unrevoked Grants Revoke applies to: every field
// that is set must match.
type GrantSelector struct {
	Account       string     // the Account's name
	Name          string     // one Grant
	Source        string     // SourcePlan, ...
	PaymentIntent string     // ref.paymentIntent (the label browserjs.dev/payment-intent)
	ExpiresAfter  *time.Time // expiresAt is after this
	Except        string     // not the Grant of this name
}

// Matches reports whether g is one of the Grants sel picks.
func (sel GrantSelector) Matches(g Grant) bool {
	switch {
	case g.Revoked != nil,
		sel.Account != "" && g.Account != sel.Account,
		sel.Name != "" && g.Name != sel.Name,
		sel.Source != "" && g.Source != sel.Source,
		sel.Except != "" && g.Name == sel.Except,
		sel.PaymentIntent != "" && (g.Ref == nil || g.Ref.PaymentIntent != sel.PaymentIntent),
		sel.ExpiresAfter != nil && g.ExpiresAt != nil && !g.ExpiresAt.After(*sel.ExpiresAfter):
		return false
	}
	return true
}

// What the backend says to Metronome and hears back (metronome.md).

type MetronomeCustomerParams struct {
	Name        string // the owner hash: no email address is sent
	IngestAlias string // the Account's name
}

type MetronomeContractParams struct {
	Customer      string
	RateCardAlias string
	StartingAt    time.Time
	UniquenessKey string // contract/<Account name>
}

type MetronomeCreditParams struct {
	Customer      string
	Name          string // what the user sees
	UniquenessKey string // the Grant key
	Priority      int    // lower is used first
	AmountMicros  int64  // sent as cents
	StartingAt    time.Time
	EndingBefore  time.Time
	CustomFields  map[string]string // grant_key, source, payment_intent
}

type MetronomeCredit struct {
	ID            string
	Name          string
	Priority      int
	AmountMicros  int64
	BalanceMicros int64
	StartingAt    time.Time
	EndingBefore  time.Time
	Archived      bool
	CustomFields  map[string]string
}

// MetronomeUsage is usage by session and UTC day.
type MetronomeUsage struct {
	Rows []MetronomeUsageRow
}

type MetronomeUsageRow struct {
	SessionID     string
	Day           string // yyyy-mm-dd
	AwakeSeconds  int64
	DiskGBSeconds int64
}

// What the backend says to Stripe and hears back, reduced to the fields
// stripe.md uses.

type CustomerParams struct {
	Email     string
	Account   string // metadata.account
	OwnerHash string // metadata.owner_hash; the idempotency key is customer-<ownerHash>
}

// The modes of a Checkout Session, and the kinds the backend makes.
const (
	ModeSetup        = "setup"
	ModeSubscription = "subscription"
	ModePayment      = "payment"

	KindSetup    = "setup"
	KindPlan     = "plan"
	KindPurchase = "purchase"
	KindRecharge = "recharge"
)

type CheckoutParams struct {
	Mode       string // ModeSetup, ModeSubscription or ModePayment
	Customer   string
	Account    string // client_reference_id and metadata.account
	Kind       string // metadata.kind
	Item       string // the catalogue lookup key; "" for setup
	Price      string // the Stripe Price ID; "" for setup
	SuccessURL string
	CancelURL  string
}

type CheckoutSession struct {
	ID                string
	URL               string
	Mode              string
	Status            string // open, complete, expired
	PaymentStatus     string // paid, unpaid, no_payment_required
	Customer          string
	ClientReferenceID string
	Subscription      string
	PaymentIntent     string
	Metadata          map[string]string
	Created           time.Time
}

type PaymentMethod struct {
	ID          string
	Customer    string
	Type        string
	Fingerprint string // card.fingerprint
	Funding     string // card.funding: credit, debit, prepaid, unknown
	Wallet      string // card.wallet.type, "" for none
	Brand       string
	Last4       string
	ExpMonth    int
	ExpYear     int
	Created     time.Time
}

type Subscription struct {
	ID                  string
	Customer            string
	Status              string
	PriceLookupKey      string
	Created             time.Time
	CurrentPeriodStart  time.Time
	CurrentPeriodEnd    time.Time
	CancelAt            *time.Time
	LatestInvoice       string
	LatestInvoiceStatus string
	Metadata            map[string]string
}

type RechargeParams struct {
	Customer       string
	PaymentMethod  string
	Account        string
	Item           string
	AmountCents    int64
	Currency       string
	IdempotencyKey string // recharge-<ownerHash>-<yyyy-mm>-<seq>
}

type PaymentIntent struct {
	ID          string
	Customer    string
	Status      string // succeeded, requires_payment_method, canceled, ...
	DeclineCode string
	AmountCents int64
	Metadata    map[string]string
	Created     time.Time
}
