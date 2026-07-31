package bridge_test

import (
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/bridge"
)

// otherUserID is a second allowlisted operator, used to prove a non-opener tap
// is refused.
const otherUserID = 2

// TestConfirmProposalRendersKeyboard proves a raised proposal renders an
// Approve/Deny inline keyboard whose callback_data carries the session id and
// verb (.agents/spec/parity/messaging-bridge.md, acceptance 2).
func TestConfirmProposalRendersKeyboard(t *testing.T) {
	t.Parallel()

	users := map[int64]bridge.AllowedUser{testUserID: {Workspace: wsDefault, AllowConfirm: true}}
	rig := newFixture(t, users)
	rig.chat.proposeOn = msgPropose

	scriptSingles(rig.tg, messageUpdate(testUserID, testChatID, msgPropose))

	runBridge(t, rig.br)

	waitFor(t, func() bool { return len(rig.tg.sentMessages()) == 1 })

	sent := rig.tg.sentMessages()
	proposal := sent[0]

	if !proposal.hasKeyboard || proposal.keyboard == nil {
		t.Fatalf("proposal send had no keyboard: %+v", proposal)
	}

	if len(proposal.keyboard.InlineKeyboard) != 1 || len(proposal.keyboard.InlineKeyboard[0]) != 2 {
		t.Fatalf("keyboard shape = %+v, want one row of two buttons", proposal.keyboard.InlineKeyboard)
	}

	approve := proposal.keyboard.InlineKeyboard[0][0]
	deny := proposal.keyboard.InlineKeyboard[0][1]

	if want := confirmData(testSessionID, actionApprove); approve.CallbackData != want {
		t.Errorf("approve callback_data = %q, want %q", approve.CallbackData, want)
	}

	if want := confirmData(testSessionID, actionDeny); deny.CallbackData != want {
		t.Errorf("deny callback_data = %q, want %q", deny.CallbackData, want)
	}

	if !strings.Contains(proposal.text, workflowDeploy) {
		t.Errorf("proposal text %q missing workflow name", proposal.text)
	}
}

// TestConfirmApproveGrantedRunsAndRelays proves an Approve tap from a granted
// opener invokes ConfirmAction(true) and relays run-started/run-finished
// (acceptance 2).
func TestConfirmApproveGrantedRunsAndRelays(t *testing.T) {
	t.Parallel()

	users := map[int64]bridge.AllowedUser{testUserID: {Workspace: wsDefault, AllowConfirm: true}}
	rig := newFixture(t, users)
	rig.chat.proposeOn = msgPropose

	scriptSingles(
		rig.tg,
		messageUpdate(testUserID, testChatID, msgPropose),
		callbackUpdate(2, testUserID, testChatID, confirmData(testSessionID, actionApprove)),
	)

	runBridge(t, rig.br)

	waitFor(t, func() bool { return rig.metrics.confirmCount(confirmApproved) == 1 })

	calls := rig.chat.confirmCalls()
	if len(calls) != 1 || calls[0].sessionID != testSessionID || !calls[0].approve {
		t.Fatalf("confirm calls = %+v, want one approve for %q", calls, testSessionID)
	}

	sent := rig.tg.sentMessages()
	if !sentContains(sent, "Started run "+confirmRunID) {
		t.Errorf("run-started not relayed: %+v", sent)
	}

	if !sentContains(sent, "finished as "+confirmRunStatus) {
		t.Errorf("run-finished not relayed: %+v", sent)
	}
}

// TestConfirmDenyCallsConfirmActionFalse proves a Deny tap invokes
// ConfirmAction(false) and runs nothing (acceptance 2).
func TestConfirmDenyCallsConfirmActionFalse(t *testing.T) {
	t.Parallel()

	users := map[int64]bridge.AllowedUser{testUserID: {Workspace: wsDefault, AllowConfirm: true}}
	rig := newFixture(t, users)
	rig.chat.proposeOn = msgPropose

	scriptSingles(
		rig.tg,
		messageUpdate(testUserID, testChatID, msgPropose),
		callbackUpdate(2, testUserID, testChatID, confirmData(testSessionID, actionDeny)),
	)

	runBridge(t, rig.br)

	waitFor(t, func() bool { return rig.metrics.confirmCount(confirmDenied) == 1 })

	calls := rig.chat.confirmCalls()
	if len(calls) != 1 || calls[0].approve {
		t.Fatalf("confirm calls = %+v, want one deny", calls)
	}

	if !sentContains(rig.tg.sentMessages(), denialText) {
		t.Errorf("denial line not relayed")
	}
}

