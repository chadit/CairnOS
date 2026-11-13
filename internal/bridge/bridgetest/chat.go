package bridgetest

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/store"
)

// Turn records one SendMessage the fake agent handled.
type Turn struct {
	SessionID string
	Message   string
}

// Confirm records one ConfirmAction the fake agent handled: which session and
// whether it was an approve, so a confirm test can assert the exact calls.
type Confirm struct {
	SessionID string
	Approve   bool
}

// Chat is a bridge.ChatService stand-in: it mints sessions, records turns and the
// workspace each session was created under, can gate one message on a release
// channel to prove per-conversation serialization, can fail a named message, and
// can raise a workflow proposal on a named message so the confirm flow runs end to end.
type Chat struct {
	sessions map[string]*store.ChatSession

	// pending holds session ids with an armed proposal. ConfirmAction clears one
	// under the lock, so two concurrent confirmations arbitrate exactly as
	// chat.Agent's mutex does: the loser reads CodeNoPendingAction.
	pending map[string]bool

	release chan struct{}
	gateOn  string
	failOn  string
	toolOn  string
	reply   string

	// proposeOn names the inbound text that makes a turn raise a proposal; armGate
	// says whether raising it also arms the pending gate. A raised proposal with an
	// unarmed gate models a proposal whose confirmation gate a concurrent path already
	// cleared, so ConfirmAction reports CodeNoPendingAction.
	proposeOn string

	turns    []Turn
	created  []string
	confirms []Confirm

	seq     int
	mu      sync.Mutex
	armGate bool
}

// NewChat builds a fake agent with the canned reply and an armed gate.
func NewChat() *Chat {
	return &Chat{
		sessions: map[string]*store.ChatSession{},
		pending:  map[string]bool{},
		release:  make(chan struct{}),
		reply:    ReplyText,
		armGate:  true,
	}
}

// SetReply changes the assistant text an ordinary turn streams.
func (c *Chat) SetReply(reply string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.reply = reply
}

// ProposeOn makes the named inbound text raise a workflow proposal instead of replying.
func (c *Chat) ProposeOn(message string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.proposeOn = message
}

// ToolOn makes the named inbound text emit a tool-call event before its reply.
func (c *Chat) ToolOn(message string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.toolOn = message
}

// GateOn makes the named inbound text block until Release, which is how a test
// proves a slow turn occupies only its own conversation.
func (c *Chat) GateOn(message string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.gateOn = message
}

// FailOn makes the named inbound text fail its turn with ErrTurnFailed.
func (c *Chat) FailOn(message string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.failOn = message
}

// DisarmGate makes a raised proposal leave the confirmation gate clear, standing in for an
// expiry that cleared it before a decision arrived.
func (c *Chat) DisarmGate() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.armGate = false
}

// Release unblocks the gated message.
func (c *Chat) Release() {
	close(c.release)
}

// CreateSession mints a session stamped with the request workspace.
func (c *Chat) CreateSession(ctx context.Context, personality string) (*store.ChatSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.seq++
	workspace := store.WorkspaceFromContext(ctx)
	session := &store.ChatSession{
		ID:          "sess-" + strconv.Itoa(c.seq),
		WorkspaceID: workspace,
		Personality: personality,
	}
	c.sessions[session.ID] = session
	c.created = append(c.created, workspace)

	return session, nil
}

// GetSession returns a minted session. The bridge does not call it, but
// bridge.ChatService requires it.
func (c *Chat) GetSession(_ context.Context, id string) (*store.ChatSession, error) { //nolint:discardedcontext // a map lookup under the fake's mutex; nothing to cancel
	c.mu.Lock()
	defer c.mu.Unlock()

	session, ok := c.sessions[id]
	if !ok {
		return nil, ErrNoSession
	}

	return session, nil
}

