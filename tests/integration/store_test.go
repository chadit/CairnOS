//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/store/postgres"
	"github.com/chadit/CairnOS/internal/store/storetest"
	"github.com/chadit/CairnOS/tests/integration"
)

// openStore opens a store in its own Postgres schema off the shared container,
// matching the per-case isolation the sqlite backend gets from a temp file. The
// schema is a fresh lowercased ULID; cleanup drops it and closes the pool.
func openStore(t *testing.T) store.Store {
	t.Helper()

	ctx := t.Context()
	schema := "cairnos_store_test_" + strings.ToLower(ulid.Make().String())

	db, err := postgres.OpenSchema(ctx, integration.DSN(t), schema)
	if err != nil {
		t.Fatalf("open postgres store: %v", err)
	}

	t.Cleanup(func() {
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

// TestStoreConformance runs the behavioral spec shared with the sqlite backend
// (internal/store/storetest) against the live Postgres container.
func TestStoreConformance(t *testing.T) {
	t.Parallel()

	storetest.RunConformance(t, openStore)
}
