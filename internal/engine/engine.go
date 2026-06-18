// Package engine turns a parsed workflow into a finished run: it builds the
// DAG, schedules ready actions under a concurrency cap, interpolates params,
// applies retry and circuit breaking around runner calls, and persists every
// state transition (.agents/spec/04-execution-engine.md).
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/reliability"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Canonical error codes the engine owns (.agents/spec/04-execution-engine.md,
// "Error model"). Load-time workflow codes live in internal/workflow.
const (
	// CodeCircuitOpen marks a call rejected because the runner's breaker is
	// open; the service was never touched.
	CodeCircuitOpen = "engine.circuit_open"
	// CodeRunTimeout marks a run that exceeded its workflow timeout. Timeouts
	// are failures, not cancellations, so history can tell them apart.
	CodeRunTimeout = "engine.run_timeout"
	// CodeCanceled marks an operator- or caller-initiated stop.
	CodeCanceled = "engine.canceled"
	// CodeAgentNotAvailable marks an agent action executed before Phase 1c
	// ships the agent loop.
	CodeAgentNotAvailable = "engine.agent_not_available"

	// CodePreconditionFailed marks an action that could not launch: an
	// unresolved reference, a missing upstream output, or bad resolved params.
	CodePreconditionFailed = "action.precondition_failed"
	// CodeActionTimeout marks an action that exceeded its own timeout.
	CodeActionTimeout = "action.timeout"
	// CodeActionSkipped annotates a skipped action's persisted row with why
	// it never ran. Skips never fail a run.
	CodeActionSkipped = "action.skipped"

	// CodeRunnerTransient, CodeRunnerPermanent, CodeRunnerRateLimited, and
	// CodeRunnerNeedsReauth carry a runner-reported failure, with the runner's
	// own code preserved in Details under runner_code.
	CodeRunnerTransient   = "runner.transient"
	CodeRunnerPermanent   = "runner.permanent"
	CodeRunnerRateLimited = "runner.rate_limited"
	CodeRunnerNeedsReauth = "runner.needs_reauth"

	// CodeConfigInvalid marks a reference rejected by engine configuration,
	// such as an env name outside the allowlist.
	CodeConfigInvalid = "config.invalid"
	// CodeMissingSecret marks a secrets reference with no resolvable value.
	CodeMissingSecret = "config.missing_secret" // #nosec G101 -- canonical error code, not a credential
)

const (
	// defaultMaxConcurrent caps in-flight runner calls when the config does
	// not say otherwise, per the spec's engine.max_concurrent_actions default.
	defaultMaxConcurrent = 4

	// idRandomBytes sizes the random suffix of fallback run IDs.
	idRandomBytes = 8
)

// SecretSource supplies the values behind ${{ secrets.* }} references. The
// CLI wires an env-backed implementation later; tests use a map fake.
type SecretSource interface {
	Get(ctx context.Context, name string) (string, error)
}

// AgentExecutor runs agent-kind actions: the Phase 1c loop
// (.agents/spec/15-agent-loop.md), injected as an interface so the engine
// never imports the loop package. Nil keeps the pre-1c behavior where agent
// actions fail with engine.agent_not_available.
type AgentExecutor interface {
	// ValidateAction applies the run-start half of agent validation: the
	// driver must be a registered runner implementing runner.ToolCaller, and
	// every tools entry a registered runner implementing runner.Describer.
	ValidateAction(action *workflow.Action) error

	// Execute drives the loop for one agent action. The action arrives with
	// its goal already interpolated; the task carries run identity, inputs,
	// and the action deadline. A returned *cairnerr.Error keeps its code
	// (the agent.* walls); anything else classifies like a runner failure.
	// The Result is meaningful even on error: it carries the usage spent
	// before the failure so cost accounting survives blown budgets.
	Execute(ctx context.Context, action *workflow.Action, task *runner.Task) (runner.Result, error)
}

