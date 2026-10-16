package cli

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/scheduler"
)

// drainPoll is how often the watcher re-reads the in-flight count once a
// restart has been asked for. A run finishing a second early buys nothing, and
// a tighter loop only costs a lock.
const drainPoll = time.Second

// DrainGateRunner stops the scheduler dispatching once a restart has been
// asked for. It wraps the outermost runner so a poll fire and a plain cron fire
// stop together; webhook deliveries enqueue past it, so nothing arriving during
// the drain is lost.
type DrainGateRunner struct {
	inner  scheduler.Runner
	drain  *api.Drain
	logger *slog.Logger
}

// NewDrainGateRunner wraps inner in the gate serve puts in front of its daemon.
// It is exported so a test drives the skip without standing up a whole serve.
func NewDrainGateRunner(inner scheduler.Runner, drain *api.Drain, logger *slog.Logger) *DrainGateRunner {
	return &DrainGateRunner{inner: inner, drain: drain, logger: logger}
}

// Fire skips the occurrence while draining and delegates otherwise. Skipping
// rather than erroring is what keeps the drain quiet: the schedule's next fire
// after the restart picks the workflow up again.
func (r *DrainGateRunner) Fire(ctx context.Context, sched *scheduler.Schedule) error {
	if r.drain.Draining() {
		r.logger.Debug("skipping fire while draining for a restart",
			"schedule", sched.ID, "workflow", sched.WorkflowName)

		return nil
	}

	if err := r.inner.Fire(ctx, sched); err != nil {
		return fmt.Errorf("dispatch schedule %s: %w", sched.ID, err)
	}

	return nil
}

// watchDrain waits for a restart request, then for the in-flight runs to
// finish, and stops the process the same way a SIGTERM would. The deadline is
// the ceiling the request asked for, so one stuck run cannot hold an update
// open indefinitely.
func watchDrain(ctx context.Context, drain *api.Drain, inFlight func() int, stop context.CancelFunc, logger *slog.Logger) {
	select {
	case <-ctx.Done():
		return
	case <-drain.Requested():
	}

	deadline := drain.Deadline()

	ticker := time.NewTicker(drainPoll)
	defer ticker.Stop()

	for {
		if count := inFlight(); count == 0 {
			logger.Info("drain finished; exiting for the supervisor to start the new build")
			stop()

			return
		}

		if !time.Now().Before(deadline) {
			logger.Warn("drain ceiling reached; exiting with runs still going",
				"in_flight", inFlight(), "deadline", deadline)
			stop()

			return
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
