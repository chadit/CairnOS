// Package store defines the durable state contract for runs and action
// results (.agents/spec/04-execution-engine.md, "State persistence"). The
// engine depends only on this interface; SQLite implements it in the sqlite
// subpackage, Postgres arrives in Phase 4c, and tests use an in-memory fake.
// That seam is the Phase 0 acceptance criterion from
// .agents/spec/05-memory-and-knowledge.md.
package store

import (
	"context"
	"time"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/pkg/runner"
)

// RunStatus is the run lifecycle state.
type RunStatus string

// Run lifecycle states. Timeouts end runs as failed (engine.run_timeout);
// canceled is operator-initiated only, so history can tell them apart.
const (
	RunPending   RunStatus = "pending"
	RunRunning   RunStatus = "running"
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
	RunCanceled  RunStatus = "canceled"
)

// ActionStatus is the per-action state within a run.
type ActionStatus string

// Action states. Skipped means "correctly did not run" (false condition or a
// skipped upstream) and never fails a run.
const (
	ActionPending   ActionStatus = "pending"
	ActionReady     ActionStatus = "ready"
	ActionRunning   ActionStatus = "running"
	ActionSucceeded ActionStatus = "succeeded"
	ActionFailed    ActionStatus = "failed"
	ActionSkipped   ActionStatus = "skipped"
)

// Run is one execution of a workflow.
type Run struct {
	ID           string
	WorkflowName string
	WorkflowHash string
	WorkspaceID  string // reserved for multi-tenancy; "default" until Phase 5
	Status       RunStatus
	Inputs       map[string]any
	Trigger      string // cli|schedule|api|a2a|webhook
	Error        *cairnerr.Error
	CreatedAt    time.Time
	StartedAt    time.Time // zero until the run starts
	FinishedAt   time.Time // zero until terminal
}

// ActionResult is the persisted outcome of one action within a run.
type ActionResult struct {
	RunID      string
	ActionName string
	Runner     string
	Status     ActionStatus
	Attempt    int
	Output     any
	Usage      runner.Usage
	Meta       map[string]string
	Error      *cairnerr.Error
	StartedAt  time.Time
	FinishedAt time.Time
}

// Agent-step kinds (.agents/spec/15-agent-loop.md, "Transcript persistence").
const (
	// StepDriverCall records a driver turn that requested (or badly
	// requested) a tool call.
	StepDriverCall = "driver_call"
	// StepToolCall records one tool execution and its result or failure.
	StepToolCall = "tool_call"
	// StepFinal records the driver turn that produced the final answer.
	StepFinal = "final"
)

// AgentStep is one persisted agent-loop transcript step. The loop appends a
// row per driver decision and per tool result, so a failed or budget-blown
// agent action is debuggable from history alone.
type AgentStep struct {
	RunID      string
	ActionName string
	Step       int    // 1-based position within the action's transcript
	Kind       string // StepDriverCall, StepToolCall, or StepFinal
	Tool       string // tool name, set on StepToolCall rows
	Input      any    // tool arguments for StepToolCall rows
	Output     any    // driver decision, tool result, or final answer
	Usage      runner.Usage
	CreatedAt  time.Time
}

// SuggestionStatus is the review state of a tuning suggestion.
type SuggestionStatus string

// Suggestion review states (.agents/spec/06-observability-and-tuning.md,
// "Review flow"). Phase 3 stops at approved/rejected: approval prints an
// annotated diff for the human to apply by hand, and "applied" only arrives
// with Phase 6 auto-apply.
const (
	SuggestionOpen     SuggestionStatus = "open"
	SuggestionApproved SuggestionStatus = "approved"
	SuggestionRejected SuggestionStatus = "rejected"
	// SuggestionApplied marks a suggestion the Phase 6 autonomic applier wrote
	// to a workflow file. It only ever follows approved, and only for the
	// output-neutral kinds; the version store holds the rollback point.
	SuggestionApplied SuggestionStatus = "applied"
)

// SuggestionKind names what a tuning suggestion proposes
// (.agents/spec/06-observability-and-tuning.md, "Suggestion kinds").
type SuggestionKind string

// Phase 3 suggestion kinds. Prompt refactoring is deliberately absent: it is
// the open research problem and is held back until R1 resolves.
const (
	SuggestionCaching     SuggestionKind = "caching"
	SuggestionBatching    SuggestionKind = "batching"
	SuggestionParamTuning SuggestionKind = "param_tuning"
)

// Confidence bands for estimated savings. Output-changing kinds never rise
// above low in Phase 3 because verifying output quality is unsolved (R1).
const (
	ConfidenceLow    = "low"
	ConfidenceMedium = "medium"
	ConfidenceHigh   = "high"
)

// Savings is an estimated improvement, never a promise: the numbers come
// from observed run history and carry a confidence band.
type Savings struct {
	CostUSDPerRun float64
	LatencyDelta  time.Duration
	Confidence    string // ConfidenceLow, ConfidenceMedium, or ConfidenceHigh
}

