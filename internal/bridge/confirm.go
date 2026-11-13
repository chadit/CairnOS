package bridge

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/chat"
)

// Confirm outcome labels for the confirms_total counter. They are exported for
// the same reason the Outcome labels in core.go are: the counter vocabulary has
// to be one list that callers and tests read rather than re-declare, so a rename
// breaks compilation instead of silently breaking a dashboard query.
const (
	// ConfirmApproved counts one proposal the opener approved.
	ConfirmApproved = "approved"

	// ConfirmDenied counts one proposal the opener denied.
	ConfirmDenied = "denied"

	// ConfirmExpired counts one proposal the TTL timer auto-denied.
	ConfirmExpired = "expired"

	// ConfirmRefused counts a decision from anyone but the session opener, or an
	// approve from a sender without the allow_confirm grant.
	ConfirmRefused = "refused"
)

const (

	// Operator-facing lines. A refused or stale decision is answered as an
	// ordinary message rather than a transport-specific acknowledgement: Telegram
	// allows only one answerCallbackQuery per tap and the poll loop already spent
	// it clearing the spinner, and no other transport has such an acknowledgement
	// at all.
	msgNoGrant      = "Approval isn't granted for this user."
	msgNotOpener    = "Only the operator who started this proposal can answer it."
	msgStaleExpired = "This proposal expired."
)

// route is one live workflow proposal's routing state: where the proposal went, the
// send surface that can mark it expired, the transport's reference for it, the
// opener allowed to answer it, and the expiry timer. The confirmation gate state itself
// stays inside chat.Agent; this is only what the core needs to route a decision
// and settle the message on expiry.
type route struct {
	addr   Address
	out    Outbound
	timer  *time.Timer
	ref    string
	opener string
}

// sendProposal asks the transport to render a workflow proposal, records the
// routing entry keyed by session id, and arms the expiry timer. A failed send
// counts on send_failures_total and leaves nothing pending, since without a
// delivered proposal there is nothing to answer.
func (c *Core) sendProposal(ctx context.Context, inbound *Inbound, sessionID string, proposal *chat.Proposal) {
	if proposal == nil {
		return
	}

	ref, err := inbound.Out.SendProposal(ctx, &inbound.Addr, sessionID, proposal)
	if err != nil {
		c.metrics.SendFailure(inbound.Addr.Transport, inbound.Addr.Identity)
		c.log.ErrorContext(ctx, "bridge proposal send failed",
			"error", errf(CodeSendFailed, "send proposal to %s %s: %v", inbound.Addr.Transport, inbound.Addr.ChatKey, err))

		return
	}

	c.armProposal(ctx, sessionID, &route{
		addr:   inbound.Addr,
		out:    inbound.Out,
		ref:    ref,
		opener: inbound.Sender.ID,
	})
}

// armProposal stores the routing entry and arms its TTL timer. The timer fires
// expireProposal directly rather than through the conversation worker, so an
// expiry never queues behind a busy conversation. ctx is the workspace-stamped
// turn context, kept live for the eventual ConfirmAction and expiry mark.
func (c *Core) armProposal(ctx context.Context, sessionID string, entry *route) {
	c.confirmMu.Lock()
	defer c.confirmMu.Unlock()

	// A turn can raise a second proposal while the first is still pending, and
	// chat.Agent's own gate overwrites the same way. An incumbent timer left armed
	// would later expire the replacement and is invisible to stopAllProposals.
	if prior, ok := c.pending[sessionID]; ok && prior.timer != nil {
		prior.timer.Stop()
	}

	// The timer runs on a context detached from the turn's. A turn can carry a
	// deadline, A2A bounds its detached turn with one, and expiry fires up to
	// confirmTTL after that turn ended, so sharing the deadline would cancel the
	// expiry's ConfirmAction before it ran. Drain stops every armed timer.
	timerCtx := context.WithoutCancel(ctx)

	entry.timer = time.AfterFunc(c.confirmTTL, func() {
		c.expireProposal(timerCtx, sessionID, entry)
	})
	c.pending[sessionID] = entry
}

// lookupRoute returns the live routing entry for a session id without removing
// it, so an opener or grant check that refuses a decision leaves the proposal
// pending.
func (c *Core) lookupRoute(sessionID string) (*route, bool) {
	c.confirmMu.Lock()
	defer c.confirmMu.Unlock()

	entry, ok := c.pending[sessionID]

	return entry, ok
}

