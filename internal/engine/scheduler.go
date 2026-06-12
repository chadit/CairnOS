package engine

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
)

// depsResult classifies an action's upstream state at scheduling time.
type depsResult int

const (
	// depsWaiting means at least one upstream action has not finished.
	depsWaiting depsResult = iota
	// depsSucceeded means every upstream action succeeded.
	depsSucceeded
	// depsBlocked means an upstream action failed or was skipped, so this
	// action can never run and must be skipped; skip propagates downstream
	// the same way under halt and continue.
	depsBlocked
)

// actionEvent is one worker's terminal report back to the scheduling loop.
// The worker persists its own terminal row; the loop only updates scheduling
// state from the event.
type actionEvent struct {
	name   string
	status store.ActionStatus
	output any
	cerr   *cairnerr.Error
}

// runExec is the in-memory state of one run. The scheduling loop goroutine
// owns every mutable field; workers communicate only through the events
// channel, so no lock guards the maps.
type runExec struct {
	eng       *Engine
	flow      *workflow.Workflow
	run       *store.Run
	inputs    map[string]any
	order     []string
	statuses  map[string]store.ActionStatus
	outputs   map[string]any
	firstErr  *cairnerr.Error
	cancelRun context.CancelFunc
	events    chan actionEvent
	sem       chan struct{}
	wg        sync.WaitGroup
}

// newRunExec builds the scheduling state for one run, with every action
// pending and a semaphore sized to the engine's concurrency cap.
func newRunExec(eng *Engine, flow *workflow.Workflow, run *store.Run, inputs map[string]any, cancelRun context.CancelFunc) *runExec {
	statuses := make(map[string]store.ActionStatus, len(flow.Actions))
	for name := range flow.Actions {
		statuses[name] = store.ActionPending
	}

	return &runExec{
		eng:       eng,
		flow:      flow,
		run:       run,
		inputs:    inputs,
		order:     slices.Sorted(maps.Keys(flow.Actions)),
		statuses:  statuses,
		outputs:   make(map[string]any, len(flow.Actions)),
		cancelRun: cancelRun,
		events:    make(chan actionEvent),
		sem:       make(chan struct{}, eng.cfg.MaxConcurrentActions),
	}
}

// seed moves the run to running and persists a pending row for every action,
// so a crash before the first launch still leaves a complete picture.
func (x *runExec) seed(ctx context.Context) {
	x.run.StartedAt = time.Now()
	x.run.Status = store.RunRunning
	x.eng.persistRunStatus(ctx, x.run.ID, store.RunRunning, nil)
	x.eng.emit(&Event{RunID: x.run.ID, Type: EventRunStarted, Status: string(store.RunRunning)})

	for _, name := range x.order {
		x.eng.persistAction(ctx, &store.ActionResult{
			RunID:      x.run.ID,
			ActionName: name,
			Runner:     runnerLabel(x.flow.Actions[name]),
			Status:     store.ActionPending,
		})
	}
}

// loop implements the scheduling algorithm from the spec: launch every ready
// action, wait for a terminal event, apply it, and reschedule, until every
// action is terminal.
func (x *runExec) loop(ctx context.Context) {
	x.scheduleReady(ctx)

	for !x.allTerminal() {
		evt := <-x.events
		x.applyEvent(ctx, evt)
		x.scheduleReady(ctx)
	}

	x.wg.Wait()
}

// finalize settles the run's terminal state: canceled when the caller's
// context was canceled, failed on a run timeout or any failed action, and
// succeeded when every action ended succeeded or skipped.
func (x *runExec) finalize(parent, runCtx context.Context) {
	status, runErr := x.outcome(parent, runCtx)

	x.eng.persistRunStatus(runCtx, x.run.ID, status, runErr)

	x.run.Status = status
	x.run.Error = runErr
	x.run.FinishedAt = time.Now()

	x.eng.emit(&Event{RunID: x.run.ID, Type: EventRunFinished, Status: string(status), Err: runErr})
}

// outcome picks the terminal run state. Operator cancellation wins over
// everything so history can tell it apart from a timeout, which is a failure.
func (x *runExec) outcome(parent, runCtx context.Context) (store.RunStatus, *cairnerr.Error) {
	switch {
	case errors.Is(parent.Err(), context.Canceled):
		return store.RunCanceled, errf(CodeCanceled, "the run was canceled")
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return store.RunFailed, errf(CodeRunTimeout, "the run exceeded its %s timeout", x.flow.Timeout)
	case x.firstErr != nil:
		return store.RunFailed, x.firstErr
	default:
		return store.RunSucceeded, nil
	}
}

