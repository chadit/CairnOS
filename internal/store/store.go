// Package store defines the durable state contract for runs and action results.
// The engine depends only on this interface; SQLite implements it in the sqlite
// subpackage, Postgres arrives in Phase 4c, and tests use an in-memory fake.
// That seam is the Phase 0 acceptance criterion.
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
	// RunHeld is a run parked on a human-approval action. No goroutine runs
	// while it waits, so ReconcileInterrupted leaves it alone across a restart,
	// and it is not terminal: deciding the approval resumes the same run.
	RunHeld RunStatus = "held"
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
	// ActionWaiting marks an approval action parked pending a human decision.
	// It is not terminal: the run finalizes as held while the action waits, and
	// the action re-runs to a terminal state when the run resumes.
	ActionWaiting ActionStatus = "waiting"
)

// Run is one execution of a workflow.
type Run struct {
	CreatedAt    time.Time
	StartedAt    time.Time // zero until the run starts
	FinishedAt   time.Time // zero until terminal
	Inputs       map[string]any
	Error        *cairnerr.Error
	ID           string
	WorkflowName string
	WorkflowHash string
	WorkspaceID  string // reserved for multi-tenancy; "default" until Phase 5
	Status       RunStatus
	Trigger      string // cli|schedule|api|a2a|webhook|webhook_test|poll|replay|error|workflow

	// ParentRunID links a child run to the run that spawned it: a replay to
	// the run it reruns, a workflow-call child to its caller, an error workflow
	// to the failed run that fired it. Empty means a root run.
	ParentRunID string
}

// ActionResult is the persisted outcome of one action within a run.
type ActionResult struct {
	StartedAt  time.Time
	FinishedAt time.Time
	Output     any
	Meta       map[string]string
	Error      *cairnerr.Error
	RunID      string
	ActionName string
	Runner     string
	Status     ActionStatus
	Usage      runner.Usage
	Attempt    int
}

// Agent-step kinds.
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
	CreatedAt  time.Time
	Input      any // tool arguments for StepToolCall rows
	Output     any // driver decision, tool result, or final answer
	RunID      string
	ActionName string
	Kind       string // StepDriverCall, StepToolCall, or StepFinal
	Tool       string // tool name, set on StepToolCall rows
	Usage      runner.Usage
	Step       int // 1-based position within the action's transcript
}

// SuggestionStatus is the review state of a tuning suggestion.
type SuggestionStatus string

// Suggestion review states. Phase 3 stops at approved/rejected: approval prints
// an annotated diff for the human to apply by hand, and "applied" only arrives
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

// SuggestionKind names what a tuning suggestion proposes.
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
	Confidence    string // ConfidenceLow, ConfidenceMedium, or ConfidenceHigh
	CostUSDPerRun float64
	LatencyDelta  time.Duration
}

// Suggestion is one evidence-backed tuning proposal. Nothing in Phase 3
// applies it: a human approves or rejects, and approval only renders the
// proposed change for manual application.
type Suggestion struct {
	CreatedAt  time.Time
	Evidence   map[string]any
	Proposed   map[string]any
	ID         string
	Kind       SuggestionKind
	Target     string // workflow or "workflow.action"
	Rationale  string // why, in plain language, with the evidence
	Status     SuggestionStatus
	EstSavings Savings
}

// SuggestionFilter narrows ListSuggestions.
type SuggestionFilter struct {
	Status SuggestionStatus
	Kind   SuggestionKind
	Limit  int
}

// WorkflowVersion is one captured snapshot of a workflow definition, the
// rollback point auto-apply records before it edits a file. The hash identifies
// the exact bytes (the same SHA-256 a run records), so capturing one file twice
// collides on the primary key rather than duplicating. DefinitionTOML is the
// verbatim content to restore.
type WorkflowVersion struct {
	CreatedAt      time.Time
	Hash           string
	WorkflowName   string
	DefinitionTOML string
}

// WorkflowVersionStore is the rollback slice of the store. Split out so the
// autonomic applier depends on this narrow surface instead of all of Store.
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

// PruneOptions bounds a retention sweep.
type PruneOptions struct {
	// Now anchors age math so tests can use a fixed clock.
	Now time.Time

	// RunsOlderThan deletes runs and their action results past this age.
	RunsOlderThan time.Duration

	// OutputsOlderThan truncates action outputs sooner than run deletion, so
	// large outputs do not linger while run metadata stays for reporting.
	OutputsOlderThan time.Duration
}

