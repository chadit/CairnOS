package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/reliability"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// attemptOutcome is everything one action execution produced. waiting is set
// only by an approval action that parked pending a human decision: the action
// neither succeeded nor failed, so runAction records it as ActionWaiting and
// the run quiesces to held.
type attemptOutcome struct {
	cerr     *cairnerr.Error
	result   runner.Result
	attempts int
	waiting  bool
}

// runAction is the worker side of one action once it holds a semaphore
// slot: it persists the running transition, runs the attempt pipeline for
// the action's kind, persists the terminal row, and reports back to the
// scheduling loop. Agent rows carry the driver name in the Runner column.
func (e *Engine) runAction(ctx context.Context, runID string, action *workflow.Action, inputs, outputs map[string]any) actionEvent {
	if action.Kind == workflow.KindAgent && e.cfg.AgentExecutor == nil {
		return e.failAgentAction(ctx, runID, action)
	}

	// The instrumented context flows into the attempt pipeline, so runner
	// calls and agent-loop iterations nest under this action's scope.
	ctx, finishAction := e.cfg.Instrumentation.ActionStarted(ctx, action)

	started := time.Now()
	e.persistAction(ctx, &store.ActionResult{
		RunID:      runID,
		ActionName: action.Name,
		Runner:     runnerLabel(action),
		Status:     store.ActionRunning,
		StartedAt:  started,
	})

	outcome := e.attemptFor(ctx, runID, action, inputs, outputs)

	if outcome.waiting {
		return e.parkAction(ctx, runID, action, started, finishAction)
	}

	status := store.ActionSucceeded
	if outcome.cerr != nil {
		status = store.ActionFailed
	}

	terminal := &store.ActionResult{
		RunID:      runID,
		ActionName: action.Name,
		Runner:     runnerLabel(action),
		Status:     status,
		Attempt:    outcome.attempts,
		Output:     outcome.result.Output,
		Usage:      outcome.result.Usage,
		Meta:       outcome.result.Meta,
		Error:      outcome.cerr,
		StartedAt:  started,
		FinishedAt: time.Now(),
	}

	e.persistAction(ctx, terminal)
	finishAction(terminal)

	return actionEvent{name: action.Name, status: status, output: outcome.result.Output, cerr: outcome.cerr}
}

// attemptFor routes one running action to the attempt pipeline its kind
// uses.
func (e *Engine) attemptFor(ctx context.Context, runID string, action *workflow.Action, inputs, outputs map[string]any) attemptOutcome {
	if action.Kind == workflow.KindAgent {
		return e.agentAttempt(ctx, runID, action, inputs, outputs)
	}

	if action.Kind == workflow.KindWorkflow {
		return e.workflowCallAttempt(ctx, runID, action, inputs, outputs)
	}

	if action.Kind == workflow.KindApproval {
		return e.approvalAttempt(ctx, runID, action, inputs, outputs)
	}

	if action.Foreach != nil {
		return e.foreachAttempt(ctx, runID, action, inputs, outputs)
	}

	return e.attempt(ctx, runID, action, inputs, outputs)
}

// attempt is the single-action pipeline: resolve params, validate them
// against the runner, pass the circuit breaker, run with retry, record the
// breaker outcome, and classify any failure.
func (e *Engine) attempt(ctx context.Context, runID string, action *workflow.Action, inputs, outputs map[string]any) attemptOutcome {
	params, resolveErr := e.resolveParams(ctx, action.Params, inputs, outputs)
	if resolveErr != nil {
		return attemptOutcome{cerr: asPrecondition(resolveErr)}
	}

	return e.runResolvedParams(ctx, runID, action, inputs, params)
}

// runResolvedParams is the runner half of a task attempt once params are
// resolved: look up the runner, validate the params, and run the fallback
// chain with retry and breaker. Split from attempt so a foreach item can
// share it after resolving item and index into its own params.
func (e *Engine) runResolvedParams(ctx context.Context, runID string, action *workflow.Action, inputs, params map[string]any) attemptOutcome {
	rnr, registered := e.registry.Get(action.Runner)
	if !registered {
		// checked at run start; a runner vanishing mid-run is still a launch
		// precondition failure rather than a runner-reported one
		return attemptOutcome{cerr: errf(CodePreconditionFailed, "runner %q is not registered", action.Runner)}
	}

	actCtx, cancel, deadline := actionContext(ctx, action.Timeout, e.cfg.DefaultTimeout)
	defer cancel()

	task := &runner.Task{
		RunID:      runID,
		ActionName: action.Name,
		Definition: action.Definition,
		Params:     params,
		Inputs:     inputs,
		Idempotent: action.Idempotent,
		Resolved:   true,
		Deadline:   deadline,
	}

	if err := rnr.Validate(task); err != nil {
		return attemptOutcome{cerr: errf(CodePreconditionFailed, "action %q params: %v", action.Name, err)}
	}

	// The primary runs first; configured fallbacks follow only when it fails
	// terminally. An open breaker or runner failure on one provider falls through
	// to the next, so a dead provider does not fail an action another can serve.
	var attempts int

	result, execErr := reliability.Fallback(actCtx, e.fallbackChain(action),
		func(callCtx context.Context, provider string) (runner.Result, error) {
			providerResult, providerAttempts, providerErr := e.runProvider(callCtx, action, provider, task)
			attempts = providerAttempts

			return providerResult, providerErr
		})
	if execErr != nil {
		return attemptOutcome{result: result, attempts: attempts, cerr: classifyFailure(ctx, actCtx, action, execErr)}
	}

	return attemptOutcome{result: result, attempts: attempts}
}

