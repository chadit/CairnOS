package agentloop_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Shared fixture values; goconst applies to tests in this repo's lint
// policy.
const (
	driverName    = "scripted"
	actionBrain   = "brain"
	testRunID     = "run-1"
	goalText      = "find the failing workflow"
	finalAnswer   = "the answer is 42"
	scopeProject  = "project:test"
	memoryNote    = "deploys need VPN access"
	toolForbidden = "forbidden"
	opSearch      = "search"
	keyOp         = "op"
	keyQuery      = "query"
	keyOutput     = "output"
	mockEcho      = "tool data"
	plainName     = "plain"

	// turnCost is the standard estimated spend per scripted driver turn.
	turnCost = 0.1
)

// Sentinels for the fakes.
var (
	errPlainExecute = errors.New("scripted driver only runs through ExecuteWithTools")
	errRunMissing   = errors.New("run not found")
	errMemoryDown   = errors.New("memory store down")
)

// scriptedDriver is an in-test ToolCaller that walks a canned sequence of
// turns, repeating the last turn once the script runs out, and records
// every prompt and system framing it received.
type scriptedDriver struct {
	mu      sync.Mutex
	turns   []runner.Result
	calls   int
	prompts []string
	systems []string
}

var (
	_ runner.Runner     = (*scriptedDriver)(nil)
	_ runner.ToolCaller = (*scriptedDriver)(nil)
)

// Name implements runner.Runner.
func (*scriptedDriver) Name() string {
	return driverName
}

// Validate implements runner.Runner.
func (*scriptedDriver) Validate(_ *runner.Task) error {
	return nil
}

// Execute implements runner.Runner; the loop must never use it.
func (*scriptedDriver) Execute(_ context.Context, _ *runner.Task) (runner.Result, error) {
	return runner.Result{}, errPlainExecute
}

// ExecuteWithTools implements runner.ToolCaller.
func (d *scriptedDriver) ExecuteWithTools(_ context.Context, task *runner.Task, _ []runner.ToolSpec) (runner.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	prompt, _ := task.Params["prompt"].(string)
	system, _ := task.Params["system"].(string)

	d.prompts = append(d.prompts, prompt)
	d.systems = append(d.systems, system)

	idx := min(d.calls, len(d.turns)-1)
	d.calls++

	return d.turns[idx], nil
}

// callCount reports how many turns the driver served.
func (d *scriptedDriver) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.calls
}

// promptAt returns the prompt of one recorded turn.
func (d *scriptedDriver) promptAt(t *testing.T, idx int) string {
	t.Helper()

	d.mu.Lock()
	defer d.mu.Unlock()

	if idx >= len(d.prompts) {
		t.Fatalf("driver saw %d prompts, want at least %d", len(d.prompts), idx+1)
	}

	return d.prompts[idx]
}

// systemAt returns the system framing of one recorded turn.
func (d *scriptedDriver) systemAt(t *testing.T, idx int) string {
	t.Helper()

	d.mu.Lock()
	defer d.mu.Unlock()

	if idx >= len(d.systems) {
		t.Fatalf("driver saw %d turns, want at least %d", len(d.systems), idx+1)
	}

	return d.systems[idx]
}

// toolTurn builds a driver turn requesting one tool call per the driver
// convention.
func toolTurn(tool string, arguments map[string]any, cost float64) runner.Result {
	return runner.Result{
		Output: map[string]any{
			runner.ToolCallName:      tool,
			runner.ToolCallArguments: arguments,
		},
		Usage: runner.Usage{InputTokens: 10, OutputTokens: 5, CostUSD: cost},
		Meta:  map[string]string{runner.MetaStopReason: runner.StopReasonToolUse},
	}
}

// finalTurn builds a driver turn carrying the canned final answer at the
// standard per-turn cost.
func finalTurn() runner.Result {
	return runner.Result{
		Output: finalAnswer,
		Usage:  runner.Usage{InputTokens: 10, OutputTokens: 5, CostUSD: turnCost},
		Meta:   map[string]string{runner.MetaStopReason: runner.StopReasonEndTurn},
	}
}

// finalTurnAnswer builds a final-answer turn carrying a custom answer and
// cost, so a delegated child's answer can differ from the parent's.
func finalTurnAnswer(answer string, cost float64) runner.Result {
	return runner.Result{
		Output: answer,
		Usage:  runner.Usage{InputTokens: 10, OutputTokens: 5, CostUSD: cost},
		Meta:   map[string]string{runner.MetaStopReason: runner.StopReasonEndTurn},
	}
}

