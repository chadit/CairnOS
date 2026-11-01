package engine

import (
	"context"
	"log/slog"
	"time"

	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
)

// Governance error codes.
const (
	// CodeBudgetExceeded marks a run refused because the workflow's
	// accumulated spend for the current period passed its limit and the
	// budget's on_exceed is pause. In-flight runs already created finish; only
	// new runs are blocked, until the period resets or an admin lifts it.
	CodeBudgetExceeded = "governance.budget_exceeded"

	// CodeWorkflowPaused marks a run refused because an admin paused the
	// workflow. The scheduler also skips firing paused workflows; in-flight
	// runs finish. Resume clears the flag.
	CodeWorkflowPaused = "governance.workflow_paused"
)

// BudgetChecker computes a workflow's accumulated spend over a period so the
// engine can compare it to the budget limit. The store's SpendSince satisfies
// it; tests use a fake. Nil on Config keeps budget enforcement off, which is
// the pre-governance behavior every engine test relies on.
type BudgetChecker interface {
	// SpendSince sums the estimated cost_usd of the named workflow's runs
	// created at or after since.
	SpendSince(ctx context.Context, workflowName string, since time.Time) (float64, error)
}

// PauseChecker reports whether a workflow is paused so the engine can refuse
// new runs of it. The store's GetWorkflowPause satisfies it directly; nil on
// Config keeps the pause check off. The engine ignores the returned pause
// record, reading only the paused flag.
type PauseChecker interface {
	// GetWorkflowPause reports whether the named workflow is paused.
	GetWorkflowPause(ctx context.Context, workflowName string) (*store.WorkflowPause, bool, error)
}

// BudgetNotifier is fired on a budget breach so the serve wiring can route to
// the notify runners. Nil on Config keeps the pre-governance behavior: warn
// falls back to a slog warning and denial notifies nothing beyond the returned
// error. The metric fires either way through Instrumentation.
type BudgetNotifier interface {
	// BudgetWarn reports that workflow passed limitUSD with spend over the
	// current period, while its budget's on_exceed is warn so the run
	// proceeds.
	BudgetWarn(ctx context.Context, workflowName string, spend, limitUSD float64)

	// BudgetDenied reports that workflow passed limitUSD with spend over the
	// current period and its budget's on_exceed is pause, so the run was
	// refused. Fired on the pause path only, alongside the refusal error the
	// engine returns.
	BudgetDenied(ctx context.Context, workflowName string, spend, limitUSD float64)
}

// checkGovernance applies the pre-creation governance gate: a paused workflow
// refuses every new run, and a pause-mode budget refuses a run once the
// period's accumulated spend passes the limit. A warn-mode budget never
// refuses; it notifies and counts when over the limit but lets the run
// proceed. In-flight runs already created are untouched: enforcement is at
// run-start only.
func (e *Engine) checkGovernance(ctx context.Context, flow *workflow.Workflow) error {
	if err := e.checkPaused(ctx, flow.Name); err != nil {
		return err
	}

	return e.checkBudget(ctx, flow)
}

// checkPaused refuses a run of a paused workflow. A nil Pause checker (the
// pre-governance default) and a store read failure both skip the gate: a
// failed pause read must not silently block runs, so it logs and proceeds.
func (e *Engine) checkPaused(ctx context.Context, workflowName string) error {
	if e.cfg.Pause == nil {
		return nil
	}

	_, paused, err := e.cfg.Pause.GetWorkflowPause(ctx, workflowName)
	if err != nil {
		slog.Error("governance pause check", "workflow", workflowName, "error", err)

		return nil
	}

	if paused {
		return errf(CodeWorkflowPaused, "workflow %q is paused; resume it to run again", workflowName)
	}

	return nil
}

