package engine_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Sentinels for the fakes.
var (
	errRunNotFound      = errors.New("run not found")
	errSecretNotFound   = errors.New("secret not found")
	errWorkflowNotFound = errors.New("workflow not found")
)

// fakeStore is an in-memory store.Store. The engine writing against this
// fake proves the interface seam: nothing in the engine may depend on the
// SQLite implementation.
type fakeStore struct {
	runs        map[string]*store.Run
	results     map[string]map[string]*store.ActionResult
	transitions map[string][]store.ActionStatus
	agentSteps  map[string][]*store.AgentStep
	suggestions map[string]*store.Suggestion
	pauses      map[string]*store.WorkflowPause
	approvals   map[string]*store.Approval
	audit       []*store.AuditEntry
	mu          sync.Mutex
}

var _ store.Store = (*fakeStore)(nil)

func newFakeStore() *fakeStore {
	return &fakeStore{
		runs:        map[string]*store.Run{},
		results:     map[string]map[string]*store.ActionResult{},
		transitions: map[string][]store.ActionStatus{},
		agentSteps:  map[string][]*store.AgentStep{},
		suggestions: map[string]*store.Suggestion{},
		pauses:      map[string]*store.WorkflowPause{},
		approvals:   map[string]*store.Approval{},
	}
}

// CreateRun implements store.Store. It honors cancellation the way the sqlite
// and postgres stores do, so a caller that passes a canceled context here fails
// in tests exactly as it would against a real database.
func (f *fakeStore) CreateRun(ctx context.Context, run *store.Run) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("insert run %s: %w", run.ID, err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	copied := *run
	f.runs[run.ID] = &copied

	return nil
}

// UpdateRunStatus implements store.Store, stamping timestamps the way the
// real store does.
func (f *fakeStore) UpdateRunStatus(_ context.Context, id string, status store.RunStatus, runErr *cairnerr.Error) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	run, exists := f.runs[id]
	if !exists {
		return fmt.Errorf("%w: %s", errRunNotFound, id)
	}

	run.Status = status
	run.Error = runErr

	if status == store.RunRunning && run.StartedAt.IsZero() {
		run.StartedAt = time.Now()
	}

	if status == store.RunSucceeded || status == store.RunFailed || status == store.RunCanceled {
		run.FinishedAt = time.Now()
	}

	return nil
}

// GetRun implements store.Store.
func (f *fakeStore) GetRun(_ context.Context, id string) (*store.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	run, exists := f.runs[id]
	if !exists {
		return nil, fmt.Errorf("%w: %s", errRunNotFound, id)
	}

	copied := *run

	return &copied, nil
}

// ListRuns implements store.Store.
func (f *fakeStore) ListRuns(_ context.Context, filter store.ListFilter) ([]*store.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]*store.Run, 0, len(f.runs))

	for _, id := range slices.Sorted(maps.Keys(f.runs)) {
		run := f.runs[id]
		if filter.WorkflowName != "" && run.WorkflowName != filter.WorkflowName {
			continue
		}

		if filter.Status != "" && run.Status != filter.Status {
			continue
		}

		copied := *run
		out = append(out, &copied)
	}

	return out, nil
}

// UpsertActionResult implements store.Store, recording every transition so
// tests can assert the full lifecycle was persisted.
func (f *fakeStore) UpsertActionResult(_ context.Context, result *store.ActionResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	byName, exists := f.results[result.RunID]
	if !exists {
		byName = map[string]*store.ActionResult{}
		f.results[result.RunID] = byName
	}

	copied := *result
	byName[result.ActionName] = &copied

	key := result.RunID + "/" + result.ActionName
	f.transitions[key] = append(f.transitions[key], result.Status)

	return nil
}

// ListActionResults implements store.Store, sorted by action name.
func (f *fakeStore) ListActionResults(_ context.Context, runID string) ([]*store.ActionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	byName := f.results[runID]

	out := make([]*store.ActionResult, 0, len(byName))

	for _, name := range slices.Sorted(maps.Keys(byName)) {
		copied := *byName[name]
		out = append(out, &copied)
	}

	return out, nil
}

// AppendAgentStep implements store.Store.
func (f *fakeStore) AppendAgentStep(_ context.Context, step *store.AgentStep) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	key := step.RunID + "/" + step.ActionName
	copied := *step
	f.agentSteps[key] = append(f.agentSteps[key], &copied)

	return nil
}

// ListAgentSteps implements store.Store.
func (f *fakeStore) ListAgentSteps(_ context.Context, runID, actionName string) ([]*store.AgentStep, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	stored := f.agentSteps[runID+"/"+actionName]

	out := make([]*store.AgentStep, 0, len(stored))

	for _, step := range stored {
		copied := *step
		out = append(out, &copied)
	}

	return out, nil
}

