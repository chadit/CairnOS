package sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/store/sqlite"
	"github.com/chadit/CairnOS/internal/store/storetest"
)

// openStore opens a fresh database under t.TempDir and closes it when the
// case finishes; every case exercises schema creation on a real file. It
// returns the store.Store interface so the shared conformance suite drives
// it the same way it drives the postgres backend.
func openStore(t *testing.T) store.Store {
	t.Helper()

	db, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Errorf("close store: %v", closeErr)
		}
	})

	return db
}

// TestStoreConformance runs the one behavioral spec shared with the postgres
// backend (internal/store/storetest). Proving sqlite passes the same suite
// the postgres backend must pass is the whole point of the shared suite:
// one spec, two backends.
func TestStoreConformance(t *testing.T) {
	t.Parallel()
	storetest.RunConformance(t, openStore)
}

// TestNewIDIsSortable checks the sqlite-specific NewID helper still hands out
// the time-sortable ULIDs the rest of the system assumes; the behavioral
// round-trips live in the shared suite.
func TestNewIDIsSortable(t *testing.T) {
	t.Parallel()

	first := sqlite.NewID()
	second := sqlite.NewID()

	if first == second {
		t.Fatalf("NewID returned the same id twice: %s", first)
	}

	if first >= second {
		t.Fatalf("NewID ids not time-sortable: %s !< %s", first, second)
	}
}