// rawTurn builds a turn with an arbitrary output and stop reason, for the
// unparseable-output cases.
func rawTurn(output any, stopReason string, cost float64) runner.Result {
	return runner.Result{
		Output: output,
		Usage:  runner.Usage{CostUSD: cost},
		Meta:   map[string]string{runner.MetaStopReason: stopReason},
	}
}

// perActionDriver serves a distinct script per action name, so a parent and
// its delegated child walk different turns through the one shared driver the
// child inherits by design. An action with no script ends immediately with the
// canned final answer.
type perActionDriver struct {
	mu      sync.Mutex
	scripts map[string][]runner.Result
	calls   map[string]int
}

var (
	_ runner.Runner     = (*perActionDriver)(nil)
	_ runner.ToolCaller = (*perActionDriver)(nil)
)

// newPerActionDriver builds a driver routing turns by action name.
func newPerActionDriver(scripts map[string][]runner.Result) *perActionDriver {
	return &perActionDriver{scripts: scripts, calls: map[string]int{}}
}

// Name implements runner.Runner.
func (*perActionDriver) Name() string {
	return driverName
}

// Validate implements runner.Runner.
func (*perActionDriver) Validate(_ *runner.Task) error {
	return nil
}

// Execute implements runner.Runner; the loop must never use it.
func (*perActionDriver) Execute(_ context.Context, _ *runner.Task) (runner.Result, error) {
	return runner.Result{}, errPlainExecute
}

// ExecuteWithTools implements runner.ToolCaller, walking the script registered
// for the task's action name and repeating its last turn once exhausted.
func (d *perActionDriver) ExecuteWithTools(_ context.Context, task *runner.Task, _ []runner.ToolSpec) (runner.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	name := task.ActionName

	script := d.scripts[name]
	if len(script) == 0 {
		d.calls[name]++

		return finalTurn(), nil
	}

	idx := min(d.calls[name], len(script)-1)
	d.calls[name]++

	return script[idx], nil
}

// callsFor reports how many turns the driver served for one action.
func (d *perActionDriver) callsFor(name string) int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.calls[name]
}

// plainRunner is a registered runner with no Describer and no ToolCaller,
// for the capability-check cases.
type plainRunner struct{}

// Name implements runner.Runner.
func (*plainRunner) Name() string { return plainName }

// Validate implements runner.Runner.
func (*plainRunner) Validate(_ *runner.Task) error { return nil }

// Execute implements runner.Runner.
func (*plainRunner) Execute(_ context.Context, _ *runner.Task) (runner.Result, error) {
	return runner.Result{Output: plainName}, nil
}

// stepRecorder collects persisted transcript steps in order.
type stepRecorder struct {
	mu    sync.Mutex
	steps []store.AgentStep
}

// AppendAgentStep implements agentloop.StepWriter.
func (r *stepRecorder) AppendAgentStep(_ context.Context, step *store.AgentStep) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.steps = append(r.steps, *step)

	return nil
}

// recorded returns a copy of every step seen so far.
func (r *stepRecorder) recorded() []store.AgentStep {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.steps)
}

// fakeMemory is an in-test memory.Store recording scopes and queries.
type fakeMemory struct {
	mu       sync.Mutex
	entries  []memory.Entry
	searches []memory.Query
	loads    []string
	failWith error
}

var _ memory.Store = (*fakeMemory)(nil)

// Add implements memory.Store.
func (f *fakeMemory) Add(_ context.Context, entry *memory.Entry) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.entries = append(f.entries, *entry)

	return "mem-1", nil
}

// Get implements memory.Store; the loop never calls it.
func (f *fakeMemory) Get(_ context.Context, _ string) (memory.Entry, error) {
	return memory.Entry{}, f.failWith
}

// Replace implements memory.Store; the loop never calls it.
func (f *fakeMemory) Replace(_ context.Context, _ string, _ *memory.Entry) error {
	return f.failWith
}

// Delete implements memory.Store; the loop never calls it.
func (f *fakeMemory) Delete(_ context.Context, _ string) error {
	return f.failWith
}

