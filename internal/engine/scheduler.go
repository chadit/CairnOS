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
	output any
	cerr   *cairnerr.Error
	name   string
	status store.ActionStatus
}

// seedEntry primes one action's already-known output into a new run's
// scheduling state so the loop treats it as succeeded and never launches it.
// Replay carries a prior run's succeeded outputs forward this way; resume
// reloads a held run's outputs the same way.
type seedEntry struct {
	Output any
}

// runExec is the in-memory state of one run. The scheduling loop goroutine
// owns every mutable field; workers communicate only through the events
// channel, so no lock guards the maps.
type runExec struct {
	outputs   map[string]any
	cancelRun context.CancelFunc
	run       *store.Run
	inputs    map[string]any
	sem       chan struct{}
	statuses  map[string]store.ActionStatus
	flow      *workflow.Workflow
	events    chan actionEvent
	eng       *Engine
	firstErr  *cairnerr.Error
	seeds     map[string]seedEntry

	// firstErrAction names the action behind firstErr, so an error-workflow
	// fire can report which stage failed. It stays empty when the run fails
	// without an action failure, such as a run timeout.
	firstErrAction string

	order        []string
	wg           sync.WaitGroup
	persistSeeds bool
}

// newRunExec builds the scheduling state for one run, with every action
// pending and a semaphore sized to the engine's concurrency cap. seeds
// pre-populates actions whose output is already known (replay, resume).
// persistSeeds true gives those actions a persisted succeeded row (a replay's
// new run); false primes only in-memory state so the original run's rows stay
// byte-identical (a resume).
func newRunExec(eng *Engine, flow *workflow.Workflow, run *store.Run, inputs map[string]any, cancelRun context.CancelFunc, seeds map[string]seedEntry, persistSeeds bool) *runExec {
	statuses := make(map[string]store.ActionStatus, len(flow.Actions))
	for name := range flow.Actions {
		statuses[name] = store.ActionPending
	}

	return &runExec{
		eng:          eng,
		flow:         flow,
		run:          run,
		inputs:       inputs,
		order:        slices.Sorted(maps.Keys(flow.Actions)),
		statuses:     statuses,
		outputs:      make(map[string]any, len(flow.Actions)),
		seeds:        seeds,
		persistSeeds: persistSeeds,
		cancelRun:    cancelRun,
		events:       make(chan actionEvent),
		sem:          make(chan struct{}, eng.cfg.MaxConcurrentActions),
	}
}

// seed moves the run to running and persists a pending row for every action,
// so a crash before the first launch still leaves a complete picture. A seeded
// action is primed succeeded instead, so the loop carries its output forward
// without ever launching it.
func (x *runExec) seed(ctx context.Context) {
	x.run.StartedAt = time.Now()
	x.run.Status = store.RunRunning
	x.eng.persistRunStatus(ctx, x.run.ID, store.RunRunning, nil)
	x.eng.emit(&Event{RunID: x.run.ID, Type: EventRunStarted, Status: string(store.RunRunning)})

	for _, name := range x.order {
		if entry, seeded := x.seeds[name]; seeded {
			x.seedAction(ctx, name, entry)

			continue
		}

		x.eng.persistAction(ctx, &store.ActionResult{
			RunID:      x.run.ID,
			ActionName: name,
			Runner:     runnerLabel(x.flow.Actions[name]),
			Status:     store.ActionPending,
		})
	}
}

// seedAction primes a seeded action's output into scheduling state so the loop
// treats it as already succeeded. A persisted seed (replay) also writes the
// succeeded row stamped with the run it was carried from; an in-memory seed
// (resume) writes nothing, because the original run's succeeded row already
// exists and must stay byte-identical.
func (x *runExec) seedAction(ctx context.Context, name string, entry seedEntry) {
	x.statuses[name] = store.ActionSucceeded
	x.outputs[name] = entry.Output

	if !x.persistSeeds {
		return
	}

	x.eng.persistAction(ctx, &store.ActionResult{
		RunID:      x.run.ID,
		ActionName: name,
		Runner:     runnerLabel(x.flow.Actions[name]),
		Status:     store.ActionSucceeded,
		Output:     entry.Output,
		Meta:       map[string]string{metaReplayedFrom: x.run.ParentRunID},
		FinishedAt: time.Now(),
	})
}