// fallbackChain is the ordered provider list for an action: its own runner
// first, then its fallback providers (D12). A per-action fallback list replaces
// the engine's [engine.fallback] default for that runner; with neither set the
// chain is just the runner, so a single-provider action runs as it did before.
func (e *Engine) fallbackChain(action *workflow.Action) []string {
	fallbacks := action.Fallback
	if len(fallbacks) == 0 {
		fallbacks = e.cfg.Fallback[action.Runner]
	}

	if len(fallbacks) == 0 {
		return []string{action.Runner}
	}

	chain := make([]string, 0, len(fallbacks)+1)
	chain = append(chain, action.Runner)
	chain = append(chain, fallbacks...)

	return chain
}

// runProvider runs one provider through its own breaker, retry, and execute, the
// per-provider unit the fallback chain drives. A returned error (an unregistered
// fallback, an open breaker, or a runner failure) falls through to the next
// provider; the breaker outcome and the attempt count belong to this provider.
func (e *Engine) runProvider(actCtx context.Context, action *workflow.Action, provider string, task *runner.Task) (runner.Result, int, error) {
	rnr, registered := e.registry.Get(provider)
	if !registered {
		return runner.Result{}, 0, errf(CodePreconditionFailed, "fallback runner %q is not registered", provider)
	}

	breaker := e.breakers.Get(provider)
	if err := breaker.Allow(); err != nil {
		return runner.Result{}, 0, fmt.Errorf("runner %q: %w", provider, err)
	}

	// Retry invokes the closure sequentially, so the bare counter is safe;
	// it starts at -1 because the spec's trace numbers attempts from zero.
	attemptIndex := -1

	result, attempts, execErr := reliability.Retry(actCtx, e.retryPolicy(action), action.Idempotent,
		func(callCtx context.Context) (runner.Result, error) {
			attemptIndex++

			callCtx, finishCall := e.cfg.Instrumentation.CallStarted(callCtx, &Call{
				Runner:       provider,
				Action:       action.Name,
				Attempt:      attemptIndex,
				BreakerState: breaker.State().String(),
			})

			callResult, callErr := rnr.Execute(callCtx, task)

			finishCall(callResult.Usage, callErr)

			if callErr != nil {
				return callResult, fmt.Errorf("runner %q: %w", provider, callErr)
			}

			return callResult, nil
		})

	breaker.Record(execErr == nil)

	result.Usage.Retries = attempts - 1

	// execErr is already wrapped with the provider name inside the Retry closure.
	return result, attempts, execErr //nolint:wrapcheck // wrapped in the closure above
}

// agentAttempt is the agent-kind counterpart of attempt: resolve the goal,
// derive the action deadline, and delegate to the configured AgentExecutor.
// No retry, breaker, or runner Validate wraps the loop: budget walls are its
// failure story, re-running a half-spent loop would double spend, and the loop
// validates its driver and tools itself. attempts stays 1 so Usage.Retries
// reads zero.
func (e *Engine) agentAttempt(ctx context.Context, runID string, action *workflow.Action, inputs, outputs map[string]any) attemptOutcome {
	goal, err := e.resolveGoal(ctx, action, inputs, outputs)
	if err != nil {
		return attemptOutcome{attempts: 1, cerr: asPrecondition(err)}
	}

	actCtx, cancel, deadline := actionContext(ctx, action.Timeout, e.cfg.DefaultTimeout)
	defer cancel()

	resolved := *action
	resolved.Goal = goal

	task := &runner.Task{
		RunID:      runID,
		ActionName: action.Name,
		Inputs:     inputs,
		Resolved:   true,
		Deadline:   deadline,
	}

	result, execErr := e.cfg.AgentExecutor.Execute(actCtx, &resolved, task)
	if execErr != nil {
		return attemptOutcome{result: result, attempts: 1, cerr: classifyFailure(ctx, actCtx, action, execErr)}
	}

	return attemptOutcome{result: result, attempts: 1}
}

