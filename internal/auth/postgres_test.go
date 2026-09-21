package auth_test

import (
	"testing"

	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/store/storetest"
)

// TestOpenPostgresRejectsUnreachableServer proves OpenPostgres reports the
// connection failure instead of handing back a half-built store whose first
// query fails later. The DSN points at a loopback port nothing listens on, so
// the failure is a real refused connect and needs no Postgres. Conformance
// against a live Postgres lives in tests/integration.
func TestOpenPostgresRejectsUnreachableServer(t *testing.T) {
	t.Parallel()

	store, err := auth.OpenPostgres(t.Context(), storetest.RefusedPostgresDSN(t))
	if err == nil {
		_ = store.Close()

		t.Fatal("OpenPostgres against a refused port returned no error")
	}
}
