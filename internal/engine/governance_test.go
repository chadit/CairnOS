package engine_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/reliability"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Governance fixture values; goconst applies to this package's tests.
const (
	budgetWorkflow = "budgeted"
	pauseActor     = "test"
	actionWork     = "work"
)

const budgetPauseTOML = `
[workflow]
name = "budgeted"

[workflow.budget]
period = "month"
limit_usd = 10.0
on_exceed = "pause"

[action.work]
runner = "local_fn"
params = { fn = "record" }
`

const budgetWarnTOML = `
[workflow]
name = "budgeted"

[workflow.budget]
period = "month"
limit_usd = 10.0
on_exceed = "warn"

[action.work]
runner = "local_fn"
params = { fn = "record" }
`

// budgetRecorder captures the budget instrumentation hooks. It embeds no base
// implementation: the engine only calls the two budget hooks on the
// governance path, and the other Instrumentation methods are reached through
// the nop default since this recorder is set only on Config.Instrumentation
// when the test wants to observe budget metrics.
type budgetRecorder struct {
	spends   []float64
	exceeded []string
	mu       sync.Mutex
}

func (*budgetRecorder) RunStarted(ctx context.Context, _ *store.Run) (context.Context, func(*store.Run)) {
	return ctx, func(*store.Run) {}
}

func (*budgetRecorder) ActionStarted(ctx context.Context, _ *workflow.Action) (context.Context, func(*store.ActionResult)) {
	return ctx, func(*store.ActionResult) {}
}

func (*budgetRecorder) ActionSettled(context.Context, *store.ActionResult) {}

func (*budgetRecorder) CallStarted(ctx context.Context, _ *engine.Call) (context.Context, func(runner.Usage, error)) {
	return ctx, func(runner.Usage, error) {}
}

func (r *budgetRecorder) BudgetSpend(_ context.Context, _ string, spend, _ float64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.spends = append(r.spends, spend)
}

func (r *budgetRecorder) BudgetExceeded(_ context.Context, _, action string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.exceeded = append(r.exceeded, action)
}

func (r *budgetRecorder) exceededActions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.exceeded...)
}

func (r *budgetRecorder) lastSpend() (float64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.spends) == 0 {
		return 0, false
	}

	return r.spends[len(r.spends)-1], true
}

// budgetNotifier records warn-mode and pause-mode budget notifications so a test
// can prove which path fired.
type budgetNotifier struct {
	warned []float64
	denied []float64
	mu     sync.Mutex
}

func (n *budgetNotifier) BudgetWarn(_ context.Context, _ string, spend, _ float64) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.warned = append(n.warned, spend)
}

func (n *budgetNotifier) BudgetDenied(_ context.Context, _ string, spend, _ float64) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.denied = append(n.denied, spend)
}

func (n *budgetNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()

	return len(n.warned)
}

func (n *budgetNotifier) deniedCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()

	return len(n.denied)
}

func (n *budgetNotifier) lastDenied() (float64, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if len(n.denied) == 0 {
		return 0, false
	}

	return n.denied[len(n.denied)-1], true
}

// newGovernedEngine builds an engine whose Budget and Pause checkers are the
// in-memory store fake, so governance reads its seeded spend and pause flags.
// It returns the engine and the fake for seeding and assertions.
func newGovernedEngine(t *testing.T, instr engine.Instrumentation, notifier engine.BudgetNotifier, now time.Time) (*engine.Engine, *fakeStore) {
	t.Helper()

	registry := runner.NewRegistry()

	if err := registry.Register(builtins.LocalFnName, builtins.LocalFnFactory(map[string]builtins.Func{"record": recordOK})); err != nil {
		t.Fatalf("register local_fn: %v", err)
	}

	if err := registry.BuildAll(nil); err != nil {
		t.Fatalf("build runners: %v", err)
	}

	db := newFakeStore()

	cfg := engine.Config{
		MaxConcurrentActions: 4,
		DefaultRetry:         reliability.Policy{MaxAttempts: 1},
		Breaker: reliability.BreakerConfig{
			FailureThreshold: 1,
			Window:           time.Minute,
			MinRequests:      1000,
			Cooldown:         time.Minute,
		},
		NewID:           sequentialIDs(),
		Clock:           func() time.Time { return now },
		Budget:          db,
		Pause:           db,
		Notifier:        notifier,
		Instrumentation: instr,
	}

	return engine.New(cfg, registry, db), db
}

