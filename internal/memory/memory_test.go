package memory_test

import (
	"path/filepath"
	"testing"

	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/memory/memorytest"
)

// openMemory opens a fresh database under t.TempDir and closes it when the
// case finishes; every case exercises schema and index creation. It returns
// the memory.Store interface so the shared conformance suite drives it the
// same way it drives the postgres backend.
func openMemory(t *testing.T) memory.Store {
	t.Helper()

	mem, err := memory.OpenSQLite(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}

	t.Cleanup(func() {
		if closeErr := mem.Close(); closeErr != nil {
			t.Errorf("close memory store: %v", closeErr)
		}
	})

	return mem
}

// TestMemoryConformance runs the one behavioral spec shared with the postgres
// backend (internal/memory/memorytest). Proving sqlite passes the same suite
// the postgres backend must pass is the whole point: one spec, two backends.
func TestMemoryConformance(t *testing.T) {
	t.Parallel()
	memorytest.RunConformance(t, openMemory)
}