// Suggestion is one evidence-backed tuning proposal. Nothing in Phase 3
// applies it: a human approves or rejects, and approval only renders the
// proposed change for manual application.
type Suggestion struct {
	ID         string
	Kind       SuggestionKind
	Target     string // workflow or "workflow.action"
	Rationale  string // why, in plain language, with the evidence
	Evidence   map[string]any
	Proposed   map[string]any
	EstSavings Savings
	Status     SuggestionStatus
	CreatedAt  time.Time
}

// SuggestionFilter narrows ListSuggestions.
type SuggestionFilter struct {
	Status SuggestionStatus
	Kind   SuggestionKind
	Limit  int
}

// WorkflowVersion is one captured snapshot of a workflow definition, the
// rollback point auto-apply records before it edits a file
// (.agents/spec/13-roadmap.md, Phase 6; .agents/spec/09-a2a-and-hive.md). The
// hash identifies the exact bytes (it is the same SHA-256 a run records), so a
// version captured twice from the same file collides on its primary key rather
// than duplicating; DefinitionTOML is the verbatim file content to restore.
type WorkflowVersion struct {
	Hash           string
	WorkflowName   string
	DefinitionTOML string
	CreatedAt      time.Time
}

// WorkflowVersionStore is the rollback slice of the store
// (.agents/spec/13-roadmap.md, Phase 6: "a workflow-definition version store
// so an auto-applied change can be rolled back"). Split out so the autonomic
// applier depends on this narrow surface instead of all of Store.
type WorkflowVersionStore interface {
	// SaveWorkflowVersion records one definition snapshot. A zero CreatedAt is
	// stamped with the current time. Saving the same (hash, workflow) twice is
	// a no-op: the bytes are already captured, so re-recording them changes
	// nothing.
	SaveWorkflowVersion(ctx context.Context, version *WorkflowVersion) error

	// GetWorkflowVersion fetches one captured definition by its hash.
	GetWorkflowVersion(ctx context.Context, hash string) (*WorkflowVersion, error)

	// ListWorkflowVersions returns one workflow's captured definitions
	// newest-first, so the most recent rollback point is the first row.
	ListWorkflowVersions(ctx context.Context, workflowName string) ([]*WorkflowVersion, error)
}

// ListFilter narrows ListRuns.
type ListFilter struct {
	WorkflowName string
	Status       RunStatus
	Limit        int
}

// PruneOptions bounds a retention sweep (.agents/spec/04-execution-engine.md,
// "Retention and pruning").
type PruneOptions struct {
	// RunsOlderThan deletes runs and their action results past this age.
	RunsOlderThan time.Duration
	// OutputsOlderThan truncates action outputs sooner than run deletion, so
	// large outputs do not linger while run metadata stays for reporting.
	OutputsOlderThan time.Duration
	// Now anchors age math so tests can use a fixed clock.
	Now time.Time
}

// PruneStats reports what a sweep removed.
type PruneStats struct {
	RunsDeleted        int
	OutputsTruncated   int
	SuggestionsDeleted int
}

// SuggestionStore is the tuning-suggestion slice of the store
// (.agents/spec/06-observability-and-tuning.md, "Review flow"), split out so
// the tuning engine and its surfaces can depend on this narrow surface
// instead of all of Store.
type SuggestionStore interface {
	// SaveSuggestion persists one suggestion. A zero CreatedAt is stamped
	// with the current time and an empty Status becomes open.
	SaveSuggestion(ctx context.Context, suggestion *Suggestion) error

	// GetSuggestion fetches one suggestion by id.
	GetSuggestion(ctx context.Context, id string) (*Suggestion, error)

	// ListSuggestions returns suggestions newest-first, narrowed by the
	// filter.
	ListSuggestions(ctx context.Context, filter SuggestionFilter) ([]*Suggestion, error)

	// UpdateSuggestionStatus moves a suggestion through its review states.
	UpdateSuggestionStatus(ctx context.Context, id string, status SuggestionStatus) error
}

// Chat message roles (.agents/spec/13-roadmap.md, Phase 9). A conversation
// alternates user prompts and assistant replies, with tool rows recording the
// agent's tool calls so a replayed history shows what the agent did, not just
// what it said.
const (
	// ChatRoleUser marks a message the human or bot sent.
	ChatRoleUser = "user"
	// ChatRoleAssistant marks the chat agent's reply.
	ChatRoleAssistant = "assistant"
	// ChatRoleTool marks a recorded tool call and its result, kept in history
	// so the next turn's prompt shows the agent what it already looked up.
	ChatRoleTool = "tool"
)

