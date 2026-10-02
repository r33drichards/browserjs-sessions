package billingtest

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/billing"
)

const contracts = "../../../../docs/contracts/billing/"

// The rates the vectors were written for (spike/meter_ref.py).
var vectorRates = billing.Rates{AwakeMicrosPerHour: 200_000, DiskMicrosPerGBHour: 384}

type vector struct {
	Name   string `json:"name"`
	Grants []struct {
		Name         string     `json:"name"`
		Source       string     `json:"source"`
		AmountMicros int64      `json:"amountMicros"`
		ValidFrom    time.Time  `json:"validFrom"`
		ExpiresAt    *time.Time `json:"expiresAt"`
		Revoked      bool       `json:"revoked"`
	} `json:"grants"`
	State *Meter `json:"state"`
	Ticks []struct {
		Now      time.Time           `json:"now"`
		Observed map[string]Observed `json:"observed"`
	} `json:"ticks"`
	Expect []struct {
		AwakeSeconds    map[string]int64 `json:"awakeSeconds"`
		DiskGBSeconds   map[string]int64 `json:"diskGBSeconds"`
		AwakeMicros     int64            `json:"awakeMicros"`
		DiskMicros      int64            `json:"diskMicros"`
		BalanceMicros   int64            `json:"balanceMicros"`
		Level           string           `json:"level"`
		OverdraftMicros int64            `json:"overdraftMicros"`
		ExhaustedAt     *time.Time       `json:"exhaustedAt"`
	} `json:"expect"`
}

// The Go port of the step answers every vector as the reference does, so
// the fake ledger and the operator cannot drift.
func TestStepRunsTheMeteringVectors(t *testing.T) {
	raw, err := os.ReadFile(contracts + "metering-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []vector
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 18 {
		t.Fatalf("%d vectors, want the contract's 18", len(vectors))
	}
	for _, v := range vectors {
		t.Run(v.Name, func(t *testing.T) {
			var grants []billing.Grant
			for _, g := range v.Grants {
				grant := billing.Grant{Name: g.Name, Source: g.Source, AmountMicros: g.AmountMicros, ValidFrom: g.ValidFrom, ExpiresAt: g.ExpiresAt}
				if g.Revoked {
					grant.Revoked = &billing.Revoked{Reason: "refund"}
				}
				grants = append(grants, grant)
			}
			state := &Meter{}
			if v.State != nil {
				state = v.State
			}
			if len(v.Ticks) != len(v.Expect) {
				t.Fatalf("%d ticks, %d expectations", len(v.Ticks), len(v.Expect))
			}
			for i, tick := range v.Ticks {
				got, want := Step(state, grants, tick.Observed, tick.Now, vectorRates), v.Expect[i]
				if !reflect.DeepEqual(got.AwakeSeconds, want.AwakeSeconds) || !reflect.DeepEqual(got.DiskGBSeconds, want.DiskGBSeconds) {
					t.Errorf("tick %d: seconds awake %v disk %v, want %v and %v", i, got.AwakeSeconds, got.DiskGBSeconds, want.AwakeSeconds, want.DiskGBSeconds)
				}
				if got.AwakeMicros != want.AwakeMicros || got.DiskMicros != want.DiskMicros {
					t.Errorf("tick %d: charged awake %d disk %d, want %d and %d", i, got.AwakeMicros, got.DiskMicros, want.AwakeMicros, want.DiskMicros)
				}
				if got.BalanceMicros != want.BalanceMicros || got.Level != want.Level || got.OverdraftMicros != want.OverdraftMicros {
					t.Errorf("tick %d: balance %d level %s overdraft %d, want %d %s %d", i,
						got.BalanceMicros, got.Level, got.OverdraftMicros, want.BalanceMicros, want.Level, want.OverdraftMicros)
				}
				if (got.ExhaustedAt == nil) != (want.ExhaustedAt == nil) || (got.ExhaustedAt != nil && !got.ExhaustedAt.Equal(*want.ExhaustedAt)) {
					t.Errorf("tick %d: exhaustedAt %v, want %v", i, got.ExhaustedAt, want.ExhaustedAt)
				}
			}
		})
	}
}