// recordOK is a local_fn that succeeds, returning the action name.
func recordOK(_ context.Context, task *runner.Task) (any, error) {
	return task.ActionName, nil
}

// seedSpend stages a finished run of the budgeted workflow carrying cost so
// SpendSince has spend to accumulate. created is when the run was created, so
// tests place it inside or before the budget period.
func seedSpend(t *testing.T, db *fakeStore, runID string, cost float64, created time.Time) {
	t.Helper()

	ctx := t.Context()

	err := db.CreateRun(ctx, &store.Run{
		ID:           runID,
		WorkflowName: budgetWorkflow,
		Status:       store.RunSucceeded,
		Trigger:      triggerCLI,
		CreatedAt:    created,
	})
	if err != nil {
		t.Fatalf("seed run %s: %v", runID, err)
	}

	err = db.UpsertActionResult(ctx, &store.ActionResult{
		RunID:      runID,
		ActionName: actionWork,
		Runner:     builtins.LocalFnName,
		Status:     store.ActionSucceeded,
		Usage:      runner.Usage{CostUSD: cost},
	})
	if err != nil {
		t.Fatalf("seed action %s: %v", runID, err)
	}
}

func TestBudgetUnderLimitRuns(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	rec := &budgetRecorder{}
	eng, db := newGovernedEngine(t, rec, nil, now)

	// $4 already spent this month, under the $10 ceiling.
	seedSpend(t, db, "prior", 4.0, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))

	run, err := eng.RunWorkflow(t.Context(), parseFlow(t, budgetPauseTOML), nil, triggerCLI)
	if err != nil {
		t.Fatalf("under-budget run rejected: %v", err)
	}

	wantRunStatus(t, run, store.RunSucceeded)

	if spend, ok := rec.lastSpend(); !ok || spend != 4.0 {
		t.Errorf("budget spend gauge = %v (recorded %v), want 4.0", spend, ok)
	}

	if actions := rec.exceededActions(); len(actions) != 0 {
		t.Errorf("budget exceeded fired %v under budget, want none", actions)
	}
}

func TestBudgetPauseRefusesOverLimit(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	rec := &budgetRecorder{}
	eng, db := newGovernedEngine(t, rec, nil, now)

	// $12 spent this month, over the $10 ceiling.
	seedSpend(t, db, "over", 12.0, time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC))

	_, err := eng.RunWorkflow(t.Context(), parseFlow(t, budgetPauseTOML), nil, triggerCLI)
	if err == nil {
		t.Fatal("over-budget pause run was accepted, want refusal")
	}

	cerr, ok := errors.AsType[*cairnerr.Error](err)
	if !ok || cerr.Code != engine.CodeBudgetExceeded {
		t.Fatalf("error = %v, want code %s", err, engine.CodeBudgetExceeded)
	}

	if actions := rec.exceededActions(); len(actions) != 1 || actions[0] != workflow.BudgetOnExceedPause {
		t.Errorf("exceeded actions = %v, want one pause", actions)
	}

	// No run record was created for the refused run: only the seeded one.
	if got := db.runCount(); got != 1 {
		t.Errorf("run count = %d, want 1 (refused run creates no record)", got)
	}
}

