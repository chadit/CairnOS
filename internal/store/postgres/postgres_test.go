package postgres_test

import (
	"testing"

	"github.com/chadit/CairnOS/internal/store/postgres"
	"github.com/chadit/CairnOS/internal/store/storetest"
)

// TestOpenRejectsUnreachableServer proves Open reports the connection failure
// instead of handing back a half-built store whose first query fails later.
// The DSN points at a loopback port nothing listens on, so the failure is a
// real refused connect and needs no Postgres. Conformance against a live
// Postgres lives in tests/integration.
func TestOpenRejectsUnreachableServer(t *testing.T) {
	t.Parallel()

	store, err := postgres.Open(t.Context(), storetest.RefusedPostgresDSN(t))
	if err == nil {
		_ = store.Close()

		t.Fatal("Open against a refused port returned no error")
	}
}