// Config tunes one Engine.
type Config struct {
	// MaxConcurrentActions caps in-flight runner calls across the engine so
	// a burst of ready actions cannot exhaust rate limits or local CPU.
	// Values below one fall back to the spec default of four.
	MaxConcurrentActions int

	// DefaultTimeout bounds actions that declare no timeout of their own.
	// Zero means no per-action ceiling beyond the run's.
	DefaultTimeout time.Duration

	// DefaultRetry applies to actions without a retry table.
	DefaultRetry reliability.Policy

	// Fallback is the mixed-model chain keyed by primary runner name: when an
	// action's runner fails terminally, the engine tries the listed runners in
	// order before failing the action (.agents/spec/14-open-decisions.md, D12).
	// An absent entry means the action's runner runs alone, exactly as before.
	Fallback map[string][]string

	// Breaker is shared by every per-runner circuit breaker.
	Breaker reliability.BreakerConfig

	// EnvAllowlist names the only environment variables ${{ env.* }} may
	// read; anything else fails with config.invalid.
	EnvAllowlist []string

	// Secrets resolves ${{ secrets.* }} references. Nil means every secret
	// reference fails with config.missing_secret.
	Secrets SecretSource

	// AgentExecutor runs agent-kind actions (the Phase 1c loop). Nil makes
	// every agent action fail with engine.agent_not_available.
	AgentExecutor AgentExecutor

	// NewID mints run IDs. Injected so tests get repeatable IDs; nil falls
	// back to a timestamp-plus-random generator.
	NewID func() string

	// Clock supplies the current time for budget period-start math. Injected
	// so a fixed test clock makes the period boundary repeatable; nil means
	// time.Now.
	Clock func() time.Time

	// Budget sums a workflow's spend over a period so the engine can compare
	// it to the budget limit (.agents/spec/12-ui-and-governance.md,
	// "Budgets"). Nil turns budget enforcement off, the pre-governance
	// behavior; the store satisfies it.
	Budget BudgetChecker

	// Pause reports whether a workflow is paused so the engine refuses new
	// runs of it (.agents/spec/12-ui-and-governance.md, "Control actions").
	// Nil turns the pause check off; the store satisfies it.
	Pause PauseChecker

	// Notifier is fired when a warn-mode budget is exceeded so a run that
	// proceeds still raises an alert. Nil falls back to a slog warning; the
	// budget metric fires through Instrumentation regardless.
	Notifier BudgetNotifier

	// OnEvent observes run starts, action transitions, and run finishes, so
	// the API can stream a run live. Nil means no observer. The engine calls
	// it synchronously from scheduling and worker goroutines: the hook has
	// to come back promptly and never block, or it stalls the scheduler.
	OnEvent func(Event)

	// Instrumentation observes runs, actions, and runner calls for the
	// telemetry layer. Nil means no instrumentation. Unlike OnEvent, which
	// feeds the API's live event stream, these hooks return scope contexts
	// so spans nest; both hooks can be active at once.
	Instrumentation Instrumentation
}

// Engine executes workflows against a runner registry and a durable store.
// Safe for concurrent use; runs share the breaker set and the registry.
type Engine struct {
	cfg      Config
	registry *runner.Registry
	store    store.Store
	breakers *reliability.BreakerSet
}

// New builds an engine, normalizing config defaults.
//
//nolint:gocritic // hugeParam: New runs once per process and keeps its own Config copy, so the value pass is deliberate
func New(cfg Config, registry *runner.Registry, stateStore store.Store) *Engine {
	if cfg.MaxConcurrentActions <= 0 {
		cfg.MaxConcurrentActions = defaultMaxConcurrent
	}

	if cfg.NewID == nil {
		cfg.NewID = fallbackID
	}

	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}

	if cfg.Instrumentation == nil {
		cfg.Instrumentation = nopInstrumentation{}
	}

	return &Engine{
		cfg:      cfg,
		registry: registry,
		store:    stateStore,
		breakers: reliability.NewBreakerSet(cfg.Breaker),
	}
}

// BreakerStates snapshots every created circuit breaker's position by
// runner name, the read half of the admin breaker surface
// (.agents/spec/06-observability-and-tuning.md). Breakers are created on a
// runner's first call, so a runner with no traffic yet is absent.
func (e *Engine) BreakerStates() map[string]reliability.State {
	return e.breakers.States()
}

// OnBreakerTransition registers a callback fired on every breaker state
// change, feeding the circuit metrics. Register it before the engine sees
// traffic; it applies to breakers created from then on.
func (e *Engine) OnBreakerTransition(callback func(name string, to reliability.State)) {
	e.breakers.OnTransition(callback)
}

// ResetBreaker forces one runner's breaker CLOSED, reporting whether that
// runner had a breaker to reset. The admin API wires this to its
// ResetBreaker RPC.
func (e *Engine) ResetBreaker(name string) bool {
	return e.breakers.Reset(name)
}

// RunWorkflow executes one run of a parsed workflow: it validates the
// supplied inputs, checks every task runner is registered, creates the run
// record, executes the DAG, and finalizes the run. The returned run is
// terminal; its Status and Error carry the outcome, so a failed run returns
// a nil error here. A non-nil error means the run was rejected before any
// record was created.
func (e *Engine) RunWorkflow(ctx context.Context, flow *workflow.Workflow, inputs map[string]any, trigger string) (*store.Run, error) {
	if flow == nil {
		return nil, ErrNilWorkflow
	}

	validated, err := validateInputs(flow.Inputs, inputs)
	if err != nil {
		return nil, err
	}

	if err := e.checkRunners(flow, validated); err != nil {
		return nil, err
	}

	if err := e.checkGovernance(ctx, flow); err != nil {
		return nil, err
	}

	run := &store.Run{
		ID:           e.cfg.NewID(),
		WorkflowName: flow.Name,
		WorkflowHash: flow.Hash,
		WorkspaceID:  store.WorkspaceFromContext(ctx),
		Status:       store.RunPending,
		Inputs:       validated,
		Trigger:      trigger,
		CreatedAt:    time.Now(),
	}

	if err := e.store.CreateRun(ctx, run); err != nil {
		return nil, fmt.Errorf("create run: %w", err)
	}

	e.executeDAG(ctx, flow, run, validated)

	return run, nil
}

