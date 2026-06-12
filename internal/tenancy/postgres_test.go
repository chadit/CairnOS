package tenancy_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/chadit/CairnOS/internal/tenancy"
	"github.com/chadit/CairnOS/internal/tenancy/tenancytest"
)

// pgDSNEnv names the environment variable pointing the tenancy integration
// tests at a live Postgres. Unset means skip, so the package stays green on a
// machine with no Postgres (the same env-gate the store and auth backends use).
const pgDSNEnv = "CAIRNOS_TEST_POSTGRES_DSN"

// requirePostgresDSN returns the configured DSN or skips the test.
func requirePostgresDSN(t *testing.T) string {
	t.Helper()

	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("%s not set; skipping postgres tenancy tests", pgDSNEnv)
	}

	return dsn
}

// openPostgres opens a tenancy store in its own Postgres schema so each subtest
// gets the isolation the sqlite backend gets from a per-case temp file. The
// schema name is a fresh lowercased ULID; cleanup drops it.
func openPostgres(t *testing.T) tenancy.Store {
	t.Helper()

	dsn := requirePostgresDSN(t)
	ctx := t.Context()

	schema := "cairnos_tenancy_test_" + strings.ToLower(ulid.Make().String())

	tenants, err := tenancy.OpenPostgresSchema(ctx, dsn, schema)
	if err != nil {
		t.Fatalf("open postgres tenancy store: %v", err)
	}

	t.Cleanup(func() {
		dropCtx := context.WithoutCancel(ctx)

		if dropErr := tenancy.DropPostgresSchema(dropCtx, tenants, schema); dropErr != nil {
			t.Errorf("drop test schema: %v", dropErr)
		}

		if closeErr := tenants.Close(); closeErr != nil {
			t.Errorf("close postgres tenancy store: %v", closeErr)
		}
	})

	return tenants
}

// TestPostgresConformance runs the one behavioral spec shared with the sqlite
// backend against a live Postgres, proving the two tenancy backends agree. It
// skips without a DSN so CI without Postgres stays green.
func TestPostgresConformance(t *testing.T) {
	t.Parallel()

	requirePostgresDSN(t)

	tenancytest.RunConformance(t, openPostgres)
}

// TestOpenPostgresRejectsBadDSN proves OpenPostgres surfaces a connection
// failure instead of a half-built store, with no Postgres required.
func TestOpenPostgresRejectsBadDSN(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := tenancy.OpenPostgres(ctx, "postgres://user:pass@127.0.0.1:1/nope"); err == nil {
		t.Fatal("OpenPostgres with a canceled context and dead DSN returned no error")
	}
}
