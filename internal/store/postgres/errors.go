package postgres

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/chadit/CairnOS/internal/store"
)

// SQLSTATE classes the schema can raise, spelled out because pgconn reports
// them as bare strings.
const (
	sqlStateUniqueViolation     = "23505"
	sqlStateForeignKeyViolation = "23503"
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

// constraintSentinel maps a SQLSTATE constraint violation onto the sentinel
// the store interface declares, so callers classify a conflict without
// importing pgconn. It returns nil for anything else, leaving the caller to
// wrap the raw error.
func constraintSentinel(err error) error {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok {
		return nil
	}

	switch pgErr.Code {
	case sqlStateUniqueViolation:
		return store.ErrRunExists
	case sqlStateForeignKeyViolation:
		return store.ErrRunNotFound
	default:
		return nil
	}
}

// duplicateRunErr renders a CreateRun failure, folding in store.ErrRunExists
// when Postgres reports a key collision so the caller can tell a taken id from
// a broken database. It mirrors the sqlite backend so both answer alike.
func duplicateRunErr(id string, err error) error {
	if errors.Is(constraintSentinel(err), store.ErrRunExists) {
		return fmt.Errorf("insert run %s: %w: %w", id, store.ErrRunExists, err)
	}

	return fmt.Errorf("insert run %s: %w", id, err)
}

// missingRunErr renders an action-write failure, folding in
// store.ErrRunNotFound when Postgres reports the foreign key to runs failed,
// the signal that the action names a run nothing created.
func missingRunErr(runID, action string, err error) error {
	if errors.Is(constraintSentinel(err), store.ErrRunNotFound) {
		return fmt.Errorf("upsert action %s/%s: %w: %w", runID, action, store.ErrRunNotFound, err)
	}

	return fmt.Errorf("upsert action %s/%s: %w", runID, action, err)
}