// SaveSuggestion implements store.Store.
func (f *fakeStore) SaveSuggestion(_ context.Context, suggestion *store.Suggestion) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	copied := *suggestion
	f.suggestions[suggestion.ID] = &copied

	return nil
}

// GetSuggestion implements store.Store.
func (f *fakeStore) GetSuggestion(_ context.Context, id string) (*store.Suggestion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	suggestion, exists := f.suggestions[id]
	if !exists {
		return nil, fmt.Errorf("%w: %s", store.ErrSuggestionNotFound, id)
	}

	copied := *suggestion

	return &copied, nil
}

// ListSuggestions implements store.Store.
func (f *fakeStore) ListSuggestions(_ context.Context, filter store.SuggestionFilter) ([]*store.Suggestion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]*store.Suggestion, 0, len(f.suggestions))

	for _, id := range slices.Sorted(maps.Keys(f.suggestions)) {
		suggestion := f.suggestions[id]
		if filter.Status != "" && suggestion.Status != filter.Status {
			continue
		}

		if filter.Kind != "" && suggestion.Kind != filter.Kind {
			continue
		}

		copied := *suggestion
		out = append(out, &copied)
	}

	return out, nil
}

// UpdateSuggestionStatus implements store.Store.
func (f *fakeStore) UpdateSuggestionStatus(_ context.Context, id string, status store.SuggestionStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	suggestion, exists := f.suggestions[id]
	if !exists {
		return fmt.Errorf("%w: %s", store.ErrSuggestionNotFound, id)
	}

	suggestion.Status = status

	return nil
}

// SaveWorkflowVersion implements store.Store. The engine never captures
// versions, so the fake just accepts the call.
func (*fakeStore) SaveWorkflowVersion(_ context.Context, _ *store.WorkflowVersion) error {
	return nil
}

// GetWorkflowVersion implements store.Store.
func (*fakeStore) GetWorkflowVersion(_ context.Context, hash string) (*store.WorkflowVersion, error) {
	return nil, fmt.Errorf("%w: %s", store.ErrWorkflowVersionNotFound, hash)
}

// ListWorkflowVersions implements store.Store.
func (*fakeStore) ListWorkflowVersions(_ context.Context, _ string) ([]*store.WorkflowVersion, error) {
	return nil, nil
}

// AppendAudit implements store.Store, recording the row so governance tests
// can assert what was logged.
func (f *fakeStore) AppendAudit(_ context.Context, entry *store.AuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	copied := *entry
	f.audit = append(f.audit, &copied)

	return nil
}

// ListAudit implements store.Store, newest-first.
func (f *fakeStore) ListAudit(_ context.Context, filter store.AuditFilter) ([]*store.AuditEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]*store.AuditEntry, 0, len(f.audit))

	for _, entry := range slices.Backward(f.audit) {
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}

		copied := *entry
		out = append(out, &copied)
	}

	return out, nil
}

// SetWorkflowPause implements store.Store.
func (f *fakeStore) SetWorkflowPause(_ context.Context, pause *store.WorkflowPause) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	copied := *pause
	f.pauses[pause.WorkflowName] = &copied

	return nil
}

// ClearWorkflowPause implements store.Store.
func (f *fakeStore) ClearWorkflowPause(_ context.Context, workflowName string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	_, existed := f.pauses[workflowName]
	delete(f.pauses, workflowName)

	return existed, nil
}

// GetWorkflowPause implements store.Store, reporting whether the workflow is
// paused for the engine's pause check.
func (f *fakeStore) GetWorkflowPause(_ context.Context, workflowName string) (*store.WorkflowPause, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	pause, exists := f.pauses[workflowName]
	if !exists {
		return nil, false, nil
	}

	copied := *pause

	return &copied, true, nil
}

// ListWorkflowPauses implements store.Store.
func (f *fakeStore) ListWorkflowPauses(_ context.Context) ([]*store.WorkflowPause, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]*store.WorkflowPause, 0, len(f.pauses))

	for _, name := range slices.Sorted(maps.Keys(f.pauses)) {
		copied := *f.pauses[name]
		out = append(out, &copied)
	}

	return out, nil
}

// SpendSince implements store.Store, summing cost across this fake's action
// rows whose run was created at or after since.
func (f *fakeStore) SpendSince(_ context.Context, workflowName string, since time.Time) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var spend float64

	for runID, byName := range f.results {
		run, exists := f.runs[runID]
		if !exists || run.WorkflowName != workflowName || run.CreatedAt.Before(since) {
			continue
		}

		for _, result := range byName {
			spend += result.Usage.CostUSD
		}
	}

	return spend, nil
}

