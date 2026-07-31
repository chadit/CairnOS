package engine_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
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

const (
	triggerCLI = "cli"
	fnRecord   = "record"
	fnBarrier  = "barrier"
	fnCapture  = "capture"
	fnCancel   = "cancel_run"
	fnBoom     = "boom"
	outputOK   = "ok"

	// agentAnswer is the canned final answer the fake agent executor
	// returns in the seam tests.
	agentAnswer = "the answer is 42"
)

var errNoOverlap = errors.New("the sibling action never started while this one waited")

const linearTOML = `
[workflow]
name = "linear"

[action.first]
runner = "local_fn"
params = { fn = "record" }

[action.second]
runner = "local_fn"
depends_on = ["first"]
params = { fn = "record" }

[action.third]
runner = "local_fn"
depends_on = ["second"]
params = { fn = "record" }
`

const diamondTOML = `
[workflow]
name = "diamond"

[action.top]
runner = "local_fn"
params = { fn = "record" }

[action.left]
runner = "local_fn"
depends_on = ["top"]
params = { fn = "record" }

[action.right]
runner = "local_fn"
depends_on = ["top"]
params = { fn = "record" }

[action.bottom]
runner = "local_fn"
depends_on = ["left", "right"]
params = { fn = "record" }
`

const parallelTOML = `
[workflow]
name = "parallel"

[action.east]
runner = "local_fn"
params = { fn = "barrier" }

[action.west]
runner = "local_fn"
params = { fn = "barrier" }
`

const retryIdempotentTOML = `
[workflow]
name = "retry_ok"

[action.flaky]
runner = "mock"
idempotent = true
params = { fail_class = "transient", fail_times = 1 }

[action.flaky.retry]
max_attempts = 3
backoff = "constant"
base_delay = "1ms"
`

const retryNonIdempotentTOML = `
[workflow]
name = "retry_unsafe"

[action.flaky]
runner = "mock"
params = { fail_class = "transient", fail_times = 1 }

[action.flaky.retry]
max_attempts = 3
backoff = "constant"
base_delay = "1ms"
`

const conditionTOML = `
[workflow]
name = "cond_skip"

[action.source]
runner = "mock"
params = { output = { count = 0 } }

[action.gated]
runner = "mock"
depends_on = ["source"]
condition = "${{ source.output.count }} > 0"

[action.tail]
runner = "mock"
depends_on = ["gated"]
`

const haltTOML = `
[workflow]
name = "halting"
on_error = "halt"

[action.boom]
runner = "mock"
params = { fail_class = "permanent" }

[action.after]
runner = "mock"
depends_on = ["boom"]
`

const continueTOML = `
[workflow]
name = "continuing"
on_error = "continue"

[action.boom]
runner = "mock"
params = { fail_class = "permanent" }

[action.after]
runner = "mock"
depends_on = ["boom"]

[action.solo]
runner = "local_fn"
params = { fn = "record" }
`

const breakerTripTOML = `
[workflow]
name = "trip_breaker"

[action.trip]
runner = "mock"
params = { fail_class = "permanent" }
`

const breakerProbeTOML = `
[workflow]
name = "probe_breaker"

[action.follow]
runner = "mock"
`

const timeoutTOML = `
[workflow]
name = "slow"
timeout = "50ms"

[action.nap]
runner = "mock"
params = { delay_ms = 5000 }
`

const cancelTOML = `
[workflow]
name = "cancel_me"

[action.stopper]
runner = "local_fn"
params = { fn = "cancel_run" }
`

const interpTOML = `
[workflow]
name = "interp"

[action.up]
runner = "mock"
params = { output = { field = "x", num = 7 } }

[action.down]
runner = "local_fn"
depends_on = ["up"]
params = { fn = "capture", whole = "${{ up.output }}", num = "${{ up.output.num }}", text = "value-${{ up.output.field }}", token = "${{ secrets.api_key }}" }
`

const inputsTOML = `
[workflow]
name = "with_inputs"

[workflow.inputs.region]
type = "string"
required = true

[workflow.inputs.max_price]
type = "int"
default = 500000

[action.use]
runner = "local_fn"
params = { fn = "capture", where = "${{ inputs.region }}", price = "${{ inputs.max_price }}" }
`

const secretTOML = `
[workflow]
name = "secretive"

[action.sec]
runner = "mock"
params = { token = "${{ secrets.api_key }}" }
`

const envTOML = `
[workflow]
name = "envy"

[action.read_env]
runner = "local_fn"
params = { fn = "capture", path = "${{ env.PATH }}" }
`

const soloTOML = `
[workflow]
name = "single"

[action.solo]
runner = "mock"
`

const agentTOML = `
[workflow]
name = "agentic"

[action.brain]
kind = "agent"
driver = "gpt"
goal = "demonstrate the not-yet-built loop"
budget_usd = 1.0
tools = []
`