// routed reports whether the session has a proposal awaiting a decision, which is
// what makes a bare approve or deny a decision rather than chat text.
func (c *Core) routed(sessionID string) bool {
	_, ok := c.lookupRoute(sessionID)

	return ok
}

// takeRoute removes a session's routing entry and stops its timer, best-effort:
// if the timer already fired, chat.Agent's mutex settles the decision-vs-expiry
// race and the loser reads CodeNoPendingAction.
func (c *Core) takeRoute(sessionID string) {
	c.confirmMu.Lock()
	defer c.confirmMu.Unlock()

	entry, ok := c.pending[sessionID]
	if !ok {
		return
	}

	if entry.timer != nil {
		entry.timer.Stop()
	}

	delete(c.pending, sessionID)
}

// takeRouteIf removes the session's routing entry only when the stored entry is
// this exact one, and reports whether it did. Expiry identifies its proposal by
// pointer rather than by session id so a timer that lost to a replacement
// proposal cannot delete and deny the replacement.
func (c *Core) takeRouteIf(sessionID string, entry *route) bool {
	c.confirmMu.Lock()
	defer c.confirmMu.Unlock()

	if stored, ok := c.pending[sessionID]; !ok || stored != entry {
		return false
	}

	if entry.timer != nil {
		entry.timer.Stop()
	}

	delete(c.pending, sessionID)

	return true
}

// stopAllProposals stops every armed expiry timer and clears the routing map,
// called from Drain so no timer fires after the transports stop.
func (c *Core) stopAllProposals() {
	c.confirmMu.Lock()
	defer c.confirmMu.Unlock()

	for _, entry := range c.pending {
		if entry.timer != nil {
			entry.timer.Stop()
		}
	}

	c.pending = map[string]*route{}
}

// resolveDecision settles one approve or deny against the session's live
// proposal. Order matters: a decision with no live proposal counts nothing, one
// from anyone but the opener is refused, and an approve without the
// allow_confirm grant is refused the same way. A deny always proceeds, since
// denying is safe. Authorization is operator-set config, not a token scope
// (doc.go).
func (c *Core) resolveDecision(ctx context.Context, inbound *Inbound, sessionID string, approve bool) {
	entry, ok := c.lookupRoute(sessionID)
	if !ok {
		c.send(ctx, inbound.Out, &inbound.Addr, msgStaleExpired)

		return
	}

	if inbound.Sender.ID != entry.opener {
		c.metrics.Confirm(inbound.Addr.Transport, inbound.Addr.Identity, ConfirmRefused)
		c.send(ctx, inbound.Out, &inbound.Addr, msgNotOpener)

		return
	}

	if approve && !inbound.Grant.AllowConfirm {
		c.metrics.Confirm(inbound.Addr.Transport, inbound.Addr.Identity, ConfirmRefused)
		c.send(ctx, inbound.Out, &inbound.Addr, msgNoGrant)

		return
	}

	c.resolveConfirm(ctx, inbound, sessionID, approve)
}

// resolveConfirm fires ConfirmAction for an authorized decision and relays the
// run events. If the TTL timer won the race first, ConfirmAction reports
// CodeNoPendingAction and the decision is answered as expired. Otherwise the
// outcome counts approved or denied.
func (c *Core) resolveConfirm(ctx context.Context, inbound *Inbound, sessionID string, approve bool) {
	c.takeRoute(sessionID)

	events := make(chan chat.Event, turnEventBuffer)
	result := make(chan error, resultBufferSize)

	go func() {
		result <- c.chat.ConfirmAction(ctx, sessionID, approve, events)
	}()

	c.relayConfirm(ctx, inbound, events)

	err := <-result
	if isNoPending(err) {
		c.send(ctx, inbound.Out, &inbound.Addr, msgStaleExpired)

		return
	}

	if err != nil {
		c.log.ErrorContext(ctx, "bridge confirm failed", "error", err)

		return
	}

	if approve {
		c.metrics.Confirm(inbound.Addr.Transport, inbound.Addr.Identity, ConfirmApproved)

		return
	}

	c.metrics.Confirm(inbound.Addr.Transport, inbound.Addr.Identity, ConfirmDenied)
}

