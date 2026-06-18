package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/reliability"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Metric catalog names (.agents/spec/06-observability-and-tuning.md). The
// Prometheus exporter translates without appending suffixes, so these land
// on the scrape endpoint exactly as written.
const (
	metricRuns               = "cairnos_runs_total"
	metricRunDuration        = "cairnos_run_duration_seconds"
	metricActions            = "cairnos_actions_total"
	metricActionDuration     = "cairnos_action_duration_seconds"
	metricTokens             = "cairnos_tokens_total" // #nosec G101 -- metric name, not a credential
	metricCost               = "cairnos_cost_usd_total"
	metricCacheHits          = "cairnos_cache_hits_total"
	metricCacheMisses        = "cairnos_cache_misses_total"
	metricRetries            = "cairnos_retries_total"
	metricAgentIterations    = "cairnos_agent_iterations_total"
	metricAgentToolCalls     = "cairnos_agent_tool_calls_total"
	metricCircuitState       = "cairnos_circuit_state"
	metricCircuitTransitions = "cairnos_circuit_transitions_total"
	metricBudgetSpend        = "cairnos_budget_spend_usd"
	metricBudgetExceeded     = "cairnos_budget_exceeded_total"
	metricQueueRuns          = "cairnos_queue_runs_total"
	metricQueueDeadLettered  = "cairnos_queue_dead_lettered_total"
	metricQueueHeld          = "cairnos_queue_held_total"
	metricQueueResumed       = "cairnos_queue_resumed_total"
	metricQueueReclaimed     = "cairnos_queue_reclaimed_total"
	metricQueueDepth         = "cairnos_queue_depth"
	metricQueueOldestAge     = "cairnos_queue_oldest_age_seconds"
)

// Metric label keys from the catalog.
const (
	keyWorkflow = "workflow"
	keyStatus   = "status"
	keyTrigger  = "trigger"
	keyAction   = "action"
	keyRunner   = "runner"
	keyKind     = "kind"
	keyTool     = "tool"
	keyToState  = "to_state"
	keyOutcome  = "outcome"
	keyState    = "state"
)

// Span names and span-only attribute keys, mirroring the spec's example
// trace: one run root, action children named after the action, one
// runner.call per attempt, and driver_call/tool_call under agent actions.
const (
	spanRun          = "run"
	spanActionPrefix = "action:"
	spanRunnerCall   = "runner.call"
	spanDriverCall   = "driver_call"
	spanToolCall     = "tool_call"

	attrWorkflowName = "workflow_name"
	attrRunID        = "run_id"
	attrAttempt      = "attempt"
	attrBreakerState = "breaker_state"
	attrIteration    = "iteration"
	attrInputTokens  = "input_tokens"
	attrOutputTokens = "output_tokens"
	attrCachedTokens = "cached_tokens" // #nosec G101 -- span attribute name, not a credential
	attrCostUSD      = "cost_usd"
	attrLatencyMS    = "latency_ms"
)

// Token kinds for cairnos_tokens_total.
const (
	kindInput  = "input"
	kindOutput = "output"
	kindCached = "cached"
)

// Tool-call outcome labels for cairnos_agent_tool_calls_total.
const (
	statusSucceeded = "succeeded"
	statusFailed    = "failed"
)

// Circuit state values and labels. The spec fixes the gauge encoding (0
// closed, 1 half-open, 2 open), which differs from reliability's iota
// order, so the mapping here is explicit.
const (
	gaugeClosed   = 0
	gaugeHalfOpen = 1
	gaugeOpen     = 2

	stateClosed   = "closed"
	stateHalfOpen = "half_open"
	stateOpen     = "open"
)

// workflowKey carries the run's workflow name through the context, so
// action and agent hooks can label series with the workflow without
// widening the engine or agent-loop interfaces.
type workflowKey struct{}

