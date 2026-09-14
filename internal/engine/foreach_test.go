package engine_test

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/pkg/runner"
)

const (
	fnList   = "list"
	failItem = "fail"
)

// listReturning is a local_fn that hands a fixed list to a downstream foreach.
func listReturning(items []any) builtins.Func {
	return func(_ context.Context, _ *runner.Task) (any, error) {
		return slices.Clone(items), nil
	}
}

// echoItem returns the resolved ${{ item }} it was given, so a foreach output
// is the list of elements in order.
func echoItem(_ context.Context, task *runner.Task) (any, error) {
	return task.Params["value"], nil
}

func TestForeachOrdersOutputsByItemIndex(t *testing.T) {
	t.Parallel()

	const toml = `
[workflow]
name = "fan_order"

[action.list]
runner = "local_fn"
params = { fn = "list" }

[action.each]
runner = "local_fn"
depends_on = ["list"]
params = { fn = "echo", value = "${{ item }}" }

[action.each.foreach]
items = "${{ list.output }}"
max_parallel = 2
`

	funcs := map[string]builtins.Func{
		fnList: listReturning([]any{"a", "b", "c", "d"}),
		"echo": echoItem,
	}
	eng, db := newTestEngine(t, baseConfig(), funcs)

	run := mustRun(t, eng, parseFlow(t, toml), nil)

	wantRunStatus(t, run, store.RunSucceeded)

	each := wantActionStatus(t, db, run.ID, "each", store.ActionSucceeded)
	wantItemOutputs(t, each.Output, []string{"a", "b", "c", "d"})
}

func TestForeachBindsIndex(t *testing.T) {
	t.Parallel()

	const toml = `
[workflow]
name = "fan_index"

[action.list]
runner = "local_fn"
params = { fn = "list" }

[action.each]
runner = "local_fn"
depends_on = ["list"]
params = { fn = "echo", value = "${{ index }}" }

[action.each.foreach]
items = "${{ list.output }}"
`

	funcs := map[string]builtins.Func{
		fnList: listReturning([]any{"a", "b", "c"}),
		"echo": echoItem,
	}
	eng, db := newTestEngine(t, baseConfig(), funcs)

	run := mustRun(t, eng, parseFlow(t, toml), nil)

	wantRunStatus(t, run, store.RunSucceeded)

	each := wantActionStatus(t, db, run.ID, "each", store.ActionSucceeded)

	got, isList := each.Output.([]any)
	if !isList || len(got) != 3 {
		t.Fatalf("output = %v, want three indices", each.Output)
	}

	for position, value := range got {
		if value != position {
			t.Errorf("output[%d] = %v, want the zero-based index %d", position, value, position)
		}
	}
}

func TestForeachEmptyListSucceedsWithEmptyOutput(t *testing.T) {
	t.Parallel()

	const toml = `
[workflow]
name = "fan_empty"

[action.list]
runner = "local_fn"
params = { fn = "list" }

[action.each]
runner = "local_fn"
depends_on = ["list"]
params = { fn = "count", value = "${{ item }}" }

[action.each.foreach]
items = "${{ list.output }}"
`

	counter := &callCounter{}
	funcs := map[string]builtins.Func{
		fnList:  listReturning([]any{}),
		"count": counter.run,
	}
	eng, db := newTestEngine(t, baseConfig(), funcs)

	run := mustRun(t, eng, parseFlow(t, toml), nil)

	wantRunStatus(t, run, store.RunSucceeded)

	each := wantActionStatus(t, db, run.ID, "each", store.ActionSucceeded)

	got, isList := each.Output.([]any)
	if !isList || len(got) != 0 {
		t.Fatalf("output = %v, want an empty list", each.Output)
	}

	if calls := counter.count(); calls != 0 {
		t.Fatalf("runner called %d times, want 0 for an empty foreach", calls)
	}
}