// TestConfirmApproveWithoutGrantRefused proves a chat-only user's Approve tap is
// refused: ConfirmAction is never called on it, confirms_total{refused}
// increments, and the proposal stays pending, which a follow-up Deny then
// resolves (acceptance 2; security amendment: allow_confirm gates who may
// approve).
func TestConfirmApproveWithoutGrantRefused(t *testing.T) {
	t.Parallel()

	users := map[int64]bridge.AllowedUser{testUserID: {Workspace: wsDefault, AllowConfirm: false}}
	rig := newFixture(t, users)
	rig.chat.proposeOn = msgPropose

	scriptSingles(
		rig.tg,
		messageUpdate(testUserID, testChatID, msgPropose),
		callbackUpdate(2, testUserID, testChatID, confirmData(testSessionID, actionApprove)),
		callbackUpdate(3, testUserID, testChatID, confirmData(testSessionID, actionDeny)),
	)

	runBridge(t, rig.br)

	// The follow-up Deny only resolves if the refused Approve left the proposal
	// pending, so a single denied confirm is the observable proof it stayed.
	waitFor(t, func() bool { return rig.metrics.confirmCount(confirmDenied) == 1 })

	calls := rig.chat.confirmCalls()
	if len(calls) != 1 || calls[0].approve {
		t.Fatalf("confirm calls = %+v, want a single deny (the approve was refused, not run)", calls)
	}

	if got := rig.metrics.confirmCount(confirmRefused); got != 1 {
		t.Errorf("refused count = %d, want 1", got)
	}

	if got := rig.metrics.confirmCount(confirmApproved); got != 0 {
		t.Errorf("approved count = %d, want 0", got)
	}

	if !sentContains(rig.tg.sentMessages(), msgNoGrant) {
		t.Errorf("no-grant reply not sent")
	}
}

// TestConfirmNonOpenerRefused proves a tap from any user other than the session
// opener is refused even with allow_confirm, the proposal stays pending, and the
// opener's own Deny then resolves it (acceptance 2).
func TestConfirmNonOpenerRefused(t *testing.T) {
	t.Parallel()

	users := map[int64]bridge.AllowedUser{
		testUserID:  {Workspace: wsDefault, AllowConfirm: true},
		otherUserID: {Workspace: wsDefault, AllowConfirm: true},
	}
	rig := newFixture(t, users)
	rig.chat.proposeOn = msgPropose

	scriptSingles(
		rig.tg,
		messageUpdate(testUserID, testChatID, msgPropose),
		callbackUpdate(2, otherUserID, testChatID, confirmData(testSessionID, actionApprove)),
		callbackUpdate(3, testUserID, testChatID, confirmData(testSessionID, actionDeny)),
	)

	runBridge(t, rig.br)

	waitFor(t, func() bool { return rig.metrics.confirmCount(confirmDenied) == 1 })

	calls := rig.chat.confirmCalls()
	if len(calls) != 1 || calls[0].approve {
		t.Fatalf("confirm calls = %+v, want a single deny (the non-opener approve refused)", calls)
	}

	if got := rig.metrics.confirmCount(confirmRefused); got != 1 {
		t.Errorf("refused count = %d, want 1", got)
	}

	if !sentContains(rig.tg.sentMessages(), msgNotOpener) {
		t.Errorf("non-opener reply not sent")
	}
}

// TestConfirmTTLExpiresAsDeny proves an unanswered proposal denies after
// confirm_ttl and the keyboard message is edited in place (acceptance 2;
// security amendment: confirmations expire as deny).
func TestConfirmTTLExpiresAsDeny(t *testing.T) {
	t.Parallel()

	users := map[int64]bridge.AllowedUser{testUserID: {Workspace: wsDefault, AllowConfirm: true}}
	rig := newTunedFixture(t, users, 20*time.Millisecond)
	rig.chat.proposeOn = msgPropose

	scriptSingles(rig.tg, messageUpdate(testUserID, testChatID, msgPropose))

	runBridge(t, rig.br)

	waitFor(t, func() bool { return rig.metrics.confirmCount(confirmExpired) == 1 })

	calls := rig.chat.confirmCalls()
	if len(calls) != 1 || calls[0].approve {
		t.Fatalf("confirm calls = %+v, want one deny from expiry", calls)
	}

	waitFor(t, func() bool { return len(rig.tg.editedMessages()) == 1 })

	edit := rig.tg.editedMessages()[0]
	if edit.text != msgExpired {
		t.Errorf("edited text = %q, want %q", edit.text, msgExpired)
	}
}

