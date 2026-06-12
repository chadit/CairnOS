package auth_test

import (
	"testing"

	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/auth/authtest"
)

// TestSQLiteStoreConformance runs the one behavioral spec shared with the
// postgres backend (internal/auth/authtest): key CRUD, the denylist window,
// extend-not-shorten, sub-only lift, and expiry prune. Proving sqlite passes
// the same suite the postgres backend must pass is the point of the shared
// suite. The token and signing tests stay in token_test.go and key_test.go;
// those exercise the authenticator, not the store.
func TestSQLiteStoreConformance(t *testing.T) {
	t.Parallel()

	authtest.RunConformance(t, func(t *testing.T) auth.Store {
		t.Helper()

		return openTestStore(t)
	})
}