const agentSeamTOML = `
[workflow]
name = "seamful"

[workflow.inputs.target]
type = "string"
required = true

[action.brain]
kind = "agent"
driver = "gpt"
goal = "investigate ${{ inputs.target }}"
budget_usd = 1.0
tools = []

[action.report]
runner = "local_fn"
depends_on = ["brain"]
params = { fn = "capture", echo = "${{ brain.output }}" }
`

const ghostTOML = `
[workflow]
name = "ghostly"

[action.spook]
runner = "ghost"
`

func TestRunWorkflowLinearOrdersActions(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	eng, db := newTestEngine(t, baseConfig(), map[string]builtins.Func{fnRecord: rec.record})

	flow := parseFlow(t, linearTOML)
	flow.Hash = "deadbeef"

	run := mustRun(t, eng, flow, nil)

	wantRunStatus(t, run, store.RunSucceeded)

	if run.WorkflowHash != "deadbeef" {
		t.Fatalf("workflow hash = %q, want the flow's hash stamped on the run", run.WorkflowHash)
	}

	want := []string{"first", "second", "third"}
	if got := rec.order(); !slices.Equal(got, want) {
		t.Fatalf("execution order = %v, want %v", got, want)
	}

	assertAllTerminal(t, db, run.ID, 3)
}

func TestRunWorkflowEmitsEventsInOrder(t *testing.T) {
	t.Parallel()

	sink := &eventSink{}

	cfg := baseConfig()
	cfg.OnEvent = sink.collect

	rec := &recorder{}
	eng, _ := newTestEngine(t, cfg, map[string]builtins.Func{fnRecord: rec.record})

	run := mustRun(t, eng, parseFlow(t, linearTOML), nil)

	wantRunStatus(t, run, store.RunSucceeded)

	events := sink.events()
	if len(events) == 0 {
		t.Fatal("no events emitted; want run_started, action transitions, run_finished")
	}

	first, last := events[0], events[len(events)-1]

	if first.Type != engine.EventRunStarted || first.RunID != run.ID {
		t.Fatalf("first event = %+v, want run_started for run %s", first, run.ID)
	}

	if last.Type != engine.EventRunFinished || last.Status != string(store.RunSucceeded) {
		t.Fatalf("last event = %+v, want run_finished with status succeeded", last)
	}

	for _, evt := range events[1 : len(events)-1] {
		if evt.Type != engine.EventActionTransition {
			t.Fatalf("middle event = %+v, want only action transitions between start and finish", evt)
		}
	}

	for _, name := range []string{"first", "second", "third"} {
		got := sink.statusesFor(name)

		wantStatuses := []string{
			string(store.ActionPending), string(store.ActionReady),
			string(store.ActionRunning), string(store.ActionSucceeded),
		}
		if !slices.Equal(got, wantStatuses) {
			t.Fatalf("action %q transition events = %v, want %v", name, got, wantStatuses)
		}
	}
}

func TestRunWorkflowDiamondFansOutAndBackIn(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	eng, db := newTestEngine(t, baseConfig(), map[string]builtins.Func{fnRecord: rec.record})

	run := mustRun(t, eng, parseFlow(t, diamondTOML), nil)

	wantRunStatus(t, run, store.RunSucceeded)

	order := rec.order()
	if len(order) != 4 {
		t.Fatalf("executed %d actions, want 4: %v", len(order), order)
	}

	if order[0] != "top" || order[3] != "bottom" {
		t.Fatalf("order = %v, want top first and bottom last", order)
	}

	middle := order[1:3]
	if !slices.Contains(middle, "left") || !slices.Contains(middle, "right") {
		t.Fatalf("middle of order = %v, want left and right", middle)
	}

	assertAllTerminal(t, db, run.ID, 4)
}

func TestRunWorkflowRunsIndependentActionsInParallel(t *testing.T) {
	t.Parallel()

	var entered atomic.Int32

	bothIn := make(chan struct{})

	// barrier proves real overlap: each action waits inside its runner call
	// until the sibling has also started. A serialized engine would time out
	// the first action instead of succeeding.
	barrier := func(ctx context.Context, _ *runner.Task) (any, error) {
		if entered.Add(1) == 2 {
			close(bothIn)
		}

		select {
		case <-bothIn:
			return outputOK, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
			return nil, errNoOverlap
		}
	}

	eng, db := newTestEngine(t, baseConfig(), map[string]builtins.Func{fnBarrier: barrier})

	run := mustRun(t, eng, parseFlow(t, parallelTOML), nil)

	wantRunStatus(t, run, store.RunSucceeded)
	assertAllTerminal(t, db, run.ID, 2)
}

