package auth_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/auth/authtest"
)

// pgDSNEnv names the environment variable pointing the auth integration tests
// at a live Postgres. Unset means skip, so the package stays green on a
// machine with no Postgres (the same env-gate the store backend uses).
const pgDSNEnv = "CAIRNOS_TEST_POSTGRES_DSN"

// requirePostgresDSN returns the configured DSN or skips the test.
func requirePostgresDSN(t *testing.T) string {
	t.Helper()

	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("%s not set; skipping postgres auth tests", pgDSNEnv)
	}

	return dsn
}

// openPostgresAuth opens an auth store in its own Postgres schema so each
// subtest gets the isolation the sqlite backend gets from a per-case temp
// file. The schema name is a fresh lowercased ULID; cleanup drops it.
func openPostgresAuth(t *testing.T) auth.Store {
	t.Helper()

	dsn := requirePostgresDSN(t)
	ctx := t.Context()

	schema := "cairnos_auth_test_" + strings.ToLower(ulid.Make().String())

	store, err := auth.OpenPostgresSchema(ctx, dsn, schema)
	if err != nil {
		t.Fatalf("open postgres auth store: %v", err)
	}

	t.Cleanup(func() {
		dropCtx := context.WithoutCancel(ctx)

		if dropErr := auth.DropPostgresSchema(dropCtx, store, schema); dropErr != nil {
			t.Errorf("drop test schema: %v", dropErr)
		}

		if closeErr := store.Close(); closeErr != nil {
			t.Errorf("close postgres auth store: %v", closeErr)
		}
	})

	return store
}

// TestPostgresStoreConformance runs the one behavioral spec shared with the
// sqlite backend against a live Postgres, proving the two auth backends agree
// on key CRUD and denylist behavior. It skips without a DSN so CI without
// Postgres stays green.
func TestPostgresStoreConformance(t *testing.T) {
	t.Parallel()

	requirePostgresDSN(t)

	authtest.RunConformance(t, openPostgresAuth)
}

// TestOpenPostgresRejectsBadDSN proves OpenPostgres surfaces a connection
// failure instead of a half-built store, with no Postgres required.
func TestOpenPostgresRejectsBadDSN(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := auth.OpenPostgres(ctx, "postgres://user:pass@127.0.0.1:1/nope"); err == nil {
		t.Fatal("OpenPostgres with a canceled context and dead DSN returned no error")
	}
}
