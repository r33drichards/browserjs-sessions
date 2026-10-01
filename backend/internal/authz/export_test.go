package authz

import "time"

// SetClock replaces the clock that ages the remembered owners.
func (o *Owners) SetClock(now func() time.Time) { o.now = now }
