package telemetry_test

import (
	"context"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/chadit/CairnOS/internal/agentloop"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/reliability"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/store/sqlite"
	"github.com/chadit/CairnOS/internal/telemetry"
	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Runner and trigger names the fixtures share.
const (
	llmFakeName    = "llm_fake"
	driverFakeName = "driver_fake"
	mockName       = "mock"
	triggerCLI     = "cli"
)

// Catalog names the assertions read.
const (
	nameRuns            = "cairnos_runs_total"
	nameActions         = "cairnos_actions_total"
	nameTokens          = "cairnos_tokens_total"
	nameCost            = "cairnos_cost_usd_total"
	nameCacheHits       = "cairnos_cache_hits_total"
	nameCacheMisses     = "cairnos_cache_misses_total"
	nameRetries         = "cairnos_retries_total"
	nameAgentIterations = "cairnos_agent_iterations_total"
	nameAgentToolCalls  = "cairnos_agent_tool_calls_total"
	nameCircuitState    = "cairnos_circuit_state"
)

// Bridge catalog names and the labels the bridge-metric assertions read; the
// "error" outcome is shared by the update and turn series, so it is hoisted
// here rather than repeated as a literal.
const (
	nameBridgeUpdates      = "cairnos_bridge_updates_total"
	nameBridgeTurns        = "cairnos_bridge_turns_total"
	nameBridgeConfirms     = "cairnos_bridge_confirms_total"
	nameBridgeSendFailures = "cairnos_bridge_send_failures_total"

	labelOutcome = "outcome"
	outcomeError = "error"
)

// Span fixture names.
const (
	flakyAction = "flaky"
	solveAction = "solve"
)

// costTolerance absorbs float accumulation noise in cost assertions.
const costTolerance = 1e-9

// observedTOML exercises the task pipeline: two LLM-shaped actions (one
// cache hit, one miss) and a flaky mock that retries once.
const observedTOML = `
[workflow]
name = "observed"

[action.think]
runner = "llm_fake"
params = { cached = true }

[action.draft]
runner = "llm_fake"
depends_on = ["think"]
params = { cached = false }

[action.flaky]
runner = "mock"
idempotent = true
depends_on = ["draft"]
params = { fail_class = "transient", fail_times = 1, cost_usd = 0.25 }

[action.flaky.retry]
max_attempts = 2
backoff = "constant"
base_delay = "1ms"
`

// agenticTOML exercises the agent pipeline: one tool call, then a final
// answer, well inside the walls.
const agenticTOML = `
[workflow]
name = "agentic"

[action.solve]
kind = "agent"
driver = "driver_fake"
tools = ["mock"]
goal = "exercise the loop"
max_iterations = 3
budget_usd = 1.0
`

// llmRunner reports LLM-shaped usage so the token, cost, and cache
// counters have something to count; params.cached toggles cached tokens.
type llmRunner struct{}

// Name implements runner.Runner.
func (*llmRunner) Name() string { return llmFakeName }

// Validate implements runner.Runner.
func (*llmRunner) Validate(*runner.Task) error { return nil }

// Execute implements runner.Runner.
func (*llmRunner) Execute(_ context.Context, task *runner.Task) (runner.Result, error) {
	usage := runner.Usage{
		InputTokens:  100,
		OutputTokens: 20,
		CostUSD:      0.05,
		Latency:      time.Millisecond,
	}

	if cached, _ := task.Params["cached"].(bool); cached {
		usage.CachedTokens = 5
	}

	return runner.Result{Output: "text", Usage: usage}, nil
}

// toolDriver is a runner.ToolCaller whose first turn calls the mock tool
// and whose second turn answers, driving exactly two loop iterations.
type toolDriver struct {
	mu   sync.Mutex
	turn int
}

// Name implements runner.Runner.
func (*toolDriver) Name() string { return driverFakeName }

// Validate implements runner.Runner.
func (*toolDriver) Validate(*runner.Task) error { return nil }

// Execute implements runner.Runner; the loop only uses ExecuteWithTools.
func (*toolDriver) Execute(context.Context, *runner.Task) (runner.Result, error) {
	return runner.Result{}, nil
}

// ExecuteWithTools implements runner.ToolCaller.
func (d *toolDriver) ExecuteWithTools(_ context.Context, _ *runner.Task, _ []runner.ToolSpec) (runner.Result, error) {
	d.mu.Lock()
	d.turn++
	turn := d.turn
	d.mu.Unlock()

	usage := runner.Usage{InputTokens: 10, OutputTokens: 5, CostUSD: 0.01}

	if turn == 1 {
		return runner.Result{
			Output: map[string]any{
				runner.ToolCallName:      mockName,
				runner.ToolCallArguments: map[string]any{"output": "tool says hi"},
			},
			Meta:  map[string]string{runner.MetaStopReason: runner.StopReasonToolUse},
			Usage: usage,
		}, nil
	}

	return runner.Result{
		Output: "done",
		Meta:   map[string]string{runner.MetaStopReason: runner.StopReasonEndTurn},
		Usage:  usage,
	}, nil
}

// TestEngineEmitsSpanTreeAndCatalogMetrics runs a workflow through the
// real engine with the Recorder attached and checks both halves of the
// contract: the run -> action -> runner.call span tree (attempt attrs
// included) and the catalog counters for runs, actions, tokens, cost,
// cache, and retries.
func TestEngineEmitsSpanTreeAndCatalogMetrics(t *testing.T) {
	t.Parallel()

	rec, reader, spans := newTestRecorder(t)
	eng := newInstrumentedEngine(t, rec, nil)

	run, err := eng.RunWorkflow(t.Context(), parseFlow(t, observedTOML), nil, triggerCLI)
	if err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}

	if run.Status != "succeeded" {
		t.Fatalf("run status = %q, want succeeded (error: %v)", run.Status, run.Error)
	}

	assertTaskSpanTree(t, spans.Ended())

	data := collect(t, reader)

	if got := sumInt(t, &data, nameRuns); got != 1 {
		t.Fatalf("%s = %d, want 1", nameRuns, got)
	}

	if got := intWithAttr(t, &data, nameRuns, "status", "succeeded"); got != 1 {
		t.Fatalf("%s{status=succeeded} = %d, want 1", nameRuns, got)
	}

	if got := sumInt(t, &data, nameActions); got != 3 {
		t.Fatalf("%s = %d, want 3", nameActions, got)
	}

	assertUsageCounters(t, &data)
}