// Search implements memory.Store.
func (f *fakeMemory) Search(_ context.Context, query memory.Query) (memory.SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failWith != nil {
		return memory.SearchResult{}, f.failWith
	}

	f.searches = append(f.searches, query)

	return memory.SearchResult{Entries: f.entries}, nil
}

// List implements memory.Store; the loop never calls it.
func (f *fakeMemory) List(_ context.Context, _ string) ([]memory.Entry, error) {
	return nil, f.failWith
}

// Load implements memory.Store.
func (f *fakeMemory) Load(_ context.Context, scope string, _ int) ([]memory.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failWith != nil {
		return nil, f.failWith
	}

	f.loads = append(f.loads, scope)

	return f.entries, nil
}

// searchScopes returns the scope of every recorded search.
func (f *fakeMemory) searchScopes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	scopes := make([]string, 0, len(f.searches))
	for _, query := range f.searches {
		scopes = append(scopes, query.Scope)
	}

	return scopes
}

// capturingStore is a minimal store.Store for the engine-level test: runs,
// the latest action rows, and agent steps in append order.
type capturingStore struct {
	mu      sync.Mutex
	runs    map[string]*store.Run
	actions map[string]*store.ActionResult
	steps   []store.AgentStep
}

var _ store.Store = (*capturingStore)(nil)

func newCapturingStore() *capturingStore {
	return &capturingStore{
		runs:    map[string]*store.Run{},
		actions: map[string]*store.ActionResult{},
	}
}

// CreateRun implements store.Store.
func (c *capturingStore) CreateRun(_ context.Context, run *store.Run) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	copied := *run
	c.runs[run.ID] = &copied

	return nil
}

// UpdateRunStatus implements store.Store.
func (c *capturingStore) UpdateRunStatus(_ context.Context, id string, status store.RunStatus, runErr *cairnerr.Error) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	run, exists := c.runs[id]
	if !exists {
		return fmt.Errorf("%w: %s", errRunMissing, id)
	}

	run.Status = status
	run.Error = runErr

	return nil
}

// GetRun implements store.Store.
func (c *capturingStore) GetRun(_ context.Context, id string) (*store.Run, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	run, exists := c.runs[id]
	if !exists {
		return nil, fmt.Errorf("%w: %s", errRunMissing, id)
	}

	copied := *run

	return &copied, nil
}

// ListRuns implements store.Store.
func (*capturingStore) ListRuns(_ context.Context, _ store.ListFilter) ([]*store.Run, error) {
	return nil, nil
}

// UpsertActionResult implements store.Store, keeping the latest row.
func (c *capturingStore) UpsertActionResult(_ context.Context, result *store.ActionResult) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	copied := *result
	c.actions[result.RunID+"/"+result.ActionName] = &copied

	return nil
}

// ListActionResults implements store.Store; the engine-level test reads
// rows through actionRow instead.
func (*capturingStore) ListActionResults(_ context.Context, _ string) ([]*store.ActionResult, error) {
	return nil, nil
}

// AppendAgentStep implements store.Store.
func (c *capturingStore) AppendAgentStep(_ context.Context, step *store.AgentStep) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.steps = append(c.steps, *step)

	return nil
}

// ListAgentSteps implements store.Store.
func (c *capturingStore) ListAgentSteps(_ context.Context, runID, actionName string) ([]*store.AgentStep, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var steps []*store.AgentStep

	for idx := range c.steps {
		if c.steps[idx].RunID == runID && c.steps[idx].ActionName == actionName {
			copied := c.steps[idx]
			steps = append(steps, &copied)
		}
	}

	return steps, nil
}

// SaveSuggestion implements store.Store; the agent loop never touches the
// suggestion queue.
func (*capturingStore) SaveSuggestion(_ context.Context, _ *store.Suggestion) error {
	return nil
}

// GetSuggestion implements store.Store.
func (*capturingStore) GetSuggestion(_ context.Context, id string) (*store.Suggestion, error) {
	return nil, fmt.Errorf("%w: %s", store.ErrSuggestionNotFound, id)
}

// ListSuggestions implements store.Store.
func (*capturingStore) ListSuggestions(_ context.Context, _ store.SuggestionFilter) ([]*store.Suggestion, error) {
	return nil, nil
}

// UpdateSuggestionStatus implements store.Store.
func (*capturingStore) UpdateSuggestionStatus(_ context.Context, id string, _ store.SuggestionStatus) error {
	return fmt.Errorf("%w: %s", store.ErrSuggestionNotFound, id)
}