// checkBudget compares the workflow's accumulated spend for the current
// period to its limit. Over a pause-mode limit the run is refused; over a
// warn-mode limit it proceeds with a notification and a metric. The spend
// gauge always updates so dashboards see the latest figure. A nil Budget
// checker, a budgetless workflow, or a spend read failure all skip the gate.
func (e *Engine) checkBudget(ctx context.Context, flow *workflow.Workflow) error {
	if e.cfg.Budget == nil || flow.Budget == nil {
		return nil
	}

	since := PeriodStart(flow.Budget.Period, e.cfg.Clock())

	spend, err := e.cfg.Budget.SpendSince(ctx, flow.Name, since)
	if err != nil {
		slog.Error("governance budget check", "workflow", flow.Name, "error", err)

		return nil
	}

	e.cfg.Instrumentation.BudgetSpend(ctx, flow.Name, spend, flow.Budget.LimitUSD)

	if spend < flow.Budget.LimitUSD {
		return nil
	}

	return e.budgetExceeded(ctx, flow, spend)
}

// budgetExceeded handles a spend at or over the limit: pause refuses the run
// with governance.budget_exceeded, warn notifies and proceeds. Both fire the
// exceeded counter labeled by the action taken.
func (e *Engine) budgetExceeded(ctx context.Context, flow *workflow.Workflow, spend float64) error {
	if flow.Budget.OnExceed == workflow.BudgetOnExceedPause {
		e.cfg.Instrumentation.BudgetExceeded(ctx, flow.Name, workflow.BudgetOnExceedPause)
		e.notifyBudgetDenied(ctx, flow.Name, spend, flow.Budget.LimitUSD)

		return errf(CodeBudgetExceeded,
			"workflow %q spent $%.2f this %s, at or over its $%.2f budget; new runs are paused until the period resets or an admin lifts it",
			flow.Name, spend, flow.Budget.Period, flow.Budget.LimitUSD)
	}

	e.cfg.Instrumentation.BudgetExceeded(ctx, flow.Name, workflow.BudgetOnExceedWarn)
	e.notifyBudgetWarn(ctx, flow.Name, spend, flow.Budget.LimitUSD)

	return nil
}

// notifyBudgetWarn routes a warn-mode breach to the configured notifier, or
// logs a warning when none is wired so the breach never passes silently.
func (e *Engine) notifyBudgetWarn(ctx context.Context, workflowName string, spend, limitUSD float64) {
	if e.cfg.Notifier != nil {
		e.cfg.Notifier.BudgetWarn(ctx, workflowName, spend, limitUSD)

		return
	}

	slog.Warn("workflow over its warn budget; run proceeding",
		"workflow", workflowName, "spend_usd", spend, "limit_usd", limitUSD)
}

// notifyBudgetDenied routes a pause-mode refusal to the configured notifier so
// the serve fan-out can post a budget_denied line. A nil notifier is the
// pre-governance default: the refusal is already returned as an error and
// counted, so no notifier means no extra signal, unlike the warn path which
// always surfaces a warning.
func (e *Engine) notifyBudgetDenied(ctx context.Context, workflowName string, spend, limitUSD float64) {
	if e.cfg.Notifier == nil {
		return
	}

	e.cfg.Notifier.BudgetDenied(ctx, workflowName, spend, limitUSD)
}

// daysPerWeek wraps the weekday shift that starts a budget week on Monday.
const daysPerWeek = 7

// PeriodStart returns the inclusive lower bound of the budget period
// containing now, in now's location: midnight for day, the most recent Monday
// for week, the first of the month for month. The engine's injected clock
// decides the location, so a fixed test clock makes the boundary repeatable.
// Exported so the governance API reports spend over the window the engine
// enforces.
func PeriodStart(period string, now time.Time) time.Time {
	year, month, day := now.Date()
	loc := now.Location()
	midnight := time.Date(year, month, day, 0, 0, 0, 0, loc)

	switch period {
	case workflow.BudgetPeriodWeek:
		// Go's Weekday has Sunday at 0; shift back a day so Monday is the week
		// start and Sunday lands six days in.
		offset := (int(now.Weekday()) + daysPerWeek - 1) % daysPerWeek

		return midnight.AddDate(0, 0, -offset)
	case workflow.BudgetPeriodMonth:
		return time.Date(year, month, 1, 0, 0, 0, 0, loc)
	default:
		return midnight
	}
}
