package chat

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/pkg/runner"
)

// maxTurnIterations bounds the driver turns inside one chat turn, the same wall
// an agent action carries. A turn that has not answered or proposed within the
// cap fails rather than looping forever.
const maxTurnIterations = 12

// maxToolResultChars caps how much of one tool result feeds back into the next
// driver prompt, so a large vault document cannot inflate every following call.
const maxToolResultChars = 4096

// driver task param keys, matching the LLM runners' shared prompt/system
// contract that internal/agentloop also uses.
const (
	paramPrompt = "prompt"
	paramSystem = "system"
)

// WorkflowTrigger fires a confirmed workflow under normal engine governance
// and streams its terminal status back. The narrow surface keeps internal/chat
// free of an import cycle with internal/api. The serve wiring satisfies it
// with an adapter over the run manager and resolver (D9: the spawned run obeys
// Phase 5b budgets and pause, at depth exactly one).
type WorkflowTrigger interface {
	// Run resolves the named workflow, starts it under engine governance, and
	// returns the started run id with a channel that delivers the terminal run
	// status once. A resolve or governance refusal (an unknown workflow, a
	// paused workflow, a budget breach) returns an error before any run id.
	Run(ctx context.Context, name string, inputs map[string]any) (runID string, done <-chan RunResult, err error)
}

// RunResult is one fired workflow's terminal outcome.
type RunResult struct {
	Err    error
	Status string
}

// Config wires one Agent.
type Config struct {
	// Store persists sessions and messages.
	Store store.ChatStore

	// Driver is the LLM runner that plans the turn, the same ToolCaller an
	// agent action uses as its driver.
	Driver runner.ToolCaller

	// Skills lists and loads operator-authored skills a turn can invoke. Nil
	// disables skill invocation, so a node without a skills directory runs chat
	// unchanged. A loaded skill influences the driver prompt only: it adds no
	// tool, raises no budget, and cannot move the D9 confirmation gate.
	Skills SkillSource

	// Trigger fires a confirmed workflow. Nil disables the gate: a run_workflow
	// proposal still raises the confirmation event, but an approval reports that
	// triggering is unavailable instead of starting a run.
	Trigger WorkflowTrigger

	// Tools are the read tools the agent may call inline (memory, vault_query,
	// skill, usage), keyed by name. run_workflow is not here: its call is
	// intercepted into the confirmation gate, never executed.
	Tools map[string]runner.Runner

	// Personalities resolves a session's personality to its system prompt.
	Personalities *Personalities

	// NewID mints session and proposal ids.
	NewID func() string

	// ContextDir is the directory holding per-workspace context docs
	// (<ContextDir>/<workspace>.md). Empty disables workspace context, so a node
	// without one runs chat unchanged. A loaded doc influences the driver prompt
	// only, the same additive posture as a skill.
	ContextDir string

	// Specs are the tool descriptors handed to the driver each turn, including
	// run_workflow's, in a stable order.
	Specs []runner.ToolSpec

	// FileAllowlist is the set of absolute directory roots an @-file reference in
	// a user message may read from. Empty (the default) leaves every @-token
	// inert: no reads, no notes, plain text. A path is readable only when its
	// symlink-resolved form sits inside a symlink-resolved root.
	FileAllowlist []string

	// MaxFileBytes caps how much of one @-file reference inlines into the
	// transcript; a larger file truncates at the cap with the byte counts in the
	// record. Zero leaves an inlined file untruncated.
	MaxFileBytes int

	// TurnBudgetUSD caps one chat turn's driver-and-tool spend. A turn whose
	// estimated cost reaches the cap ends with a budget error, so a runaway
	// conversation cannot bill without bound. Zero leaves the turn uncapped.
	TurnBudgetUSD float64
}

// Agent runs conversation turns over the agent-loop driver contract. It is safe
// for concurrent use across sessions; per-session pending-proposal state is
// guarded by a mutex.
type Agent struct {
	pending map[string]*pendingProposal // sessionID -> the gate awaiting confirmation
	cfg     Config
	mu      sync.Mutex
}

// New builds an Agent. NewID falls back to store.NewID when unset. The config
// is taken by pointer because it is a wide struct (gocritic hugeParam).
func New(cfg *Config) *Agent {
	if cfg.NewID == nil {
		cfg.NewID = store.NewID
	}

	return &Agent{cfg: *cfg, pending: map[string]*pendingProposal{}}
}

