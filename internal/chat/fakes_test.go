package chat_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/store/sqlite"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Shared fixture values; goconst applies to tests in this repo's lint policy.
const (
	driverName    = "scripted"
	helloWorkflow = "hello"
	plainAnswer   = "the budget is fine"
	memScope      = "ops"
	memNote       = "deploys need a VPN"
	turnCost      = 0.1
)

// errDriverScriptEmpty guards an exhausted driver script.
var errDriverScriptEmpty = errors.New("scripted driver ran past its turns")

// scriptedDriver is an in-test runner.ToolCaller that walks a canned turn
// sequence, recording the prompts it saw. It drives the chat turn the same way
// a real LLM ToolCaller would, but deterministically.
type scriptedDriver struct {
	mu      sync.Mutex
	turns   []runner.Result
	calls   int
	prompts []string
}

var (
	_ runner.Runner     = (*scriptedDriver)(nil)
	_ runner.ToolCaller = (*scriptedDriver)(nil)
)

// Name implements runner.Runner.
func (*scriptedDriver) Name() string { return driverName }

// Validate implements runner.Runner.
func (*scriptedDriver) Validate(_ *runner.Task) error { return nil }

// Execute implements runner.Runner; the chat turn only uses ExecuteWithTools.
func (*scriptedDriver) Execute(_ context.Context, _ *runner.Task) (runner.Result, error) {
	return runner.Result{}, errDriverScriptEmpty
}

// ExecuteWithTools implements runner.ToolCaller, serving the next scripted turn.
func (d *scriptedDriver) ExecuteWithTools(_ context.Context, task *runner.Task, _ []runner.ToolSpec) (runner.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	prompt, _ := task.Params["prompt"].(string)
	d.prompts = append(d.prompts, prompt)

	if d.calls >= len(d.turns) {
		return runner.Result{}, errDriverScriptEmpty
	}

	turn := d.turns[d.calls]
	d.calls++

	return turn, nil
}

// callCount reports how many turns the driver served.
func (d *scriptedDriver) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.calls
}

// lastPrompt returns the most recent prompt the driver saw, so a test can
// prove the conversation history replayed into it.
func (d *scriptedDriver) lastPrompt(t *testing.T) string {
	t.Helper()

	d.mu.Lock()
	defer d.mu.Unlock()

	if len(d.prompts) == 0 {
		t.Fatal("driver saw no prompts")
	}

	return d.prompts[len(d.prompts)-1]
}

// finalTurn is a driver turn carrying a plain text answer.
func finalTurn(answer string) runner.Result {
	return runner.Result{
		Output: answer,
		Usage:  runner.Usage{CostUSD: turnCost},
		Meta:   map[string]string{runner.MetaStopReason: runner.StopReasonEndTurn},
	}
}

// toolTurn is a driver turn requesting one tool call per the driver convention.
func toolTurn(tool string, arguments map[string]any) runner.Result {
	return runner.Result{
		Output: map[string]any{
			runner.ToolCallName:      tool,
			runner.ToolCallArguments: arguments,
		},
		Usage: runner.Usage{CostUSD: turnCost},
		Meta:  map[string]string{runner.MetaStopReason: runner.StopReasonToolUse},
	}
}

// runWorkflowTurn is a driver turn proposing a workflow trigger, the D9 gate
// the chat agent intercepts.
func runWorkflowTurn(workflowName string, inputs map[string]any) runner.Result {
	return toolTurn(chat.RunWorkflowToolName, map[string]any{
		"workflow": workflowName,
		"inputs":   inputs,
	})
}

// openStore opens a fresh sqlite store, registering cleanup.
func openStore(t *testing.T) *sqlite.Store {
	t.Helper()

	db, err := sqlite.Open(t.TempDir() + "/chat.db")
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

// collectEvents drains an event channel into a slice, so a test reads the whole
// stream after the turn finishes.
func collectEvents(events <-chan chat.Event) []chat.Event {
	var collected []chat.Event

	for event := range events {
		collected = append(collected, event)
	}

	return collected
}

// eventTypes maps events to their types for order assertions.
func eventTypes(events []chat.Event) []chat.EventType {
	types := make([]chat.EventType, len(events))
	for idx, event := range events {
		types[idx] = event.Type
	}

	return types
}
