package tenancy_test

import (
	"context"
	"testing"

	"github.com/chadit/CairnOS/internal/tenancy"
)

// TestOpenPostgresRejectsBadDSN proves OpenPostgres surfaces a connection failure
// instead of a half-built store, with no Postgres required. The live-Postgres
// conformance test lives in tests/integration.
func TestOpenPostgresRejectsBadDSN(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := tenancy.OpenPostgres(ctx, "postgres://user:pass@127.0.0.1:1/nope"); err == nil {
		t.Fatal("OpenPostgres with a canceled context and dead DSN returned no error")
	}
}
