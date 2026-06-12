package rules_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/chadit/CairnOS/internal/rules"
	"github.com/chadit/CairnOS/internal/rules/rulestest"
)

// pgDSNEnv names the environment variable pointing the rules integration tests
// at a live Postgres. Unset means skip, so the package stays green on a
// machine with no Postgres (the same env-gate the other stores use).
const pgDSNEnv = "CAIRNOS_TEST_POSTGRES_DSN"

// requireDSN returns the configured DSN or skips the test.
func requireDSN(t *testing.T) string {
	t.Helper()

	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("%s not set; skipping postgres rules tests", pgDSNEnv)
	}

	return dsn
}

// openPostgresRules opens a rules store in its own Postgres schema so each
// subtest gets the isolation the sqlite backend gets from a per-case temp
// file. The schema name is a fresh lowercased ULID; cleanup drops it.
func openPostgresRules(t *testing.T) rules.Service {
	t.Helper()

	dsn := requireDSN(t)
	ctx := t.Context()

	schema := "cairnos_rules_test_" + strings.ToLower(ulid.Make().String())

	svc, err := rules.OpenPostgresSchema(ctx, dsn, schema)
	if err != nil {
		t.Fatalf("open postgres rules store: %v", err)
	}

	t.Cleanup(func() {
		dropCtx := context.WithoutCancel(ctx)

		if dropErr := rules.DropPostgresSchema(dropCtx, svc, schema); dropErr != nil {
			t.Errorf("drop test schema: %v", dropErr)
		}

		if closeErr := svc.Close(); closeErr != nil {
			t.Errorf("close postgres rules store: %v", closeErr)
		}
	})

	return svc
}

// TestPostgresRulesConformance runs the one behavioral spec shared with the
// sqlite backend against a live Postgres, proving the two rules backends agree
// on the merge, list-languages, and unknown-language behavior. It skips
// without a DSN so CI without Postgres stays green.
func TestPostgresRulesConformance(t *testing.T) {
	t.Parallel()

	requireDSN(t)

	rulestest.RunConformance(t, openPostgresRules)
}

// TestOpenPostgresRejectsBadDSN proves OpenPostgres surfaces a connection
// failure instead of a half-built store, with no Postgres required.
func TestOpenPostgresRejectsBadDSN(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := rules.OpenPostgres(ctx, "postgres://user:pass@127.0.0.1:1/nope"); err == nil {
		t.Fatal("OpenPostgres with a canceled context and dead DSN returned no error")
	}
}
