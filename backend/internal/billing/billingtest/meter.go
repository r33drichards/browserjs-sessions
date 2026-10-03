package billingtest

import (
	"sort"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/billing"
)

// MaxGap is the longest gap between two observations that is billed.
const MaxGap = 150 * time.Second

// Meter is Account.status.meter: the metering step's working state.
type Meter struct {
	Sessions        map[string]MeterSession `json:"sessions,omitempty"`
	Consumed        map[string]int64        `json:"consumed,omitempty"` // by Grant name
	Carry           Carry                   `json:"carry"`
	ChargedMicros   int64                   `json:"chargedMicros,omitempty"`
	OverdraftMicros int64                   `json:"overdraftMicros,omitempty"`
	ExhaustedAt     *time.Time              `json:"exhaustedAt,omitempty"`
	ObservedAt      time.Time               `json:"observedAt"`
}

type MeterSession struct {
	LastSeen time.Time `json:"lastSeen"`
	Awake    bool      `json:"awake"`
}

type Carry struct {
	Awake int64 `json:"awake"`
	Disk  int64 `json:"disk"`
}

// Observed is one session as a tick sees it.
type Observed struct {
	Awake      bool       `json:"awake"`
	ReadySince *time.Time `json:"readySince"`
	DiskGB     int64      `json:"diskGB"`
	// Size is the session's size: "" is small. Its awake seconds are
	// charged at that size's rate.
	Size string `json:"size,omitempty"`
}

// Charged is what one tick charged and left.
type Charged struct {
	AwakeSeconds    map[string]int64
	DiskGBSeconds   map[string]int64
	AwakeMicros     int64
	DiskMicros      int64
	BalanceMicros   int64
	Level           string
	OverdraftMicros int64
	ExhaustedAt     *time.Time
}

// live reports whether g counts at now: begun, not expired, not revoked.
func live(g billing.Grant, now time.Time) bool {
	if g.Revoked != nil || g.ValidFrom.After(now) {
		return false
	}
	return g.ExpiresAt == nil || now.Before(*g.ExpiresAt)
}

// Step is the metering step of docs/contracts/billing/metering.md, a port
// of spike/meter_ref.py: one tick of one account. It is here so that the
// fake ledger charges as the operator does; metering-vectors.json is run
// against it, as it is against the operator. state is changed in place.
func Step(state *Meter, grants []billing.Grant, observed map[string]Observed, now time.Time, rates billing.Rates) Charged {
	return StepSized(state, grants, observed, now, billing.Catalogue{Rates: rates})
}

// StepSized is Step with the awake seconds of each session charged at the
// rate of its size (cat.AwakeRate): what Metronome does with a metric and a
// rate for each size.
func StepSized(state *Meter, grants []billing.Grant, observed map[string]Observed, now time.Time, cat billing.Catalogue) Charged {
	rates := cat.Rates
	if state.Sessions == nil {
		state.Sessions = map[string]MeterSession{}
	}
	if state.Consumed == nil {
		state.Consumed = map[string]int64{}
	}
	out := Charged{AwakeSeconds: map[string]int64{}, DiskGBSeconds: map[string]int64{}}
	// awakeWorth is awake seconds x micro-dollars an hour, over the sessions.
	var awakeWorth, diskSeconds int64
	for id, o := range observed {
		prev, known := state.Sessions[id]
		gap := now.Sub(prev.LastSeen)
		seen := known && gap > 0 && gap <= MaxGap
		if seen {
			// The disk existed at both sights.
			out.DiskGBSeconds[id] = int64(gap/time.Second) * o.DiskGB
			diskSeconds += out.DiskGBSeconds[id]
		}
		if o.Awake {
			var a int64
			if seen && prev.Awake {
				a = int64(gap / time.Second)
			} else if o.ReadySince != nil {
				// First sight of this run, or the meter was away too long,
				// or the clock went backwards: only from when the pod
				// became Ready, and only if that was recent enough to have
				// been seen.
				if since := now.Sub(*o.ReadySince); since >= 0 && since <= MaxGap {
					a = int64(since / time.Second)
				}
			}
			if a != 0 {
				out.AwakeSeconds[id] = a
				awakeWorth += a * cat.AwakeRate(o.Size)
			}
		}
		state.Sessions[id] = MeterSession{LastSeen: now, Awake: o.Awake}
	}
	for id := range state.Sessions {
		if _, ok := observed[id]; !ok {
			delete(state.Sessions, id) // deleted: the time since lastSeen is never billed
		}
	}

	awake := awakeWorth + state.Carry.Awake
	disk := diskSeconds*rates.DiskMicrosPerGBHour + state.Carry.Disk
	out.AwakeMicros, state.Carry.Awake = awake/3600, awake%3600
	out.DiskMicros, state.Carry.Disk = disk/3600, disk%3600
	owed := out.AwakeMicros + out.DiskMicros
	state.ChargedMicros += owed

	var counting []billing.Grant
	for _, g := range grants {
		if live(g, now) {
			counting = append(counting, g)
		}
	}
	// Earliest expiry first (no expiry last), then earliest validFrom, then
	// name.
	sort.SliceStable(counting, func(i, j int) bool {
		a, b := counting[i], counting[j]
		switch {
		case (a.ExpiresAt == nil) != (b.ExpiresAt == nil):
			return b.ExpiresAt == nil
		case a.ExpiresAt != nil && !a.ExpiresAt.Equal(*b.ExpiresAt):
			return a.ExpiresAt.Before(*b.ExpiresAt)
		case !a.ValidFrom.Equal(b.ValidFrom):
			return a.ValidFrom.Before(b.ValidFrom)
		}
		return a.Name < b.Name
	})
	for _, g := range counting {
		if owed == 0 {
			break
		}
		if take := min(owed, g.AmountMicros-state.Consumed[g.Name]); take > 0 {
			state.Consumed[g.Name] += take
			owed -= take
		}
	}
	// What no grant covers was used but is not owed by anyone.
	state.OverdraftMicros += owed

	var allowance int64
	for _, g := range counting {
		out.BalanceMicros += g.AmountMicros - state.Consumed[g.Name]
		if g.Source == billing.SourcePlan {
			allowance += g.AmountMicros
		}
	}
	switch {
	case out.BalanceMicros <= 0:
		if state.ExhaustedAt == nil {
			at := now
			state.ExhaustedAt = &at
		}
		out.Level = billing.LevelExhausted
	// Low: at most a fifth of the plan's credit, or a dollar, whichever is
	// more.
	case out.BalanceMicros*5 <= allowance || out.BalanceMicros <= 1_000_000:
		state.ExhaustedAt = nil
		out.Level = billing.LevelLow
	default:
		state.ExhaustedAt = nil
		out.Level = billing.LevelOK
	}
	state.ObservedAt = now
	out.OverdraftMicros, out.ExhaustedAt = state.OverdraftMicros, state.ExhaustedAt
	return out
}