// CreateSession implements store.Store; the engine never touches chat.
func (*fakeStore) CreateSession(_ context.Context, _ *store.ChatSession) error {
	return nil
}

// GetSession implements store.Store.
func (*fakeStore) GetSession(_ context.Context, id string) (*store.ChatSession, error) {
	return nil, fmt.Errorf("%w: %s", store.ErrChatSessionNotFound, id)
}

// ListSessions implements store.Store.
func (*fakeStore) ListSessions(_ context.Context, _ store.ChatSessionFilter) ([]*store.ChatSession, error) {
	return nil, nil
}

// AppendMessage implements store.Store.
func (*fakeStore) AppendMessage(_ context.Context, _ *store.ChatMessage) error {
	return nil
}

// ListMessages implements store.Store.
func (*fakeStore) ListMessages(_ context.Context, _ string) ([]*store.ChatMessage, error) {
	return nil, nil
}

// SearchMessages implements store.Store.
func (*fakeStore) SearchMessages(_ context.Context, _ store.ChatSearchQuery) ([]*store.ChatMessage, error) {
	return nil, nil
}

// approvalKey keys one gate by run and action.
func approvalKey(runID, actionName string) string {
	return runID + "/" + actionName
}

// CreateApproval implements store.Store, defaulting workspace, status, and
// timestamp the way the real backends do.
func (f *fakeStore) CreateApproval(ctx context.Context, approval *store.Approval) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	copied := *approval

	if copied.WorkspaceID == "" {
		copied.WorkspaceID = store.WorkspaceFromContext(ctx)
	}

	if copied.Status == "" {
		copied.Status = store.ApprovalPending
	}

	if copied.CreatedAt.IsZero() {
		copied.CreatedAt = time.Now()
	}

	f.approvals[approvalKey(approval.RunID, approval.ActionName)] = &copied

	return nil
}

// GetApproval implements store.Store, scoped to the request workspace.
func (f *fakeStore) GetApproval(ctx context.Context, runID, actionName string) (*store.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	approval, exists := f.approvals[approvalKey(runID, actionName)]
	if !exists || approval.WorkspaceID != store.WorkspaceFromContext(ctx) {
		return nil, fmt.Errorf("%w: %s/%s", store.ErrApprovalNotFound, runID, actionName)
	}

	copied := *approval

	return &copied, nil
}

// ListPendingApprovals implements store.Store, ordered by action name.
func (f *fakeStore) ListPendingApprovals(ctx context.Context, runID string) ([]*store.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	workspace := store.WorkspaceFromContext(ctx)

	var out []*store.Approval

	for _, key := range slices.Sorted(maps.Keys(f.approvals)) {
		approval := f.approvals[key]
		if approval.RunID != runID || approval.WorkspaceID != workspace || approval.Status != store.ApprovalPending {
			continue
		}

		copied := *approval
		out = append(out, &copied)
	}

	return out, nil
}

// DecideApproval implements store.Store, moving a pending gate once.
func (f *fakeStore) DecideApproval(ctx context.Context, runID, actionName string, status store.ApprovalStatus, actor, comment string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	approval, exists := f.approvals[approvalKey(runID, actionName)]
	if !exists || approval.WorkspaceID != store.WorkspaceFromContext(ctx) || approval.Status != store.ApprovalPending {
		return fmt.Errorf("%w: %s/%s", store.ErrApprovalNotFound, runID, actionName)
	}

	approval.Status = status
	approval.Actor = actor
	approval.Comment = comment
	approval.DecidedAt = time.Now()

	return nil
}

// BindBridgeSession implements store.Store; the engine never touches the
// messaging bridge.
func (*fakeStore) BindBridgeSession(_ context.Context, _, _, _ string) error {
	return nil
}

// LookupBridgeSession implements store.Store.
func (*fakeStore) LookupBridgeSession(_ context.Context, transport, chatKey string) (string, error) {
	return "", fmt.Errorf("%w: %s/%s", store.ErrBridgeSessionNotFound, transport, chatKey)
}

// BindMessageThread implements store.Store.
func (*fakeStore) BindMessageThread(_ context.Context, _, _, _, _ string) error {
	return nil
}

// LookupMessageThread implements store.Store.
func (*fakeStore) LookupMessageThread(_ context.Context, _, _, key string) (string, error) {
	return "", fmt.Errorf("%w: %s", store.ErrMessageThreadNotFound, key)
}

// ReconcileInterrupted implements store.Store; the fake never has
// interrupted runs to reconcile.
func (*fakeStore) ReconcileInterrupted(_ context.Context) (int, error) {
	return 0, nil
}

// Prune implements store.Store as a no-op.
func (*fakeStore) Prune(_ context.Context, _ store.PruneOptions) (store.PruneStats, error) {
	return store.PruneStats{}, nil
}