// Recorder implements engine.Instrumentation and agentloop.Instrumentation
// over one tracer and one meter: spans follow the spec's trace tree, and
// every hook feeds the metric catalog. Safe for concurrent use; all state
// is write-once at construction.
type Recorder struct {
	tracer trace.Tracer
	meter  metric.Meter

	runs              metric.Int64Counter
	runDuration       metric.Float64Histogram
	actions           metric.Int64Counter
	actionDuration    metric.Float64Histogram
	tokens            metric.Int64Counter
	cost              metric.Float64Counter
	cacheHits         metric.Int64Counter
	cacheMisses       metric.Int64Counter
	retries           metric.Int64Counter
	agentIterations   metric.Int64Counter
	agentToolCalls    metric.Int64Counter
	circuitChanges    metric.Int64Counter
	budgetSpend       metric.Float64Gauge
	budgetExceeded    metric.Int64Counter
	queueRuns         metric.Int64Counter
	queueDeadLettered metric.Int64Counter
	queueHeld         metric.Int64Counter
	queueResumed      metric.Int64Counter
	queueReclaimed    metric.Int64Counter
	queueDepth        metric.Float64Gauge
	queueOldestAge    metric.Float64Gauge
}

// NewRecorder builds a Recorder over the given tracer and meter. Exposed
// (rather than only through Setup) so tests can plug in the SDK's manual
// reader and span recorder, and other embedders can bring their own
// providers.
func NewRecorder(tracer trace.Tracer, meter metric.Meter) (*Recorder, error) {
	rec := &Recorder{tracer: tracer, meter: meter}

	if err := rec.buildInstruments(meter); err != nil {
		return nil, err
	}

	return rec, nil
}

// ActionSettled implements engine.Instrumentation: counters move for an
// action that reached a terminal state without executing; no span opens
// and no duration records, so latency views only see real executions.
func (r *Recorder) ActionSettled(ctx context.Context, result *store.ActionResult) {
	r.recordAction(context.WithoutCancel(ctx), workflowFrom(ctx), result, 0)
}

// ActionStarted implements engine.Instrumentation: it opens the action's
// span under the run scope in ctx; the finish func closes the span and
// feeds the action, token, cost, cache, and retry counters from the
// terminal row.
//
//nolint:spancheck // the span ends inside the returned finish func, which the caller invokes exactly once
func (r *Recorder) ActionStarted(ctx context.Context, action *workflow.Action) (context.Context, func(*store.ActionResult)) {
	ctx, span := r.tracer.Start(ctx, spanActionPrefix+action.Name, trace.WithAttributes(
		attribute.String(keyAction, action.Name),
		attribute.String(keyRunner, actionRunner(action)),
	))

	workflowName := workflowFrom(ctx)
	started := time.Now()

	return ctx, func(result *store.ActionResult) {
		span.SetAttributes(attribute.String(keyStatus, string(result.Status)))

		if result.Error != nil {
			span.SetStatus(otelcodes.Error, result.Error.Message)
		}

		span.End()

		r.recordAction(context.WithoutCancel(ctx), workflowName, result, time.Since(started))
	}
}

// BudgetExceeded implements engine.Instrumentation: it counts one budget
// breach into cairnos_budget_exceeded_total, labeled by the workflow and the
// action taken (pause or warn).
func (r *Recorder) BudgetExceeded(ctx context.Context, workflowName, action string) {
	r.budgetExceeded.Add(context.WithoutCancel(ctx), 1, metric.WithAttributes(
		attribute.String(keyWorkflow, workflowName),
		attribute.String(keyAction, action),
	))
}

// BudgetSpend implements engine.Instrumentation: it records the workflow's
// current-period spend on the cairnos_budget_spend_usd gauge so dashboards
// see how close each budgeted workflow is to its ceiling.
func (r *Recorder) BudgetSpend(ctx context.Context, workflowName string, spend, _ float64) {
	r.budgetSpend.Record(context.WithoutCancel(ctx), spend, metric.WithAttributes(
		attribute.String(keyWorkflow, workflowName),
	))
}

// QueueItemRun counts one queued scheduled run on cairnos_queue_runs_total,
// labeled ok or failed, so the dashboard tracks worker throughput and the
// failure rate.
func (r *Recorder) QueueItemRun(ctx context.Context, ok bool) {
	r.queueRuns.Add(context.WithoutCancel(ctx), 1, metric.WithAttributes(
		attribute.String(keyOutcome, queueOutcome(ok)),
	))
}

// QueueDeadLettered counts one work item dead-lettered after exhausting its
// deliveries, the signal a scheduled workflow keeps failing.
func (r *Recorder) QueueDeadLettered(ctx context.Context) {
	r.queueDeadLettered.Add(context.WithoutCancel(ctx), 1)
}

