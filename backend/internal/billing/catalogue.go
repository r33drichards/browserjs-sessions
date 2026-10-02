package billing

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"sigs.k8s.io/yaml"
)

// Catalogue is catalogue.yaml: what is sold, the two rates, and what each
// option gives. Data, not code: the file is a ConfigMap, re-read when it
// changes.
type Catalogue struct {
	Version       int           `json:"version"`
	Currency      string        `json:"currency"`
	Rates         Rates         `json:"rates"`
	SessionDiskGB int           `json:"sessionDiskGB"`
	SignupCredit  SignupOffer   `json:"signupCredit"`
	Payg          Tier          `json:"payg"`
	Plans         []Item        `json:"plans"`
	Packs         []Item        `json:"packs"`
	AutoRecharge  RechargeOffer `json:"autoRecharge"`
}

type Rates struct {
	AwakeMicrosPerHour  int64 `json:"awakeMicrosPerHour"`
	DiskMicrosPerGBHour int64 `json:"diskMicrosPerGBHour"`
}

type SignupOffer struct {
	AmountMicros  int64    `json:"amountMicros"`
	ValidDays     int      `json:"validDays"`
	RefuseFunding []string `json:"refuseFunding,omitempty"`
	RefuseWallets bool     `json:"refuseWallets,omitempty"`
}

// Tier is the limits an account has: pay as you go's, or its plan's.
type Tier struct {
	Name        string `json:"name,omitempty"`
	MaxSessions int    `json:"maxSessions"`
	MaxAwake    int    `json:"maxAwake"`
}

// Item is a plan or a pack.
type Item struct {
	Key          string `json:"key"`
	Name         string `json:"name"`
	ProductID    string `json:"productId,omitempty"`
	LookupKey    string `json:"lookupKey"`
	Amount       int64  `json:"amount"` // US cents
	CreditMicros int64  `json:"creditMicros"`
	MaxSessions  int    `json:"maxSessions,omitempty"`
	MaxAwake     int    `json:"maxAwake,omitempty"`
	ValidDays    int    `json:"validDays,omitempty"`
	Enabled      bool   `json:"enabled"`
}

type RechargeOffer struct {
	DefaultThresholdMicros int64  `json:"defaultThresholdMicros"`
	DefaultPack            string `json:"defaultPack"`
	DefaultMonthlyCapCents int64  `json:"defaultMonthlyCapCents"`
	MaxMonthlyCapCents     int64  `json:"maxMonthlyCapCents"`
}

// PlanPayg is the plan of an account with no subscription.
const PlanPayg = "payg"

// ParseCatalogue reads a catalogue and refuses one that could not be
// enforced from: no rates, no limits.
func ParseCatalogue(data []byte) (Catalogue, error) {
	var c Catalogue
	if err := yaml.UnmarshalStrict(data, &c); err != nil {
		return Catalogue{}, fmt.Errorf("catalogue: %w", err)
	}
	switch {
	case c.Rates.AwakeMicrosPerHour <= 0 || c.Rates.DiskMicrosPerGBHour < 0:
		return Catalogue{}, errors.New("catalogue: rates.awakeMicrosPerHour must be positive and rates.diskMicrosPerGBHour not negative")
	case c.SessionDiskGB <= 0:
		return Catalogue{}, errors.New("catalogue: sessionDiskGB must be positive")
	case c.Payg.MaxSessions <= 0 || c.Payg.MaxAwake <= 0:
		return Catalogue{}, errors.New("catalogue: payg.maxSessions and payg.maxAwake must be positive")
	}
	for _, p := range c.Plans {
		if p.Key == "" || p.Key == PlanPayg || p.MaxSessions <= 0 || p.MaxAwake <= 0 {
			return Catalogue{}, fmt.Errorf("catalogue: plan %q needs a key, maxSessions and maxAwake", p.Key)
		}
	}
	return c, nil
}

// Tier is the limits of the plan with key (status.plan). A plan the
// catalogue no longer has, like no plan, has pay as you go's.
func (c Catalogue) Tier(plan string) Tier {
	if p, ok := c.Plan(plan); ok {
		return Tier{Name: p.Name, MaxSessions: p.MaxSessions, MaxAwake: p.MaxAwake}
	}
	return c.Payg
}

// Plan finds a plan by its key, enabled or not: a subscriber stays on a plan
// that is no longer sold.
func (c Catalogue) Plan(key string) (Item, bool) {
	for _, p := range c.Plans {
		if p.Key == key {
			return p, true
		}
	}
	return Item{}, false
}

