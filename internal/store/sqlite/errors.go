package sqlite

import (
	"errors"
	"fmt"

	sqlitedrv "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/chadit/CairnOS/internal/store"
)

// Sentinel errors for the sqlite store package.
var (
	// ErrRunNotFound aliases the interface-level sentinel so existing callers
	// keep working while backend-agnostic code (the API server) matches on
	// store.ErrRunNotFound without importing this backend.
	ErrRunNotFound = store.ErrRunNotFound
)

// constraintSentinel maps a driver constraint violation onto the sentinel the
// store interface declares, so callers classify a conflict without importing
// modernc's error type or knowing sqlite result codes. It returns nil for
// anything else, leaving the caller to wrap the raw error.
func constraintSentinel(err error) error {
	sqliteErr, ok := errors.AsType[*sqlitedrv.Error](err)
	if !ok {
		return nil
	}

	switch sqliteErr.Code() {
	case sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY, sqlite3.SQLITE_CONSTRAINT_UNIQUE:
		return store.ErrRunExists
	case sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY:
		return store.ErrRunNotFound
	default:
		return nil
	}
}

// duplicateRunErr renders a CreateRun failure, folding in store.ErrRunExists
// when sqlite reports a key collision so the caller can tell a taken id from a
// broken database.
func duplicateRunErr(id string, err error) error {
	if errors.Is(constraintSentinel(err), store.ErrRunExists) {
		return fmt.Errorf("insert run %s: %w: %w", id, store.ErrRunExists, err)
	}

	return fmt.Errorf("insert run %s: %w", id, err)
}

// missingRunErr renders an action-write failure, folding in
// store.ErrRunNotFound when sqlite reports the foreign key to runs failed, the
// signal that the action names a run nothing created.
func missingRunErr(runID, action string, err error) error {
	if errors.Is(constraintSentinel(err), store.ErrRunNotFound) {
		return fmt.Errorf("upsert action %s/%s: %w: %w", runID, action, store.ErrRunNotFound, err)
	}

	return fmt.Errorf("upsert action %s/%s: %w", runID, action, err)
}
