//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/chadit/CairnOS/internal/tenancy"
	"github.com/chadit/CairnOS/internal/tenancy/tenancytest"
	"github.com/chadit/CairnOS/tests/integration"
)

// openTenancy opens a tenancy store in its own Postgres schema off the shared
// container, matching the per-case isolation the sqlite backend gets from a temp
// file. The schema is a fresh lowercased ULID; cleanup drops it.
func openTenancy(t *testing.T) *tenancy.Postgres {
	t.Helper()

	ctx := t.Context()
	schema := "cairnos_tenancy_test_" + strings.ToLower(ulid.Make().String())

	tenants, err := tenancy.OpenPostgresSchema(ctx, integration.DSN(t), schema)
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

// TestTenancyConformance runs the behavioral spec shared with the sqlite backend
// against the live Postgres container, proving the two tenancy backends agree.
func TestTenancyConformance(t *testing.T) {
	t.Parallel()

	tenancytest.RunConformance(t, openTenancy)
}