// assertUsageCounters checks the token, cost, cache, and retry series the
// observed workflow must produce.
func assertUsageCounters(t *testing.T, data *metricdata.ResourceMetrics) {
	t.Helper()

	if got := intWithAttr(t, data, nameTokens, "kind", "input"); got != 200 {
		t.Fatalf("%s{kind=input} = %d, want 200", nameTokens, got)
	}

	if got := intWithAttr(t, data, nameTokens, "kind", "cached"); got != 5 {
		t.Fatalf("%s{kind=cached} = %d, want 5", nameTokens, got)
	}

	if got := sumFloat(t, data, nameCost); math.Abs(got-0.35) > costTolerance {
		t.Fatalf("%s = %v, want 0.35", nameCost, got)
	}

	if got := sumInt(t, data, nameCacheHits); got != 1 {
		t.Fatalf("%s = %d, want 1 (the cached think action)", nameCacheHits, got)
	}

	if got := sumInt(t, data, nameCacheMisses); got != 1 {
		t.Fatalf("%s = %d, want 1 (the uncached draft action; the mock has no tokens)", nameCacheMisses, got)
	}

	if got := sumInt(t, data, nameRetries); got != 1 {
		t.Fatalf("%s = %d, want 1 (flaky retried once)", nameRetries, got)
	}
}

// assertTaskSpanTree checks the run -> action -> runner.call nesting and
// the per-attempt spans of the retried action.
func assertTaskSpanTree(t *testing.T, ended []sdktrace.ReadOnlySpan) {
	t.Helper()

	roots := spansNamed(ended, "run")
	if len(roots) != 1 {
		t.Fatalf("run spans = %d, want 1", len(roots))
	}

	root := roots[0]

	actions := spansNamed(ended, "action:think")
	actions = append(actions, spansNamed(ended, "action:draft")...)
	actions = append(actions, spansNamed(ended, "action:"+flakyAction)...)

	if len(actions) != 3 {
		t.Fatalf("action spans = %d, want 3", len(actions))
	}

	for _, span := range actions {
		if span.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Fatalf("action span %q is not a child of the run span", span.Name())
		}
	}

	calls := spansNamed(ended, "runner.call")
	if len(calls) != 4 {
		t.Fatalf("runner.call spans = %d, want 4 (one per attempt)", len(calls))
	}

	flakySpan := spansNamed(ended, "action:"+flakyAction)[0]
	attempts := make(map[int64]bool, len(calls))

	for _, call := range calls {
		if strAttr(call, "action") != flakyAction {
			continue
		}

		if call.Parent().SpanID() != flakySpan.SpanContext().SpanID() {
			t.Fatal("runner.call span is not a child of its action span")
		}

		attempts[intAttr(call, "attempt")] = true
	}

	if !attempts[0] || !attempts[1] || len(attempts) != 2 {
		t.Fatalf("flaky attempts = %v, want exactly {0, 1}", attempts)
	}
}

