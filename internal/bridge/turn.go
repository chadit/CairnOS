package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/store"
)

const (
	// silentMarker is the reply the agent uses to say it has nothing to post. The
	// turn still counts ok and stays in the session transcript.
	silentMarker = "[SILENT]"

	// verbApprove and verbDeny are the typed decisions a conversation can carry
	// instead of chat text, the only way to answer a proposal on a transport with
	// no buttons to tap.
	verbApprove = "approve"
	verbDeny    = "deny"
)

// process runs one inbound on its conversation's worker: it counts the inbound,
// stamps the sender's workspace onto the context exactly as the gRPC interceptor
// would, then resolves a transport-parsed decision or handles the message.
func (c *Core) process(ctx context.Context, inbound *Inbound) {
	// The completion signal fires on every exit, including the canceled-context
	// path below, so a request-scoped transport waiting for its reply is
	// released whether the turn ran or not.
	defer completeTurn(inbound.Out)

	if ctx.Err() != nil {
		return
	}

	c.metrics.Update(inbound.Addr.Transport, inbound.Addr.Identity, OutcomeHandled)

	ctx = store.ContextWithWorkspace(ctx, inbound.Grant.Workspace)

	switch inbound.Kind {
	case KindDecision:
		c.resolveDecision(ctx, inbound, inbound.Session, inbound.Approve)
	case KindMessage:
		c.handleMessage(ctx, inbound)
	}
}

// handleMessage runs a bridge command or hands the text on to the turn path.
func (c *Core) handleMessage(ctx context.Context, inbound *Inbound) {
	switch strings.TrimSpace(inbound.Text) {
	case cmdNew:
		c.startNewSession(ctx, inbound)
	case cmdSession:
		c.replySessionID(ctx, inbound)
	default:
		c.runOrDecide(ctx, inbound)
	}
}

// runOrDecide answers a bare approve or deny as a decision on the conversation's
// live proposal and treats everything else as a chat turn. The typed verbs are
// checked only while a proposal is pending, so an ordinary message that happens
// to read "approve" still reaches the agent.
func (c *Core) runOrDecide(ctx context.Context, inbound *Inbound) {
	sessionID, err := c.ensureSession(ctx, inbound)
	if err != nil {
		c.metrics.Turn(inbound.Addr.Transport, inbound.Addr.Identity, false)
		c.log.ErrorContext(ctx, "bridge session ensure failed", "error", err)

		return
	}

	approve, decided := decisionVerb(inbound.Text)
	if decided && c.routed(sessionID) {
		c.resolveDecision(ctx, inbound, sessionID, approve)

		return
	}

	c.runTurn(ctx, inbound, sessionID)
}

// decisionVerb reads a bare approve or deny reply. The first result is the
// decision itself; the second reports whether the text was a decision at all,
// and false there means the caller runs it as an ordinary turn.
func decisionVerb(text string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case verbApprove:
		return true, true
	case verbDeny:
		return false, true
	default:
		return false, false
	}
}

// ensureSession resolves the conversation's bound session, creating and binding a
// fresh one on first contact (ErrBridgeSessionNotFound). The binding lives in the
// store so it survives a serve restart.
func (c *Core) ensureSession(ctx context.Context, inbound *Inbound) (string, error) {
	sessionID, err := c.store.LookupBridgeSession(ctx, inbound.Addr.Transport, inbound.Addr.ChatKey)
	if err == nil {
		return sessionID, nil
	}

	if !errors.Is(err, store.ErrBridgeSessionNotFound) {
		return "", fmt.Errorf("lookup bridge session: %w", err)
	}

	return c.newSession(ctx, inbound)
}

// newSession creates a session with the sender's personality and binds the
// conversation to it, overwriting any prior binding (the /new path).
func (c *Core) newSession(ctx context.Context, inbound *Inbound) (string, error) {
	session, err := c.chat.CreateSession(ctx, inbound.Grant.Personality)
	if err != nil {
		return "", fmt.Errorf("create chat session: %w", err)
	}

	if err := c.store.BindBridgeSession(ctx, inbound.Addr.Transport, inbound.Addr.ChatKey, session.ID); err != nil {
		return "", fmt.Errorf("bind bridge session: %w", err)
	}

	return session.ID, nil
}