// CreateSession opens a new conversation with the given personality, falling
// back to the default when the name is empty. An unknown (non-empty)
// personality is rejected so a typo fails at creation rather than silently
// running the default.
func (a *Agent) CreateSession(ctx context.Context, personality string) (*store.ChatSession, error) {
	if personality != "" && !a.cfg.Personalities.Known(personality) {
		return nil, fmt.Errorf("%w: %q", ErrUnknownPersonality, personality)
	}

	if personality == "" {
		personality = a.cfg.Personalities.DefaultName()
	}

	session := &store.ChatSession{
		ID:          a.cfg.NewID(),
		WorkspaceID: store.WorkspaceFromContext(ctx),
		Personality: personality,
	}

	if err := a.cfg.Store.CreateSession(ctx, session); err != nil {
		return nil, fmt.Errorf("create chat session: %w", err)
	}

	return a.GetSession(ctx, session.ID)
}

// GetSession fetches one session, scoped to the request workspace.
func (a *Agent) GetSession(ctx context.Context, id string) (*store.ChatSession, error) {
	session, err := a.cfg.Store.GetSession(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get chat session: %w", err)
	}

	return session, nil
}

// ListSessions lists the workspace's sessions newest-first.
func (a *Agent) ListSessions(ctx context.Context, limit int) ([]*store.ChatSession, error) {
	sessions, err := a.cfg.Store.ListSessions(ctx, store.ChatSessionFilter{Limit: limit})
	if err != nil {
		return nil, fmt.Errorf("list chat sessions: %w", err)
	}

	return sessions, nil
}

// ListMessages returns one session's full transcript, the persisted context
// that replays into the next turn.
func (a *Agent) ListMessages(ctx context.Context, sessionID string) ([]*store.ChatMessage, error) {
	messages, err := a.cfg.Store.ListMessages(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list chat messages: %w", err)
	}

	return messages, nil
}

// SearchMessages runs full-text search over the workspace's transcripts, best
// match first with (session_id, seq) as the stable tie-break.
func (a *Agent) SearchMessages(ctx context.Context, query store.ChatSearchQuery) ([]*store.ChatMessage, error) {
	messages, err := a.cfg.Store.SearchMessages(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("search chat messages: %w", err)
	}

	return messages, nil
}

// emit sends one event, returning the context error if the consumer's context
// ended so a dropped client stops the turn promptly. The event rides by pointer
// because it is a wide struct (gocritic hugeParam).
func emit(ctx context.Context, out chan<- Event, event *Event) error {
	select {
	case out <- *event:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("chat turn canceled: %w", ctx.Err())
	}
}

// truncateResult bounds a rendered tool result so one large value does not
// dominate the next prompt.
func truncateResult(text string) string {
	if len(text) <= maxToolResultChars {
		return text
	}

	return text[:maxToolResultChars] + "... (truncated)"
}

// Each history line reads "role: content", one message per line.
const (
	historyRoleSep = ": "
	historyLineEnd = "\n"
)

// joinHistory renders the persisted messages into the prompt's conversation
// section, so the next turn sees the whole thread.
func joinHistory(messages []*store.ChatMessage) string {
	var total int

	for _, message := range messages {
		total += len(message.Role) + len(historyRoleSep) + len(message.Content) + len(historyLineEnd)
	}

	var text strings.Builder

	// Grow once up front: the per-message writes must not reallocate.
	text.Grow(total)

	for _, message := range messages {
		text.WriteString(message.Role)
		text.WriteString(historyRoleSep)
		text.WriteString(message.Content)
		text.WriteString(historyLineEnd)
	}

	return text.String()
}

// pendingFor reports the gate awaiting confirmation for one session, if any.
func (a *Agent) pendingFor(sessionID string) (*pendingProposal, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	proposal, waiting := a.pending[sessionID]

	return proposal, waiting
}

// setPending records the gate for one session.
func (a *Agent) setPending(sessionID string, proposal *pendingProposal) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.pending[sessionID] = proposal
}

// clearPending drops the gate for one session once it is confirmed or denied.
func (a *Agent) clearPending(sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	delete(a.pending, sessionID)
}
