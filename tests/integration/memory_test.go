//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/memory/memorytest"
	"github.com/chadit/CairnOS/tests/integration"
)

// openMemory opens a memory store in its own Postgres schema off the shared
// container, matching the per-case isolation the sqlite backend gets from a temp
// file. The schema is a fresh lowercased ULID; cleanup drops it.
func openMemory(t *testing.T) memory.Store {
	t.Helper()

	ctx := t.Context()
	schema := "cairnos_mem_test_" + strings.ToLower(ulid.Make().String())

	mem, err := memory.OpenPostgresSchema(ctx, integration.DSN(t), schema)
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

// TestMemoryConformance runs the behavioral spec shared with the sqlite backend
// against the live Postgres container, proving the two memory backends agree on
// keyword search, Load ordering, and the semantic fallback flag.
func TestMemoryConformance(t *testing.T) {
	t.Parallel()

	memorytest.RunConformance(t, openMemory)
}