// SaveWorkflowVersion implements store.Store.
func (*capturingStore) SaveWorkflowVersion(_ context.Context, _ *store.WorkflowVersion) error {
	return nil
}

// GetWorkflowVersion implements store.Store.
func (*capturingStore) GetWorkflowVersion(_ context.Context, hash string) (*store.WorkflowVersion, error) {
	return nil, fmt.Errorf("%w: %s", store.ErrWorkflowVersionNotFound, hash)
}

// ListWorkflowVersions implements store.Store.
func (*capturingStore) ListWorkflowVersions(_ context.Context, _ string) ([]*store.WorkflowVersion, error) {
	return nil, nil
}

// AppendAudit implements store.Store.
func (*capturingStore) AppendAudit(_ context.Context, _ *store.AuditEntry) error {
	return nil
}

// ListAudit implements store.Store.
func (*capturingStore) ListAudit(_ context.Context, _ store.AuditFilter) ([]*store.AuditEntry, error) {
	return nil, nil
}

// SetWorkflowPause implements store.Store.
func (*capturingStore) SetWorkflowPause(_ context.Context, _ *store.WorkflowPause) error {
	return nil
}

// ClearWorkflowPause implements store.Store.
func (*capturingStore) ClearWorkflowPause(_ context.Context, _ string) (bool, error) {
	return false, nil
}

// GetWorkflowPause implements store.Store.
func (*capturingStore) GetWorkflowPause(_ context.Context, _ string) (*store.WorkflowPause, bool, error) {
	return nil, false, nil
}

// ListWorkflowPauses implements store.Store.
func (*capturingStore) ListWorkflowPauses(_ context.Context) ([]*store.WorkflowPause, error) {
	return nil, nil
}

// SpendSince implements store.Store.
func (*capturingStore) SpendSince(_ context.Context, _ string, _ time.Time) (float64, error) {
	return 0, nil
}

// CreateSession implements store.Store; the agent loop never touches chat.
func (*capturingStore) CreateSession(_ context.Context, _ *store.ChatSession) error {
	return nil
}

// GetSession implements store.Store.
func (*capturingStore) GetSession(_ context.Context, id string) (*store.ChatSession, error) {
	return nil, fmt.Errorf("%w: %s", store.ErrChatSessionNotFound, id)
}

// ListSessions implements store.Store.
func (*capturingStore) ListSessions(_ context.Context, _ store.ChatSessionFilter) ([]*store.ChatSession, error) {
	return nil, nil
}

// AppendMessage implements store.Store.
func (*capturingStore) AppendMessage(_ context.Context, _ *store.ChatMessage) error {
	return nil
}

// ListMessages implements store.Store.
func (*capturingStore) ListMessages(_ context.Context, _ string) ([]*store.ChatMessage, error) {
	return nil, nil
}

// SearchMessages implements store.Store.
func (*capturingStore) SearchMessages(_ context.Context, _ store.ChatSearchQuery) ([]*store.ChatMessage, error) {
	return nil, nil
}

// CreateApproval implements store.Store; the agent loop never parks approvals.
func (*capturingStore) CreateApproval(_ context.Context, _ *store.Approval) error {
	return nil
}

// GetApproval implements store.Store.
func (*capturingStore) GetApproval(_ context.Context, runID, actionName string) (*store.Approval, error) {
	return nil, fmt.Errorf("%w: %s/%s", store.ErrApprovalNotFound, runID, actionName)
}

// ListPendingApprovals implements store.Store.
func (*capturingStore) ListPendingApprovals(_ context.Context, _ string) ([]*store.Approval, error) {
	return nil, nil
}

// DecideApproval implements store.Store.
func (*capturingStore) DecideApproval(_ context.Context, runID, actionName string, _ store.ApprovalStatus, _, _ string) error {
	return fmt.Errorf("%w: %s/%s", store.ErrApprovalNotFound, runID, actionName)
}

// BindBridgeSession implements store.Store; the agent loop never touches the
// messaging bridge.
func (*capturingStore) BindBridgeSession(_ context.Context, _, _, _ string) error {
	return nil
}

// LookupBridgeSession implements store.Store.
func (*capturingStore) LookupBridgeSession(_ context.Context, transport, chatKey string) (string, error) {
	return "", fmt.Errorf("%w: %s/%s", store.ErrBridgeSessionNotFound, transport, chatKey)
}

