//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"github.com/chadit/CairnOS/internal/webhook"
	"github.com/chadit/CairnOS/internal/webhook/webhooktest"
	"github.com/chadit/CairnOS/tests/integration"
)

// newPostgresWebhooks opens a registration store in its own schema so parallel
// conformance subtests never share one webhook_registrations table. The schema
// is a fresh lowercased ULID (a safe identifier that starts with a letter);
// cleanup drops it and closes the pool the store borrows.
func newPostgresWebhooks(t *testing.T) *webhook.PostgresWebhooks {
	t.Helper()

	ctx := t.Context()
	schema := "cairnos_webhook_test_" + strings.ToLower(ulid.Make().String())

	cfg, err := pgxpool.ParseConfig(integration.DSN(t))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}

	// search_path pins the unqualified webhook_registrations table to this
	// per-run schema, so NewPostgresWebhooks creates and queries it there.
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}

	if _, execErr := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); execErr != nil {
		pool.Close()
		t.Fatalf("create schema: %v", execErr)
	}

	store, err := webhook.NewPostgresWebhooks(ctx, pool)
	if err != nil {
		pool.Close()
		t.Fatalf("new postgres webhooks: %v", err)
	}

	t.Cleanup(func() {
		dropCtx := context.WithoutCancel(ctx)

		if _, dropErr := pool.Exec(dropCtx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); dropErr != nil {
			t.Errorf("drop test schema: %v", dropErr)
		}

		pool.Close()
	})

	return store
}

// TestWebhookStoreConformance runs the shared registration-store contract
// against a live Postgres, the backend counterpart to the sqlite unit test.
func TestWebhookStoreConformance(t *testing.T) {
	t.Parallel()

	webhooktest.RunConformance(t, newPostgresWebhooks)
}