// checkRunners applies the run-start half of validation rule 7: every task
// action's runner must be registered with a structural Validate pass
// (Resolved false), and every agent action's driver and tools must clear
// the agent executor's capability checks. With no executor configured,
// agent actions fail at execution time with engine.agent_not_available
// instead.
func (e *Engine) checkRunners(flow *workflow.Workflow, inputs map[string]any) error {
	var errs cairnerr.Errors

	for _, name := range slices.Sorted(maps.Keys(flow.Actions)) {
		action := flow.Actions[name]
		if action.Kind != workflow.KindTask {
			errs = append(errs, e.checkAgentAction(action)...)

			continue
		}

		rnr, registered := e.registry.Get(action.Runner)
		if !registered {
			errs = append(errs, errf(workflow.CodeInvalid,
				"action %q needs runner %q, which is not registered", name, action.Runner))

			continue
		}

		for _, provider := range action.Fallback {
			if _, ok := e.registry.Get(provider); !ok {
				errs = append(errs, errf(workflow.CodeInvalid,
					"action %q lists fallback runner %q, which is not registered", name, provider))
			}
		}

		task := &runner.Task{
			ActionName: name,
			Params:     action.Params,
			Inputs:     inputs,
			Idempotent: action.Idempotent,
			Resolved:   false,
		}

		if err := rnr.Validate(task); err != nil {
			errs = append(errs, errf(workflow.CodeInvalid, "action %q params: %v", name, err))
		}
	}

	if len(errs) > 0 {
		return errs
	}

	return nil
}

// checkAgentAction delegates an agent action's run-start checks to the
// configured executor, normalizing whatever error shape it returns into the
// run-rejection list. Nil executor means no checks here: the action fails
// at execution time with engine.agent_not_available, the pre-1c behavior.
func (e *Engine) checkAgentAction(action *workflow.Action) cairnerr.Errors {
	if e.cfg.AgentExecutor == nil {
		return nil
	}

	err := e.cfg.AgentExecutor.ValidateAction(action)
	if err == nil {
		return nil
	}

	if list, isList := errors.AsType[cairnerr.Errors](err); isList {
		return list
	}

	if cerr, isCanonical := errors.AsType[*cairnerr.Error](err); isCanonical {
		return cairnerr.Errors{cerr}
	}

	return cairnerr.Errors{errf(workflow.CodeInvalid, "agent action %q: %v", action.Name, err)}
}

// validateInputs applies the run-input rules from the workflow schema:
// unknown keys are rejected, required inputs must be present, declared types
// must match, and defaults fill what the caller omitted.
func validateInputs(decls map[string]workflow.InputDecl, supplied map[string]any) (map[string]any, error) {
	var errs cairnerr.Errors

	for _, name := range slices.Sorted(maps.Keys(supplied)) {
		if _, declared := decls[name]; !declared {
			errs = append(errs, errf(workflow.CodeInvalid, "unknown input %q", name))
		}
	}

	validated := make(map[string]any, len(decls))

	for _, name := range slices.Sorted(maps.Keys(decls)) {
		decl := decls[name]

		value, given := supplied[name]
		if !given {
			if decl.HasDefault {
				validated[name] = decl.Default

				continue
			}

			if decl.Required {
				errs = append(errs, errf(workflow.CodeInvalid, "required input %q was not supplied", name))
			}

			continue
		}

		if !inputMatches(decl.Type, value) {
			errs = append(errs, errf(workflow.CodeInvalid,
				"input %q must be %s, got %T", name, decl.Type, value))

			continue
		}

		validated[name] = value
	}

	if len(errs) > 0 {
		return nil, errs
	}

	return validated, nil
}

// inputMatches checks a supplied value against a declared input type,
// accepting the native Go shapes callers produce (TOML int64, JSON float64,
// CLI-typed values).
func inputMatches(typeName string, value any) bool {
	switch typeName {
	case workflow.TypeString:
		_, ok := value.(string)

		return ok
	case workflow.TypeInt:
		return matchesInt(value)
	case workflow.TypeFloat:
		return matchesFloat(value)
	case workflow.TypeBool:
		_, ok := value.(bool)

		return ok
	case workflow.TypeStringSlice:
		return matchesStrings(value)
	default:
		return false
	}
}

func matchesInt(value any) bool {
	switch value.(type) {
	case int, int64:
		return true
	default:
		return false
	}
}

func matchesFloat(value any) bool {
	switch value.(type) {
	case float64, int, int64:
		return true
	default:
		return false
	}
}

func matchesStrings(value any) bool {
	switch typed := value.(type) {
	case []string:
		return true
	case []any:
		for _, item := range typed {
			if _, isStr := item.(string); !isStr {
				return false
			}
		}

		return true
	default:
		return false
	}
}

// fallbackID mints a time-sortable run ID when no generator is injected.
func fallbackID() string {
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")

	var buf [idRandomBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand failure is unrecoverable per its contract; degrade to
		// the timestamp alone rather than panicking in library code.
		return stamp
	}

	return stamp + "-" + hex.EncodeToString(buf[:])
}
