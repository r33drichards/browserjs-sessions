package api

import "time"

// SetClock replaces the clock that ages the remembered admin flags.
func (a *API) SetClock(now func() time.Time) { a.now = now }
