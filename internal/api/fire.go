package api

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/chadit/CairnOS/internal/scheduler"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
)

// triggerSchedule marks daemon-fired runs in run history.
const triggerSchedule = "schedule"

// SchedulePauseChecker reports whether a workflow is paused so the scheduler
// skips firing it. The store satisfies it.
type SchedulePauseChecker interface {
	GetWorkflowPause(ctx context.Context, workflowName string) (*store.WorkflowPause, bool, error)
}

// FireRunner adapts the run manager to the scheduler's Runner so scheduled
// fires share the engine, its breakers, and the run manager with API runs.
// Routing every run through the manager is what keeps the manager's run-ID
// reservation correlation sound (see Manager), and it makes scheduled runs
// cancelable through CancelRun for free.
type FireRunner struct {
	manager *Manager
	pauses  SchedulePauseChecker
	logger  *slog.Logger
}

// NewFireRunner wires a FireRunner over the manager. The pause checker lets
// the scheduler skip a paused workflow before firing; a nil checker keeps the
// engine's own pause refusal as the only gate. A nil logger uses the default.
func NewFireRunner(manager *Manager, pauses SchedulePauseChecker, logger *slog.Logger) *FireRunner {
	if logger == nil {
		logger = slog.Default()
	}

	return &FireRunner{manager: manager, pauses: pauses, logger: logger}
}

// Fire loads and runs one scheduled workflow with default inputs and the
// schedule trigger. It is FireRun with nothing extra, so a plain scheduled
// occurrence behaves exactly as before and any holder of a scheduler.Runner
// sees no change.
func (r *FireRunner) Fire(ctx context.Context, sched *scheduler.Schedule) error {
	return r.FireRun(ctx, sched, nil, triggerSchedule)
}

// FireRun loads and runs one scheduled workflow with caller-supplied inputs and
// trigger label. It reads the workflow fresh from disk each fire, so edits apply
// without re-adding the schedule. A paused workflow is skipped without running
// or erroring, so the scheduler does not log a refusal every poll. A failed run
// returns an error and the daemon keeps running.
func (r *FireRunner) FireRun(ctx context.Context, sched *scheduler.Schedule, inputs map[string]any, trigger string) error {
	flow, err := workflow.Load(sched.WorkflowPath)
	if err != nil {
		return fmt.Errorf("load %s: %w", sched.WorkflowPath, err)
	}

	if r.skipPaused(ctx, flow.Name) {
		return nil
	}

	if inputs == nil {
		inputs = map[string]any{}
	}

	handle, err := r.manager.StartRun(ctx, flow, inputs, false, trigger)
	if err != nil {
		return fmt.Errorf("run %s: %w", sched.WorkflowName, err)
	}

	out := <-handle.Outcome
	if out.Err != nil {
		return fmt.Errorf("run %s: %w", sched.WorkflowName, out.Err)
	}

	if out.Run.Status == store.RunHeld {
		// A parked run acks its queue item: run parking lives in the runs table,
		// and resuming as a refire would mint a duplicate run, so the fire is done
		// once the run exists and waits for an operator decision.
		r.logger.Info("scheduled run parked awaiting approval", "run_id", out.Run.ID, "workflow", sched.WorkflowName)

		return nil
	}

	if out.Run.Status != store.RunSucceeded {
		return fmt.Errorf("%w: run %s finished %s", ErrScheduledRunFailed, out.Run.ID, out.Run.Status)
	}

	return nil
}

// skipPaused reports whether the workflow is paused, so Fire returns without
// starting a run. A nil checker never skips; a pause-read failure logs and
// proceeds rather than silently dropping a scheduled fire.
func (r *FireRunner) skipPaused(ctx context.Context, workflowName string) bool {
	if r.pauses == nil {
		return false
	}

	_, paused, err := r.pauses.GetWorkflowPause(ctx, workflowName)
	if err != nil {
		r.logger.Error("scheduler pause check", "workflow", workflowName, "error", err)

		return false
	}

	if paused {
		r.logger.Info("skipping paused workflow", "workflow", workflowName)
	}

	return paused
}
