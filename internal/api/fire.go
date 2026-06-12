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
// skips firing it (.agents/spec/12-ui-and-governance.md, "Control actions").
// The store satisfies it.
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

// Fire loads and runs one scheduled workflow, fresh from disk each fire so
// edits apply without re-adding the schedule. A paused workflow is skipped
// cleanly (no run, no error) so the scheduler does not log a refusal every
// poll. A failed run surfaces as an error so the daemon logs it; the daemon
// keeps running either way.
func (r *FireRunner) Fire(ctx context.Context, sched *scheduler.Schedule) error {
	flow, err := workflow.Load(sched.WorkflowPath)
	if err != nil {
		return fmt.Errorf("load %s: %w", sched.WorkflowPath, err)
	}

	if r.skipPaused(ctx, flow.Name) {
		return nil
	}

	handle, err := r.manager.StartRun(ctx, flow, map[string]any{}, false, triggerSchedule)
	if err != nil {
		return fmt.Errorf("run %s: %w", sched.WorkflowName, err)
	}

	out := <-handle.Outcome
	if out.Err != nil {
		return fmt.Errorf("run %s: %w", sched.WorkflowName, out.Err)
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