// TestAgentLoopEmitsIterationSpansAndMetrics drives an agent action
// through the engine plus the real loop, asserting the driver_call and
// tool_call child spans and the two agent metrics.
func TestAgentLoopEmitsIterationSpansAndMetrics(t *testing.T) {
	t.Parallel()

	rec, reader, spans := newTestRecorder(t)
	eng := newInstrumentedEngine(t, rec, &toolDriver{})

	run, err := eng.RunWorkflow(t.Context(), parseFlow(t, agenticTOML), nil, triggerCLI)
	if err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}

	if run.Status != "succeeded" {
		t.Fatalf("run status = %q, want succeeded (error: %v)", run.Status, run.Error)
	}

	assertAgentSpanTree(t, spans.Ended())

	data := collect(t, reader)

	if got := sumInt(t, &data, nameAgentIterations); got != 2 {
		t.Fatalf("%s = %d, want 2", nameAgentIterations, got)
	}

	if got := intWithAttr(t, &data, nameAgentIterations, "workflow", "agentic"); got != 2 {
		t.Fatalf("%s{workflow=agentic} = %d, want 2", nameAgentIterations, got)
	}

	if got := intWithAttr(t, &data, nameAgentToolCalls, "status", "succeeded"); got != 1 {
		t.Fatalf("%s{status=succeeded} = %d, want 1", nameAgentToolCalls, got)
	}
}

// assertAgentSpanTree checks the iteration child spans hang under the
// agent action's span with 1-based iteration attributes.
func assertAgentSpanTree(t *testing.T, ended []sdktrace.ReadOnlySpan) {
	t.Helper()

	actionSpans := spansNamed(ended, "action:"+solveAction)
	if len(actionSpans) != 1 {
		t.Fatalf("agent action spans = %d, want 1", len(actionSpans))
	}

	actionID := actionSpans[0].SpanContext().SpanID()

	driverCalls := spansNamed(ended, "driver_call")
	if len(driverCalls) != 2 {
		t.Fatalf("driver_call spans = %d, want 2", len(driverCalls))
	}

	iterations := make(map[int64]bool, len(driverCalls))

	for _, span := range driverCalls {
		if span.Parent().SpanID() != actionID {
			t.Fatal("driver_call span is not a child of the action span")
		}

		iterations[intAttr(span, "iteration")] = true
	}

	if !iterations[1] || !iterations[2] {
		t.Fatalf("iterations = %v, want {1, 2}", iterations)
	}

	toolCalls := spansNamed(ended, "tool_call")
	if len(toolCalls) != 1 {
		t.Fatalf("tool_call spans = %d, want 1", len(toolCalls))
	}

	if toolCalls[0].Parent().SpanID() != actionID {
		t.Fatal("tool_call span is not a child of the action span")
	}

	if got := strAttr(toolCalls[0], "tool"); got != mockName {
		t.Fatalf("tool_call tool attr = %q, want %q", got, mockName)
	}
}

// TestObserveBreakersGaugeEncodesStates proves the gauge callback renders
// the spec's encoding (0 closed, 1 half-open, 2 open) per runner.
func TestObserveBreakersGaugeEncodesStates(t *testing.T) {
	t.Parallel()

	rec, reader, _ := newTestRecorder(t)

	err := rec.ObserveBreakers(func() map[string]reliability.State {
		return map[string]reliability.State{
			"calm":    reliability.StateClosed,
			"probing": reliability.StateHalfOpen,
			"broken":  reliability.StateOpen,
		}
	})
	if err != nil {
		t.Fatalf("ObserveBreakers: %v", err)
	}

	data := collect(t, reader)

	tests := []struct {
		runnerName string
		want       int64
	}{
		{runnerName: "calm", want: 0},
		{runnerName: "probing", want: 1},
		{runnerName: "broken", want: 2},
	}

	for _, tt := range tests {
		if got := gaugeFor(t, &data, nameCircuitState, tt.runnerName); got != tt.want {
			t.Fatalf("%s{runner=%s} = %d, want %d", nameCircuitState, tt.runnerName, got, tt.want)
		}
	}
}