// QueueHeld counts one work item parked for re-authentication, the signal a
// subscription CLI tool's credential expired and a human must sign in again.
func (r *Recorder) QueueHeld(ctx context.Context) {
	r.queueHeld.Add(context.WithoutCancel(ctx), 1)
}

// QueueResumed counts one held work item returned to ready after its credential
// became valid again, the held-to-running recovery that closes the re-auth loop.
func (r *Recorder) QueueResumed(ctx context.Context) {
	r.queueResumed.Add(context.WithoutCancel(ctx), 1)
}

// QueueLeaseReclaimed counts leases the sweep returned to ready after a worker
// crashed mid-run, so a flapping worker shows up as redelivery pressure.
func (r *Recorder) QueueLeaseReclaimed(ctx context.Context, count int) {
	r.queueReclaimed.Add(context.WithoutCancel(ctx), int64(count))
}

// QueueDepth records the queue-depth snapshot: one gauge sample per state plus
// the oldest ready item's age, the panels that show backlog building up.
func (r *Recorder) QueueDepth(ctx context.Context, ready, leased, held, dead int, oldestAgeSeconds float64) {
	safe := context.WithoutCancel(ctx)

	for _, sample := range []struct {
		state string
		count int
	}{
		{"ready", ready},
		{"leased", leased},
		{"held", held},
		{"dead", dead},
	} {
		r.queueDepth.Record(safe, float64(sample.count), metric.WithAttributes(
			attribute.String(keyState, sample.state),
		))
	}

	r.queueOldestAge.Record(safe, oldestAgeSeconds)
}

// BreakerTransition records one circuit state change. The reliability
// callback carries no context, so the embedder closes over its process
// context when wiring this to OnTransition.
func (r *Recorder) BreakerTransition(ctx context.Context, name string, to reliability.State) {
	r.circuitChanges.Add(ctx, 1, metric.WithAttributes(
		attribute.String(keyRunner, name),
		attribute.String(keyToState, stateLabel(to)),
	))
}

// CallStarted implements engine.Instrumentation: one runner.call span per
// attempt, carrying the attempt index, the breaker state at launch, and
// the attempt's usage once it finishes.
//
//nolint:spancheck // the span ends inside the returned finish func, which the caller invokes exactly once
func (r *Recorder) CallStarted(ctx context.Context, call *engine.Call) (context.Context, func(runner.Usage, error)) {
	ctx, span := r.tracer.Start(ctx, spanRunnerCall, trace.WithAttributes(
		attribute.String(keyRunner, call.Runner),
		attribute.String(keyAction, call.Action),
		attribute.Int(attrAttempt, call.Attempt),
		attribute.String(attrBreakerState, call.BreakerState),
	))

	return ctx, func(usage runner.Usage, err error) {
		endSpan(span, usage, err)
	}
}

// DriverCall implements agentloop.Instrumentation: each iteration counts
// toward cairnos_agent_iterations_total and opens a driver_call child span
// under the action scope in ctx.
//
//nolint:spancheck // the span ends inside the returned finish func, which the caller invokes exactly once
func (r *Recorder) DriverCall(ctx context.Context, actionName string, iteration int) (context.Context, func(runner.Usage, error)) {
	r.agentIterations.Add(ctx, 1, metric.WithAttributes(
		attribute.String(keyWorkflow, workflowFrom(ctx)),
		attribute.String(keyAction, actionName),
	))

	ctx, span := r.tracer.Start(ctx, spanDriverCall, trace.WithAttributes(
		attribute.String(keyAction, actionName),
		attribute.Int(attrIteration, iteration),
	))

	return ctx, func(usage runner.Usage, err error) {
		endSpan(span, usage, err)
	}
}

// ObserveBreakers registers the cairnos_circuit_state gauge over a breaker
// snapshot function, normally engine.BreakerStates. Call it once, after
// the engine exists; every metric collection then reads a fresh snapshot,
// so the gauge needs no transition bookkeeping.
func (r *Recorder) ObserveBreakers(states func() map[string]reliability.State) error {
	gauge, err := r.meter.Int64ObservableGauge(metricCircuitState,
		metric.WithDescription("Circuit breaker position per runner: 0 closed, 1 half-open, 2 open."))
	if err != nil {
		return fmt.Errorf("instrument %s: %w", metricCircuitState, err)
	}

	_, err = r.meter.RegisterCallback(func(callCtx context.Context, observer metric.Observer) error {
		if ctxErr := callCtx.Err(); ctxErr != nil {
			return fmt.Errorf("circuit gauge collection: %w", ctxErr)
		}

		for name, state := range states() {
			observer.ObserveInt64(gauge, stateValue(state),
				metric.WithAttributes(attribute.String(keyRunner, name)))
		}

		return nil
	}, gauge)
	if err != nil {
		return fmt.Errorf("register %s callback: %w", metricCircuitState, err)
	}

	return nil
}

