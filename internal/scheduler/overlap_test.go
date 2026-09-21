package scheduler_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/scheduler"
)

// errQueueDown is the store failure the fail-open case injects.
var errQueueDown = errors.New("queue unavailable")

// fakeOccurrences answers the overlap guard with a fixed verdict, so the guard's
// own decision is under test rather than a queue's bookkeeping.
type fakeOccurrences struct {
	err     error
	pending bool
}

func (f fakeOccurrences) OccurrencePending(context.Context, string, time.Time) (bool, error) {
	return f.pending, f.err
}

// TestSkipOverlappingHoldsOneRunPerSchedule covers the three answers the queue
// can give. The error case fires on purpose: losing a scheduled run to a
// transient store failure is worse than running one workflow twice.
func TestSkipOverlappingHoldsOneRunPerSchedule(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		queue     fakeOccurrences
		wantFires int
	}{
		{
			name:      "previous occurrence finished",
			queue:     fakeOccurrences{},
			wantFires: 1,
		},
		{
			name:      "previous occurrence still in flight",
			queue:     fakeOccurrences{pending: true},
			wantFires: 0,
		},
		{
			name:      "queue read failed",
			queue:     fakeOccurrences{err: errQueueDown},
			wantFires: 1,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runner := &recordRunner{}
			guard := scheduler.NewSkipOverlapping(runner, tt.queue, nil)

			err := guard.Fire(t.Context(), &scheduler.Schedule{ID: fastScheduleID, WorkflowName: flowName})
			if err != nil {
				t.Fatalf("Fire = %v, want nil", err)
			}

			if got := runner.count(); got != tt.wantFires {
				t.Fatalf("inner runner fired %d times, want %d", got, tt.wantFires)
			}
		})
	}
}

// TestSkipOverlappingReportsInnerFailure checks the guard stays a pass-through
// on the path that matters to the daemon's log: a fire it did not skip still
// reports why it failed.
func TestSkipOverlappingReportsInnerFailure(t *testing.T) {
	t.Parallel()

	runner := &recordRunner{fireErr: errFireBoom}
	guard := scheduler.NewSkipOverlapping(runner, fakeOccurrences{}, nil)

	err := guard.Fire(t.Context(), &scheduler.Schedule{ID: fastScheduleID, WorkflowName: flowName})
	if !errors.Is(err, errFireBoom) {
		t.Fatalf("Fire = %v, want the inner runner's error", err)
	}
}

// Lease spans for the crash cases. The dead worker took its lease long enough
// ago that it has run out by the time the guard reads the clock; the live one's
// sits well ahead of it.
const (
	crashedAgo = 2 * time.Minute
	deadLease  = time.Minute
	liveLease  = 5 * time.Minute
)

// TestSkipOverlappingClearsACrashedWorkersOccurrence walks the production
// failure: a worker leases an occurrence, the process dies without acking or
// naking it, and a fresh process opens the same store. The abandoned lease used
// to read as in flight for as long as it had left, which on a 30-second
// schedule killed the schedule until an operator re-added it under a new id.
func TestSkipOverlappingClearsACrashedWorkersOccurrence(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "queue.db")
	dead := openQueueAt(t, path)
	fireTime := time.Now().Add(-crashedAgo)

	enqueueOccurrence(t, dead, fireTime)

	// The worker leases the occurrence, then the process dies mid-run: no ack,
	// no nak, nothing left behind but the lease.
	claimed, err := dead.Claim(t.Context(), fireTime, deadLease, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %d items, %v; want 1 and no error", len(claimed), err)
	}

	if closeErr := dead.Close(); closeErr != nil {
		t.Fatalf("close crashed queue: %v", closeErr)
	}

	fresh := openQueueAt(t, path)

	pending, err := fresh.OccurrencePending(t.Context(), fastScheduleID, fireTime)
	if err != nil || !pending {
		t.Fatalf("pending while the lease was live = %t, %v; want true and no error", pending, err)
	}

	runner := &recordRunner{}
	guard := scheduler.NewSkipOverlapping(runner, fresh, discardLogger())

	if fireErr := guard.Fire(t.Context(), &scheduler.Schedule{ID: fastScheduleID, WorkflowName: flowName}); fireErr != nil {
		t.Fatalf("Fire = %v, want nil", fireErr)
	}

	if got := runner.count(); got != 1 {
		t.Fatalf("inner runner fired %d times after the lease lapsed, want 1", got)
	}

	// The abandoned work is not lost either: the sweep still returns it to ready
	// for redelivery, which is what makes the queue at-least-once.
	reclaimed, _, err := fresh.SweepExpired(t.Context(), time.Now(), 5)
	if err != nil || reclaimed != 1 {
		t.Fatalf("sweep reclaimed %d, %v; want 1 and no error", reclaimed, err)
	}
}

// TestSkipOverlappingSkipsALiveLease is the other half: an occurrence a worker
// is still running, with a lease it keeps renewing, holds the schedule back
// exactly as before.
func TestSkipOverlappingSkipsALiveLease(t *testing.T) {
	t.Parallel()

	queue := openQueueAt(t, filepath.Join(t.TempDir(), "queue.db"))
	fireTime := time.Now()

	enqueueOccurrence(t, queue, fireTime)

	claimed, err := queue.Claim(t.Context(), fireTime, liveLease, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %d items, %v; want 1 and no error", len(claimed), err)
	}

	runner := &recordRunner{}
	guard := scheduler.NewSkipOverlapping(runner, queue, discardLogger())

	if fireErr := guard.Fire(t.Context(), &scheduler.Schedule{ID: fastScheduleID, WorkflowName: flowName}); fireErr != nil {
		t.Fatalf("Fire = %v, want nil", fireErr)
	}

	if got := runner.count(); got != 0 {
		t.Fatalf("inner runner fired %d times while an occurrence was running, want 0", got)
	}
}

// openQueueAt opens the work queue at path, closing it when the test ends. A
// crash case reopens the same path, so the path is the caller's to choose.
func openQueueAt(t *testing.T, path string) *scheduler.SQLiteQueue {
	t.Helper()

	queue, err := scheduler.OpenSQLiteQueue(path)
	if err != nil {
		t.Fatalf("open sqlite queue: %v", err)
	}

	t.Cleanup(func() { _ = queue.Close() })

	return queue
}

// enqueueOccurrence enqueues one occurrence of the fast schedule, available at
// its fire time so a claim dated there can take it.
func enqueueOccurrence(t *testing.T, queue scheduler.WorkQueue, fireTime time.Time) {
	t.Helper()

	if _, err := queue.Enqueue(t.Context(), &scheduler.WorkItem{
		DedupKey:    scheduler.DedupKeyFor(fastScheduleID, fireTime),
		Payload:     []byte(`{"workflow":"x"}`),
		AvailableAt: fireTime,
		CreatedAt:   fireTime,
	}); err != nil {
		t.Fatalf("enqueue occurrence: %v", err)
	}
}
