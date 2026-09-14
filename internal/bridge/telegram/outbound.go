package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/chadit/CairnOS/internal/bridge"
	"github.com/chadit/CairnOS/internal/chat"
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

	// msgExpired is the edited-in-place text an expired proposal keyboard is
	// rewritten to, so the operator sees it lapsed rather than a live keyboard.
	msgExpired = "This proposal expired; nothing was run."

	// maxMessageChars is the Bot API per-message text cap, a character count, so
	// the core's rune-counted split matches what Telegram enforces.
	maxMessageChars = 4096

	// idBase is the numeric base every Telegram id formats through on its way
	// into and out of the core's opaque address and reference strings.
	idBase = 10
)

// outbound is the Bot API side of bridge.Outbound: it turns the core's neutral
// sends into Bot API calls and renders a proposal as the inline keyboard a tap
// comes back through. It holds no state beyond the client, so one value serves
// every conversation.
type outbound struct {
	api BotAPI
}

// Send posts one plain-text message and returns the sent message id as the
// core's opaque reference.
func (o outbound) Send(ctx context.Context, addr *bridge.Address, text string) (string, error) {
	target, err := parseChatID(addr)
	if err != nil {
		return "", err
	}

	messageID, err := o.api.SendMessage(ctx, target, text, nil)
	if err != nil {
		return "", fmt.Errorf("telegram send: %w", err)
	}

	return strconv.FormatInt(messageID, idBase), nil
}

// SendProposal renders a D9 proposal as an Approve/Deny inline keyboard whose
// callback_data carries the session id, so a tap comes back as a decision the
// core can route without re-reading the conversation.
func (o outbound) SendProposal(
	ctx context.Context,
	addr *bridge.Address,
	sessionID string,
	proposal *chat.Proposal,
) (string, error) {
	target, err := parseChatID(addr)
	if err != nil {
		return "", err
	}

	keyboard := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{{
			{Text: buttonApprove, CallbackData: callbackData(sessionID, actionApprove)},
			{Text: buttonDeny, CallbackData: callbackData(sessionID, actionDeny)},
		}},
	}

	messageID, err := o.api.SendMessage(ctx, target, bridge.ProposalText(proposal), keyboard)
	if err != nil {
		return "", fmt.Errorf("telegram send proposal: %w", err)
	}

	return strconv.FormatInt(messageID, idBase), nil
}

// Expired rewrites the proposal message in place so the stale keyboard reads as
// settled rather than still awaiting a tap.
func (o outbound) Expired(ctx context.Context, addr *bridge.Address, ref string) error {
	target, err := parseChatID(addr)
	if err != nil {
		return err
	}

	messageID, err := strconv.ParseInt(ref, idBase, 64)
	if err != nil {
		return errf(CodeTelegramAPI, "telegram expire: message reference %q is not a message id", ref)
	}

	if err := o.api.EditMessageText(ctx, target, messageID, msgExpired); err != nil {
		return fmt.Errorf("telegram mark expired: %w", err)
	}

	return nil
}

// MaxChars is the Bot API per-message cap the core splits long replies on.
func (outbound) MaxChars() int {
	return maxMessageChars
}

// parseChatID reads the numeric chat id back out of the address the adapter built.
// The core carries it as an opaque string, so the parse is the one place the
// numeric shape is restored.
func parseChatID(addr *bridge.Address) (int64, error) {
	target, err := strconv.ParseInt(addr.Channel, idBase, 64)
	if err != nil {
		return 0, errf(CodeTelegramAPI, "telegram send: channel %q is not a chat id", addr.Channel)
	}

	return target, nil
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

	sessionID, action, found := strings.CutLast(rest, callbackSep)
	if !found {
		return "", "", false
	}

	if sessionID == "" || (action != actionApprove && action != actionDeny) {
		return "", "", false
	}

	return sessionID, action, true
}