// TestBridgeMetricsRecorded drives the four Telegram-bridge recorder methods
// through the manual reader and asserts each lands on the right Prometheus
// series with the right outcome label, so the bridge dashboard panels have
// data behind them (S3). BridgeConfirm's refused outcome is checked explicitly
// because it is the counter behind the confirm-time grant refusals S5 enforces.
func TestBridgeMetricsRecorded(t *testing.T) {
	t.Parallel()

	rec, reader, _ := newTestRecorder(t)
	ctx := t.Context()

	rec.BridgeUpdate(ctx, "handled")
	rec.BridgeUpdate(ctx, "unauthorized")
	rec.BridgeUpdate(ctx, outcomeError)
	rec.BridgeTurn(ctx, true)
	rec.BridgeTurn(ctx, false)
	rec.BridgeConfirm(ctx, "approved")
	rec.BridgeConfirm(ctx, "denied")
	rec.BridgeConfirm(ctx, "expired")
	rec.BridgeConfirm(ctx, "refused")
	rec.BridgeSendFailure(ctx)

	data := collect(t, reader)

	tests := []struct {
		name   string
		metric string
		key    string
		value  string
		want   int64
	}{
		{"update handled", nameBridgeUpdates, labelOutcome, "handled", 1},
		{"update unauthorized", nameBridgeUpdates, labelOutcome, "unauthorized", 1},
		{"update error", nameBridgeUpdates, labelOutcome, outcomeError, 1},
		{"turn ok", nameBridgeTurns, labelOutcome, "ok", 1},
		{"turn error", nameBridgeTurns, labelOutcome, outcomeError, 1},
		{"confirm approved", nameBridgeConfirms, labelOutcome, "approved", 1},
		{"confirm denied", nameBridgeConfirms, labelOutcome, "denied", 1},
		{"confirm expired", nameBridgeConfirms, labelOutcome, "expired", 1},
		{"confirm refused", nameBridgeConfirms, labelOutcome, "refused", 1},
		{"send failures", nameBridgeSendFailures, "", "", 1},
	}

	for _, tt := range tests {
		if got := sumIntFiltered(t, &data, tt.metric, tt.key, tt.value); got != tt.want {
			t.Errorf("%s: %s{%s=%s} = %d, want %d", tt.name, tt.metric, tt.key, tt.value, got, tt.want)
		}
	}
}

// newTestRecorder builds a Recorder over the SDK's manual reader and span
// recorder, so tests assert real exported data without exporters.
func newTestRecorder(t *testing.T) (*telemetry.Recorder, *sdkmetric.ManualReader, *tracetest.SpanRecorder) {
	t.Helper()

	spans := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	rec, err := telemetry.NewRecorder(tracerProvider.Tracer("test"), meterProvider.Meter("test"))
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}

	return rec, reader, spans
}

// newInstrumentedEngine wires a real engine over a real SQLite store with
// the recorder attached; driver non-nil also wires the agent loop.
func newInstrumentedEngine(t *testing.T, rec *telemetry.Recorder, driver *toolDriver) *engine.Engine {
	t.Helper()

	registry := runner.NewRegistry()

	if err := registry.Register(builtins.MockName, builtins.MockFactory); err != nil {
		t.Fatalf("register mock: %v", err)
	}

	if err := registry.Register(llmFakeName, func(map[string]any) (runner.Runner, error) {
		return &llmRunner{}, nil
	}); err != nil {
		t.Fatalf("register llm fake: %v", err)
	}

	if driver != nil {
		if err := registry.Register(driverFakeName, func(map[string]any) (runner.Runner, error) {
			return driver, nil
		}); err != nil {
			t.Fatalf("register driver fake: %v", err)
		}
	}

	if err := registry.BuildAll(nil); err != nil {
		t.Fatalf("build runners: %v", err)
	}

	db, err := sqlite.Open(filepath.Join(t.TempDir(), "telemetry_test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	cfg := engine.Config{
		MaxConcurrentActions: 4,
		DefaultRetry:         reliability.Policy{MaxAttempts: 1},
		Breaker: reliability.BreakerConfig{
			FailureThreshold: 1,
			Window:           time.Minute,
			MinRequests:      1000,
			Cooldown:         time.Minute,
		},
		Instrumentation: rec,
	}

	if driver != nil {
		cfg.AgentExecutor = agentloop.New(registry, nil, db, rec)
	}

	return engine.New(cfg, registry, db)
}

// parseFlow parses a workflow fixture.
func parseFlow(t *testing.T, text string) *workflow.Workflow {
	t.Helper()

	flow, err := workflow.Parse([]byte(text))
	if err != nil {
		t.Fatalf("parse workflow: %v", err)
	}

	return flow
}

// collect gathers the current metric state from the manual reader.
func collect(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()

	var data metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &data); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	return data
}