func TestForeachNonListItemsFailsPreconditionWithoutRunning(t *testing.T) {
	t.Parallel()

	const toml = `
[workflow]
name = "fan_scalar"

[action.list]
runner = "local_fn"
params = { fn = "scalar" }

[action.each]
runner = "local_fn"
depends_on = ["list"]
params = { fn = "count", value = "${{ item }}" }

[action.each.foreach]
items = "${{ list.output }}"
`

	scalar := func(_ context.Context, _ *runner.Task) (any, error) { return "not a list", nil }
	counter := &callCounter{}
	funcs := map[string]builtins.Func{
		"scalar": scalar,
		"count":  counter.run,
	}
	eng, db := newTestEngine(t, baseConfig(), funcs)

	run := mustRun(t, eng, parseFlow(t, toml), nil)

	wantRunStatus(t, run, store.RunFailed)

	each := wantActionStatus(t, db, run.ID, "each", store.ActionFailed)
	wantErrCode(t, each.Error, engine.CodePreconditionFailed)

	if calls := counter.count(); calls != 0 {
		t.Fatalf("runner called %d times, want 0 when items is not a list", calls)
	}
}

func TestForeachRetriesPerItem(t *testing.T) {
	t.Parallel()

	const toml = `
[workflow]
name = "fan_retry"

[action.list]
runner = "local_fn"
params = { fn = "list" }

[action.each]
runner = "local_fn"
idempotent = true
depends_on = ["list"]
params = { fn = "flaky", value = "${{ item }}" }

[action.each.retry]
max_attempts = 3
backoff = "constant"
base_delay = "1ms"

[action.each.foreach]
items = "${{ list.output }}"
max_parallel = 2
`

	flaky := &flakyItems{attempts: map[string]int{}, failOn: "b"}
	funcs := map[string]builtins.Func{
		fnList:  listReturning([]any{"a", "b", "c"}),
		"flaky": flaky.run,
	}
	eng, db := newTestEngine(t, baseConfig(), funcs)

	run := mustRun(t, eng, parseFlow(t, toml), nil)

	wantRunStatus(t, run, store.RunSucceeded)

	each := wantActionStatus(t, db, run.ID, "each", store.ActionSucceeded)
	wantItemOutputs(t, each.Output, []string{"a", "b", "c"})

	if tries := flaky.tries("b"); tries != 2 {
		t.Fatalf("item b ran %d times, want 2: one transient failure then a retry", tries)
	}
}

func TestForeachFailFastNamesItemIndex(t *testing.T) {
	t.Parallel()

	const toml = `
[workflow]
name = "fan_failfast"

[action.list]
runner = "local_fn"
params = { fn = "list" }

[action.each]
runner = "local_fn"
depends_on = ["list"]
params = { fn = "boom", value = "${{ item }}" }

[action.each.foreach]
items = "${{ list.output }}"
max_parallel = 1
`

	calls := &itemCalls{failOn: failItem}
	funcs := map[string]builtins.Func{
		fnList: listReturning([]any{"a", "b", failItem, "d"}),
		"boom": calls.run,
	}
	eng, db := newTestEngine(t, baseConfig(), funcs)

	run := mustRun(t, eng, parseFlow(t, toml), nil)

	wantRunStatus(t, run, store.RunFailed)

	each := wantActionStatus(t, db, run.ID, "each", store.ActionFailed)
	wantErrCode(t, each.Error, engine.CodeRunnerPermanent)

	idx, isInt := each.Error.Details["item_index"].(int)
	if !isInt || idx != 2 {
		t.Fatalf("item_index detail = %v, want the failing element index 2", each.Error.Details["item_index"])
	}

	// Serial fan-out means the fourth element must never run once the third fails.
	if ran := calls.recorded(); !slices.Equal(ran, []string{"a", "b", failItem}) {
		t.Fatalf("elements run = %v, want the failure to cancel the remaining element", ran)
	}
}

