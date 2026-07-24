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
	"github.com/chadit/CairnOS/internal/store"
)

const (
	// callbackPrefix and callbackSep frame a proposal keyboard's callback_data as
	// "confirm:<sessionID>:approve|deny" (.agents/spec/parity/messaging-bridge.md,
	// decision 5). The session id is parsed from the last separator so an id that
	// itself carries a colon still round-trips.
	callbackPrefix = "confirm"
	callbackSep    = ":"

	// actionApprove and actionDeny are the callback_data verbs a tap carries.
	actionApprove = "approve"
	actionDeny    = "deny"

	// buttonApprove and buttonDeny label the inline keyboard.
	buttonApprove = "Approve"
	buttonDeny    = "Deny"

	// Confirm outcome labels for the confirms_total counter: approved, denied,
	// expired, and refused. refused covers a wrong-user tap or an Approve from a
	// user without allow_confirm.
	confirmApproved = "approved"
	confirmDenied   = "denied"
	confirmExpired  = "expired"
	confirmRefused  = "refused"

	// Operator-facing lines. A refused or stale tap is answered as a chat message
	// rather than a second answerCallbackQuery: the poll loop already answers every
	// callback once to clear the spinner, and Telegram allows only one answer per
	// query, so a worker-side second answer would fail against the real API.
	msgNoGrant      = "Approval isn't granted for this user."
	msgNotOpener    = "Only the operator who started this proposal can answer it."
	msgStaleExpired = "This proposal expired."

	// msgExpired is the edited-in-place text an expired proposal keyboard is
	// rewritten to, so the operator sees it lapsed rather than a live keyboard.
	msgExpired = "This proposal expired; nothing was run."
)

// proposalRoute is one live D9 proposal's routing state: the chat and message the
// keyboard was sent to, the opener allowed to answer it, and the expiry timer.
// The D9 gate state itself stays inside chat.Agent; this is only what the bridge
// needs to route a tap and edit the message on expiry.
type proposalRoute struct {
	chatID    int64
	messageID int64
	openerID  int64
	timer     *time.Timer
}

// sendProposal renders a D9 workflow proposal as an inline Approve/Deny keyboard,
// records the routing entry keyed by session id, and arms the expiry timer. A
// failed send counts on send_failures_total and leaves nothing pending, since
// without a delivered keyboard there is nothing to answer.
func (b *Bridge) sendProposal(ctx context.Context, chatID int64, sessionID string, openerID int64, proposal *chat.Proposal) {
	if proposal == nil {
		return
	}

	keyboard := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{{
			{Text: buttonApprove, CallbackData: callbackData(sessionID, actionApprove)},
			{Text: buttonDeny, CallbackData: callbackData(sessionID, actionDeny)},
		}},
	}

	messageID, err := b.api.SendMessage(ctx, chatID, proposalText(proposal), keyboard)
	if err != nil {
		b.metrics.SendFailure()
		b.log.ErrorContext(ctx, "bridge proposal send failed",
			"error", errf(CodeSendFailed, "send proposal to chat %d: %v", chatID, err))

		return
	}

	b.armProposal(ctx, sessionID, &proposalRoute{chatID: chatID, messageID: messageID, openerID: openerID})
}

// armProposal stores the routing entry and arms its TTL timer. The timer fires
// expireProposal directly rather than through the chat worker, so an expiry never
// queues behind a busy chat. ctx is the workspace-stamped turn context, kept live
// for the eventual ConfirmAction and message edit.
func (b *Bridge) armProposal(ctx context.Context, sessionID string, entry *proposalRoute) {
	b.confirmMu.Lock()
	defer b.confirmMu.Unlock()

	entry.timer = time.AfterFunc(b.confirmTTL, func() {
		b.expireProposal(ctx, sessionID, entry)
	})
	b.pending[sessionID] = entry
}

// lookupRoute returns the live routing entry for a session id without removing
// it, so an opener or grant check that refuses a tap leaves the proposal pending.
func (b *Bridge) lookupRoute(sessionID string) (*proposalRoute, bool) {
	b.confirmMu.Lock()
	defer b.confirmMu.Unlock()

	entry, ok := b.pending[sessionID]

	return entry, ok
}

// takeRoute removes a session's routing entry and stops its timer, best-effort:
// if the timer already fired, chat.Agent's mutex settles the tap-vs-expiry race
// and the loser reads CodeNoPendingAction.
func (b *Bridge) takeRoute(sessionID string) {
	b.confirmMu.Lock()
	defer b.confirmMu.Unlock()

	entry, ok := b.pending[sessionID]
	if !ok {
		return
	}

	if entry.timer != nil {
		entry.timer.Stop()
	}

	delete(b.pending, sessionID)
}

// stopAllProposals stops every armed expiry timer and clears the routing map,
// called from Run on shutdown so no timer fires after the bridge stops.
func (b *Bridge) stopAllProposals() {
	b.confirmMu.Lock()
	defer b.confirmMu.Unlock()

	for _, entry := range b.pending {
		if entry.timer != nil {
			entry.timer.Stop()
		}
	}

	b.pending = map[string]*proposalRoute{}
}