func TestBudgetWarnAllowsAndNotifies(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	rec := &budgetRecorder{}
	notifier := &budgetNotifier{}
	eng, db := newGovernedEngine(t, rec, notifier, now)

	// $20 spent, over the $10 ceiling, but on_exceed is warn.
	seedSpend(t, db, "over", 20.0, time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC))

	run, err := eng.RunWorkflow(t.Context(), parseFlow(t, budgetWarnTOML), nil, triggerCLI)
	if err != nil {
		t.Fatalf("warn-budget run refused: %v", err)
	}

	wantRunStatus(t, run, store.RunSucceeded)

	if notifier.count() != 1 {
		t.Errorf("notifier fired %d times, want 1", notifier.count())
	}

	// The warn path never denies: BudgetDenied is the pause-only signal.
	if got := notifier.deniedCount(); got != 0 {
		t.Errorf("BudgetDenied fired %d times on warn, want 0", got)
	}

	if actions := rec.exceededActions(); len(actions) != 1 || actions[0] != workflow.BudgetOnExceedWarn {
		t.Errorf("exceeded actions = %v, want one warn", actions)
	}
}

func TestBudgetPauseDeniesAndNotifies(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	rec := &budgetRecorder{}
	notifier := &budgetNotifier{}
	eng, db := newGovernedEngine(t, rec, notifier, now)

	// $15 spent this month, over the $10 pause ceiling.
	seedSpend(t, db, "over", 15.0, time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC))

	_, err := eng.RunWorkflow(t.Context(), parseFlow(t, budgetPauseTOML), nil, triggerCLI)
	if err == nil {
		t.Fatal("over-budget pause run was accepted, want refusal")
	}

	cerr, ok := errors.AsType[*cairnerr.Error](err)
	if !ok || cerr.Code != engine.CodeBudgetExceeded {
		t.Fatalf("error = %v, want code %s", err, engine.CodeBudgetExceeded)
	}

	// The pause refusal fires BudgetDenied once with the over-limit spend, and
	// never BudgetWarn: denial and warning are distinct signals.
	if got := notifier.deniedCount(); got != 1 {
		t.Errorf("BudgetDenied fired %d times, want 1", got)
	}

	if spend, denied := notifier.lastDenied(); !denied || spend != 15.0 {
		t.Errorf("BudgetDenied spend = %v (fired %v), want 15.0", spend, denied)
	}

	if got := notifier.count(); got != 0 {
		t.Errorf("BudgetWarn fired %d times on pause, want 0", got)
	}
}

func TestBudgetPeriodWindowExcludesPriorPeriods(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	rec := &budgetRecorder{}
	eng, db := newGovernedEngine(t, rec, nil, now)

	// $30 spent last month falls outside the month period, so this month's
	// spend is zero and the pause-budget run proceeds.
	seedSpend(t, db, "last-month", 30.0, time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC))

	run, err := eng.RunWorkflow(t.Context(), parseFlow(t, budgetPauseTOML), nil, triggerCLI)
	if err != nil {
		t.Fatalf("run refused despite prior-period spend: %v", err)
	}

	wantRunStatus(t, run, store.RunSucceeded)

	if actions := rec.exceededActions(); len(actions) != 0 {
		t.Errorf("exceeded fired %v, want none (prior-period spend is out of window)", actions)
	}
}

func TestPausedWorkflowRefusesNewRuns(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	eng, db := newGovernedEngine(t, &budgetRecorder{}, nil, now)

	if err := db.SetWorkflowPause(t.Context(), &store.WorkflowPause{WorkflowName: budgetWorkflow, Actor: pauseActor}); err != nil {
		t.Fatalf("pause workflow: %v", err)
	}

	_, err := eng.RunWorkflow(t.Context(), parseFlow(t, budgetWarnTOML), nil, triggerCLI)
	if err == nil {
		t.Fatal("paused workflow run was accepted, want refusal")
	}

	cerr, ok := errors.AsType[*cairnerr.Error](err)
	if !ok || cerr.Code != engine.CodeWorkflowPaused {
		t.Fatalf("error = %v, want code %s", err, engine.CodeWorkflowPaused)
	}

	// Clearing the pause lets the workflow run again.
	if _, err := db.ClearWorkflowPause(t.Context(), budgetWorkflow); err != nil {
		t.Fatalf("clear pause: %v", err)
	}

	run := mustRun(t, eng, parseFlow(t, budgetWarnTOML), nil)
	wantRunStatus(t, run, store.RunSucceeded)
}