func TestRetryPolicyHonorsIdempotence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		flowTOML    string
		wantRun     store.RunStatus
		wantAction  store.ActionStatus
		wantAttempt int
		wantRetries int
		wantCode    string
	}{
		{
			name:        "idempotent transient failure retries then succeeds",
			flowTOML:    retryIdempotentTOML,
			wantRun:     store.RunSucceeded,
			wantAction:  store.ActionSucceeded,
			wantAttempt: 2,
			wantRetries: 1,
		},
		{
			name:        "non-idempotent transient failure never retries",
			flowTOML:    retryNonIdempotentTOML,
			wantRun:     store.RunFailed,
			wantAction:  store.ActionFailed,
			wantAttempt: 1,
			wantRetries: 0,
			wantCode:    engine.CodeRunnerTransient,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			eng, db := newTestEngine(t, baseConfig(), nil)

			run := mustRun(t, eng, parseFlow(t, tt.flowTOML), nil)

			wantRunStatus(t, run, tt.wantRun)

			result := wantActionStatus(t, db, run.ID, "flaky", tt.wantAction)

			if result.Attempt != tt.wantAttempt {
				t.Fatalf("attempt = %d, want %d", result.Attempt, tt.wantAttempt)
			}

			if result.Usage.Retries != tt.wantRetries {
				t.Fatalf("usage retries = %d, want %d", result.Usage.Retries, tt.wantRetries)
			}

			if tt.wantCode != "" {
				wantErrCode(t, result.Error, tt.wantCode)
			}
		})
	}
}

func TestFalseConditionSkipsDownstreamAndRunSucceeds(t *testing.T) {
	t.Parallel()

	eng, db := newTestEngine(t, baseConfig(), nil)

	run := mustRun(t, eng, parseFlow(t, conditionTOML), nil)

	wantRunStatus(t, run, store.RunSucceeded)

	if run.Error != nil {
		t.Fatalf("run error = %v, want nil for a skipped-only run", run.Error)
	}

	wantActionStatus(t, db, run.ID, "source", store.ActionSucceeded)
	wantActionStatus(t, db, run.ID, "gated", store.ActionSkipped)
	wantActionStatus(t, db, run.ID, "tail", store.ActionSkipped)
}

func TestOnErrorHaltFailsRunAndSkipsDownstream(t *testing.T) {
	t.Parallel()

	eng, db := newTestEngine(t, baseConfig(), nil)

	run := mustRun(t, eng, parseFlow(t, haltTOML), nil)

	wantRunStatus(t, run, store.RunFailed)
	wantErrCode(t, run.Error, engine.CodeRunnerPermanent)

	failed := wantActionStatus(t, db, run.ID, "boom", store.ActionFailed)
	if failed.Error.Details["runner_code"] != "mock.permanent" {
		t.Fatalf("runner_code detail = %v, want mock.permanent", failed.Error.Details["runner_code"])
	}

	wantActionStatus(t, db, run.ID, "after", store.ActionSkipped)
}

func TestOnErrorContinueFinishesIndependentBranch(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	eng, db := newTestEngine(t, baseConfig(), map[string]builtins.Func{fnRecord: rec.record})

	run := mustRun(t, eng, parseFlow(t, continueTOML), nil)

	wantRunStatus(t, run, store.RunFailed)
	wantErrCode(t, run.Error, engine.CodeRunnerPermanent)

	wantActionStatus(t, db, run.ID, "boom", store.ActionFailed)
	wantActionStatus(t, db, run.ID, "after", store.ActionSkipped)
	wantActionStatus(t, db, run.ID, "solo", store.ActionSucceeded)

	if got := rec.order(); !slices.Equal(got, []string{"solo"}) {
		t.Fatalf("independent branch executions = %v, want [solo]", got)
	}
}

// TestFallbackChainServesFailedPrimary proves the mixed-model layer: when the
// action's runner fails terminally and a fallback is configured, the engine runs
// the fallback and the action succeeds (D12).
func TestFallbackChainServesFailedPrimary(t *testing.T) {
	t.Parallel()

	boom := func(_ context.Context, _ *runner.Task) (any, error) {
		return nil, errors.New("primary provider down")
	}

	cfg := baseConfig()
	cfg.Fallback = map[string][]string{builtins.LocalFnName: {builtins.MockName}}

	eng, db := newTestEngine(t, cfg, map[string]builtins.Func{fnBoom: boom})

	const fallbackTOML = `
[workflow]
name = "fallback"

[action.work]
runner = "local_fn"
params = { fn = "boom" }
`

	run := mustRun(t, eng, parseFlow(t, fallbackTOML), nil)

	wantRunStatus(t, run, store.RunSucceeded)

	work := wantActionStatus(t, db, run.ID, "work", store.ActionSucceeded)
	if work.Output != outputOK {
		t.Errorf("output = %v, want the fallback runner's ok", work.Output)
	}
}

// TestFallbackChainFailsWhenEveryProviderFails confirms the action still fails
// when the primary and every fallback fail, so fallback is resilience, not a
// guarantee of success.
func TestFallbackChainFailsWhenEveryProviderFails(t *testing.T) {
	t.Parallel()

	boom := func(_ context.Context, _ *runner.Task) (any, error) {
		return nil, errors.New("primary provider down")
	}

	cfg := baseConfig()
	cfg.Fallback = map[string][]string{builtins.LocalFnName: {builtins.MockName}}

	eng, db := newTestEngine(t, cfg, map[string]builtins.Func{fnBoom: boom})

	// The fallback mock is told to fail too, so no provider can serve the action.
	const bothFailTOML = `
[workflow]
name = "exhausted"

[action.work]
runner = "local_fn"
params = { fn = "boom", fail_class = "permanent" }
`

	run := mustRun(t, eng, parseFlow(t, bothFailTOML), nil)

	wantRunStatus(t, run, store.RunFailed)
	wantActionStatus(t, db, run.ID, "work", store.ActionFailed)
}

