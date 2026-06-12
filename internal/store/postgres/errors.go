package postgres

import (
	"github.com/jackc/pgx/v5"

	"github.com/chadit/CairnOS/internal/store"
)

// Sentinel errors for the postgres store package.
var (
	// errNoRows is pgx's "query returned no rows", aliased so the rest of
	// the package classifies a missing row without importing pgx everywhere.
	errNoRows = pgx.ErrNoRows

	// ErrRunNotFound aliases the interface-level sentinel so callers match
	// on store.ErrRunNotFound regardless of which backend answered,
	// mirroring the sqlite backend's alias.
	ErrRunNotFound = store.ErrRunNotFound
)