// handleCallback resolves one inline-keyboard tap. The poll loop already answered
// the callback to clear the client spinner; here the worker checks the opener and
// the allow_confirm grant, then approves, denies, or refuses. Authorization is
// operator-set config, not a token scope: approving a workflow is the distinct
// allow_confirm grant, and only the session opener may answer (doc.go, decision 4).
func (b *Bridge) handleCallback(ctx context.Context, item *inbound) {
	callback := item.update.CallbackQuery
	chatID := item.chatKey

	sessionID, action, valid := parseCallback(callback.Data)
	if !valid {
		b.log.WarnContext(ctx, "bridge bad callback",
			"error", errf(CodeBadCallback, "callback data %q", callback.Data))

		return
	}

	// Stamp the tapper's workspace exactly as the gRPC interceptor would, before
	// touching the agent. A non-opener is refused below, so by the time
	// ConfirmAction runs the tapper is the opener and this is their workspace.
	ctx = store.ContextWithWorkspace(ctx, item.user.Workspace)

	entry, routed := b.lookupRoute(sessionID)
	if !routed {
		b.reply(ctx, chatID, msgStaleExpired)

		return
	}

	if callback.From == nil || callback.From.ID != entry.openerID {
		b.metrics.Confirm(confirmRefused)
		b.reply(ctx, chatID, msgNotOpener)

		return
	}

	if action == actionApprove && !item.user.AllowConfirm {
		b.metrics.Confirm(confirmRefused)
		b.reply(ctx, chatID, msgNoGrant)

		return
	}

	b.resolveConfirm(ctx, chatID, sessionID, action == actionApprove)
}

// resolveConfirm fires ConfirmAction for an authorized tap and relays the run
// events. If the TTL timer won the race first, ConfirmAction reports
// CodeNoPendingAction and the tap is answered as expired. Otherwise the outcome
// counts approved or denied.
func (b *Bridge) resolveConfirm(ctx context.Context, chatID int64, sessionID string, approve bool) {
	b.takeRoute(sessionID)

	events := make(chan chat.Event, turnEventBuffer)
	result := make(chan error, 1)

	go func() {
		result <- b.chat.ConfirmAction(ctx, sessionID, approve, events)
	}()

	b.relayConfirm(ctx, chatID, events)

	err := <-result
	if isNoPending(err) {
		b.reply(ctx, chatID, msgStaleExpired)

		return
	}

	if err != nil {
		b.log.ErrorContext(ctx, "bridge confirm failed", "error", err)

		return
	}

	if approve {
		b.metrics.Confirm(confirmApproved)

		return
	}

	b.metrics.Confirm(confirmDenied)
}

// relayConfirm streams a confirmed turn's events to the chat: run-started and
// run-finished go as their own status lines, and a denial's turn-done text is
// relayed only when no run started, so an approve does not double-report its run.
func (b *Bridge) relayConfirm(ctx context.Context, chatID int64, events <-chan chat.Event) {
	var sawRun bool

	for event := range events {
		switch event.Type {
		case chat.EventRunStarted:
			b.reply(ctx, chatID, "Started run "+event.RunID+".")

			sawRun = true
		case chat.EventRunFinished:
			b.reply(ctx, chatID, "Run "+event.RunID+" finished as "+event.RunStatus+".")
		case chat.EventTurnDone:
			if !sawRun && event.Text != "" {
				b.reply(ctx, chatID, event.Text)
			}
		case chat.EventAssistantChunk, chat.EventToolCall, chat.EventConfirmationRequired:
		}
	}
}

// expireProposal denies an unanswered proposal so the denial persists to history,
// then edits the keyboard message to say it lapsed. It fires straight from the
// TTL timer, not through the chat worker, so a busy chat never delays expiry. If a
// tap resolved the proposal first, ConfirmAction reports CodeNoPendingAction and
// expiry does nothing.
func (b *Bridge) expireProposal(ctx context.Context, sessionID string, entry *proposalRoute) {
	b.takeRoute(sessionID)

	// The denial event stream is left unread on purpose: expiry edits the proposal
	// message rather than relaying the denial line, and the buffered channel holds
	// the single turn-done event ConfirmAction emits before it closes.
	events := make(chan chat.Event, turnEventBuffer)

	err := b.chat.ConfirmAction(ctx, sessionID, false, events)
	if isNoPending(err) {
		return
	}

	if err != nil {
		b.log.ErrorContext(ctx, "bridge proposal expiry failed", "error", err)

		return
	}

	b.metrics.Confirm(confirmExpired)

	if editErr := b.api.EditMessageText(ctx, entry.chatID, entry.messageID, msgExpired); editErr != nil {
		b.log.WarnContext(ctx, "bridge edit expired proposal failed", "error", editErr)
	}
}

// callbackData builds a proposal button's callback_data from a session id and a
// verb.
func callbackData(sessionID, action string) string {
	return callbackPrefix + callbackSep + sessionID + callbackSep + action
}

// parseCallback splits a proposal button's callback_data back into its session id
// and verb, reporting ok false for anything that is not a known confirm verb. The
// session id is taken up to the last separator so an id carrying a colon survives.
func parseCallback(data string) (string, string, bool) {
	rest, found := strings.CutPrefix(data, callbackPrefix+callbackSep)
	if !found {
		return "", "", false
	}

	idx := strings.LastIndex(rest, callbackSep)
	if idx < 0 {
		return "", "", false
	}

	sessionID := rest[:idx]
	action := rest[idx+1:]

	if sessionID == "" || (action != actionApprove && action != actionDeny) {
		return "", "", false
	}

	return sessionID, action, true
}

// isNoPending reports whether err is chat's CodeNoPendingAction, the signal that a
// concurrent tap or expiry already cleared the gate and this caller lost the race.
func isNoPending(err error) bool {
	apiErr, ok := errors.AsType[*cairnerr.Error](err)

	return ok && apiErr.Code == chat.CodeNoPendingAction
}

// proposalText renders the proposal's workflow, inputs, and estimated budget as
// plain text (no parse_mode, so nothing needs escaping).
func proposalText(proposal *chat.Proposal) string {
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