// PruneStats reports what a sweep removed.
type PruneStats struct {
	RunsDeleted        int
	OutputsTruncated   int
	SuggestionsDeleted int
}

// SuggestionStore is the tuning-suggestion slice of the store, split out so the
// tuning engine and its surfaces can depend on this narrow surface instead of
// all of Store.
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

// Chat message roles. A conversation alternates user prompts and assistant
// replies, with tool rows recording the agent's tool calls so a replayed
// history shows what the agent did, not just what it said. A system row carries
// framing a client seeds ahead of the first user turn.
const (
	// ChatRoleUser marks a message the human or bot sent.
	ChatRoleUser = "user"
	// ChatRoleAssistant marks the chat agent's reply.
	ChatRoleAssistant = "assistant"
	// ChatRoleSystem marks a seeded framing message, the role an
	// OpenAI-compatible client's `system` message lands as when the adapter
	// replays prior history into a fresh session.
	ChatRoleSystem = "system"
	// ChatRoleTool marks a recorded tool call and its result, kept in history
	// so the next turn's prompt shows the agent what it already looked up.
	ChatRoleTool = "tool"
)

// ChatSession is one long-lived conversation: a persisted thread tied to a
// workspace, carrying the personality (system prompt) that frames every turn.
// The session outlives any single turn, so its context persists across calls.
type ChatSession struct {
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ID          string
	WorkspaceID string // reserved for multi-tenancy; "default" until a token scopes it
	Personality string // named personality whose system prompt frames the turn
}

// ChatMessage is one entry in a conversation, ordered by Seq within its
// session. Roles are ChatRoleUser, ChatRoleAssistant, ChatRoleSystem, or
// ChatRoleTool; the content is the message text (or the rendered tool exchange
// for a tool row).
type ChatMessage struct {
	CreatedAt time.Time
	SessionID string
	Role      string
	Content   string
	Seq       int // 1-based position within the session
}

// ChatSessionFilter narrows ListSessions.
type ChatSessionFilter struct {
	// Limit caps the result count; zero means no cap.
	Limit int
}

// ChatSearchQuery narrows SearchMessages.
type ChatSearchQuery struct {
	// Text is the full-text query. Empty returns no rows at the store;
	// the API layer rejects it with InvalidArgument before it gets here.
	Text string

	// SessionID optionally narrows hits to one session. Empty searches
	// every session in the request workspace.
	SessionID string

	// Limit caps the result count; zero means no cap, matching
	// ChatSessionFilter.Limit and memory's SearchQuery.
	Limit int
}

// ChatStore is the conversation slice of the store, split out so the chat agent
// and its API surface depend on this narrow surface instead of all of Store.
// Every method is workspace-scoped through the request context the way runs
// are, so one tenant never reads another's conversations.
type ChatStore interface {
	ChatSessionStore
	ChatMessageStore
}

// ChatSessionStore creates and finds conversations, the half of ChatStore a
// caller needs before it has a session id to append to.
type ChatSessionStore interface {
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
}

// ChatMessageStore records and reads the messages inside a session. Messages
// are insert-only, so this role has no update or delete.
type ChatMessageStore interface {
	// AppendMessage records one message at the next sequence in its session
	// and stamps the session's updated_at. Role is one of ChatRoleUser,
	// ChatRoleAssistant, ChatRoleSystem, or ChatRoleTool; the column is a plain
	// string, so a new role needs no schema change. Messages are insert-only; a
	// conversation is never rewritten. Appending to a session in another
	// workspace fails with ErrChatSessionNotFound.
	AppendMessage(ctx context.Context, message *ChatMessage) error

	// ListMessages returns one session's messages ordered by seq, scoped to
	// the request workspace.
	ListMessages(ctx context.Context, sessionID string) ([]*ChatMessage, error)

	// SearchMessages returns messages whose content matches the query,
	// best match first with (session_id, seq) as the stable tie-break,
	// scoped to the request workspace. A SessionID filter naming a foreign
	// or missing session simply matches nothing; it is a filter, not a
	// lookup, so it never reports ErrChatSessionNotFound.
	SearchMessages(ctx context.Context, query ChatSearchQuery) ([]*ChatMessage, error)
}

