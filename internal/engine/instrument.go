package engine

import (
	"context"

	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// ExecutionInstrumentation follows a run through its actions and runner calls,
// the hooks that build the run -> action -> runner.call span tree
// (.agents/spec/06-observability-and-tuning.md). A Started hook's context
// carries its scope into the work it covers, and each finish func is called
// exactly once.
type ExecutionInstrumentation interface {
	// RunStarted opens the run scope as the run record turns running. The
	// finish func runs once the run is terminal, with status and error
	// settled on the record.
	RunStarted(ctx context.Context, run *store.Run) (context.Context, func(*store.Run))

	// ActionStarted opens one executing action's scope under the run scope
	// carried by ctx. The finish func receives the action's terminal row.
	// Actions that never execute go through ActionSettled instead.
	ActionStarted(ctx context.Context, action *workflow.Action) (context.Context, func(*store.ActionResult))

	// ActionSettled records an action that reached a terminal state without
	// executing: a skip, a pre-launch failure, or a missing agent executor.
	// Counters move; no span opens, so duration views stay honest.
	ActionSettled(ctx context.Context, result *store.ActionResult)

	// CallStarted opens one runner.call attempt under the action scope in
	// ctx. The finish func receives the attempt's usage and outcome.
	CallStarted(ctx context.Context, call *Call) (context.Context, func(usage runner.Usage, err error))
}

// BudgetInstrumentation receives the budget gate's spend readings and breaches.
type BudgetInstrumentation interface {
	// BudgetSpend records a workflow's accumulated spend against its limit for
	// the current period, feeding the cairnos_budget_spend_usd gauge. The
	// engine calls it whenever a budgeted run is evaluated, so the gauge
	// reflects the latest computed spend.
	BudgetSpend(ctx context.Context, workflowName string, spend, limitUSD float64)

	// BudgetExceeded records one budget-limit breach, feeding the
	// cairnos_budget_exceeded_total counter labeled by the action taken
	// (pause or warn).
	BudgetExceeded(ctx context.Context, workflowName, action string)
}

// Instrumentation observes execution milestones so the telemetry layer can
// build its spans and budget metrics without the engine importing a telemetry
// SDK. Implementations must be concurrency-safe and return promptly: every
// hook sits on the hot path.
type Instrumentation interface {
	ExecutionInstrumentation
	BudgetInstrumentation
}

// Call identifies one runner.call attempt for instrumentation.
type Call struct {
	Runner string
	Action string

	// BreakerState is the runner's circuit position when the call launched.
	BreakerState string

	// Attempt is zero-based, so retries show as attempt 1 and up, matching
	// the spec's example trace.
	Attempt int
}

// nopInstrumentation is the default when Config.Instrumentation is nil: the
// engine pays one interface call per milestone and nothing else.
type nopInstrumentation struct{}

// RunStarted implements Instrumentation.
func (nopInstrumentation) RunStarted(ctx context.Context, _ *store.Run) (context.Context, func(*store.Run)) {
	return ctx, func(*store.Run) {}
}

// ActionStarted implements Instrumentation.
func (nopInstrumentation) ActionStarted(ctx context.Context, _ *workflow.Action) (context.Context, func(*store.ActionResult)) {
	return ctx, func(*store.ActionResult) {}
}

// ActionSettled does nothing: with no telemetry wired there is no counter to
// move.
func (nopInstrumentation) ActionSettled(context.Context, *store.ActionResult) {}

// CallStarted implements Instrumentation.
func (nopInstrumentation) CallStarted(ctx context.Context, _ *Call) (context.Context, func(runner.Usage, error)) {
	return ctx, func(runner.Usage, error) {}
}

// BudgetSpend does nothing: with no telemetry wired there is no gauge to set.
func (nopInstrumentation) BudgetSpend(context.Context, string, float64, float64) {}

// BudgetExceeded does nothing: with no telemetry wired there is no counter to
// bump.
func (nopInstrumentation) BudgetExceeded(context.Context, string, string) {}