// RunStarted implements engine.Instrumentation: it opens the run's root
// span and stashes the workflow name for every child hook; the finish func
// closes the span and feeds the run counters from the terminal record.
//
//nolint:spancheck // the span ends inside the returned finish func, which the caller invokes exactly once
func (r *Recorder) RunStarted(ctx context.Context, run *store.Run) (context.Context, func(*store.Run)) {
	ctx = context.WithValue(ctx, workflowKey{}, run.WorkflowName)

	ctx, span := r.tracer.Start(ctx, spanRun, trace.WithAttributes(
		attribute.String(attrWorkflowName, run.WorkflowName),
		attribute.String(attrRunID, run.ID),
		attribute.String(keyTrigger, run.Trigger),
	))

	started := time.Now()

	return ctx, func(final *store.Run) {
		status := string(final.Status)

		span.SetAttributes(attribute.String(keyStatus, status))

		if final.Error != nil {
			span.SetStatus(otelcodes.Error, final.Error.Message)
		}

		span.End()

		meterCtx := context.WithoutCancel(ctx)

		r.runs.Add(meterCtx, 1, metric.WithAttributes(
			attribute.String(keyWorkflow, final.WorkflowName),
			attribute.String(keyStatus, status),
			attribute.String(keyTrigger, final.Trigger),
		))

		r.runDuration.Record(meterCtx, time.Since(started).Seconds(), metric.WithAttributes(
			attribute.String(keyWorkflow, final.WorkflowName),
			attribute.String(keyStatus, status),
		))
	}
}

// ToolCall implements agentloop.Instrumentation: a tool_call child span
// under the action scope, counting into cairnos_agent_tool_calls_total by
// outcome when it finishes.
//
//nolint:spancheck // the span ends inside the returned finish func, which the caller invokes exactly once
func (r *Recorder) ToolCall(ctx context.Context, actionName, tool string) (context.Context, func(runner.Usage, error)) {
	ctx, span := r.tracer.Start(ctx, spanToolCall, trace.WithAttributes(
		attribute.String(keyAction, actionName),
		attribute.String(keyTool, tool),
	))

	return ctx, func(usage runner.Usage, err error) {
		endSpan(span, usage, err)

		status := statusSucceeded
		if err != nil {
			status = statusFailed
		}

		r.agentToolCalls.Add(context.WithoutCancel(ctx), 1, metric.WithAttributes(
			attribute.String(keyTool, tool),
			attribute.String(keyStatus, status),
		))
	}
}