// loop implements the scheduling algorithm from the spec: launch every ready
// action, wait for a terminal event, apply it, and reschedule, until every
// action is terminal.
func (x *runExec) loop(ctx context.Context) {
	x.scheduleReady(ctx)

	// The loop runs until every action is terminal, with one exception: nothing
	// in flight but an action parked on a human decision quiesces the run, and
	// the break lets finalize settle it as held. Every in-flight branch still
	// reports first, since inFlight stays true until the last worker's event
	// lands.
	for !x.allTerminal() {
		if !x.inFlight() {
			break
		}

		evt := <-x.events
		x.applyEvent(ctx, evt)
		x.scheduleReady(ctx)
	}

	x.wg.Wait()
}

// inFlight reports whether any action is launched and still owes a terminal
// event, so the loop keeps receiving until the last worker reports. An approval
// that parks reports ActionWaiting and stops being in flight, which is how a run
// reaches quiescence and parks.
func (x *runExec) inFlight() bool {
	for _, status := range x.statuses {
		switch status {
		case store.ActionReady, store.ActionRunning:
			return true
		case store.ActionPending, store.ActionWaiting, store.ActionSucceeded, store.ActionFailed, store.ActionSkipped:
		}
	}

	return false
}

// anyWaiting reports whether an action is parked waiting on a human decision,
// the signal outcome reads to finalize a quiesced run as held.
func (x *runExec) anyWaiting() bool {
	for _, status := range x.statuses {
		if status == store.ActionWaiting {
			return true
		}
	}

	return false
}

// finalize settles the run's terminal state: canceled when the caller's
// context was canceled, failed on a run timeout or any failed action, and
// succeeded when every action ended succeeded or skipped.
func (x *runExec) finalize(parent, runCtx context.Context) {
	status, runErr := x.outcome(parent, runCtx)

	x.eng.persistRunStatus(runCtx, x.run.ID, status, runErr)

	x.run.Status = status
	x.run.Error = runErr

	// A held run is parked, not finished: leave FinishedAt zero so the in-memory
	// run matches the store, which never stamps finished_at for a non-terminal
	// status.
	if status != store.RunHeld {
		x.run.FinishedAt = time.Now()
	}

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
	case x.anyWaiting():
		// Cancel and failure win above; a run that only quiesced on a pending
		// approval parks as held, carrying no error since it has not failed.
		return store.RunHeld, nil
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
		case store.ActionPending, store.ActionReady, store.ActionRunning, store.ActionWaiting:
			// A parked (waiting) upstream is not terminal, so a downstream action
			// cannot proceed until the approval resumes and it succeeds.
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
		x.noteFailure(ctx, evt.name, evt.cerr)
	case store.ActionWaiting, store.ActionSkipped, store.ActionPending, store.ActionReady, store.ActionRunning:
		// An approval worker that parked reports waiting, and a worker whose run
		// ended before it started reports skipped; the status update above is all
		// the bookkeeping either needs, and workers never report the other
		// non-terminal states
	default:
	}
}

// noteFailure records the first failure as the run error and, under halt,
// cancels in-flight actions and skips everything still pending. name is the
// failing action, kept alongside firstErr so an error-workflow fire can name
// the stage that failed.
func (x *runExec) noteFailure(ctx context.Context, name string, cerr *cairnerr.Error) {
	if x.firstErr == nil {
		x.firstErr = cerr
		x.firstErrAction = name
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
	x.noteFailure(ctx, name, cerr)
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
		case store.ActionPending, store.ActionReady, store.ActionRunning, store.ActionWaiting:
			return false
		default:
			return false
		}
	}

	return true
}

// executeDAG runs a created run to completion from scratch, with no seeded
// actions: every action starts pending and runs through the loop.
func (e *Engine) executeDAG(ctx context.Context, flow *workflow.Workflow, run *store.Run, inputs map[string]any) {
	e.executeSeededDAG(ctx, flow, run, inputs, nil, false)
}

