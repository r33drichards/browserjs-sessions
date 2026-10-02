package billingtest

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/r33drichards/browserjs-sessions/backend/internal/billing"
)

// Metronome is billing.Metronome in maps: customers, contracts and credits
// (a used uniqueness key answers conflict), and the usage it was sent. It
// is also the observer and Metronome's burn-down: Tick observes the fake
// Sessions, runs the metering step, and draws the credits down in priority
// order, never below zero.
type Metronome struct {
	clock     billing.Clock
	sessions  *Sessions
	catalogue billing.CatalogueSource

	mu          sync.Mutex
	n           int
	unreachable bool
	calls       int
	customers   map[string]*metronomeCustomer // by ID
	keys        map[string]bool               // every uniqueness key ever used
}

type metronomeCustomer struct {
	id, name, alias string
	contracts       map[string]bool
	credits         []*billing.MetronomeCredit
	meter           Meter
	usage           map[[2]string]*billing.MetronomeUsageRow // by session and day
	free            int64                                    // used with no credit left: owed by nobody
}

func NewMetronome(clock billing.Clock, sessions *Sessions, catalogue billing.CatalogueSource) *Metronome {
	return &Metronome{clock: clock, sessions: sessions, catalogue: catalogue,
		customers: map[string]*metronomeCustomer{}, keys: map[string]bool{}}
}

var errMetronomeDown = errors.New("metronome: connection refused")

// Unreachable makes every call fail, as an unreachable Metronome does.
func (m *Metronome) Unreachable(down bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unreachable = down
}

// Calls is how many calls were made to Metronome, failed ones included.
func (m *Metronome) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// call counts a call and says whether Metronome answers.
func (m *Metronome) call() error {
	m.calls++
	if m.unreachable {
		return errMetronomeDown
	}
	return nil
}

func (m *Metronome) CreateCustomer(_ context.Context, p billing.MetronomeCustomerParams) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.call(); err != nil {
		return "", err
	}
	for _, c := range m.customers {
		if c.alias == p.IngestAlias {
			return "", errors.New("metronome: the ingest alias is taken")
		}
	}
	m.n++
	id := "mcus-" + strconv.Itoa(m.n)
	m.customers[id] = &metronomeCustomer{id: id, name: p.Name, alias: p.IngestAlias, contracts: map[string]bool{},
		usage: map[[2]string]*billing.MetronomeUsageRow{}}
	return id, nil
}

func (m *Metronome) CustomerByAlias(_ context.Context, alias string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.call(); err != nil {
		return "", false, err
	}
	for id, c := range m.customers {
		if c.alias == alias {
			return id, true, nil
		}
	}
	return "", false, nil
}

func (m *Metronome) customer(id string) (*metronomeCustomer, error) {
	c, ok := m.customers[id]
	if !ok {
		return nil, errors.New("metronome: no such customer " + id)
	}
	return c, nil
}

func (m *Metronome) CreateContract(_ context.Context, p billing.MetronomeContractParams) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.call(); err != nil {
		return err
	}
	c, err := m.customer(p.Customer)
	if err != nil {
		return err
	}
	c.contracts[p.UniquenessKey] = true // a 409 is nil
	return nil
}

func (m *Metronome) CreateCredit(_ context.Context, p billing.MetronomeCreditParams) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.call(); err != nil {
		return "", false, err
	}
	c, err := m.customer(p.Customer)
	if err != nil {
		return "", false, err
	}
	// A key stays taken, whoever used it and whatever became of the credit.
	if m.keys[p.UniquenessKey] {
		return "", true, nil
	}
	m.keys[p.UniquenessKey] = true
	m.n++
	fields := map[string]string{}
	for k, v := range p.CustomFields {
		fields[k] = v
	}
	credit := &billing.MetronomeCredit{ID: "mcred-" + strconv.Itoa(m.n), Name: p.Name, Priority: p.Priority, AmountMicros: p.AmountMicros,
		BalanceMicros: p.AmountMicros, StartingAt: p.StartingAt, EndingBefore: p.EndingBefore, CustomFields: fields}
	c.credits = append(c.credits, credit)
	return credit.ID, false, nil
}

func (m *Metronome) Credits(_ context.Context, customer string) ([]billing.MetronomeCredit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.call(); err != nil {
		return nil, err
	}
	c, err := m.customer(customer)
	if err != nil {
		return nil, err
	}
	out := make([]billing.MetronomeCredit, 0, len(c.credits))
	for _, credit := range c.credits {
		out = append(out, *credit)
	}
	return out, nil
}

func (m *Metronome) ArchiveCredit(_ context.Context, customer, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.call(); err != nil {
		return err
	}
	c, err := m.customer(customer)
	if err != nil {
		return err
	}
	for _, credit := range c.credits {
		if credit.ID == id {
			credit.Archived = true
			return nil
		}
	}
	return errors.New("metronome: no such credit " + id)
}

