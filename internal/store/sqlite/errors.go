package sqlite

import "github.com/chadit/CairnOS/internal/store"

// Sentinel errors for the sqlite store package.
var (
	// ErrRunNotFound aliases the interface-level sentinel so existing callers
	// keep working while backend-agnostic code (the API server) matches on
	// store.ErrRunNotFound without importing this backend.
	ErrRunNotFound = store.ErrRunNotFound
)
