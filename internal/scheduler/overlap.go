package scheduler

import (
	"context"
	"log/slog"
)

// SkipOverlapping wraps a Runner so one schedule never has two occurrences in
// flight. A 30-second schedule on a job that sometimes runs a minute would
// otherwise queue faster than the worker drains, and the backlog outlives the
// slow run that caused it.
type SkipOverlapping struct {
	inner  Runner
	queue  OccurrenceQueue
	logger *slog.Logger
}

// NewSkipOverlapping wraps inner with the overlap guard; a nil logger means
// slog.Default.
func NewSkipOverlapping(inner Runner, queue OccurrenceQueue, logger *slog.Logger) *SkipOverlapping {
	if logger == nil {
		logger = slog.Default()
	}

	return &SkipOverlapping{inner: inner, queue: queue, logger: logger}
}

// Fire drops this occurrence when the schedule's previous one has not finished,
// and otherwise delegates. A queue read that fails fires anyway: a duplicate run
// is cheaper than a fire lost to a transient store error.
func (r *SkipOverlapping) Fire(ctx context.Context, sched *Schedule) error {
	pending, err := r.queue.OccurrencePending(ctx, sched.ID)
	if err != nil {
		r.logger.Error("scheduler overlap check", "schedule", sched.ID, "error", err)
	}

	if pending {
		r.logger.Info("skipping fire; the previous occurrence is still in flight",
			"schedule", sched.ID, "workflow", sched.WorkflowName, "fire", sched.NextFire)

		return nil
	}

	return r.inner.Fire(ctx, sched) //nolint:wrapcheck // the caller prefixes the schedule it fired
}
