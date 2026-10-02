package stripe

import (
	"fmt"

	"github.com/r33drichards/computer-use/backend/internal/billing"
)

func find(items []billing.Item, match func(billing.Item) bool) (billing.Item, bool) {
	for _, it := range items {
		if match(it) {
			return it, true
		}
	}
	return billing.Item{}, false
}

// planOf is the plan with a lookup key, enabled or not: a subscriber stays
// on a plan that is no longer sold, and its credit is still the plan's.
func planOf(c billing.Catalogue, lookupKey string) (billing.Item, bool) {
	return find(c.Plans, func(it billing.Item) bool { return it.LookupKey == lookupKey })
}

// packOf is the pack with a lookup key, enabled or not.
func packOf(c billing.Catalogue, lookupKey string) (billing.Item, bool) {
	return find(c.Packs, func(it billing.Item) bool { return it.LookupKey == lookupKey })
}

// packByKey is the pack with a catalogue key ("credit-20").
func packByKey(c billing.Catalogue, key string) (billing.Item, bool) {
	return find(c.Packs, func(it billing.Item) bool { return it.Key == key })
}

// LookupKeys is the lookup key of everything that is sold now.
func LookupKeys(c billing.Catalogue) []string {
	var keys []string
	for _, it := range append(append([]billing.Item{}, c.Plans...), c.Packs...) {
		if it.Enabled {
			keys = append(keys, it.LookupKey)
		}
	}
	return keys
}

func refusesFunding(c billing.Catalogue, funding string) bool {
	for _, f := range c.SignupCredit.RefuseFunding {
		if f == funding {
			return true
		}
	}
	return false
}

// CheckCatalogue refuses a catalogue Stripe's objects could not be made
// from: billing.ParseCatalogue checks what enforcement needs of it, this
// what is sold.
func CheckCatalogue(c billing.Catalogue) error {
	if c.Currency == "" {
		return fmt.Errorf("catalogue: currency is required")
	}
	keys := map[string]bool{}
	for _, list := range []struct {
		kind  string
		items []billing.Item
	}{{"plan", c.Plans}, {"pack", c.Packs}} {
		for _, it := range list.items {
			switch {
			case it.Key == "" || it.LookupKey == "" || it.ProductID == "":
				return fmt.Errorf("catalogue: a %s needs key, productId and lookupKey", list.kind)
			case it.Amount <= 0 || it.CreditMicros <= 0:
				return fmt.Errorf("catalogue: %s %q needs a positive amount and creditMicros", list.kind, it.Key)
			case list.kind == "pack" && it.ValidDays <= 0:
				return fmt.Errorf("catalogue: pack %q needs validDays", it.Key)
			case keys[it.LookupKey]:
				return fmt.Errorf("catalogue: lookupKey %q is used twice", it.LookupKey)
			}
			keys[it.LookupKey] = true
		}
	}
	return nil
}