// TestPerActionFallbackServesFailedPrimary proves the per-action override
// (Slice 5-c): with no engine-level [engine.fallback] default, an action can
// name its own fallback and have it serve a failed primary.
func TestPerActionFallbackServesFailedPrimary(t *testing.T) {
	t.Parallel()

	boom := func(_ context.Context, _ *runner.Task) (any, error) {
		return nil, errors.New("primary provider down")
	}

	eng, db := newTestEngine(t, baseConfig(), map[string]builtins.Func{fnBoom: boom})

	const fallbackTOML = `
[workflow]
name = "peraction"

[action.work]
runner = "local_fn"
fallback = ["mock"]
params = { fn = "boom" }
`

	run := mustRun(t, eng, parseFlow(t, fallbackTOML), nil)

	wantRunStatus(t, run, store.RunSucceeded)

	work := wantActionStatus(t, db, run.ID, "work", store.ActionSucceeded)
	if work.Output != outputOK {
		t.Errorf("output = %v, want the fallback runner's ok", work.Output)
	}
}

// TestPerActionFallbackReplacesConfigDefault proves the override replaces, not
// merges with, the engine default. The config default would route to the mock
// (which succeeds), but the action's own fallback list points back at the
// failing runner, so the action fails: the config default was never consulted.
func TestPerActionFallbackReplacesConfigDefault(t *testing.T) {
	t.Parallel()

	boom := func(_ context.Context, _ *runner.Task) (any, error) {
		return nil, errors.New("primary provider down")
	}

	cfg := baseConfig()
	cfg.Fallback = map[string][]string{builtins.LocalFnName: {builtins.MockName}}

	eng, db := newTestEngine(t, cfg, map[string]builtins.Func{fnBoom: boom})

	const overrideTOML = `
[workflow]
name = "override"

[action.work]
runner = "local_fn"
fallback = ["local_fn"]
params = { fn = "boom" }
`

	run := mustRun(t, eng, parseFlow(t, overrideTOML), nil)

	wantRunStatus(t, run, store.RunFailed)
	wantActionStatus(t, db, run.ID, "work", store.ActionFailed)
}

// TestPerActionFallbackUnregisteredRejectsRun confirms a fallback name is held
// to the same run-start registration check as the primary runner: an unknown
// provider rejects the run before any record is created.
func TestPerActionFallbackUnregisteredRejectsRun(t *testing.T) {
	t.Parallel()

	eng, db := newTestEngine(t, baseConfig(), nil)

	const ghostFallbackTOML = `
[workflow]
name = "ghostfallback"

[action.work]
runner = "mock"
fallback = ["ghost"]
`

	_, err := eng.RunWorkflow(t.Context(), parseFlow(t, ghostFallbackTOML), nil, triggerCLI)

	errs, isErrs := errors.AsType[cairnerr.Errors](err)
	if !isErrs {
		t.Fatalf("error = %v (%T), want cairnerr.Errors", err, err)
	}

	wantErrCode(t, errs[0], workflow.CodeInvalid)

	if db.runCount() != 0 {
		t.Fatalf("runs created = %d, want 0", db.runCount())
	}
}

func TestOpenBreakerFailsFastOnNextRun(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.Breaker = reliability.BreakerConfig{
		FailureThreshold: 0.5,
		Window:           time.Minute,
		MinRequests:      1,
		Cooldown:         time.Minute,
	}

	eng, db := newTestEngine(t, cfg, nil)

	first := mustRun(t, eng, parseFlow(t, breakerTripTOML), nil)
	wantRunStatus(t, first, store.RunFailed)
	wantErrCode(t, first.Error, engine.CodeRunnerPermanent)

	second := mustRun(t, eng, parseFlow(t, breakerProbeTOML), nil)
	wantRunStatus(t, second, store.RunFailed)
	wantErrCode(t, second.Error, engine.CodeCircuitOpen)

	blocked := wantActionStatus(t, db, second.ID, "follow", store.ActionFailed)
	wantErrCode(t, blocked.Error, engine.CodeCircuitOpen)
}

func TestRunTimeoutFailsRunWithRunTimeout(t *testing.T) {
	t.Parallel()

	eng, db := newTestEngine(t, baseConfig(), nil)

	run := mustRun(t, eng, parseFlow(t, timeoutTOML), nil)

	wantRunStatus(t, run, store.RunFailed)
	wantErrCode(t, run.Error, engine.CodeRunTimeout)
	assertAllTerminal(t, db, run.ID, 1)
}

func TestOperatorCancelEndsRunCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stop := func(runCtx context.Context, _ *runner.Task) (any, error) {
		cancel()
		<-runCtx.Done()

		return nil, runCtx.Err()
	}

	eng, db := newTestEngine(t, baseConfig(), map[string]builtins.Func{fnCancel: stop})

	run, err := eng.RunWorkflow(ctx, parseFlow(t, cancelTOML), nil, triggerCLI)
	if err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}

	wantRunStatus(t, run, store.RunCanceled)
	wantErrCode(t, run.Error, engine.CodeCanceled)
	assertAllTerminal(t, db, run.ID, 1)
}

// TestCanceledStartStillPersistsRunRecord proves the run insert survives a
// context that is already canceled when RunWorkflow is entered. The manager
// hands out a reserved run id before this write lands, so a Cancel arriving in
// that window used to kill the insert: the caller got "create run: context
// canceled" and no record existed to mark canceled, which left an A2A task
// answering Internal with nothing in the store.
func TestCanceledStartStillPersistsRunRecord(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	rec := &recorder{}
	eng, db := newTestEngine(t, baseConfig(), map[string]builtins.Func{fnRecord: rec.record})

	run, err := eng.RunWorkflow(ctx, parseFlow(t, linearTOML), nil, triggerCLI)
	if err != nil {
		t.Fatalf("RunWorkflow on a canceled context: %v", err)
	}

	stored, err := db.GetRun(t.Context(), run.ID)
	if err != nil {
		t.Fatalf("run record was not persisted for a canceled start: %v", err)
	}

	if stored.Status != store.RunCanceled {
		t.Errorf("persisted run status = %q, want %q", stored.Status, store.RunCanceled)
	}
}

func TestInterpolationResolvesNativeAndEmbeddedValues(t *testing.T) {
	t.Parallel()

	sink := &paramSink{}

	cfg := baseConfig()
	cfg.Secrets = &fakeSecrets{values: map[string]string{"api_key": "s3cr3t"}}

	eng, db := newTestEngine(t, cfg, map[string]builtins.Func{fnCapture: sink.capture})

	run := mustRun(t, eng, parseFlow(t, interpTOML), nil)

	wantRunStatus(t, run, store.RunSucceeded)
	assertAllTerminal(t, db, run.ID, 2)

	got := sink.snapshot()

	whole, isTable := got["whole"].(map[string]any)
	if !isTable {
		t.Fatalf("whole = %T, want map[string]any (native type kept)", got["whole"])
	}

	if whole["field"] != "x" || whole["num"] != int64(7) {
		t.Fatalf("whole = %v, want the upstream output map", whole)
	}

	if got["num"] != int64(7) {
		t.Fatalf("num = %v (%T), want native int64 7", got["num"], got["num"])
	}

	if got["text"] != "value-x" {
		t.Fatalf("text = %v, want embedded render value-x", got["text"])
	}

	if got["token"] != "s3cr3t" {
		t.Fatalf("token = %v, want the secret value", got["token"])
	}
}

func TestRunInputsApplyDefaultsAndResolve(t *testing.T) {
	t.Parallel()

	sink := &paramSink{}
	eng, _ := newTestEngine(t, baseConfig(), map[string]builtins.Func{fnCapture: sink.capture})

	run := mustRun(t, eng, parseFlow(t, inputsTOML), map[string]any{"region": "lax"})

	wantRunStatus(t, run, store.RunSucceeded)

	got := sink.snapshot()
	if got["where"] != "lax" {
		t.Fatalf("where = %v, want the supplied input", got["where"])
	}

	if got["price"] != int64(500000) {
		t.Fatalf("price = %v (%T), want the declared default 500000", got["price"], got["price"])
	}
}

func TestMissingRequiredInputRejectsRunBeforeCreation(t *testing.T) {
	t.Parallel()

	eng, db := newTestEngine(t, baseConfig(), map[string]builtins.Func{fnCapture: (&paramSink{}).capture})

	_, err := eng.RunWorkflow(t.Context(), parseFlow(t, inputsTOML), nil, triggerCLI)

	errs, isErrs := errors.AsType[cairnerr.Errors](err)
	if !isErrs {
		t.Fatalf("error = %v (%T), want cairnerr.Errors", err, err)
	}

	wantErrCode(t, errs[0], workflow.CodeInvalid)

	if db.runCount() != 0 {
		t.Fatalf("runs created = %d, want 0: validation must reject before the record exists", db.runCount())
	}
}

func TestMissingSecretFailsAction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		secrets engine.SecretSource
	}{
		{name: "nil secret source", secrets: nil},
		{name: "secret missing from source", secrets: &fakeSecrets{values: map[string]string{}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := baseConfig()
			cfg.Secrets = tt.secrets

			eng, db := newTestEngine(t, cfg, nil)

			run := mustRun(t, eng, parseFlow(t, secretTOML), nil)

			wantRunStatus(t, run, store.RunFailed)
			wantErrCode(t, run.Error, engine.CodeMissingSecret)

			failed := wantActionStatus(t, db, run.ID, "sec", store.ActionFailed)
			wantErrCode(t, failed.Error, engine.CodeMissingSecret)
		})
	}
}