// scheduleReady sweeps pending actions until a sweep changes nothing, so a
// skip propagates transitively downstream in one call.
func (x *runExec) scheduleReady(ctx context.Context) {
	for x.scheduleOnce(ctx) {
	}
}

// scheduleOnce makes one pass over pending actions, reporting whether any
// action changed state.
func (x *runExec) scheduleOnce(ctx context.Context) bool {
	var changed bool

	for _, name := range x.order {
		if x.statuses[name] != store.ActionPending {
			continue
		}

		switch x.depsState(x.flow.Actions[name]) {
		case depsWaiting:
		case depsBlocked:
			x.markSkipped(ctx, name, "an upstream action failed or was skipped")

			changed = true
		case depsSucceeded:
			x.activate(ctx, name)

			changed = true
		}
	}

	return changed
}

// depsState reduces an action's upstream statuses to one scheduling answer.
func (x *runExec) depsState(action *workflow.Action) depsResult {
	state := depsSucceeded

	for _, dep := range action.DependsOn {
		switch x.statuses[dep] {
		case store.ActionFailed, store.ActionSkipped:
			return depsBlocked
		case store.ActionSucceeded:
		case store.ActionPending, store.ActionReady, store.ActionRunning:
			state = depsWaiting
		default:
			state = depsWaiting
		}
	}

	return state
}

// activate evaluates the action's condition at readiness time, then either
// skips it (false), fails it (evaluation error), or launches it.
func (x *runExec) activate(ctx context.Context, name string) {
	action := x.flow.Actions[name]

	if action.CondExpr != nil {
		pass, err := workflow.EvalBool(action.CondExpr, x.eng.resolver(ctx, x.inputs, x.outputs))
		if err != nil {
			x.failBeforeLaunch(ctx, name, asPrecondition(err))

			return
		}

		if !pass {
			x.markSkipped(ctx, name, "its condition evaluated false")

			return
		}
	}

	x.launch(ctx, action)
}

// launch marks the action ready and hands it to a worker goroutine. The
// outputs snapshot is safe because every upstream is already terminal.
func (x *runExec) launch(ctx context.Context, action *workflow.Action) {
	x.statuses[action.Name] = store.ActionReady
	x.eng.persistAction(ctx, &store.ActionResult{
		RunID:      x.run.ID,
		ActionName: action.Name,
		Runner:     runnerLabel(action),
		Status:     store.ActionReady,
	})

	snapshot := maps.Clone(x.outputs)

	x.wg.Go(func() {
		x.work(ctx, action, snapshot)
	})
}

// work is one worker goroutine: it waits for a semaphore slot (the engine's
// concurrency cap), runs the action, and reports the terminal event. A run
// that ends while the action still waits skips it, since it never started.
func (x *runExec) work(ctx context.Context, action *workflow.Action, snapshot map[string]any) {
	select {
	case x.sem <- struct{}{}:
	case <-ctx.Done():
		x.events <- x.eng.skipUnstarted(ctx, x.run.ID, action)

		return
	}

	defer func() { <-x.sem }()

	x.events <- x.eng.runAction(ctx, x.run.ID, action, x.inputs, snapshot)
}

// applyEvent folds one worker's terminal report into scheduling state.
func (x *runExec) applyEvent(ctx context.Context, evt actionEvent) {
	x.statuses[evt.name] = evt.status

	switch evt.status {
	case store.ActionSucceeded:
		x.outputs[evt.name] = evt.output
	case store.ActionFailed:
		x.noteFailure(ctx, evt.cerr)
	case store.ActionSkipped, store.ActionPending, store.ActionReady, store.ActionRunning:
		// a worker whose run ended before it could start reports skipped;
		// the status update above is all the bookkeeping it needs, and
		// workers never report the non-terminal states
	default:
	}
}

// noteFailure records the first failure as the run error and, under halt,
// cancels in-flight actions and skips everything still pending.
func (x *runExec) noteFailure(ctx context.Context, cerr *cairnerr.Error) {
	if x.firstErr == nil {
		x.firstErr = cerr
	}

	if x.flow.OnError != workflow.OnErrorHalt {
		return
	}

	x.cancelRun()
	x.skipPending(ctx, "the run halted after an action failed")
}