func TestForeachNeverExceedsMaxParallel(t *testing.T) {
	t.Parallel()

	const toml = `
[workflow]
name = "fan_bound"
timeout = "5s"

[action.list]
runner = "local_fn"
params = { fn = "list" }

[action.each]
runner = "local_fn"
depends_on = ["list"]
params = { fn = "gauge", value = "${{ item }}" }

[action.each.foreach]
items = "${{ list.output }}"
max_parallel = 2
`

	gauge := newPeakGauge(2)
	funcs := map[string]builtins.Func{
		fnList:  listReturning([]any{"a", "b", "c", "d", "e", "f"}),
		"gauge": gauge.run,
	}
	eng, db := newTestEngine(t, baseConfig(), funcs)

	run := mustRun(t, eng, parseFlow(t, toml), nil)

	wantRunStatus(t, run, store.RunSucceeded)
	wantActionStatus(t, db, run.ID, "each", store.ActionSucceeded)

	// The local semaphore caps concurrency at max_parallel; the barrier gauge
	// forces the fan-out to reach it, so the observed peak must equal 2.
	if peak := gauge.peaked(); peak != 2 {
		t.Fatalf("peak concurrent runner calls = %d, want exactly max_parallel 2", peak)
	}
}

// wantItemOutputs asserts a foreach action's output is the ordered list of the
// expected string elements.
func wantItemOutputs(t *testing.T, output any, want []string) {
	t.Helper()

	got, isList := output.([]any)
	if !isList {
		t.Fatalf("foreach output = %T, want []any", output)
	}

	if len(got) != len(want) {
		t.Fatalf("foreach output = %v, want %v", got, want)
	}

	for idx := range want {
		if got[idx] != want[idx] {
			t.Errorf("output[%d] = %v, want %q", idx, got[idx], want[idx])
		}
	}
}

// callCounter tracks how many times a foreach element runner was invoked, so a
// check can prove an empty or misshapen list never reaches the runner.
type callCounter struct {
	calls atomic.Int32
}

func (c *callCounter) run(_ context.Context, task *runner.Task) (any, error) {
	c.calls.Add(1)

	return task.Params["value"], nil
}

func (c *callCounter) count() int32 {
	return c.calls.Load()
}

// flakyItems fails the first attempt for one target element, then succeeds, so
// a check can prove retry applies per element rather than across the fan-out.
type flakyItems struct {
	attempts map[string]int
	failOn   string
	mu       sync.Mutex
}

func (f *flakyItems) run(_ context.Context, task *runner.Task) (any, error) {
	value, _ := task.Params["value"].(string)

	f.mu.Lock()
	defer f.mu.Unlock()

	f.attempts[value]++

	if value == f.failOn && f.attempts[value] == 1 {
		return nil, runner.NewError(runner.ClassTransient, "flaky.transient", "first attempt fails")
	}

	return value, nil
}

func (f *flakyItems) tries(value string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.attempts[value]
}

// itemCalls records every element a runner saw and fails on one target, so a
// fail-fast check can prove later elements never ran.
type itemCalls struct {
	failOn string
	calls  []string
	mu     sync.Mutex
}

func (c *itemCalls) run(_ context.Context, task *runner.Task) (any, error) {
	value, _ := task.Params["value"].(string)

	c.mu.Lock()
	c.calls = append(c.calls, value)
	c.mu.Unlock()

	if value == c.failOn {
		return nil, runner.NewError(runner.ClassPermanent, "boom.permanent", "element failed")
	}

	return value, nil
}

func (c *itemCalls) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.calls)
}

// peakGauge records the peak number of elements running at once and forces the
// fan-out to reach the bound: the first element blocks until a second is also
// in flight, which proves overlap without a sleep. Once the bound is reached
// the gate opens for good, so no later element can strand and deadlock.
type peakGauge struct {
	ready   chan struct{}
	limit   int
	running int
	peak    int
	mu      sync.Mutex
	opened  bool
}

func newPeakGauge(limit int) *peakGauge {
	return &peakGauge{limit: limit, ready: make(chan struct{})}
}

func (g *peakGauge) run(ctx context.Context, task *runner.Task) (any, error) {
	open := g.enter()

	if !open {
		select {
		case <-g.ready:
		case <-ctx.Done():
		}
	}

	g.leave()

	return task.Params["value"], nil
}

// enter records one more in-flight element, opening the gate once the bound is
// reached. It reports whether the gate is already open, in which case the
// caller runs without blocking.
func (g *peakGauge) enter() bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.running++
	if g.running > g.peak {
		g.peak = g.running
	}

	if g.running >= g.limit && !g.opened {
		g.opened = true
		close(g.ready)
	}

	return g.opened
}

func (g *peakGauge) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.running--
}

func (g *peakGauge) peaked() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.peak
}