// buildInstruments creates every catalog instrument, reporting all
// failures at once so a bad name surfaces with its neighbors.
func (r *Recorder) buildInstruments(meter metric.Meter) error {
	// Duration buckets are tuned per histogram: runs span seconds to an
	// hour, actions milliseconds to minutes; the SDK defaults are scaled
	// to millisecond latencies and would dump every run into one bucket.
	runBuckets := []float64{0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800, 3600}
	actionBuckets := []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}

	var errs []error

	r.runs = newCounter(meter, &errs, metricRuns, "Runs by outcome.")
	r.runDuration = newHistogram(meter, &errs, metricRunDuration, "Wall-clock seconds per run.", runBuckets)
	r.actions = newCounter(meter, &errs, metricActions, "Actions by outcome.")
	r.actionDuration = newHistogram(meter, &errs, metricActionDuration, "Per-action latency in seconds.", actionBuckets)
	r.tokens = newCounter(meter, &errs, metricTokens, "Token consumption by kind.")
	r.cost = newFloatCounter(meter, &errs, metricCost, "Estimated spend in US dollars, runner-computed.")
	r.cacheHits = newCounter(meter, &errs, metricCacheHits, "Actions whose usage reported cached tokens.")
	r.cacheMisses = newCounter(meter, &errs, metricCacheMisses, "Actions that consumed input tokens with no cached ones.")
	r.retries = newCounter(meter, &errs, metricRetries, "Retry attempts beyond each action's first try.")
	r.agentIterations = newCounter(meter, &errs, metricAgentIterations, "Driver iterations inside agent-loop actions.")
	r.agentToolCalls = newCounter(meter, &errs, metricAgentToolCalls, "Tool calls made by agent loops.")
	r.circuitChanges = newCounter(meter, &errs, metricCircuitTransitions, "Breaker state changes by destination state.")
	r.budgetSpend = newFloatGauge(meter, &errs, metricBudgetSpend, "Workflow spend for the current budget period in US dollars, runner-estimated.")
	r.budgetExceeded = newCounter(meter, &errs, metricBudgetExceeded, "Budget limit breaches by workflow and the action taken (pause or warn).")
	r.queueRuns = newCounter(meter, &errs, metricQueueRuns, "Queued scheduled runs by outcome (ok or failed).")
	r.queueDeadLettered = newCounter(meter, &errs, metricQueueDeadLettered, "Work items dead-lettered after exhausting deliveries.")
	r.queueHeld = newCounter(meter, &errs, metricQueueHeld, "Work items held for re-authentication after a credential expired.")
	r.queueResumed = newCounter(meter, &errs, metricQueueResumed, "Held work items resumed after their credential became valid again.")
	r.queueReclaimed = newCounter(meter, &errs, metricQueueReclaimed, "Expired leases the sweep returned to ready for redelivery.")
	r.queueDepth = newFloatGauge(meter, &errs, metricQueueDepth, "Work-queue depth by item state (ready, leased, held, dead).")
	r.queueOldestAge = newFloatGauge(meter, &errs, metricQueueOldestAge, "Age in seconds of the oldest ready work item.")

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("build metric instruments: %w", err)
	}

	return nil
}

// recordAction feeds one terminal action row into the catalog. A zero
// elapsed means the action never executed, so the duration histogram stays
// untouched.
func (r *Recorder) recordAction(ctx context.Context, workflowName string, result *store.ActionResult, elapsed time.Duration) {
	r.actions.Add(ctx, 1, metric.WithAttributes(
		attribute.String(keyWorkflow, workflowName),
		attribute.String(keyAction, result.ActionName),
		attribute.String(keyRunner, result.Runner),
		attribute.String(keyStatus, string(result.Status)),
	))

	if elapsed > 0 {
		r.actionDuration.Record(ctx, elapsed.Seconds(), metric.WithAttributes(
			attribute.String(keyRunner, result.Runner),
			attribute.String(keyAction, result.ActionName),
		))
	}

	r.recordUsage(ctx, workflowName, result)
}

// recordUsage moves the token, cost, cache, and retry counters from one
// action's usage. Cache accounting is action-level by design: an action
// whose usage reports cached tokens counts one hit, an action that
// consumed input tokens without any cached ones counts one miss, and an
// action with no token usage at all (CLI wrappers, webhooks) touches
// neither counter. Per-request hit ratios need the runner's own numbers;
// this is the workflow-level view the Phase 3 tuning engine reads.
func (r *Recorder) recordUsage(ctx context.Context, workflowName string, result *store.ActionResult) {
	usage := result.Usage
	runnerAttr := attribute.String(keyRunner, result.Runner)
	actionAttr := attribute.String(keyAction, result.ActionName)

	r.addTokens(ctx, runnerAttr, actionAttr, kindInput, usage.InputTokens)
	r.addTokens(ctx, runnerAttr, actionAttr, kindOutput, usage.OutputTokens)
	r.addTokens(ctx, runnerAttr, actionAttr, kindCached, usage.CachedTokens)

	if usage.CostUSD > 0 {
		r.cost.Add(ctx, usage.CostUSD, metric.WithAttributes(
			runnerAttr, attribute.String(keyWorkflow, workflowName),
		))
	}

	switch {
	case usage.CachedTokens > 0:
		r.cacheHits.Add(ctx, 1, metric.WithAttributes(runnerAttr))
	case usage.InputTokens > 0:
		r.cacheMisses.Add(ctx, 1, metric.WithAttributes(runnerAttr))
	}

	if usage.Retries > 0 {
		r.retries.Add(ctx, int64(usage.Retries), metric.WithAttributes(runnerAttr, actionAttr))
	}
}