// executeSeededDAG runs a created run to completion: it derives the run
// context from the workflow timeout, seeds any pre-known action outputs,
// drives the scheduling loop, and finalizes the run record. seeds and
// persistSeeds pass straight through to newRunExec, so a replay carries a
// prior run's succeeded outputs forward without re-invoking their runners.
func (e *Engine) executeSeededDAG(ctx context.Context, flow *workflow.Workflow, run *store.Run, inputs map[string]any, seeds map[string]seedEntry, persistSeeds bool) {
	// Stamp the call context every action inherits: this run owns any child a
	// workflow-call action spawns, so it becomes the child's parent, and this
	// run's name extends the ancestry chain the depth cap prints. A root run
	// starts from the zero callInfo; a workflow-call child inherits the depth
	// and ancestry the caller set.
	incoming := callInfoFrom(ctx)
	ctx = withCallInfo(ctx, callInfo{
		parentRunID: run.ID,
		depth:       incoming.depth,
		ancestry:    append(slices.Clone(incoming.ancestry), flow.Name),
	})

	runCtx, cancelRun := runContext(ctx, flow.Timeout)
	defer cancelRun()

	// The instrumented context is the one every action inherits, so the
	// run scope (span, metric labels) parents the whole tree; finishRun
	// fires after finalize so it observes the terminal status and error.
	runCtx, finishRun := e.cfg.Instrumentation.RunStarted(runCtx, run)

	exec := newRunExec(e, flow, run, inputs, cancelRun, seeds, persistSeeds)

	exec.seed(runCtx)
	exec.loop(runCtx)
	exec.finalize(ctx, runCtx)

	finishRun(run)

	// finalize already persisted the terminal status, so the error-workflow
	// fire runs after every watcher has seen the failure and cannot race a
	// one-shot process exit. It reads exec.firstErrAction to name the stage
	// that failed.
	e.fireErrorWorkflow(ctx, flow, run, exec.firstErrAction)
}

// triggerError marks a run created by an error-workflow fire. A run already
// carrying this trigger never fires its own error workflow, the n8n loop guard.
const triggerError = "error"

// fireErrorWorkflow runs a failed run's declared error_workflow to completion,
// synchronously, before executeSeededDAG returns. Only a failed run fires, and
// only when it was not itself an error run. The fire never alters the original
// run's terminal state: a missing source, an unresolvable name, a rejection, or
// a failing error run each log at error level and return.
func (e *Engine) fireErrorWorkflow(ctx context.Context, flow *workflow.Workflow, run *store.Run, failedAction string) {
	if run.Status != store.RunFailed || run.Trigger == triggerError || flow.ErrorWorkflow == "" {
		return
	}

	if e.cfg.Workflows == nil {
		slog.Error("error workflow not fired: no workflow source configured on the engine",
			"run_id", run.ID, "workflow", flow.Name, "error_workflow", flow.ErrorWorkflow)

		return
	}

	child, err := e.cfg.Workflows.Resolve(flow.ErrorWorkflow)
	if err != nil {
		slog.Error("error workflow not fired: resolve failed",
			"run_id", run.ID, "workflow", flow.Name, "error_workflow", flow.ErrorWorkflow, "error", err)

		return
	}

	// WithoutCancel detaches the fire from a timed-out or canceled parent so the
	// error run still reports; it copies context values, keeping the ancestry
	// key the child inherits and the parent_run_id this run stamped.
	fireCtx := context.WithoutCancel(ctx)

	childRun, err := e.RunWorkflow(fireCtx, child, failureInputs(child, run, failedAction), triggerError)
	if err != nil {
		slog.Error("error workflow rejected before any run record",
			"run_id", run.ID, "error_workflow", child.Name, "error", err)

		return
	}

	if childRun.Status != store.RunSucceeded {
		slog.Error("error workflow did not succeed",
			"run_id", run.ID, "error_run_id", childRun.ID, "status", string(childRun.Status), "error", childRun.Error)
	}
}

// failureInputs builds the failure-context inputs an error workflow receives,
// keeping only the keys it declares because validateInputs rejects unknown
// ones. An error workflow declaring none of them gets an empty map.
func failureInputs(child *workflow.Workflow, run *store.Run, failedAction string) map[string]any {
	full := failureContext(run, failedAction)

	inputs := make(map[string]any, len(full))

	for key, value := range full {
		if _, declared := child.Inputs[key]; declared {
			inputs[key] = value
		}
	}

	return inputs
}

// failureContext is the full set of fields an error workflow may declare out of
// its failed parent: the run id, the workflow name, the action that failed, and
// the canonical code and message behind the failure.
func failureContext(run *store.Run, failedAction string) map[string]any {
	var code, message string

	if run.Error != nil {
		code = run.Error.Code
		message = run.Error.Message
	}

	return map[string]any{
		fieldRunID:      run.ID,
		"workflow":      run.WorkflowName,
		"failed_action": failedAction,
		"error_code":    code,
		"error_message": message,
	}
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