// findMetric locates one instrument by catalog name.
func findMetric(data *metricdata.ResourceMetrics, name string) (metricdata.Metrics, bool) {
	for _, scope := range data.ScopeMetrics {
		for _, item := range scope.Metrics {
			if item.Name == name {
				return item, true
			}
		}
	}

	return metricdata.Metrics{}, false
}

// sumInt totals every datapoint of an int64 counter; a missing metric
// totals zero so "never emitted" asserts cleanly.
func sumInt(t *testing.T, data *metricdata.ResourceMetrics, name string) int64 {
	t.Helper()

	return sumIntFiltered(t, data, name, "", "")
}

// intWithAttr totals the datapoints of an int64 counter carrying one
// attribute value.
func intWithAttr(t *testing.T, data *metricdata.ResourceMetrics, name, key, value string) int64 {
	t.Helper()

	return sumIntFiltered(t, data, name, key, value)
}

// sumIntFiltered is the shared int64 counter walk; an empty key matches
// every datapoint.
func sumIntFiltered(t *testing.T, data *metricdata.ResourceMetrics, name, key, value string) int64 {
	t.Helper()

	item, found := findMetric(data, name)
	if !found {
		return 0
	}

	sum, isSum := item.Data.(metricdata.Sum[int64])
	if !isSum {
		t.Fatalf("metric %s is %T, want Sum[int64]", name, item.Data)
	}

	var total int64

	for _, point := range sum.DataPoints {
		if key != "" {
			attr, present := point.Attributes.Value(attribute.Key(key))
			if !present || attr.AsString() != value {
				continue
			}
		}

		total += point.Value
	}

	return total
}

// sumFloat totals every datapoint of a float64 counter.
func sumFloat(t *testing.T, data *metricdata.ResourceMetrics, name string) float64 {
	t.Helper()

	item, found := findMetric(data, name)
	if !found {
		return 0
	}

	sum, isSum := item.Data.(metricdata.Sum[float64])
	if !isSum {
		t.Fatalf("metric %s is %T, want Sum[float64]", name, item.Data)
	}

	var total float64
	for _, point := range sum.DataPoints {
		total += point.Value
	}

	return total
}

// gaugeFor reads one runner's datapoint off an int64 gauge.
func gaugeFor(t *testing.T, data *metricdata.ResourceMetrics, name, runnerName string) int64 {
	t.Helper()

	item, found := findMetric(data, name)
	if !found {
		t.Fatalf("metric %s was never observed", name)
	}

	gauge, isGauge := item.Data.(metricdata.Gauge[int64])
	if !isGauge {
		t.Fatalf("metric %s is %T, want Gauge[int64]", name, item.Data)
	}

	for _, point := range gauge.DataPoints {
		attr, present := point.Attributes.Value(attribute.Key("runner"))
		if present && attr.AsString() == runnerName {
			return point.Value
		}
	}

	t.Fatalf("metric %s has no datapoint for runner %q", name, runnerName)

	return 0
}

// spansNamed filters ended spans by name.
func spansNamed(ended []sdktrace.ReadOnlySpan, name string) []sdktrace.ReadOnlySpan {
	var matched []sdktrace.ReadOnlySpan

	for _, span := range ended {
		if span.Name() == name {
			matched = append(matched, span)
		}
	}

	return matched
}

// strAttr reads one string attribute off a span, empty when absent.
func strAttr(span sdktrace.ReadOnlySpan, key string) string {
	for _, attr := range span.Attributes() {
		if string(attr.Key) == key {
			return attr.Value.AsString()
		}
	}

	return ""
}

// intAttr reads one int attribute off a span, -1 when absent.
func intAttr(span sdktrace.ReadOnlySpan, key string) int64 {
	for _, attr := range span.Attributes() {
		if string(attr.Key) == key {
			return attr.Value.AsInt64()
		}
	}

	return -1
}