// Close implements store.Store.
func (*fakeStore) Close() error {
	return nil
}

// runCount reports how many runs were ever created.
func (f *fakeStore) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.runs)
}

// storedRun returns a copy of one run row, or nil when absent.
func (f *fakeStore) storedRun(id string) *store.Run {
	f.mu.Lock()
	defer f.mu.Unlock()

	run, exists := f.runs[id]
	if !exists {
		return nil
	}

	copied := *run

	return &copied
}

// storedAction returns a copy of one action row, or nil when absent.
func (f *fakeStore) storedAction(runID, name string) *store.ActionResult {
	f.mu.Lock()
	defer f.mu.Unlock()

	result, exists := f.results[runID][name]
	if !exists {
		return nil
	}

	copied := *result

	return &copied
}

// transitionsFor returns every persisted status for one action, in order.
func (f *fakeStore) transitionsFor(runID, name string) []store.ActionStatus {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.transitions[runID+"/"+name])
}

// eventSink collects every emitted engine event. The mutex matters: the
// engine fires the hook from scheduling and worker goroutines.
type eventSink struct {
	seen []engine.Event
	mu   sync.Mutex
}

//nolint:gocritic // hugeParam: the signature is fixed by the Config.OnEvent hook contract
func (s *eventSink) collect(evt engine.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.seen = append(s.seen, evt)
}

func (s *eventSink) events() []engine.Event {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.seen)
}

// statusesFor returns the transition statuses one action emitted, in order.
func (s *eventSink) statusesFor(action string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var statuses []string

	for _, evt := range s.seen {
		if evt.Type == engine.EventActionTransition && evt.Action == action {
			statuses = append(statuses, evt.Status)
		}
	}

	return statuses
}

// fakeAgentExecutor scripts the engine's agent seam: ValidateAction returns
// a canned error and Execute returns a canned result or error while
// capturing the resolved action and task it received.
type fakeAgentExecutor struct {
	validateErr error
	execErr     error
	gotAction   *workflow.Action
	gotTask     *runner.Task
	result      runner.Result
	mu          sync.Mutex
}

var _ engine.AgentExecutor = (*fakeAgentExecutor)(nil)

// ValidateAction implements engine.AgentExecutor.
func (f *fakeAgentExecutor) ValidateAction(_ *workflow.Action) error {
	return f.validateErr
}

// Execute implements engine.AgentExecutor.
func (f *fakeAgentExecutor) Execute(_ context.Context, action *workflow.Action, task *runner.Task) (runner.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.gotAction = action
	f.gotTask = task

	return f.result, f.execErr
}

// captured returns the action and task the last Execute call received.
func (f *fakeAgentExecutor) captured() (*workflow.Action, *runner.Task) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.gotAction, f.gotTask
}

// fakeWorkflowSource is a map-backed engine.WorkflowSource: it resolves a child
// workflow-call target by name from a fixed set of parsed workflows, so a
// workflow-call action runs a real child run without touching the filesystem.
type fakeWorkflowSource struct {
	flows map[string]*workflow.Workflow
}

var _ engine.WorkflowSource = (*fakeWorkflowSource)(nil)

// Resolve implements engine.WorkflowSource.
func (f *fakeWorkflowSource) Resolve(name string) (*workflow.Workflow, error) {
	flow, exists := f.flows[name]
	if !exists {
		return nil, fmt.Errorf("%w: %s", errWorkflowNotFound, name)
	}

	return flow, nil
}

// fakeSecrets is a map-backed SecretSource.
type fakeSecrets struct {
	values map[string]string
}

func (f *fakeSecrets) Get(_ context.Context, name string) (string, error) {
	value, exists := f.values[name]
	if !exists {
		return "", fmt.Errorf("%w: %s", errSecretNotFound, name)
	}

	return value, nil
}

// recorder collects action names in completion order through a local_fn, so
// ordering tests observe real execution rather than store timestamps.
type recorder struct {
	names []string
	mu    sync.Mutex
}

func (r *recorder) record(_ context.Context, task *runner.Task) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.names = append(r.names, task.ActionName)

	return task.ActionName, nil
}

func (r *recorder) order() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.names)
}

// paramSink captures the fully resolved params one action received, and the
// definition its task carried.
type paramSink struct {
	params     map[string]any
	definition []runner.DefinitionFile
	mu         sync.Mutex
}

func (p *paramSink) capture(_ context.Context, task *runner.Task) (any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.params = maps.Clone(task.Params)
	p.definition = task.Definition

	return "captured", nil
}

func (p *paramSink) seenDefinition() []runner.DefinitionFile {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.definition
}

func (p *paramSink) snapshot() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.params
}
