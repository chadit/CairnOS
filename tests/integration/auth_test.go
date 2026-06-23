//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/auth/authtest"
	"github.com/chadit/CairnOS/tests/integration"
)

// openAuth opens an auth store in its own Postgres schema off the shared
// container, matching the per-case isolation the sqlite backend gets from a temp
// file. The schema is a fresh lowercased ULID; cleanup drops it.
func openAuth(t *testing.T) auth.Store {
	t.Helper()

	ctx := t.Context()
	schema := "cairnos_auth_test_" + strings.ToLower(ulid.Make().String())

	store, err := auth.OpenPostgresSchema(ctx, integration.DSN(t), schema)
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

// TestAuthConformance runs the behavioral spec shared with the sqlite backend
// against the live Postgres container, proving the two auth backends agree on
// key CRUD and denylist behavior.
func TestAuthConformance(t *testing.T) {
	t.Parallel()

	authtest.RunConformance(t, openAuth)
}
