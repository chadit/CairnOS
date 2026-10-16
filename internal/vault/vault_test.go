package vault_test

import (
	"path/filepath"
	"testing"

	"github.com/chadit/CairnOS/internal/vault"
	"github.com/chadit/CairnOS/internal/vault/vaulttest"
)

// openVault opens a fresh database under t.TempDir and closes it when the case
// finishes; every case exercises schema and index creation.
func openVault(t *testing.T) *vault.SQLite {
	t.Helper()

	store, err := vault.OpenSQLite(filepath.Join(t.TempDir(), "vault.db"))
	if err != nil {
		t.Fatalf("open vault store: %v", err)
	}

	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Errorf("close vault store: %v", closeErr)
		}
	})

	return store
}

// TestVaultConformance runs the one behavioral spec shared with the postgres
// backend (internal/vault/vaulttest). Proving sqlite passes the same suite the
// postgres backend must pass is the whole point: one spec, two backends.
func TestVaultConformance(t *testing.T) {
	t.Parallel()
	vaulttest.RunConformance(t, openVault)
}