// relayConfirm streams a confirmed turn's events to the conversation: run-started
// and run-finished go as their own status lines, and a denial's turn-done text is
// relayed only when no run started, so an approve does not double-report its run.
func (c *Core) relayConfirm(ctx context.Context, inbound *Inbound, events <-chan chat.Event) {
	var sawRun bool

	for event := range events {
		switch event.Type {
		case chat.EventRunStarted:
			c.send(ctx, inbound.Out, &inbound.Addr, "Started run "+event.RunID+".")

			sawRun = true
		case chat.EventRunFinished:
			c.send(ctx, inbound.Out, &inbound.Addr, "Run "+event.RunID+" finished as "+event.RunStatus+".")
		case chat.EventTurnDone:
			c.relayTurnDone(ctx, inbound, event.Text, sawRun)
		case chat.EventAssistantChunk, chat.EventToolCall, chat.EventConfirmationRequired:
		}
	}
}

// relayTurnDone posts a confirmed turn's closing text, which is the explanation a
// denial carries. An approve already reported its run through the lifecycle
// lines, so relaying the text there would report the same run twice.
func (c *Core) relayTurnDone(ctx context.Context, inbound *Inbound, text string, sawRun bool) {
	if sawRun || text == "" {
		return
	}

	c.send(ctx, inbound.Out, &inbound.Addr, text)
}

// expireProposal denies an unanswered proposal so the denial persists to history,
// then asks the transport to mark it lapsed. It fires straight from the TTL
// timer, not through the conversation worker, so a busy conversation never
// delays expiry. A decision or a replacement that got there first leaves this
// entry off the map, and the take reports false.
func (c *Core) expireProposal(ctx context.Context, sessionID string, entry *route) {
	if !c.takeRouteIf(sessionID, entry) {
		return
	}

	// The denial event stream is left unread on purpose: expiry marks the proposal
	// message rather than relaying the denial line, and the buffered channel holds
	// the single turn-done event ConfirmAction emits before it closes.
	events := make(chan chat.Event, turnEventBuffer)

	err := c.chat.ConfirmAction(ctx, sessionID, false, events)
	if isNoPending(err) {
		return
	}

	if err != nil {
		c.log.ErrorContext(ctx, "bridge proposal expiry failed", "error", err)

		return
	}

	c.metrics.Confirm(entry.addr.Transport, entry.addr.Identity, ConfirmExpired)

	expirer, marks := entry.out.(Expirer)
	if !marks {
		return
	}

	if markErr := expirer.Expired(ctx, &entry.addr, entry.ref); markErr != nil {
		c.log.WarnContext(ctx, "bridge mark expired proposal failed", "error", markErr)
	}
}

// isNoPending reports whether err is chat's CodeNoPendingAction, the signal that a
// concurrent decision or expiry already cleared the gate and this caller lost the
// race.
func isNoPending(err error) bool {
	apiErr, ok := errors.AsType[*cairnerr.Error](err)

	return ok && apiErr.Code == chat.CodeNoPendingAction
}

// ProposalText renders a proposal's workflow, inputs, and budget as plain text,
// closing with the line that asks for a decision. It is exported because every
// transport renders the same body: Telegram wraps it in an inline keyboard, and
// a transport without buttons takes the typed approve or deny. Nothing here
// needs escaping, so no transport picks a parse mode.
func ProposalText(proposal *chat.Proposal) string {
	var builder strings.Builder

	builder.WriteString("Proposed workflow: ")
	builder.WriteString(proposal.WorkflowName)

	if len(proposal.Inputs) > 0 {
		builder.WriteString("\nInputs: ")
		builder.WriteString(renderInputs(proposal.Inputs))
	}

	builder.WriteString("\nEstimated budget: $")
	builder.WriteString(strconv.FormatFloat(proposal.EstimatedBudgetUSD, 'f', 2, 64))
	builder.WriteString("\nApprove or deny below.")

	return builder.String()
}

// renderInputs formats proposal inputs as a sorted, comma-joined key=value list,
// sorted so the rendered line is stable across runs.
func renderInputs(inputs map[string]any) string {
	keys := make([]string, 0, len(inputs))

	for key := range inputs {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	parts := make([]string, 0, len(keys))

	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", key, inputs[key]))
	}

	return strings.Join(parts, ", ")
}