// startNewSession rebinds the conversation to a fresh session and confirms it, so
// /new starts a clean conversation while the prior transcript stays readable by
// its own session id.
func (c *Core) startNewSession(ctx context.Context, inbound *Inbound) {
	sessionID, err := c.newSession(ctx, inbound)
	if err != nil {
		c.log.ErrorContext(ctx, "bridge /new failed", "error", err)
		c.send(ctx, inbound.Out, &inbound.Addr, "Could not start a new session.")

		return
	}

	c.send(ctx, inbound.Out, &inbound.Addr, "Started a new session: "+sessionID)
}

// replySessionID answers /session with the conversation's current session id so
// an operator can pick the same conversation up from cairnos chat --session.
func (c *Core) replySessionID(ctx context.Context, inbound *Inbound) {
	sessionID, err := c.ensureSession(ctx, inbound)
	if err != nil {
		c.log.ErrorContext(ctx, "bridge /session failed", "error", err)
		c.send(ctx, inbound.Out, &inbound.Addr, "Could not resolve this chat's session.")

		return
	}

	c.send(ctx, inbound.Out, &inbound.Addr, "Session: "+sessionID)
}

// turn is one in-flight chat turn: the inbound that raised it, the session it
// runs on, and the assistant text folded out of the event stream.
type turn struct {
	inbound   *Inbound
	sessionID string
	reply     strings.Builder
}

// runTurn runs one chat turn on the conversation's session and delivers the
// reply. The turn's outcome counts on turns_total; a failed delivery counts on
// send_failures_total and is never retried, since the reply is already in session
// history. The sender is recorded on any proposal this turn raises so only that
// operator can approve it (confirm.go).
func (c *Core) runTurn(ctx context.Context, inbound *Inbound, sessionID string) {
	events := make(chan chat.Event, turnEventBuffer)
	result := make(chan error, resultBufferSize)

	go func() {
		result <- c.chat.SendMessage(ctx, sessionID, inbound.Text, events)
	}()

	active := &turn{inbound: inbound, sessionID: sessionID}

	for event := range events {
		c.consume(ctx, active, &event)
	}

	turnErr := <-result

	c.metrics.Turn(inbound.Addr.Transport, inbound.Addr.Identity, turnErr == nil)

	if turnErr != nil {
		c.log.ErrorContext(ctx, "bridge chat turn failed", "error", turnErr)

		return
	}

	c.deliverReply(ctx, active)
}

// deliverReply posts a completed turn's assistant text. A reply of exactly
// [SILENT] posts nothing: the turn already counted ok and stays in the session
// transcript, which is what the marker asks for.
func (c *Core) deliverReply(ctx context.Context, active *turn) {
	text := active.reply.String()
	if text == "" || strings.TrimSpace(text) == silentMarker {
		return
	}

	c.deliver(ctx, active.inbound.Out, &active.inbound.Addr, text)
}

// consume folds one turn event toward the outbound reply: assistant text buffers
// into one message, a tool call is relayed as a status line only in verbose mode,
// and a workflow proposal renders the confirmation the transport asks for.
// Run-lifecycle and turn-done events carry nothing new to send here.
func (c *Core) consume(ctx context.Context, active *turn, event *chat.Event) {
	switch event.Type {
	case chat.EventAssistantChunk:
		active.reply.WriteString(event.Text)
	case chat.EventToolCall:
		if c.verbose {
			c.send(ctx, active.inbound.Out, &active.inbound.Addr, "tool: "+event.Tool)
		}
	case chat.EventConfirmationRequired:
		c.sendProposal(ctx, active.inbound, active.sessionID, event.Proposal)
	case chat.EventRunStarted, chat.EventRunFinished, chat.EventTurnDone:
	}
}

// deliver sends one turn's reply, splitting anything over the transport's
// per-message cap on paragraph boundaries so a long answer arrives as ordered
// messages rather than a rejected one.
func (c *Core) deliver(ctx context.Context, out Outbound, addr *Address, text string) {
	for _, part := range splitMessage(text, out.MaxChars()) {
		c.send(ctx, out, addr, part)
	}
}

// send posts one message, counting and logging a delivery failure without
// retrying: the turn already ran and its reply is in session history. The failure
// is stamped CodeSendFailed over the adapter's already-redacted cause, so the log
// carries no token.
func (c *Core) send(ctx context.Context, out Outbound, addr *Address, text string) {
	if _, err := out.Send(ctx, addr, text); err != nil {
		c.metrics.SendFailure(addr.Transport, addr.Identity)
		c.log.ErrorContext(ctx, "bridge send failed",
			"error", errf(CodeSendFailed, "deliver to %s %s: %v", addr.Transport, addr.ChatKey, err))
	}
}
