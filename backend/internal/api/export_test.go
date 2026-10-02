package api

// SetPetName replaces the generator of names for sessions created without one.
func (a *API) SetPetName(f func() string) { a.petName = f }
