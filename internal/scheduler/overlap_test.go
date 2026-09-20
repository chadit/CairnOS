package scheduler_test

import (
	"context"
	"errors"
	"testing"

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

func (f fakeOccurrences) OccurrencePending(context.Context, string) (bool, error) {
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