// ReconcileInterrupted implements store.Store.
func (*capturingStore) ReconcileInterrupted(_ context.Context) (int, error) {
	return 0, nil
}

// Prune implements store.Store.
func (*capturingStore) Prune(_ context.Context, _ store.PruneOptions) (store.PruneStats, error) {
	return store.PruneStats{}, nil
}

// Close implements store.Store.
func (*capturingStore) Close() error {
	return nil
}

// actionRow returns the latest persisted row for one action.
func (c *capturingStore) actionRow(runID, name string) *store.ActionResult {
	c.mu.Lock()
	defer c.mu.Unlock()

	row, exists := c.actions[runID+"/"+name]
	if !exists {
		return nil
	}

	copied := *row

	return &copied
}

// recordedSteps returns every persisted agent step in append order.
func (c *capturingStore) recordedSteps() []store.AgentStep {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.steps)
}

// newTestRegistry wires the given driver, the mock tool, the plain runner,
// and (when a memory store is given) the memory tool. The driver is taken as a
// runner.Runner so delegate tests can swap in a per-action driver.
func newTestRegistry(t *testing.T, driver runner.Runner, mem memory.Store) *runner.Registry {
	t.Helper()

	registry := runner.NewRegistry()

	if err := registry.Register(driverName, func(_ map[string]any) (runner.Runner, error) {
		return driver, nil
	}); err != nil {
		t.Fatalf("register driver: %v", err)
	}

	if err := registry.Register(builtins.MockName, builtins.MockFactory); err != nil {
		t.Fatalf("register mock: %v", err)
	}

	if err := registry.Register("plain", func(_ map[string]any) (runner.Runner, error) {
		return &plainRunner{}, nil
	}); err != nil {
		t.Fatalf("register plain: %v", err)
	}

	if mem != nil {
		if err := registry.Register(builtins.MemoryName, builtins.MemoryToolFactory(mem)); err != nil {
			t.Fatalf("register memory tool: %v", err)
		}
	}

	if err := registry.BuildAll(nil); err != nil {
		t.Fatalf("build runners: %v", err)
	}

	return registry
}

// agentAction builds a validated-shaped agent action with generous walls;
// cases tighten the fields they exercise.
func agentAction(tools []string) *workflow.Action {
	return &workflow.Action{
		Name:          actionBrain,
		Kind:          workflow.KindAgent,
		Driver:        driverName,
		Goal:          goalText,
		Tools:         tools,
		HasTools:      true,
		MaxIterations: 8,
		BudgetUSD:     100,
	}
}

// agentTask builds the engine-shaped task the loop receives.
func agentTask() *runner.Task {
	return &runner.Task{
		RunID:      testRunID,
		ActionName: actionBrain,
		Resolved:   true,
		Deadline:   time.Now().Add(time.Minute),
	}
}

// assertStepSequence checks kinds and 1-based ordering of recorded steps.
func assertStepSequence(t *testing.T, steps []store.AgentStep, wantKinds []string) {
	t.Helper()

	if len(steps) != len(wantKinds) {
		t.Fatalf("recorded %d steps, want %d (%v)", len(steps), len(wantKinds), wantKinds)
	}

	for idx := range steps {
		if steps[idx].Kind != wantKinds[idx] {
			t.Errorf("step %d kind = %q, want %q", idx+1, steps[idx].Kind, wantKinds[idx])
		}

		if steps[idx].Step != idx+1 {
			t.Errorf("step %d numbered %d, want steps persisted in order", idx+1, steps[idx].Step)
		}

		if steps[idx].RunID != testRunID || steps[idx].ActionName != actionBrain {
			t.Errorf("step %d identity = %s/%s, want %s/%s",
				idx+1, steps[idx].RunID, steps[idx].ActionName, testRunID, actionBrain)
		}
	}
}

// wantLoopErrCode expects a canonical loop error with the given code.
func wantLoopErrCode(t *testing.T, err error, code string) {
	t.Helper()

	cerr, isCanonical := errors.AsType[*cairnerr.Error](err)
	if !isCanonical {
		t.Fatalf("error = %v (%T), want a *cairnerr.Error with code %s", err, err, code)
	}

	if cerr.Code != code {
		t.Fatalf("error code = %s, want %s (message: %s)", cerr.Code, code, cerr.Message)
	}
}
