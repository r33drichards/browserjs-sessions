package billingtest

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/r33drichards/computer-use/backend/internal/billing"
	"github.com/r33drichards/computer-use/backend/internal/sessions"
)

// Accounts is billing.Accounts in a map.
type Accounts struct {
	clock billing.Clock

	mu     sync.Mutex
	byName map[string]billing.Account
	made   int
	reads  int
}

func NewAccounts(clock billing.Clock) *Accounts {
	return &Accounts{clock: clock, byName: map[string]billing.Account{}}
}

// clone copies a spec deeply, so that nothing a caller holds is the map's.
func clone(spec billing.AccountSpec) billing.AccountSpec {
	raw, err := json.Marshal(spec)
	if err != nil {
		panic(err)
	}
	var out billing.AccountSpec
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return out
}

func copyOf(a billing.Account) billing.Account {
	a.Spec = clone(a.Spec)
	return a
}

func (a *Accounts) Ensure(_ context.Context, owner string) (billing.Account, error) {
	owner = strings.ToLower(strings.TrimSpace(owner))
	a.mu.Lock()
	defer a.mu.Unlock()
	name := billing.AccountName(owner)
	acc, ok := a.byName[name]
	if !ok {
		acc = billing.Account{Name: name, Created: a.clock.Now(),
			Spec: billing.AccountSpec{Owner: owner, OwnerHash: sessions.OwnerLabel(owner)}}
		a.byName[name] = acc
		a.made++
	}
	return copyOf(acc), nil
}

func (a *Accounts) Get(_ context.Context, name string) (billing.Account, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reads++
	acc, ok := a.byName[name]
	if !ok {
		return billing.Account{}, billing.ErrNotFound
	}
	return copyOf(acc), nil
}

// Reads is how many times an Account was read by name.
func (a *Accounts) Reads() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reads
}

func (a *Accounts) find(match func(billing.AccountSpec) bool) (billing.Account, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, acc := range a.byName {
		if match(acc.Spec) {
			return copyOf(acc), nil
		}
	}
	return billing.Account{}, billing.ErrNotFound
}

func (a *Accounts) ByCustomer(_ context.Context, customer string) (billing.Account, error) {
	return a.find(func(s billing.AccountSpec) bool { return customer != "" && s.StripeCustomerID == customer })
}

func (a *Accounts) ByMetronomeCustomer(_ context.Context, id string) (billing.Account, error) {
	return a.find(func(s billing.AccountSpec) bool { return id != "" && s.MetronomeCustomerID == id })
}

func (a *Accounts) ByPaymentMethod(_ context.Context, id string) (billing.Account, error) {
	return a.find(func(s billing.AccountSpec) bool {
		if s.PaymentMethod == nil {
			return false
		}
		for _, have := range s.PaymentMethod.IDs {
			if have == id {
				return true
			}
		}
		return false
	})
}

func (a *Accounts) WithCustomer(context.Context) ([]billing.Account, error) {
	var out []billing.Account
	for _, acc := range a.All() {
		if acc.Spec.StripeCustomerID != "" {
			out = append(out, acc)
		}
	}
	return out, nil
}

func (a *Accounts) Update(_ context.Context, name string, change func(*billing.AccountSpec) error) (billing.Account, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	acc, ok := a.byName[name]
	if !ok {
		return billing.Account{}, billing.ErrNotFound
	}
	spec := clone(acc.Spec)
	if err := change(&spec); err != nil {
		return billing.Account{}, err
	}
	acc.Spec = spec
	a.byName[name] = acc
	return copyOf(acc), nil
}

// All is every Account, by name.
func (a *Accounts) All() []billing.Account {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]billing.Account, 0, len(a.byName))
	for _, acc := range a.byName {
		out = append(out, copyOf(acc))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Made is how many Accounts were ever made.
func (a *Accounts) Made() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.made
}
