package scheduler_test

import (
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/scheduler"
)

// TestDedupKeyForExactlyOncePerOccurrence pins the contract the queue's
// exactly-once enqueue relies on: one key per (schedule, fire-time), stable
// across calls and time zones, distinct across occurrences and schedules.
func TestDedupKeyForExactlyOncePerOccurrence(t *testing.T) {
	t.Parallel()

	fire := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	base := scheduler.DedupKeyFor("sched-1", fire)

	if again := scheduler.DedupKeyFor("sched-1", fire); again != base {
		t.Errorf("same schedule and fire-time must repeat the key: %q vs %q", base, again)
	}

	if other := scheduler.DedupKeyFor("sched-1", fire.Add(time.Minute)); other == base {
		t.Error("a different fire-time must change the key")
	}

	if other := scheduler.DedupKeyFor("sched-2", fire); other == base {
		t.Error("a different schedule must change the key")
	}

	// The key normalizes to UTC, so the same instant in another zone keys the
	// same row. Without this, two nodes in different zones would double-enqueue.
	zoned := scheduler.DedupKeyFor("sched-1", fire.In(time.FixedZone("plus1", 3600)))
	if zoned != base {
		t.Errorf("key must be time-zone independent: %q vs %q", base, zoned)
	}
}
