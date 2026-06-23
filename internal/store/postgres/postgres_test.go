package postgres_test

import (
	"context"
	"testing"

	"github.com/chadit/CairnOS/internal/store/postgres"
)

// TestOpenRejectsBadDSN proves Open surfaces a connection failure instead of
// returning a half-built store, with no Postgres required: an unroutable DSN
// fails to connect. The live-Postgres conformance test lives in tests/integration.
func TestOpenRejectsBadDSN(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // a canceled context fails the connect attempt fast and offline

	if _, err := postgres.Open(ctx, "postgres://user:pass@127.0.0.1:1/nope"); err == nil {
		t.Fatal("Open with a canceled context and dead DSN returned no error")
	}
}