func TestEnvReferences(t *testing.T) {
	t.Parallel()

	t.Run("name outside the allowlist fails the action", func(t *testing.T) {
		t.Parallel()

		eng, db := newTestEngine(t, baseConfig(), map[string]builtins.Func{fnCapture: (&paramSink{}).capture})

		run := mustRun(t, eng, parseFlow(t, envTOML), nil)

		wantRunStatus(t, run, store.RunFailed)
		wantErrCode(t, run.Error, engine.CodeConfigInvalid)

		failed := wantActionStatus(t, db, run.ID, "read_env", store.ActionFailed)
		wantErrCode(t, failed.Error, engine.CodeConfigInvalid)
	})

	t.Run("allowlisted name resolves", func(t *testing.T) {
		t.Parallel()

		sink := &paramSink{}

		cfg := baseConfig()
		cfg.EnvAllowlist = []string{"PATH"}

		eng, _ := newTestEngine(t, cfg, map[string]builtins.Func{fnCapture: sink.capture})

		run := mustRun(t, eng, parseFlow(t, envTOML), nil)

		wantRunStatus(t, run, store.RunSucceeded)

		if got := sink.snapshot()["path"]; got != os.Getenv("PATH") {
			t.Fatalf("path = %v, want the process PATH", got)
		}
	})
}

func TestEveryTransitionIsPersisted(t *testing.T) {
	t.Parallel()

	eng, db := newTestEngine(t, baseConfig(), nil)

	run := mustRun(t, eng, parseFlow(t, soloTOML), nil)

	wantRunStatus(t, run, store.RunSucceeded)

	want := []store.ActionStatus{store.ActionPending, store.ActionReady, store.ActionRunning, store.ActionSucceeded}
	if got := db.transitionsFor(run.ID, "solo"); !slices.Equal(got, want) {
		t.Fatalf("persisted transitions = %v, want %v", got, want)
	}

	stored := db.storedRun(run.ID)
	if stored == nil || stored.Status != store.RunSucceeded {
		t.Fatalf("stored run = %+v, want a terminal succeeded row", stored)
	}

	if stored.FinishedAt.IsZero() || stored.StartedAt.IsZero() {
		t.Fatalf("stored run timestamps not stamped: started %v, finished %v", stored.StartedAt, stored.FinishedAt)
	}
}

func TestAgentActionFailsWithoutExecutor(t *testing.T) {
	t.Parallel()

	eng, db := newTestEngine(t, baseConfig(), nil)

	run := mustRun(t, eng, parseFlow(t, agentTOML), nil)

	wantRunStatus(t, run, store.RunFailed)
	wantErrCode(t, run.Error, engine.CodeAgentNotAvailable)

	failed := wantActionStatus(t, db, run.ID, "brain", store.ActionFailed)
	if !strings.Contains(failed.Error.Message, "agent executor") {
		t.Fatalf("agent failure message = %q, want it to name the missing agent executor", failed.Error.Message)
	}
}

func TestAgentSeamDelegatesResolvesGoalAndFlowsOutput(t *testing.T) {
	t.Parallel()

	executor := &fakeAgentExecutor{
		result: runner.Result{
			Output: agentAnswer,
			Usage:  runner.Usage{CostUSD: 0.75, InputTokens: 100, OutputTokens: 50},
		},
	}

	cfg := baseConfig()
	cfg.AgentExecutor = executor

	sink := &paramSink{}
	eng, db := newTestEngine(t, cfg, map[string]builtins.Func{fnCapture: sink.capture})

	run := mustRun(t, eng, parseFlow(t, agentSeamTOML), map[string]any{"target": "prod"})

	wantRunStatus(t, run, store.RunSucceeded)

	brain := wantActionStatus(t, db, run.ID, "brain", store.ActionSucceeded)
	if brain.Runner != "gpt" {
		t.Fatalf("agent action Runner column = %q, want the driver name %q", brain.Runner, "gpt")
	}

	if brain.Output != agentAnswer || brain.Usage.CostUSD != 0.75 {
		t.Fatalf("agent action row = output %#v usage %+v, want the executor's result persisted", brain.Output, brain.Usage)
	}

	gotAction, gotTask := executor.captured()
	if gotAction == nil || gotAction.Goal != "investigate prod" {
		t.Fatalf("executor saw goal %#v, want refs resolved to %q", gotAction, "investigate prod")
	}

	if gotTask == nil || gotTask.RunID != run.ID || !gotTask.Resolved {
		t.Fatalf("executor task = %+v, want the run's id with Resolved true", gotTask)
	}

	if got := sink.snapshot(); got["echo"] != agentAnswer {
		t.Fatalf("downstream params = %#v, want the agent output flowing through ${{ brain.output }}", got)
	}

	want := []store.ActionStatus{store.ActionPending, store.ActionReady, store.ActionRunning, store.ActionSucceeded}
	if got := db.transitionsFor(run.ID, "brain"); !slices.Equal(got, want) {
		t.Fatalf("agent transitions = %v, want %v", got, want)
	}
}

