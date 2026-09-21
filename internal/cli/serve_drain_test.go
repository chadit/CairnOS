package cli_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/scheduler"
)

// countingRunner is the scheduler runner under the drain gate: it records every
// occurrence that got through.
type countingRunner struct {
	mu    sync.Mutex
	fires int
}

// Fire counts one dispatched occurrence.
func (r *countingRunner) Fire(context.Context, *scheduler.Schedule) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.fires++

	return nil
}

// count reads the dispatched total.
func (r *countingRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.fires
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// TestDrainGateStopsScheduleFiresOnceDraining pins the half of the drain that
// stops work arriving: an occurrence dispatched before the request runs, and
// the same occurrence after it is skipped rather than failed, so the schedule
// picks it up again after the restart.
func TestDrainGateStopsScheduleFiresOnceDraining(t *testing.T) {
	t.Parallel()

	inner := &countingRunner{}
	drain := api.NewDrain()
	gate := cli.NewDrainGateRunner(inner, drain, discardLogger())
	sched := &scheduler.Schedule{ID: "sched", WorkflowName: "nightly"}

	if err := gate.Fire(t.Context(), sched); err != nil {
		t.Fatalf("Fire before the drain: %v", err)
	}

	if inner.count() != 1 {
		t.Fatalf("fires before the drain = %d, want 1", inner.count())
	}

	drain.Request(time.Minute, time.Now())

	if err := gate.Fire(t.Context(), sched); err != nil {
		t.Fatalf("Fire while draining must skip, not fail: %v", err)
	}

	if inner.count() != 1 {
		t.Errorf("fires while draining = %d, want the gate to dispatch nothing new", inner.count())
	}
}

// TestDrainWatcherWaitsForTheLastRunThenStops holds one run in flight past the
// request, which is what a webhook delivery landing mid-drain looks like, and
// checks the process only stops once that run is gone.
func TestDrainWatcherWaitsForTheLastRunThenStops(t *testing.T) {
	t.Parallel()

	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	var (
		guard    sync.Mutex
		inFlight = 1
		polls    int
	)

	count := func() int {
		guard.Lock()
		defer guard.Unlock()

		polls++

		// The run finishes between the first and second look, so the watcher
		// has to poll rather than answer from the count the request saw.
		if polls > 1 {
			inFlight = 0
		}

		return inFlight
	}

	drain := api.NewDrain()
	drain.Request(time.Minute, time.Now())

	cli.RunDrainWatcher(ctx, drain, count, stop, discardLogger())

	if ctx.Err() == nil {
		t.Fatal("the watcher must cancel the serve context once nothing is running")
	}

	guard.Lock()
	defer guard.Unlock()

	if polls < 2 {
		t.Errorf("polls = %d, want the watcher to wait for the run rather than exit on the first look", polls)
	}
}

// TestDrainWatcherStopsAtTheCeiling proves a run that never ends cannot hold an
// update open: the deadline the request set wins.
func TestDrainWatcherStopsAtTheCeiling(t *testing.T) {
	t.Parallel()

	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	drain := api.NewDrain()
	drain.Request(0, time.Now())

	cli.RunDrainWatcher(ctx, drain, func() int { return 3 }, stop, discardLogger())

	if ctx.Err() == nil {
		t.Fatal("the watcher must cancel the serve context once the ceiling passes")
	}
}

// TestDrainWatcherLeavesAnUnaskedProcessAlone guards the everyday case: no
// restart was requested, so a canceled serve context ends the watcher and
// nothing else.
func TestDrainWatcherLeavesAnUnaskedProcessAlone(t *testing.T) {
	t.Parallel()

	ctx, stop := context.WithCancel(t.Context())

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)

		cli.RunDrainWatcher(ctx, api.NewDrain(), func() int { return 0 }, stop, discardLogger())
	}()

	select {
	case <-stopped:
		t.Fatal("the watcher exited before a restart was requested or the context ended")
	case <-time.After(50 * time.Millisecond):
	}

	stop()
	<-stopped
}