// SendMessage records the turn, optionally blocking a gated message until
// Release, then streams the canned reply, a tool call, or a raised proposal.
func (c *Chat) SendMessage(ctx context.Context, sessionID, userMessage string, out chan<- chat.Event) error {
	defer close(out)

	script := c.recordTurn(sessionID, userMessage)

	if userMessage == script.gateOn {
		select {
		case <-c.release:
		case <-ctx.Done():
			return fmt.Errorf("bridgetest chat canceled: %w", ctx.Err())
		}
	}

	if userMessage == script.failOn {
		return ErrTurnFailed
	}

	if script.proposeOn != "" && userMessage == script.proposeOn {
		c.streamProposal(sessionID, script.armGate, out)

		return nil
	}

	if script.toolOn != "" && userMessage == script.toolOn {
		out <- chat.Event{Type: chat.EventToolCall, Tool: Tool}
	}

	// A run-lifecycle event on the turn path posts nothing, so streaming one here
	// is what proves the core's fold ignores it.
	out <- chat.Event{Type: chat.EventRunStarted, RunID: RunID}

	out <- chat.Event{Type: chat.EventAssistantChunk, Text: script.reply}

	out <- chat.Event{Type: chat.EventTurnDone, Text: script.reply}

	return nil
}

// ConfirmAction models the confirmation gate: it records the call, clears the pending flag
// under the lock, and either streams run events (approve) or a denial line (deny).
// An unarmed session reports CodeNoPendingAction, so a concurrent decision and
// expiry race exactly as chat.Agent does and the loser gets that code.
func (c *Chat) ConfirmAction(_ context.Context, sessionID string, approve bool, out chan<- chat.Event) error { //nolint:discardedcontext // the fake streams canned events and returns; nothing to cancel
	defer close(out)

	c.mu.Lock()
	armed := c.pending[sessionID]
	delete(c.pending, sessionID)
	c.confirms = append(c.confirms, Confirm{SessionID: sessionID, Approve: approve})
	c.mu.Unlock()

	if !armed {
		return &cairnerr.Error{Code: chat.CodeNoPendingAction, Message: "no pending action"}
	}

	if approve {
		out <- chat.Event{Type: chat.EventRunStarted, RunID: RunID}

		out <- chat.Event{Type: chat.EventRunFinished, RunID: RunID, RunStatus: RunStatus}

		out <- chat.Event{Type: chat.EventTurnDone}

		return nil
	}

	out <- chat.Event{Type: chat.EventTurnDone, Text: DenialText}

	return nil
}

// Confirms returns a copy of the recorded ConfirmAction calls in order.
func (c *Chat) Confirms() []Confirm {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]Confirm(nil), c.confirms...)
}

// Turns returns a copy of the handled turns in order.
func (c *Chat) Turns() []Turn {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]Turn(nil), c.turns...)
}

// CreatedWorkspaces returns the workspaces sessions were created under, in order.
func (c *Chat) CreatedWorkspaces() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]string(nil), c.created...)
}

// HasTurn reports whether any recorded turn carried message.
func (c *Chat) HasTurn(message string) bool {
	for _, turn := range c.Turns() {
		if turn.Message == message {
			return true
		}
	}

	return false
}

// TurnIndex returns the position of the first turn carrying message, or -1.
func (c *Chat) TurnIndex(message string) int {
	for index, turn := range c.Turns() {
		if turn.Message == message {
			return index
		}
	}

	return -1
}

// script is one turn's snapshot of the configured behavior, read once under the
// lock so the streaming below never touches shared state.
type script struct {
	gateOn    string
	failOn    string
	toolOn    string
	reply     string
	proposeOn string
	armGate   bool
}

// recordTurn appends one handled turn and returns the behavior snapshot for it.
func (c *Chat) recordTurn(sessionID, userMessage string) script {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.turns = append(c.turns, Turn{SessionID: sessionID, Message: userMessage})

	return script{
		gateOn:    c.gateOn,
		failOn:    c.failOn,
		toolOn:    c.toolOn,
		reply:     c.reply,
		proposeOn: c.proposeOn,
		armGate:   c.armGate,
	}
}

// streamProposal arms the confirmation gate when configured and emits the proposal event a
// confirmation turn ends on.
func (c *Chat) streamProposal(sessionID string, armGate bool, out chan<- chat.Event) {
	if armGate {
		c.arm(sessionID)
	}

	out <- chat.Event{Type: chat.EventConfirmationRequired, Proposal: Proposal()}

	out <- chat.Event{Type: chat.EventTurnDone}
}

// arm marks a session as having a proposal awaiting confirmation, so the next
// ConfirmAction for it resolves rather than reporting no pending action.
func (c *Chat) arm(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.pending[sessionID] = true
}
