package scheduler_test

import (
	"path/filepath"
	"testing"

	"github.com/chadit/CairnOS/internal/scheduler"
	"github.com/chadit/CairnOS/internal/scheduler/schedulertest"
)

// testHoldKey is the provider a held item waits on in the worker tests, the
// value Hold records and Resume clears (D12).
const testHoldKey = "codex"

// TestSQLiteQueueConformance runs the shared work-queue contract against the
// sqlite backend. The postgres counterpart lives in tests/integration so it can
// run against a real database without an env-gate.
func TestSQLiteQueueConformance(t *testing.T) {
	t.Parallel()

	schedulertest.RunWorkQueueConformance(t, func(t *testing.T) *scheduler.SQLiteQueue {
		t.Helper()

		queue, err := scheduler.OpenSQLiteQueue(filepath.Join(t.TempDir(), "queue.db"))
		if err != nil {
			t.Fatalf("open sqlite queue: %v", err)
		}

		t.Cleanup(func() { _ = queue.Close() })

		return queue
	})
}
