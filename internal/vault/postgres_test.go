package vault_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/chadit/CairnOS/internal/vault"
	"github.com/chadit/CairnOS/internal/vault/vaulttest"
)

// pgDSNEnv names the environment variable pointing the vault integration tests
// at a live Postgres. Unset means skip, so the package stays green on a
// machine with no Postgres (the same env-gate the other stores use).
const pgDSNEnv = "CAIRNOS_TEST_POSTGRES_DSN"

// requireDSN returns the configured DSN or skips the test.
func requireDSN(t *testing.T) string {
	t.Helper()

	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("%s not set; skipping postgres vault tests", pgDSNEnv)
	}

	return dsn
}

// openPostgresVault opens a vault store in its own Postgres schema so each
// subtest gets the isolation the sqlite backend gets from a per-case temp
// file. The schema name is a fresh lowercased ULID, a valid identifier that
// never collides across parallel subtests; cleanup drops it.
func openPostgresVault(t *testing.T) vault.Store {
	t.Helper()

	dsn := requireDSN(t)
	ctx := t.Context()

	schema := "cairnos_vault_test_" + strings.ToLower(ulid.Make().String())

	store, err := vault.OpenPostgresSchema(ctx, dsn, schema)
	if err != nil {
		t.Fatalf("open postgres vault store: %v", err)
	}

	t.Cleanup(func() {
		dropCtx := context.WithoutCancel(ctx)

		if dropErr := vault.DropPostgresSchema(dropCtx, store, schema); dropErr != nil {
			t.Errorf("drop test schema: %v", dropErr)
		}

		if closeErr := store.Close(); closeErr != nil {
			t.Errorf("close postgres vault store: %v", closeErr)
		}
	})

	return store
}

// TestPostgresVaultConformance runs the one behavioral spec shared with the
// sqlite backend against a live Postgres, proving the two vault backends agree
// on graph traversal, backlink derivation, keyword search, and the semantic
// fallback flag. It skips without a DSN so CI without Postgres stays green.
func TestPostgresVaultConformance(t *testing.T) {
	t.Parallel()

	requireDSN(t)

	vaulttest.RunConformance(t, openPostgresVault)
}

// TestOpenPostgresRejectsBadDSN proves OpenPostgres surfaces a connection
// failure instead of a half-built store, with no Postgres required.
func TestOpenPostgresRejectsBadDSN(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := vault.OpenPostgres(ctx, "postgres://user:pass@127.0.0.1:1/nope"); err == nil {
		t.Fatal("OpenPostgres with a canceled context and dead DSN returned no error")
	}
}
