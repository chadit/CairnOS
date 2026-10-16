package tenancy_test

import (
	"path/filepath"
	"testing"

	"github.com/chadit/CairnOS/internal/tenancy"
	"github.com/chadit/CairnOS/internal/tenancy/tenancytest"
)

// openSQLite opens a fresh tenancy database under t.TempDir and closes it when
// the case finishes.
func openSQLite(t *testing.T) *tenancy.SQLite {
	t.Helper()

	tenants, err := tenancy.OpenSQLite(filepath.Join(t.TempDir(), "tenancy.db"))
	if err != nil {
		t.Fatalf("open tenancy store: %v", err)
	}

	t.Cleanup(func() {
		if closeErr := tenants.Close(); closeErr != nil {
			t.Errorf("close tenancy store: %v", closeErr)
		}
	})

	return tenants
}

// TestSQLiteConformance runs the one behavioral spec shared with the postgres
// backend (internal/tenancy/tenancytest): workspace CRUD, the unique-name
// invariant, membership roles, and the isolation invariant. Proving sqlite
// passes the same suite the postgres backend must pass is the point of the
// shared suite.
func TestSQLiteConformance(t *testing.T) {
	t.Parallel()

	tenancytest.RunConformance(t, openSQLite)
}