// retryPolicy maps the action's retry table onto the reliability policy,
// falling back to the engine default when the action declares none.
func (e *Engine) retryPolicy(action *workflow.Action) reliability.Policy {
	if action.Retry == nil {
		return e.cfg.DefaultRetry
	}

	return reliability.Policy{
		MaxAttempts: action.Retry.MaxAttempts,
		Backoff:     action.Retry.Backoff,
		BaseDelay:   action.Retry.BaseDelay,
		MaxDelay:    action.Retry.MaxDelay,
		Jitter:      action.Retry.Jitter,
	}
}

// failAgentAction fails an agent-kind action cleanly when the embedder
// wired no agent executor onto the engine config.
func (e *Engine) failAgentAction(ctx context.Context, runID string, action *workflow.Action) actionEvent {
	cerr := errf(CodeAgentNotAvailable,
		"action %q needs the agent loop, but no agent executor is configured on this engine", action.Name)

	result := &store.ActionResult{
		RunID:      runID,
		ActionName: action.Name,
		Runner:     runnerLabel(action),
		Status:     store.ActionFailed,
		Error:      cerr,
		FinishedAt: time.Now(),
	}

	e.persistAction(ctx, result)
	e.cfg.Instrumentation.ActionSettled(ctx, result)

	return actionEvent{name: action.Name, status: store.ActionFailed, cerr: cerr}
}

// skipUnstarted records an action whose run ended while it waited for a
// semaphore slot. It never started, so it skips rather than fails.
func (e *Engine) skipUnstarted(ctx context.Context, runID string, action *workflow.Action) actionEvent {
	cerr := errf(CodeActionSkipped, "action %q never started: the run ended first", action.Name)

	result := &store.ActionResult{
		RunID:      runID,
		ActionName: action.Name,
		Runner:     action.Runner,
		Status:     store.ActionSkipped,
		Error:      cerr,
		FinishedAt: time.Now(),
	}

	e.persistAction(ctx, result)
	e.cfg.Instrumentation.ActionSettled(ctx, result)

	return actionEvent{name: action.Name, status: store.ActionSkipped, cerr: cerr}
}

// actionContext derives the per-action deadline: the action's own timeout,
// falling back to the engine default, layered onto the run context. The
// returned deadline feeds Task.Deadline so runners can budget their work.
func actionContext(ctx context.Context, timeout, fallback time.Duration) (context.Context, context.CancelFunc, time.Time) {
	if timeout <= 0 {
		timeout = fallback
	}

	if timeout <= 0 {
		deadline, _ := ctx.Deadline()

		return ctx, func() {}, deadline
	}

	actCtx, cancel := context.WithTimeout(ctx, timeout)
	deadline, _ := actCtx.Deadline()

	return actCtx, cancel, deadline
}

// classifyFailure maps an execution failure to its canonical code. Run-level
// endings win: a cancellation or run timeout explains the failure better
// than whatever error the interrupted runner happened to return.
func classifyFailure(runCtx, actCtx context.Context, action *workflow.Action, err error) *cairnerr.Error {
	switch {
	case errors.Is(runCtx.Err(), context.Canceled):
		return errf(CodeCanceled, "action %q stopped: the run was canceled", action.Name)
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return errf(CodeRunTimeout, "action %q stopped: the run timed out", action.Name)
	case errors.Is(actCtx.Err(), context.DeadlineExceeded):
		return errf(CodeActionTimeout, "action %q exceeded its timeout", action.Name)
	}

	// Agent-loop walls arrive as canonical errors; their codes pass through
	// so agent.max_iterations and friends survive classification.
	if canonical, isCanonical := errors.AsType[*cairnerr.Error](err); isCanonical {
		return canonical
	}

	// A classified runner failure wins over a bare circuit-open: a real
	// rejection (or a re-auth need) from one provider is more actionable than
	// another provider's open breaker.
	if classified, isClassified := errors.AsType[*runner.Error](err); isClassified {
		cerr := errf(classCode(classified.Class), "action %q: %s", action.Name, classified.Message)
		cerr.Details = map[string]any{"runner_code": classified.Code}

		return cerr
	}

	// A chain that failed only because every provider's breaker was open
	// surfaces as circuit-open rather than a generic permanent failure.
	if errors.Is(err, reliability.ErrCircuitOpen) {
		return errf(CodeCircuitOpen, "action %q: %v", action.Name, err)
	}

	return errf(CodeRunnerPermanent, "action %q failed: %v", action.Name, err)
}

// classCode maps a runner error class to its canonical runner.* code.
func classCode(class runner.ErrorClass) string {
	switch class {
	case runner.ClassTransient:
		return CodeRunnerTransient
	case runner.ClassRateLimited:
		return CodeRunnerRateLimited
	case runner.ClassNeedsReauth:
		return CodeRunnerNeedsReauth
	case runner.ClassPermanent:
		return CodeRunnerPermanent
	default:
		return CodeRunnerPermanent
	}
}
