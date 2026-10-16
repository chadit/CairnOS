//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/chadit/CairnOS/internal/rules"
	"github.com/chadit/CairnOS/internal/rules/rulestest"
	"github.com/chadit/CairnOS/tests/integration"
)

// openRules opens a rules store in its own Postgres schema off the shared
// container, matching the per-case isolation the sqlite backend gets from a temp
// file. The schema is a fresh lowercased ULID; cleanup drops it.
func openRules(t *testing.T) *rules.Postgres {
	t.Helper()

	ctx := t.Context()
	schema := "cairnos_rules_test_" + strings.ToLower(ulid.Make().String())

	svc, err := rules.OpenPostgresSchema(ctx, integration.DSN(t), schema)
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

// TestRulesConformance runs the behavioral spec shared with the sqlite backend
// against the live Postgres container, proving the two rules backends agree on
// the merge, list-languages, and unknown-language behavior.
func TestRulesConformance(t *testing.T) {
	t.Parallel()

	rulestest.RunConformance(t, openRules)
}
