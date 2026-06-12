package postgres_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/store/postgres"
	"github.com/chadit/CairnOS/internal/store/storetest"
)

// dsnEnv names the environment variable that points the integration tests at
// a live Postgres. Unset means skip, so CI without Postgres stays green while
// a developer with one reachable runs the full suite by exporting it (the
// same env-gate pattern a livetest build tag would give).
const dsnEnv = "CAIRNOS_TEST_POSTGRES_DSN"

// requireDSN returns the configured DSN or skips the test. Skipping (not
// failing) is what keeps `go test ./...` green on a machine with no Postgres.
func requireDSN(t *testing.T) string {
	t.Helper()

	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("%s not set; skipping postgres integration tests", dsnEnv)
	}

	return dsn
}

// openStore opens a store in its own Postgres schema, so each subtest gets
// the isolation the sqlite backend gets from a per-case temp file. A shared
// truncated database would let parallel subtests wipe each other's rows
// mid-test; a unique schema per call removes the contention entirely. The
// schema name is a fresh lowercased ULID so it is a valid identifier and
// never collides across parallel subtests. Cleanup drops the schema and
// closes the pool.
func openStore(t *testing.T) store.Store {
	t.Helper()

	dsn := requireDSN(t)
	ctx := t.Context()

	schema := "cairnos_test_" + strings.ToLower(ulid.Make().String())

	db, err := postgres.OpenSchema(ctx, dsn, schema)
	if err != nil {
		t.Fatalf("open postgres store: %v", err)
	}

	t.Cleanup(func() {
		// Detach cleanup from the test context: by Cleanup time t.Context is
		// already canceled, but the schema still needs dropping.
		dropCtx := context.WithoutCancel(ctx)

		if dropErr := postgres.DropSchema(dropCtx, db, schema); dropErr != nil {
			t.Errorf("drop test schema: %v", dropErr)
		}

		if closeErr := db.Close(); closeErr != nil {
			t.Errorf("close postgres store: %v", closeErr)
		}
	})

	return db
}

// TestStoreConformance runs the one behavioral spec shared with the sqlite
// backend (internal/store/storetest) against a live Postgres. It skips
// without a DSN, so the shared suite is the only thing that has to run for
// CI to prove the two backends agree; this is the live confirmation when a
// Postgres is available.
func TestStoreConformance(t *testing.T) {
	t.Parallel()

	// The skip decision happens before the suite forks subtests, so a
	// missing DSN skips the whole suite once rather than per subtest.
	requireDSN(t)

	storetest.RunConformance(t, openStore)
}

// TestOpenRejectsBadDSN proves Open surfaces a connection failure instead of
// returning a half-built store, with no Postgres required: an unroutable DSN
// fails to connect.
func TestOpenRejectsBadDSN(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // a canceled context fails the connect attempt fast and offline

	if _, err := postgres.Open(ctx, "postgres://user:pass@127.0.0.1:1/nope"); err == nil {
		t.Fatal("Open with a canceled context and dead DSN returned no error")
	}
}