func TestAgentSeamWallErrorsKeepTheirCodes(t *testing.T) {
	t.Parallel()

	executor := &fakeAgentExecutor{
		result:  runner.Result{Usage: runner.Usage{CostUSD: 1.25}},
		execErr: &cairnerr.Error{Code: "agent.budget_exhausted", Message: "spent it all"},
	}

	cfg := baseConfig()
	cfg.AgentExecutor = executor

	eng, db := newTestEngine(t, cfg, nil)

	run := mustRun(t, eng, parseFlow(t, agentTOML), nil)

	wantRunStatus(t, run, store.RunFailed)
	wantErrCode(t, run.Error, "agent.budget_exhausted")

	failed := wantActionStatus(t, db, run.ID, "brain", store.ActionFailed)
	if failed.Usage.CostUSD != 1.25 {
		t.Fatalf("failed agent usage = %+v, want the partial spend persisted", failed.Usage)
	}
}

func TestAgentSeamValidateRejectsRunBeforeCreation(t *testing.T) {
	t.Parallel()

	executor := &fakeAgentExecutor{
		validateErr: cairnerr.Errors{
			{Code: "agent.driver_incapable", Message: "driver gpt lacks tool calling"},
		},
	}

	cfg := baseConfig()
	cfg.AgentExecutor = executor

	eng, db := newTestEngine(t, cfg, nil)

	_, err := eng.RunWorkflow(t.Context(), parseFlow(t, agentTOML), nil, triggerCLI)

	errs, isErrs := errors.AsType[cairnerr.Errors](err)
	if !isErrs {
		t.Fatalf("error = %v (%T), want cairnerr.Errors", err, err)
	}

	wantErrCode(t, errs[0], "agent.driver_incapable")

	if db.runCount() != 0 {
		t.Fatalf("runs created = %d, want 0: validation failures reject before any run row", db.runCount())
	}
}

func TestUnregisteredRunnerRejectsRunBeforeCreation(t *testing.T) {
	t.Parallel()

	eng, db := newTestEngine(t, baseConfig(), nil)

	_, err := eng.RunWorkflow(t.Context(), parseFlow(t, ghostTOML), nil, triggerCLI)

	errs, isErrs := errors.AsType[cairnerr.Errors](err)
	if !isErrs {
		t.Fatalf("error = %v (%T), want cairnerr.Errors", err, err)
	}

	wantErrCode(t, errs[0], workflow.CodeInvalid)

	if db.runCount() != 0 {
		t.Fatalf("runs created = %d, want 0", db.runCount())
	}
}

// baseConfig is a quiet engine config: no breaker trips, no default retry,
// repeatable run IDs.
func baseConfig() *engine.Config {
	return &engine.Config{
		MaxConcurrentActions: 4,
		DefaultRetry:         reliability.Policy{MaxAttempts: 1},
		Breaker: reliability.BreakerConfig{
			FailureThreshold: 1,
			Window:           time.Minute,
			MinRequests:      1000,
			Cooldown:         time.Minute,
		},
		NewID: sequentialIDs(),
	}
}

func sequentialIDs() func() string {
	var counter atomic.Int64

	return func() string {
		return "run-" + strconv.FormatInt(counter.Add(1), 10)
	}
}

// newTestEngine wires an engine over the mock and local_fn runners and the
// in-memory store fake.
func newTestEngine(t *testing.T, cfg *engine.Config, funcs map[string]builtins.Func) (*engine.Engine, *fakeStore) {
	t.Helper()

	registry := runner.NewRegistry()

	if err := registry.Register(builtins.MockName, builtins.MockFactory); err != nil {
		t.Fatalf("register mock: %v", err)
	}

	if err := registry.Register(builtins.LocalFnName, builtins.LocalFnFactory(funcs)); err != nil {
		t.Fatalf("register local_fn: %v", err)
	}

	if err := registry.BuildAll(nil); err != nil {
		t.Fatalf("build runners: %v", err)
	}

	db := newFakeStore()

	return engine.New(*cfg, registry, db), db
}

func parseFlow(t *testing.T, text string) *workflow.Workflow {
	t.Helper()

	flow, err := workflow.Parse([]byte(text))
	if err != nil {
		t.Fatalf("parse workflow: %v", err)
	}

	return flow
}

func mustRun(t *testing.T, eng *engine.Engine, flow *workflow.Workflow, inputs map[string]any) *store.Run {
	t.Helper()

	run, err := eng.RunWorkflow(t.Context(), flow, inputs, triggerCLI)
	if err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}

	return run
}

func wantRunStatus(t *testing.T, run *store.Run, want store.RunStatus) {
	t.Helper()

	if run.Status != want {
		t.Fatalf("run status = %q, want %q (run error: %v)", run.Status, want, run.Error)
	}
}