// BridgeSessionStore maps a messaging-transport chat to its chat session, so
// the binding survives a serve restart. Every method is workspace-scoped
// through the request context: the bridge stamps the mapped workspace the way
// the gRPC interceptor stamps a verified token's workspace, so a chat bound in
// one tenant is invisible from another.
type BridgeSessionStore interface {
	// BindBridgeSession upserts the binding for (transport, chatKey) in the
	// request workspace. A rebind (the bridge's /new path) overwrites the prior
	// session id rather than failing on the primary key, so switching a chat to
	// a fresh conversation is a single call.
	BindBridgeSession(ctx context.Context, transport, chatKey, sessionID string) error

	// LookupBridgeSession returns the bound session id for (transport, chatKey)
	// in the request workspace, or ErrBridgeSessionNotFound when no binding
	// exists. The bridge reads that sentinel as first contact and creates then
	// binds a session.
	LookupBridgeSession(ctx context.Context, transport, chatKey string) (string, error)
}

// MessageThreadStore remembers the root post each message thread_key opened, so
// a scheduled workflow keeps replying in one thread across runs and restarts.
type MessageThreadStore interface {
	// BindMessageThread upserts the root post id for (identity, target, key) in
	// the request workspace, overwriting a root that went missing.
	BindMessageThread(ctx context.Context, identity, target, key, rootID string) error

	// LookupMessageThread returns the bound root post id, or
	// ErrMessageThreadNotFound when no post has opened that thread yet.
	LookupMessageThread(ctx context.Context, identity, target, key string) (string, error)
}

// ApprovalStatus is the decision state of a human-approval gate.
type ApprovalStatus string

// Approval decision states. A gate opens pending and moves once to approved or
// denied.
const (
	ApprovalPending  ApprovalStatus = "pending"
	ApprovalApproved ApprovalStatus = "approved"
	ApprovalDenied   ApprovalStatus = "denied"
)

// Approval is one human-approval gate on a run's approval action. The primary
// key is (RunID, ActionName): parallel branches can each park an approval, so
// two pending approvals in one run is a legal state. Message is the resolved
// prompt, which validation keeps secret-free. Actor and Comment record who
// decided and why, empty while pending.
type Approval struct {
	CreatedAt   time.Time
	DecidedAt   time.Time // zero while pending
	RunID       string
	ActionName  string
	WorkspaceID string // scoping, same convention as runs
	Status      ApprovalStatus
	Message     string
	Actor       string
	Comment     string
}

// ApprovalStore is the human-approval slice of the store, split out so the
// engine and the decision surfaces depend on this narrow surface instead of all
// of Store. Every method is workspace-scoped the way runs are, so a decision or
// read across the tenancy boundary reads as not-found.
type ApprovalStore interface {
	// CreateApproval records a new pending approval gate. A zero CreatedAt is
	// stamped with the current time, an empty WorkspaceID becomes the request
	// workspace, and an empty Status becomes pending.
	CreateApproval(ctx context.Context, approval *Approval) error

	// GetApproval fetches one gate by (runID, actionName), scoped to the request
	// workspace so an approval in another workspace reads as ErrApprovalNotFound.
	GetApproval(ctx context.Context, runID, actionName string) (*Approval, error)

	// ListPendingApprovals returns a run's still-pending gates ordered by action
	// name, scoped to the request workspace. A name-omitted decision uses it to
	// pick the single pending gate or to list the ambiguous ones.
	ListPendingApprovals(ctx context.Context, runID string) ([]*Approval, error)

	// DecideApproval moves a pending gate to approved or denied, recording the
	// actor and comment and stamping decided_at. Deciding a gate that is missing
	// in the request workspace, already decided, or owned by another tenant fails
	// with ErrApprovalNotFound.
	DecideApproval(ctx context.Context, runID, actionName string, status ApprovalStatus, actor, comment string) error
}

// AgentStepStore is the transcript slice of the store, split out so the agent
// loop can depend on this narrow surface instead of all of Store.
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
	// methods.
	GovernanceStore

	// ChatStore carries the conversation-session and message methods Phase 9
	// chat persists.
	ChatStore

	// ApprovalStore carries the human-approval gate methods the engine parks a
	// run on and the decision surfaces resolve.
	ApprovalStore

	// BridgeSessionStore carries the messaging-bridge chat-to-session bindings
	// the Telegram long-poll loop resolves per turn.
	BridgeSessionStore

	// MessageThreadStore carries the thread_key roots the message runner posts
	// replies under.
	MessageThreadStore

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
