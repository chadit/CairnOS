//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/chadit/CairnOS/internal/vault"
	"github.com/chadit/CairnOS/internal/vault/vaulttest"
	"github.com/chadit/CairnOS/tests/integration"
)

// openVault opens a vault store in its own Postgres schema off the shared
// container, matching the per-case isolation the sqlite backend gets from a temp
// file. The schema is a fresh lowercased ULID; cleanup drops it.
func openVault(t *testing.T) vault.Store {
	t.Helper()

	ctx := t.Context()
	schema := "cairnos_vault_test_" + strings.ToLower(ulid.Make().String())

	store, err := vault.OpenPostgresSchema(ctx, integration.DSN(t), schema)
	if err != nil {
		t.Fatalf("open postgres vault store: %v", err)
	}

	t.Cleanup(func() {
		dropCtx := context.WithoutCancel(ctx)

		if dropErr := vault.DropPostgresSchema(dropCtx, store, schema); dropErr != nil {
			t.Errorf("drop test schema: %v", dropErr)
		}

		if closeErr := store.Close(); closeErr != nil {
			t.Errorf("close postgres vault store: %v", closeErr)
		}
	})

	return store
}

// TestVaultConformance runs the behavioral spec shared with the sqlite backend
// against the live Postgres container, proving the two vault backends agree on
// graph traversal, backlink derivation, keyword search, and the semantic
// fallback flag.
func TestVaultConformance(t *testing.T) {
	t.Parallel()

	vaulttest.RunConformance(t, openVault)
}