// addTokens counts one token kind, skipping zeros so runners that report
// no usage never mint empty series.
func (r *Recorder) addTokens(ctx context.Context, runnerAttr, actionAttr attribute.KeyValue, kind string, count int64) {
	if count <= 0 {
		return
	}

	r.tokens.Add(ctx, count, metric.WithAttributes(
		runnerAttr, actionAttr, attribute.String(keyKind, kind),
	))
}

// actionRunner names what executes the action: the runner for task
// actions, the driver for agent ones, mirroring the engine's persisted
// Runner column so spans and rows agree.
func actionRunner(action *workflow.Action) string {
	if action.Kind == workflow.KindAgent {
		return action.Driver
	}

	return action.Runner
}

// endSpan stamps the usage attributes and outcome onto a finishing span;
// the spec wants every call-level span to carry tokens, cost, and latency.
func endSpan(span trace.Span, usage runner.Usage, err error) {
	span.SetAttributes(
		attribute.Int64(attrInputTokens, usage.InputTokens),
		attribute.Int64(attrOutputTokens, usage.OutputTokens),
		attribute.Int64(attrCachedTokens, usage.CachedTokens),
		attribute.Float64(attrCostUSD, usage.CostUSD),
		attribute.Int64(attrLatencyMS, usage.Latency.Milliseconds()),
	)

	if err != nil {
		span.SetStatus(otelcodes.Error, err.Error())
	}

	span.End()
}

// newCounter builds one Int64Counter, parking any failure on errs so the
// caller reports every bad instrument together.
func newCounter(meter metric.Meter, errs *[]error, name, description string) metric.Int64Counter {
	counter, err := meter.Int64Counter(name, metric.WithDescription(description))
	if err != nil {
		*errs = append(*errs, fmt.Errorf("instrument %s: %w", name, err))
	}

	return counter
}

// newFloatCounter is newCounter for float-valued series (cost).
func newFloatCounter(meter metric.Meter, errs *[]error, name, description string) metric.Float64Counter {
	counter, err := meter.Float64Counter(name, metric.WithDescription(description))
	if err != nil {
		*errs = append(*errs, fmt.Errorf("instrument %s: %w", name, err))
	}

	return counter
}

// newFloatGauge is newCounter for synchronous float gauges (budget spend),
// which record the latest observed value rather than accumulating.
func newFloatGauge(meter metric.Meter, errs *[]error, name, description string) metric.Float64Gauge {
	gauge, err := meter.Float64Gauge(name, metric.WithDescription(description))
	if err != nil {
		*errs = append(*errs, fmt.Errorf("instrument %s: %w", name, err))
	}

	return gauge
}

// newHistogram is newCounter for histograms, with explicit second-scaled
// buckets.
func newHistogram(meter metric.Meter, errs *[]error, name, description string, buckets []float64) metric.Float64Histogram {
	histogram, err := meter.Float64Histogram(name,
		metric.WithDescription(description),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(buckets...))
	if err != nil {
		*errs = append(*errs, fmt.Errorf("instrument %s: %w", name, err))
	}

	return histogram
}

// stateLabel renders a breaker state as its metric label.
func stateLabel(state reliability.State) string {
	switch state {
	case reliability.StateClosed:
		return stateClosed
	case reliability.StateHalfOpen:
		return stateHalfOpen
	case reliability.StateOpen:
		return stateOpen
	default:
		return stateClosed
	}
}

// queueOutcome labels a queued run's result for the outcome attribute.
func queueOutcome(ok bool) string {
	if ok {
		return "ok"
	}

	return "failed"
}

// stateValue renders a breaker state as the spec's gauge encoding.
func stateValue(state reliability.State) int64 {
	switch state {
	case reliability.StateClosed:
		return gaugeClosed
	case reliability.StateHalfOpen:
		return gaugeHalfOpen
	case reliability.StateOpen:
		return gaugeOpen
	default:
		return gaugeClosed
	}
}

// workflowFrom reads the stashed workflow name; hooks reached outside a
// run scope answer with an empty label rather than failing.
func workflowFrom(ctx context.Context) string {
	name, _ := ctx.Value(workflowKey{}).(string)

	return name
}