// ChatSession is one long-lived conversation (.agents/spec/13-roadmap.md,
// Phase 9): a persisted thread tied to a workspace, carrying the personality
// (system prompt) that frames every turn. The session outlives any single
// turn, so its context persists across calls.
type ChatSession struct {
	ID          string
	WorkspaceID string // reserved for multi-tenancy; "default" until a token scopes it
	Personality string // named personality whose system prompt frames the turn
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ChatMessage is one entry in a conversation, ordered by Seq within its
// session. Roles are ChatRoleUser, ChatRoleAssistant, or ChatRoleTool; the
// content is the message text (or the rendered tool exchange for a tool row).
type ChatMessage struct {
	SessionID string
	Seq       int // 1-based position within the session
	Role      string
	Content   string
	CreatedAt time.Time
}

// ChatSessionFilter narrows ListSessions.
type ChatSessionFilter struct {
	// Limit caps the result count; zero means no cap.
	Limit int
}

// ChatStore is the conversation slice of the store (.agents/spec/13-roadmap.md,
// Phase 9), split out so the chat agent and its API surface depend on this
// narrow surface instead of all of Store. Every method is workspace-scoped
// through the request context the way runs are, so one tenant never reads
// another's conversations.
type ChatStore interface {
	// CreateSession persists a new conversation. A zero CreatedAt/UpdatedAt is
	// stamped with the current time and an empty WorkspaceID becomes the
	// request workspace.
	CreateSession(ctx context.Context, session *ChatSession) error

	// GetSession fetches one session by id, scoped to the request workspace so
	// a session in another workspace reads as ErrChatSessionNotFound.
	GetSession(ctx context.Context, id string) (*ChatSession, error)

	// ListSessions returns the request workspace's sessions, most-recently-
	// updated first, narrowed by the filter.
	ListSessions(ctx context.Context, filter ChatSessionFilter) ([]*ChatSession, error)

	// AppendMessage records one message at the next sequence in its session
	// and stamps the session's updated_at. Messages are insert-only; a
	// conversation is never rewritten. Appending to a session in another
	// workspace fails with ErrChatSessionNotFound.
	AppendMessage(ctx context.Context, message *ChatMessage) error

	// ListMessages returns one session's messages ordered by seq, scoped to
	// the request workspace.
	ListMessages(ctx context.Context, sessionID string) ([]*ChatMessage, error)
}

// AgentStepStore is the transcript slice of the store
// (.agents/spec/15-agent-loop.md, "Transcript persistence"), split out so
// the agent loop can depend on this narrow surface instead of all of Store.
type AgentStepStore interface {
	// AppendAgentStep persists one agent-loop transcript step. The loop
	// writes steps in order; (RunID, ActionName, Step) is unique.
	AppendAgentStep(ctx context.Context, step *AgentStep) error

	// ListAgentSteps returns one agent action's transcript ordered by step.
	ListAgentSteps(ctx context.Context, runID, actionName string) ([]*AgentStep, error)
}

// Store is the durable record of runs and action results. Implementations
// must be safe for concurrent use; the engine writes from many goroutines.
//
//nolint:interfacebloat // Store is the deliberate aggregate of the whole persistence surface; consumers needing less depend on the narrow slices (AgentStepStore, SuggestionStore) instead
type Store interface {
	// CreateRun persists a new run in its initial state.
	CreateRun(ctx context.Context, run *Run) error

	// UpdateRunStatus moves a run through its lifecycle, stamping StartedAt
	// on the first transition to running and FinishedAt on terminal states.
	UpdateRunStatus(ctx context.Context, id string, status RunStatus, runErr *cairnerr.Error) error

	// GetRun fetches one run by id, including its terminal error if any.
	GetRun(ctx context.Context, id string) (*Run, error)

	// ListRuns returns runs newest-first, narrowed by the filter.
	ListRuns(ctx context.Context, filter ListFilter) ([]*Run, error)

	// UpsertActionResult writes an action's current state. Called on every
	// transition so a crash mid-run loses nothing already finished.
	UpsertActionResult(ctx context.Context, result *ActionResult) error

	// AgentStepStore carries the agent-loop transcript methods.
	AgentStepStore

	// SuggestionStore carries the tuning-suggestion review methods.
	SuggestionStore

	// WorkflowVersionStore carries the rollback-snapshot methods Phase 6
	// auto-apply records before it edits a workflow file.
	WorkflowVersionStore

	// GovernanceStore carries the budget-spend, audit-log, and workflow-pause
	// methods (.agents/spec/12-ui-and-governance.md).
	GovernanceStore

	// ChatStore carries the conversation-session and message methods Phase 9
	// chat persists (.agents/spec/13-roadmap.md, Phase 9).
	ChatStore

	// ListActionResults returns every action row for a run.
	ListActionResults(ctx context.Context, runID string) ([]*ActionResult, error)

	// ReconcileInterrupted marks running actions of running runs as failed
	// with engine.interrupted and finalizes those runs. Called once at
	// startup; CairnOS never silently resumes a call it cannot prove alive.
	ReconcileInterrupted(ctx context.Context) (int, error)

	// Prune enforces retention and reports what it removed.
	Prune(ctx context.Context, opts PruneOptions) (PruneStats, error)

	// Close releases the underlying database.
	Close() error
}