// TestConfirmStaleTapReplies proves a tap for a proposal with no live routing
// entry answers "this proposal expired" and calls ConfirmAction for nothing
// (acceptance 2).
func TestConfirmStaleTapReplies(t *testing.T) {
	t.Parallel()

	users := map[int64]bridge.AllowedUser{testUserID: {Workspace: wsDefault, AllowConfirm: true}}
	rig := newFixture(t, users)

	scriptSingles(rig.tg, callbackUpdate(1, testUserID, testChatID, confirmData("ghost", actionApprove)))

	runBridge(t, rig.br)

	waitFor(t, func() bool { return sentContains(rig.tg.sentMessages(), msgStaleExpired) })

	if calls := rig.chat.confirmCalls(); len(calls) != 0 {
		t.Fatalf("ConfirmAction called on a stale tap: %+v", calls)
	}
}

// TestConfirmTapAfterGateClearedRepliesExpired proves the CodeNoPendingAction
// path: a routed tap whose D9 gate was already cleared (a lost tap-vs-expiry
// race) is answered as expired and does not count approved (acceptance 2).
func TestConfirmTapAfterGateClearedRepliesExpired(t *testing.T) {
	t.Parallel()

	users := map[int64]bridge.AllowedUser{testUserID: {Workspace: wsDefault, AllowConfirm: true}}
	rig := newFixture(t, users)
	rig.chat.proposeOn = msgPropose

	// Raise the proposal so the bridge routes it, but leave the agent's gate
	// unarmed, standing in for an expiry that cleared the gate before the tap.
	rig.chat.armGate = false

	scriptSingles(
		rig.tg,
		messageUpdate(testUserID, testChatID, msgPropose),
		callbackUpdate(2, testUserID, testChatID, confirmData(testSessionID, actionApprove)),
	)

	runBridge(t, rig.br)

	// The fake records the ConfirmAction call on entry, but resolveConfirm only
	// replies expired after that call returns, so waiting on the call count can
	// observe the assertions below before the reply is sent. The reply is the
	// last step on this path, which makes it the wait that covers the rest.
	waitFor(t, func() bool { return sentContains(rig.tg.sentMessages(), msgStaleExpired) })

	calls := rig.chat.confirmCalls()
	if len(calls) != 1 || !calls[0].approve {
		t.Fatalf("confirm calls = %+v, want one approve", calls)
	}

	if got := rig.metrics.confirmCount(confirmApproved); got != 0 {
		t.Errorf("approved count = %d, want 0 on a cleared gate", got)
	}
}

// TestConfirmExpiryTapRaceUnderRace drives a tap and a short-TTL expiry against
// one proposal so the race detector proves the shared routing map is safe. The
// timer's expireProposal and the worker's tap both take the map under confirmMu;
// however the interleaving settles, exactly one path wins the gate (approved or
// expired) and nothing is refused (acceptance 2, decision 2).
func TestConfirmExpiryTapRaceUnderRace(t *testing.T) {
	t.Parallel()

	users := map[int64]bridge.AllowedUser{testUserID: {Workspace: wsDefault, AllowConfirm: true}}
	rig := newTunedFixture(t, users, time.Millisecond)
	rig.chat.proposeOn = msgPropose

	scriptSingles(
		rig.tg,
		messageUpdate(testUserID, testChatID, msgPropose),
		callbackUpdate(2, testUserID, testChatID, confirmData(testSessionID, actionApprove)),
	)

	runBridge(t, rig.br)

	waitFor(t, func() bool {
		return rig.metrics.confirmCount(confirmApproved)+rig.metrics.confirmCount(confirmExpired) == 1
	})

	won := rig.metrics.confirmCount(confirmApproved) + rig.metrics.confirmCount(confirmExpired)
	if won != 1 {
		t.Errorf("winners = %d, want exactly one (approved or expired)", won)
	}

	if got := rig.metrics.confirmCount(confirmRefused); got != 0 {
		t.Errorf("refused count = %d, want 0 for an opener with grant", got)
	}
}