// Pack finds a pack by its key or its lookup key.
func (c Catalogue) Pack(key string) (Item, bool) {
	for _, p := range c.Packs {
		if p.Key == key || p.LookupKey == key {
			return p, true
		}
	}
	return Item{}, false
}

// CatalogueSource is where the current catalogue is had from.
type CatalogueSource interface{ Catalogue() Catalogue }

// Catalogue is its own source, for one that never changes (tests).
func (c Catalogue) Catalogue() Catalogue { return c }

// CatalogueFile is the catalogue in a file, re-read when the file changes.
// A file that cannot be read or does not parse keeps the last good one.
type CatalogueFile struct {
	path string

	mu      sync.Mutex
	current Catalogue
	checked time.Time
	stamp   time.Time
	size    int64
}

// How often the file is looked at.
const catalogueRecheck = 10 * time.Second

// LoadCatalogue reads the catalogue at path, which must be good.
func LoadCatalogue(path string) (*CatalogueFile, error) {
	f := &CatalogueFile{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("catalogue: %w", err)
	}
	if f.current, err = ParseCatalogue(data); err != nil {
		return nil, err
	}
	if info, err := os.Stat(path); err == nil {
		f.stamp, f.size = info.ModTime(), info.Size()
	}
	f.checked = time.Now()
	return f, nil
}

func (f *CatalogueFile) Catalogue() Catalogue {
	f.mu.Lock()
	defer f.mu.Unlock()
	if time.Since(f.checked) < catalogueRecheck {
		return f.current
	}
	f.checked = time.Now()
	// A ConfigMap's file is a symlink that is swapped: Stat follows it.
	info, err := os.Stat(f.path)
	if err != nil || (info.ModTime().Equal(f.stamp) && info.Size() == f.size) {
		return f.current
	}
	f.stamp, f.size = info.ModTime(), info.Size()
	data, err := os.ReadFile(f.path)
	if err == nil {
		var next Catalogue
		if next, err = ParseCatalogue(data); err == nil {
			f.current = next
			slog.Info("billing catalogue re-read", "path", f.path)
			return f.current
		}
	}
	slog.Error("billing catalogue not re-read; keeping the last good one", "path", f.path, "err", err)
	return f.current
}

// PublicCatalogue is the catalogue as a caller may see it
// (backend-api.yaml, Catalogue): enabled items only, no product IDs.
type PublicCatalogue struct {
	Currency     string       `json:"currency"`
	Rates        PublicRates  `json:"rates"`
	SignupCredit PublicSignup `json:"signupCredit"`
	Payg         PublicTier   `json:"payg"`
	Plans        []PublicItem `json:"plans"`
	Packs        []PublicItem `json:"packs"`
}

type PublicRates struct {
	AwakeMicrosPerHour  int64 `json:"awakeMicrosPerHour"`
	DiskMicrosPerGBHour int64 `json:"diskMicrosPerGBHour"`
	SessionDiskGB       int   `json:"sessionDiskGB"`
}

type PublicSignup struct {
	AmountMicros int64 `json:"amountMicros"`
	ValidDays    int   `json:"validDays"`
}

type PublicTier struct {
	MaxSessions int `json:"maxSessions"`
	MaxAwake    int `json:"maxAwake"`
}

type PublicItem struct {
	Key          string `json:"key"`
	Name         string `json:"name"`
	LookupKey    string `json:"lookupKey"`
	Amount       int64  `json:"amount"`
	CreditMicros int64  `json:"creditMicros"`
	MaxSessions  int    `json:"maxSessions,omitempty"`
	MaxAwake     int    `json:"maxAwake,omitempty"`
	ValidDays    int    `json:"validDays,omitempty"`
}

func (c Catalogue) publicRates() PublicRates {
	return PublicRates{c.Rates.AwakeMicrosPerHour, c.Rates.DiskMicrosPerGBHour, c.SessionDiskGB}
}

// Public is the catalogue for GET /api/billing/catalogue.
func (c Catalogue) Public() PublicCatalogue {
	items := func(all []Item) []PublicItem {
		out := []PublicItem{}
		for _, i := range all {
			if i.Enabled {
				out = append(out, PublicItem{i.Key, i.Name, i.LookupKey, i.Amount, i.CreditMicros, i.MaxSessions, i.MaxAwake, i.ValidDays})
			}
		}
		return out
	}
	return PublicCatalogue{
		Currency:     c.Currency,
		Rates:        c.publicRates(),
		SignupCredit: PublicSignup{c.SignupCredit.AmountMicros, c.SignupCredit.ValidDays},
		Payg:         PublicTier{c.Payg.MaxSessions, c.Payg.MaxAwake},
		Plans:        items(c.Plans),
		Packs:        items(c.Packs),
	}
}