// failBeforeLaunch persists a failure that happened before the runner was
// called, such as a condition that could not evaluate.
func (x *runExec) failBeforeLaunch(ctx context.Context, name string, cerr *cairnerr.Error) {
	x.statuses[name] = store.ActionFailed

	result := &store.ActionResult{
		RunID:      x.run.ID,
		ActionName: name,
		Runner:     runnerLabel(x.flow.Actions[name]),
		Status:     store.ActionFailed,
		Error:      cerr,
		FinishedAt: time.Now(),
	}

	x.eng.persistAction(ctx, result)
	x.eng.cfg.Instrumentation.ActionSettled(ctx, result)
	x.noteFailure(ctx, cerr)
}

// markSkipped persists a skip with its reason, so history shows why the
// action correctly did not run.
func (x *runExec) markSkipped(ctx context.Context, name, reason string) {
	x.statuses[name] = store.ActionSkipped

	result := &store.ActionResult{
		RunID:      x.run.ID,
		ActionName: name,
		Runner:     runnerLabel(x.flow.Actions[name]),
		Status:     store.ActionSkipped,
		Error:      errf(CodeActionSkipped, "action %q skipped: %s", name, reason),
		FinishedAt: time.Now(),
	}

	x.eng.persistAction(ctx, result)
	x.eng.cfg.Instrumentation.ActionSettled(ctx, result)
}

// skipPending skips every action that has not been handed to a worker yet.
func (x *runExec) skipPending(ctx context.Context, reason string) {
	for _, name := range x.order {
		if x.statuses[name] == store.ActionPending {
			x.markSkipped(ctx, name, reason)
		}
	}
}

// allTerminal reports whether every action reached a terminal state.
func (x *runExec) allTerminal() bool {
	for _, status := range x.statuses {
		switch status {
		case store.ActionSucceeded, store.ActionFailed, store.ActionSkipped:
		case store.ActionPending, store.ActionReady, store.ActionRunning:
			return false
		default:
			return false
		}
	}

	return true
}

// executeDAG runs a created run to completion: it derives the run context
// from the workflow timeout, drives the scheduling loop, and finalizes the
// run record.
func (e *Engine) executeDAG(ctx context.Context, flow *workflow.Workflow, run *store.Run, inputs map[string]any) {
	runCtx, cancelRun := runContext(ctx, flow.Timeout)
	defer cancelRun()

	// The instrumented context is the one every action inherits, so the
	// run scope (span, metric labels) parents the whole tree; finishRun
	// fires after finalize so it observes the terminal status and error.
	runCtx, finishRun := e.cfg.Instrumentation.RunStarted(runCtx, run)

	exec := newRunExec(e, flow, run, inputs, cancelRun)

	exec.seed(runCtx)
	exec.loop(runCtx)
	exec.finalize(ctx, runCtx)

	finishRun(run)
}

// persistAction writes one action transition. The write context is detached
// from cancellation so a canceled or timed-out run still records its final
// states; a store failure is logged rather than crashing the run, since the
// in-memory state keeps driving scheduling. Every persisted transition also
// reaches the event hook, so API stream consumers see exactly what history
// records.
func (e *Engine) persistAction(ctx context.Context, result *store.ActionResult) {
	if err := e.store.UpsertActionResult(context.WithoutCancel(ctx), result); err != nil {
		slog.Error("persist action result",
			"run_id", result.RunID,
			"action_name", result.ActionName,
			"status", string(result.Status),
			"error", err)
	}

	e.emit(&Event{
		RunID:   result.RunID,
		Type:    EventActionTransition,
		Action:  result.ActionName,
		Status:  string(result.Status),
		Attempt: result.Attempt,
		Err:     result.Error,
	})
}

// persistRunStatus writes one run transition, detached from cancellation for
// the same reason as persistAction.
func (e *Engine) persistRunStatus(ctx context.Context, id string, status store.RunStatus, runErr *cairnerr.Error) {
	if err := e.store.UpdateRunStatus(context.WithoutCancel(ctx), id, status, runErr); err != nil {
		slog.Error("persist run status", "run_id", id, "status", string(status), "error", err)
	}
}

// runContext layers the workflow's whole-run timeout onto the caller's
// context. The cancel func doubles as the halt signal for in-flight actions.
func runContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}

	return context.WithCancel(ctx)
}

// runnerLabel names what executes the action for persisted rows: the runner
// for task actions, the driver for agent actions.
func runnerLabel(action *workflow.Action) string {
	if action.Kind == workflow.KindAgent {
		return action.Driver
	}

	return action.Runner
}
