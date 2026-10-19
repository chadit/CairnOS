// Package builtins holds the runners that ship inside the CairnOS binary.
// Phase 0 carries mock and local_fn so the engine is runnable and testable
// with no external services (.agents/spec/03-runner-interface.md).
package builtins

import (
	"context"
	"sync"
	"time"

	"github.com/chadit/CairnOS/pkg/runner"
)

// MockName is the registry name of the test runner.
const MockName = "mock"

// Mock returns canned output or failures driven by action params. The engine's
// acceptance tests run against it.
//
// All params are optional; unknown ones are ignored:
//
//	output      any     the action output (default "ok")
//	fail_class  string  "transient", "permanent", or "rate_limited"
//	fail_times  int     fail the first N attempts, then succeed
//	delay_ms    int     sleep before answering, honoring cancellation
//	cost_usd    float   reported in run usage
type Mock struct {
	// attempts counts executions per run/action so fail_times can model a
	// flaky service that recovers after a few tries.
	attempts sync.Map
}

// NewMock builds the test runner.
func NewMock() *Mock {
	return &Mock{}
}

// MockFactory adapts NewMock to the registry contract. The mock reads no
// config, so every block builds the same runner.
func MockFactory() runner.Factory {
	return asFactory(func(map[string]any) (*Mock, error) { return NewMock(), nil })
}

// Name implements runner.Runner.
func (*Mock) Name() string {
	return MockName
}

// Validate implements runner.Runner. The mock accepts anything; it exists to
// exercise the engine, not to police fixtures.
func (*Mock) Validate(_ *runner.Task) error {
	return nil
}

// Describe implements runner.Describer so workflows can exercise the agent
// loop with no external services: the mock is the canonical test tool, per
// the Phase 1c acceptance criteria (.agents/spec/15-agent-loop.md).
func (*Mock) Describe() runner.ToolSpec {
	return runner.ToolSpec{
		Name: MockName,
		Description: "Test tool that echoes its output argument (or \"ok\" without one); " +
			"it exists to exercise agent loops without external services.",
		InputSchema: map[string]any{
			schemaType: schemaObject,
			schemaProperties: map[string]any{
				"output": map[string]any{schemaDesc: "value to echo back"},
			},
		},
	}
}

// Execute implements runner.Runner.
func (m *Mock) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	if err := mockSleep(ctx, task); err != nil {
		return runner.Result{}, err
	}

	if err := m.maybeFail(task); err != nil {
		return runner.Result{}, err
	}

	output := any("ok")
	if configured, exists := task.Params["output"]; exists {
		output = configured
	}

	usage := runner.Usage{}

	if cost, exists := task.Params["cost_usd"]; exists {
		if num, isFloat := toFloat(cost); isFloat {
			usage.CostUSD = num
		}
	}

	return runner.Result{Output: output, Usage: usage}, nil
}

func mockSleep(ctx context.Context, task *runner.Task) error {
	delayMs, exists := task.Params["delay_ms"]
	if !exists {
		return nil
	}

	num, isNum := toFloat(delayMs)
	if !isNum {
		return nil
	}

	timer := time.NewTimer(time.Duration(num) * time.Millisecond)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return runner.Wrapf(ctx.Err(), runner.ClassTransient, "mock.canceled", "canceled during delay")
	case <-timer.C:
		return nil
	}
}

func (m *Mock) maybeFail(task *runner.Task) error {
	classText, exists := task.Params["fail_class"]
	if !exists {
		return nil
	}

	if times, hasTimes := toFloat(task.Params["fail_times"]); hasTimes && m.failuresSpent(task, times) {
		return nil
	}

	switch classText {
	case "transient":
		return taskError(runner.ClassTransient, "mock.transient", "configured transient failure")
	case "rate_limited":
		return taskError(runner.ClassRateLimited, "mock.rate_limited", "configured rate limit")
	default:
		return taskError(runner.ClassPermanent, "mock.permanent", "configured failure %v", classText)
	}
}

// failuresSpent counts this attempt against the action's fail_times budget and
// reports whether the budget was already used up, so the attempt succeeds. The
// count is keyed per run and action, which is what lets a retry recover.
func (m *Mock) failuresSpent(task *runner.Task, times float64) bool {
	key := task.RunID + "/" + task.ActionName

	count, _ := m.attempts.LoadOrStore(key, new(int64))

	attempts, isCounter := count.(*int64)
	if !isCounter {
		return false
	}

	*attempts++

	return float64(*attempts) > times
}

func toFloat(value any) (float64, bool) {
	switch num := value.(type) {
	case float64:
		return num, true
	case int64:
		return float64(num), true
	case int:
		return float64(num), true
	default:
		return 0, false
	}
}
