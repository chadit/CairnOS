//go:build integration

package integration_test

import (
	"os"
	"testing"

	"github.com/chadit/CairnOS/tests/integration"
)

// TestMain reaps the shared Postgres container after the suite. The container
// starts lazily on the first DSN call; with the Ryuk reaper disabled for podman,
// this explicit teardown is what removes it.
func TestMain(m *testing.M) {
	code := m.Run()

	integration.Stop()

	os.Exit(code)
}
