package memory_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/memory/memorytest"
)

// pgDSNEnv names the environment variable pointing the memory integration
// tests at a live Postgres. Unset means skip, so the package stays green on a
// machine with no Postgres (the same env-gate the store backend uses).
const pgDSNEnv = "CAIRNOS_TEST_POSTGRES_DSN"

// requireDSN returns the configured DSN or skips the test.
func requireDSN(t *testing.T) string {
	t.Helper()

	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("%s not set; skipping postgres memory tests", pgDSNEnv)
	}

	return dsn
}

// openPostgresMemory opens a memory store in its own Postgres schema so each
// subtest gets the isolation the sqlite backend gets from a per-case temp
// file. The schema name is a fresh lowercased ULID, a valid identifier that
// never collides across parallel subtests; cleanup drops it.
func openPostgresMemory(t *testing.T) memory.Store {
	t.Helper()

	dsn := requireDSN(t)
	ctx := t.Context()

	schema := "cairnos_mem_test_" + strings.ToLower(ulid.Make().String())

	mem, err := memory.OpenPostgresSchema(ctx, dsn, schema)
	if err != nil {
		t.Fatalf("open postgres memory store: %v", err)
	}

	t.Cleanup(func() {
		dropCtx := context.WithoutCancel(ctx)

		if dropErr := memory.DropPostgresSchema(dropCtx, mem, schema); dropErr != nil {
			t.Errorf("drop test schema: %v", dropErr)
		}

		if closeErr := mem.Close(); closeErr != nil {
			t.Errorf("close postgres memory store: %v", closeErr)
		}
	})

	return mem
}

// TestPostgresMemoryConformance runs the one behavioral spec shared with the
// sqlite backend against a live Postgres, proving the two memory backends
// agree on keyword search, Load ordering, and the semantic fallback flag. It
// skips without a DSN so CI without Postgres stays green.
func TestPostgresMemoryConformance(t *testing.T) {
	t.Parallel()

	requireDSN(t)

	memorytest.RunConformance(t, openPostgresMemory)
}

// TestOpenPostgresRejectsBadDSN proves OpenPostgres surfaces a connection
// failure instead of a half-built store, with no Postgres required.
func TestOpenPostgresRejectsBadDSN(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := memory.OpenPostgres(ctx, "postgres://user:pass@127.0.0.1:1/nope"); err == nil {
		t.Fatal("OpenPostgres with a canceled context and dead DSN returned no error")
	}
}
