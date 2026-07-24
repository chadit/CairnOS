//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

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

// TestOpenReAddsParentRunIDColumn proves the guarded ALTER upgrades a Postgres
// database created before replay: dropping runs.parent_run_id and reopening
// re-adds it (ADD COLUMN IF NOT EXISTS), and a child run with ParentRunID set
// then round-trips (.agents/spec/parity/workflow-language.md).
func TestOpenReAddsParentRunIDColumn(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	schema := "cairnos_replay_test_" + strings.ToLower(ulid.Make().String())

	db, err := postgres.OpenSchema(ctx, integration.DSN(t), schema)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() {
		dropCtx := context.WithoutCancel(ctx)

		if dropErr := postgres.DropSchema(dropCtx, db, schema); dropErr != nil {
			t.Errorf("drop test schema: %v", dropErr)
		}

		if closeErr := db.Close(); closeErr != nil {
			t.Errorf("close store: %v", closeErr)
		}
	})

	// Simulate a database from before replay by dropping the column the ALTER
	// added, then reopen so the guarded ALTER re-adds it.
	if _, err := db.Pool().Exec(ctx, "ALTER TABLE runs DROP COLUMN parent_run_id"); err != nil {
		t.Fatalf("drop parent_run_id: %v", err)
	}

	reopened, err := postgres.OpenSchema(ctx, integration.DSN(t), schema)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}

	t.Cleanup(func() {
		if closeErr := reopened.Close(); closeErr != nil {
			t.Errorf("close reopened store: %v", closeErr)
		}
	})

	child := &store.Run{
		ID:           "run-child",
		WorkflowName: "alpha",
		WorkflowHash: "hash",
		Status:       store.RunPending,
		Trigger:      "replay",
		ParentRunID:  "run-parent",
		CreatedAt:    time.Now(),
	}
	if err := reopened.CreateRun(ctx, child); err != nil {
		t.Fatalf("create child run after reopen: %v", err)
	}

	got, err := reopened.GetRun(ctx, "run-child")
	if err != nil {
		t.Fatalf("get child run: %v", err)
	}

	if got.ParentRunID != "run-parent" {
		t.Errorf("child parent_run_id = %q, want run-parent", got.ParentRunID)
	}
}