func wantActionStatus(t *testing.T, db *fakeStore, runID, name string, want store.ActionStatus) *store.ActionResult {
	t.Helper()

	result := db.storedAction(runID, name)
	if result == nil {
		t.Fatalf("no stored result for action %q", name)
	}

	if result.Status != want {
		t.Fatalf("action %q status = %q, want %q (error: %v)", name, result.Status, want, result.Error)
	}

	return result
}

func wantErrCode(t *testing.T, cerr *cairnerr.Error, want string) {
	t.Helper()

	if cerr == nil {
		t.Fatalf("error = nil, want code %q", want)
	}

	if cerr.Code != want {
		t.Fatalf("error code = %q, want %q (message: %s)", cerr.Code, want, cerr.Message)
	}
}

// listIndexTOML exercises numeric path segments: a list index in a param, a
// list index in a condition, and a map whose literal "0" key stays a field.
const listIndexTOML = `
[workflow]
name = "list_index_ok"

[action.up]
runner = "mock"
params = { output = { items = [ { name = "alice" }, { name = "bob" } ], nums = [ 10, 20, 30 ], bykey = { "0" = "zero-field" } } }

[action.down]
runner = "local_fn"
depends_on = ["up"]
condition = "${{ up.output.nums.1 }} > 15"
params = { fn = "capture", picked = "${{ up.output.items.0.name }}", third = "${{ up.output.nums.2 }}", mapfield = "${{ up.output.bykey.0 }}" }
`

func TestListIndexResolvesInParamsAndCondition(t *testing.T) {
	t.Parallel()

	sink := &paramSink{}
	eng, db := newTestEngine(t, baseConfig(), map[string]builtins.Func{fnCapture: sink.capture})

	run := mustRun(t, eng, parseFlow(t, listIndexTOML), nil)

	wantRunStatus(t, run, store.RunSucceeded)

	// down running at all proves the list index resolved inside the condition;
	// a failed resolve there would skip or fail it instead.
	wantActionStatus(t, db, run.ID, "down", store.ActionSucceeded)

	got := sink.snapshot()

	if got["picked"] != "alice" {
		t.Fatalf("picked = %v, want alice from items.0.name", got["picked"])
	}

	if got["third"] != int64(30) {
		t.Fatalf("third = %v (%T), want int64 30 from nums.2", got["third"], got["third"])
	}

	// A numeric segment on a map stays a field name, so {"0": ...} keeps meaning.
	if got["mapfield"] != "zero-field" {
		t.Fatalf("mapfield = %v, want the literal 0 key read as a field", got["mapfield"])
	}
}

func TestListIndexMisuseFailsBeforeRunner(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ref  string
	}{
		{name: "out of range", ref: "${{ up.output.nums.9 }}"},
		{name: "negative index", ref: "${{ up.output.nums.-1 }}"},
		{name: "index into non-list", ref: "${{ up.output.field.0 }}"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			flowTOML := `
[workflow]
name = "list_index_bad"

[action.up]
runner = "mock"
params = { output = { nums = [ 10, 20, 30 ], field = "x" } }

[action.down]
runner = "local_fn"
depends_on = ["up"]
params = { fn = "capture", bad = "` + tt.ref + `" }
`

			sink := &paramSink{}
			eng, db := newTestEngine(t, baseConfig(), map[string]builtins.Func{fnCapture: sink.capture})

			run := mustRun(t, eng, parseFlow(t, flowTOML), nil)

			wantRunStatus(t, run, store.RunFailed)

			failed := wantActionStatus(t, db, run.ID, "down", store.ActionFailed)
			wantErrCode(t, failed.Error, engine.CodePreconditionFailed)

			if sink.snapshot() != nil {
				t.Fatalf("capture ran with %v, want the runner untouched on a resolve failure", sink.snapshot())
			}
		})
	}
}

// assertAllTerminal checks the run persisted a terminal row for every action
// and that the run row itself is terminal.
func assertAllTerminal(t *testing.T, db *fakeStore, runID string, count int) {
	t.Helper()

	results, err := db.ListActionResults(t.Context(), runID)
	if err != nil {
		t.Fatalf("ListActionResults: %v", err)
	}

	if len(results) != count {
		t.Fatalf("persisted %d action rows, want %d", len(results), count)
	}

	for _, result := range results {
		switch result.Status {
		case store.ActionSucceeded, store.ActionFailed, store.ActionSkipped:
		case store.ActionPending, store.ActionReady, store.ActionRunning, store.ActionWaiting:
			t.Fatalf("action %q ended %q, want a terminal status", result.ActionName, result.Status)
		default:
			t.Fatalf("action %q has unknown status %q", result.ActionName, result.Status)
		}
	}

	stored := db.storedRun(runID)
	if stored == nil {
		t.Fatalf("run %q has no stored row", runID)
	}

	switch stored.Status {
	case store.RunSucceeded, store.RunFailed, store.RunCanceled:
	case store.RunPending, store.RunRunning, store.RunHeld:
		t.Fatalf("run status = %q, want a terminal status", stored.Status)
	default:
		t.Fatalf("run has unknown status %q", stored.Status)
	}
}