func (m *Metronome) Usage(_ context.Context, customer string, from, to time.Time) (billing.MetronomeUsage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.call(); err != nil {
		return billing.MetronomeUsage{}, err
	}
	c, err := m.customer(customer)
	if err != nil {
		return billing.MetronomeUsage{}, err
	}
	var out billing.MetronomeUsage
	for _, row := range c.usage {
		day, _ := time.Parse("2006-01-02", row.Day)
		if day.Before(from.UTC().Truncate(24*time.Hour)) || !day.Before(to) {
			continue
		}
		out.Rows = append(out.Rows, *row)
	}
	sort.Slice(out.Rows, func(i, j int) bool {
		if out.Rows[i].Day != out.Rows[j].Day {
			return out.Rows[i].Day < out.Rows[j].Day
		}
		return out.Rows[i].SessionID < out.Rows[j].SessionID
	})
	return out, nil
}

// balance is what is left of the credits that count at now.
func (c *metronomeCustomer) balance(now time.Time) (left int64) {
	for _, credit := range c.credits {
		if !credit.Archived && !credit.StartingAt.After(now) && now.Before(credit.EndingBefore) {
			left += credit.BalanceMicros
		}
	}
	return left
}

// AllCredits is every credit ever made, of every customer, by key.
func (m *Metronome) AllCredits() []billing.MetronomeCredit {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []billing.MetronomeCredit
	for _, c := range m.customers {
		for _, credit := range c.credits {
			out = append(out, *credit)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CustomFields[billing.FieldGrantKey] < out[j].CustomFields[billing.FieldGrantKey]
	})
	return out
}

// Charged is everything the account's usage ever came to, and Free the
// part of it there was no credit for (owed by nobody).
func (m *Metronome) Charged(account string) (charged, free int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.customers {
		if c.alias == account {
			return c.meter.ChargedMicros, c.free
		}
	}
	return 0, 0
}

// MetronomeEvent is a notification as Metronome sends it.
type MetronomeEvent struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Properties struct {
		CustomerID       string `json:"customer_id"`
		RemainingBalance int64  `json:"remaining_balance"`
	} `json:"properties"`
}

// AlertType is the alert at a zero balance.
const AlertType = "alerts.low_remaining_contract_credit_and_commit_balance_reached"

func (e MetronomeEvent) Body() []byte {
	raw, err := json.Marshal(e)
	if err != nil {
		panic(err)
	}
	return raw
}

// Alert is the zero-balance alert for a customer, whatever its balance is:
// a replay, or a stale event.
func (m *Metronome) Alert(customer string) MetronomeEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.alert(customer)
}

func (m *Metronome) alert(customer string) MetronomeEvent {
	m.n++
	e := MetronomeEvent{ID: "mevt-" + strconv.Itoa(m.n), Type: AlertType}
	e.Properties.CustomerID = customer
	return e
}

// Tick is the observer's look at now and Metronome's burn-down of what it
// saw: for every customer, the metering step over its account's sessions
// as they are, the usage recorded, and the money drawn from its credits in
// priority order. It returns the alert Metronome would send for each
// customer whose balance reached zero in this tick. It is not a call to
// Metronome, and works while Metronome is unreachable to the backend.
func (m *Metronome) Tick(now time.Time) []MetronomeEvent {
	cat := m.catalogue.Catalogue()
	observedBy := m.sessions.observedByAccount(int64(cat.SessionDiskGB))
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.customers))
	for id := range m.customers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var alerts []MetronomeEvent
	for _, id := range ids {
		c := m.customers[id]
		// The alert is for usage taking the balance to zero; a credit
		// that ends unused sends none (the balance pass notices it).
		before := c.balance(now)
		observed := observedBy[c.alias]
		if observed == nil {
			observed = map[string]Observed{}
		}
		// The step's seconds and money; the credits are Metronome's to draw.
		charged := Step(&c.meter, nil, observed, now, cat.Rates)
		day := now.UTC().Format("2006-01-02")
		for session := range observed {
			key := [2]string{session, day}
			if c.usage[key] == nil {
				c.usage[key] = &billing.MetronomeUsageRow{SessionID: session, Day: day}
			}
			c.usage[key].AwakeSeconds += charged.AwakeSeconds[session]
			c.usage[key].DiskGBSeconds += charged.DiskGBSeconds[session]
		}
		owed := charged.AwakeMicros + charged.DiskMicros
		live := make([]*billing.MetronomeCredit, 0, len(c.credits))
		for _, credit := range c.credits {
			if !credit.Archived && !credit.StartingAt.After(now) && now.Before(credit.EndingBefore) {
				live = append(live, credit)
			}
		}
		sort.SliceStable(live, func(i, j int) bool {
			if live[i].Priority != live[j].Priority {
				return live[i].Priority < live[j].Priority
			}
			return live[i].EndingBefore.Before(live[j].EndingBefore)
		})
		for _, credit := range live {
			take := min(owed, credit.BalanceMicros)
			credit.BalanceMicros -= take
			owed -= take
		}
		// With no credit left the list rate is zero: nothing is owed.
		c.free += owed
		if before > 0 && c.balance(now) == 0 {
			alerts = append(alerts, m.alert(id))
		}
	}
	return alerts
}

// MetronomeSecret is the made-up secret tests sign Metronome's
// notifications with.
const MetronomeSecret = "metronome_test_secret"

// SignMetronome is the Metronome-Webhook-Signature header for body sent at
// date (the X-Metronome-Date header): an HMAC-SHA256 of date, a newline
// and the body, in hex.
func SignMetronome(secret, date string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(date + "\n"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
