package bridge_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/bridge"
	"github.com/chadit/CairnOS/internal/bridge/bridgetest"
)

// TestCoreNewRebindsAndSessionReplies proves /new starts a fresh session and
// rebinds the conversation to it, and /session answers the current session id
// (FR19).
func TestCoreNewRebindsAndSessionReplies(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, grantChat())

	rig.handle(t, rig.message(msgGo), rig.message("/new"), rig.message("/session"))

	bridgetest.WaitFor(t, func() bool { return len(rig.out.Sent()) == 3 })

	sent := rig.out.Texts()
	first := rig.chat.Turns()[0].SessionID

	if !strings.HasPrefix(sent[1], "Started a new session: ") {
		t.Fatalf("/new reply = %q", sent[1])
	}

	rebound := strings.TrimPrefix(sent[1], "Started a new session: ")
	if rebound == first {
		t.Errorf("/new did not rebind: still %q", first)
	}

	if want := "Session: " + rebound; sent[2] != want {
		t.Errorf("/session reply = %q, want %q", sent[2], want)
	}
}

// TestCoreTurnFoldsEventsIntoOneReply proves the turn fold: assistant chunks
// buffer into a single message and run-lifecycle events post nothing while the
// turn is running.
func TestCoreTurnFoldsEventsIntoOneReply(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, grantChat())

	rig.handle(t, rig.message(msgGo))

	bridgetest.WaitFor(t, func() bool { return len(rig.out.Sent()) == 1 })

	sent := rig.out.Sent()
	if sent[0].Text != bridgetest.ReplyText {
		t.Errorf("reply = %q, want %q", sent[0].Text, bridgetest.ReplyText)
	}

	if sent[0].Addr.Channel != testChannel {
		t.Errorf("reply went to channel %q, want %q", sent[0].Addr.Channel, testChannel)
	}

	turnsOK, turnsErr := rig.metrics.TurnCounts()
	if turnsOK != 1 || turnsErr != 0 {
		t.Errorf("turn counts = ok:%d err:%d, want ok:1 err:0", turnsOK, turnsErr)
	}
}

// TestCoreVerboseRelaysToolCall proves a tool call is relayed as its own status
// line before the buffered reply, and only when the core is verbose.
func TestCoreVerboseRelaysToolCall(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		want    []string
		verbose bool
	}{
		{name: "verbose relays the tool line", want: []string{toolPrefix + bridgetest.Tool, bridgetest.ReplyText}, verbose: true},
		{name: "quiet relays only the reply", want: []string{bridgetest.ReplyText}, verbose: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rig := newTunedFixture(t, grantChat(), time.Minute, tt.verbose)
			rig.chat.ToolOn(msgTool)

			rig.handle(t, rig.message(msgTool))

			bridgetest.WaitFor(t, func() bool { return len(rig.out.Sent()) == len(tt.want) })

			if got := rig.out.Texts(); !slices.Equal(got, tt.want) {
				t.Errorf("sends = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCoreSilentReplyPostsNothing proves a reply of exactly [SILENT] posts
// nothing while the turn still counts ok (FR16). The follow-up turn is what makes
// the absence observable: the conversation runs FIFO, so once its reply lands the
// silent turn has already had its chance to post.
func TestCoreSilentReplyPostsNothing(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, grantChat())
	rig.chat.SetReply("  [SILENT]\n")

	rig.handle(t, rig.message(msgGo))

	bridgetest.WaitFor(t, func() bool { return len(rig.chat.Turns()) == 1 })

	rig.chat.SetReply(bridgetest.ReplyText)

	rig.handle(t, rig.message(msgGo))

	bridgetest.WaitFor(t, func() bool { return len(rig.out.Sent()) == 1 })

	if got := rig.out.Texts(); !slices.Equal(got, []string{bridgetest.ReplyText}) {
		t.Errorf("sends = %v, want only the second turn's reply", got)
	}

	turnsOK, turnsErr := rig.metrics.TurnCounts()
	if turnsOK != 2 || turnsErr != 0 {
		t.Errorf("turn counts = ok:%d err:%d, want ok:2 err:0", turnsOK, turnsErr)
	}
}

// TestCoreProposalRendersThroughOutbound proves a raised proposal reaches the
// transport as a proposal, not as text, carrying the session id the decision will
// come back under (FR21).
func TestCoreProposalRendersThroughOutbound(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, grantConfirm())
	rig.chat.ProposeOn(msgPropose)

	rig.handle(t, rig.message(msgPropose))

	bridgetest.WaitFor(t, func() bool { return len(rig.out.Sent()) == 1 })

	proposal := rig.out.Sent()[0]
	if proposal.Proposal == nil {
		t.Fatalf("send was plain text, want a proposal: %+v", proposal)
	}

	if proposal.SessionID != bridgetest.FirstSessionID {
		t.Errorf("proposal session = %q, want %q", proposal.SessionID, bridgetest.FirstSessionID)
	}

	if !strings.Contains(proposal.Text, bridgetest.WorkflowName) {
		t.Errorf("proposal text %q missing workflow name", proposal.Text)
	}
}

// TestCoreTypedDecisionResolvesProposal proves a bare approve or deny from the
// opener resolves the pending proposal and relays the run lines, which is the
// only way to answer on a transport with no buttons (FR22).
func TestCoreTypedDecisionResolvesProposal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		verb    string
		outcome string
		want    string
		approve bool
	}{
		{name: "approve runs", verb: "approve", outcome: confirmApproved, want: "Started run " + bridgetest.RunID + ".", approve: true},
		{name: "deny relays the denial", verb: "DENY ", outcome: confirmDenied, want: bridgetest.DenialText, approve: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rig := newFixture(t, grantConfirm())
			rig.chat.ProposeOn(msgPropose)

			rig.handle(t, rig.message(msgPropose), rig.message(tt.verb))

			bridgetest.WaitFor(t, func() bool { return rig.confirmCount(tt.outcome) == 1 })

			calls := rig.chat.Confirms()
			if len(calls) != 1 || calls[0].SessionID != bridgetest.FirstSessionID || calls[0].Approve != tt.approve {
				t.Fatalf("confirm calls = %+v, want one %v for %q", calls, tt.approve, bridgetest.FirstSessionID)
			}

			bridgetest.WaitFor(t, func() bool { return rig.out.HasText(tt.want) })
		})
	}
}

// TestCoreDecisionRefusalsLeaveProposalPending proves the two refusal paths: an
// approve from a sender without the grant, and any decision from someone other
// than the opener. Both answer once, count refused, and leave the proposal live,
// which the opener's own deny then resolves (FR22).
func TestCoreDecisionRefusalsLeaveProposalPending(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		sender string
		want   string
		grant  bridge.Grant
	}{
		{name: "approve without the grant", sender: testSenderID, want: msgNoGrant, grant: grantChat()},
		{name: "decision from a non-opener", sender: otherSenderID, want: msgNotOpener, grant: grantConfirm()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rig := newFixture(t, tt.grant)
			rig.chat.ProposeOn(msgPropose)

			rig.handle(t,
				rig.message(msgPropose),
				rig.messageFrom(tt.sender, "approve"),
				rig.message("deny"),
			)

			// The opener's deny only resolves if the refusal left the proposal
			// pending, so a single denied confirm is the observable proof it stayed.
			bridgetest.WaitFor(t, func() bool { return rig.confirmCount(confirmDenied) == 1 })

			calls := rig.chat.Confirms()
			if len(calls) != 1 || calls[0].Approve {
				t.Fatalf("confirm calls = %+v, want a single deny (the refusal never ran)", calls)
			}

			if got := rig.confirmCount(confirmRefused); got != 1 {
				t.Errorf("refused count = %d, want 1", got)
			}

			if got := rig.confirmCount(confirmApproved); got != 0 {
				t.Errorf("approved count = %d, want 0", got)
			}

			if !rig.out.HasText(tt.want) {
				t.Errorf("refusal reply %q not sent; sends = %v", tt.want, rig.out.Texts())
			}
		})
	}
}

// TestCoreStaleDecisionReplies proves a decision with no live proposal is
// answered once, counts nothing, and never reaches the agent (FR24).
func TestCoreStaleDecisionReplies(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, grantConfirm())

	rig.handle(t, rig.decision(testSenderID, "ghost", true))

	bridgetest.WaitFor(t, func() bool { return rig.out.HasText(msgStaleExpired) })

	if calls := rig.chat.Confirms(); len(calls) != 0 {
		t.Fatalf("ConfirmAction called on a stale decision: %+v", calls)
	}

	if got := rig.confirmCount(confirmRefused); got != 0 {
		t.Errorf("refused count = %d, want 0 on a stale decision", got)
	}
}

// TestCoreTransportDecisionResolvesProposal proves a decision the transport
// already parsed, the shape a Telegram keyboard tap arrives as, resolves the
// proposal without going through the text path.
func TestCoreTransportDecisionResolvesProposal(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, grantConfirm())
	rig.chat.ProposeOn(msgPropose)

	rig.handle(t, rig.message(msgPropose), rig.decision(testSenderID, bridgetest.FirstSessionID, true))

	bridgetest.WaitFor(t, func() bool { return rig.confirmCount(confirmApproved) == 1 })

	calls := rig.chat.Confirms()
	if len(calls) != 1 || !calls[0].Approve {
		t.Fatalf("confirm calls = %+v, want one approve", calls)
	}

	bridgetest.WaitFor(t, func() bool {
		return rig.out.HasText("Run " + bridgetest.RunID + " finished as " + bridgetest.RunStatus + ".")
	})
}

// TestCoreProposalExpiresAsDeny proves an unanswered proposal denies after
// confirm_ttl and the transport is asked to mark the message it sent (FR23).
func TestCoreProposalExpiresAsDeny(t *testing.T) {
	t.Parallel()

	rig := newTunedFixture(t, grantConfirm(), 20*time.Millisecond, false)
	rig.chat.ProposeOn(msgPropose)

	rig.handle(t, rig.message(msgPropose))

	bridgetest.WaitFor(t, func() bool { return rig.confirmCount(confirmExpired) == 1 })

	calls := rig.chat.Confirms()
	if len(calls) != 1 || calls[0].Approve {
		t.Fatalf("confirm calls = %+v, want one deny from expiry", calls)
	}

	bridgetest.WaitFor(t, func() bool { return len(rig.out.ExpiredRefs()) == 1 })

	// The reference is the one the recording outbound handed back for the proposal
	// send, which is what proves expiry settles that exact message.
	if refs := rig.out.ExpiredRefs(); refs[0] != "ref-1" {
		t.Errorf("expired reference = %q, want the proposal's own reference", refs[0])
	}
}

// TestCoreInboxOverflowDropsNewest proves a full conversation inbox drops the
// newest inbound and counts it rather than blocking the transport's reader
// (FR20).
func TestCoreInboxOverflowDropsNewest(t *testing.T) {
	t.Parallel()

	const blockText = "BLOCK"

	rig := newFixture(t, grantChat())
	rig.chat.GateOn(blockText)

	rig.handle(t, rig.message(blockText))

	// The blocked turn has to be off the inbox and running before the fill starts,
	// or the fill would be short by one and nothing would overflow.
	bridgetest.WaitFor(t, func() bool { return len(rig.chat.Turns()) == 1 })

	for range testInboxSize {
		rig.handle(t, rig.message(msgGo))
	}

	if got := rig.updateCount(bridge.OutcomeError); got != 0 {
		t.Fatalf("dropped %d inbounds while the inbox still had room", got)
	}

	rig.handle(t, rig.message(msgGo))

	if got := rig.updateCount(bridge.OutcomeError); got != 1 {
		t.Errorf("overflow count = %d, want 1", got)
	}

	if !strings.Contains(rig.logs.String(), "conversation inbox full") {
		t.Errorf("overflow not logged: %s", rig.logs.String())
	}

	rig.chat.Release()
}

// TestCoreDrainStopsWorkersAndDropsLater proves Drain waits for the queued work,
// is safe to call twice (every transport's Run drains on the way out), and leaves
// the core refusing new inbounds rather than starting a worker nothing waits for.
func TestCoreDrainStopsWorkersAndDropsLater(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, grantChat())

	rig.handle(t, rig.message(msgGo))
	rig.core.Drain()

	if got := rig.out.Texts(); !slices.Equal(got, []string{bridgetest.ReplyText}) {
		t.Fatalf("sends after drain = %v, want the completed turn's reply", got)
	}

	rig.core.Drain()

	rig.handle(t, rig.message(msgGo))
	rig.core.Drain()

	if turns := rig.chat.Turns(); len(turns) != 1 {
		t.Errorf("ran %d turns, want 1 (the post-drain inbound is dropped)", len(turns))
	}
}

// TestCoreStampsWorkspaceAndTransport proves every turn is stamped with the
// sender's workspace, exactly as the gRPC interceptor would, and that the store
// binding is keyed by the address's own transport rather than a hardcoded one.
func TestCoreStampsWorkspaceAndTransport(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, bridge.Grant{Workspace: wsAcme})

	rig.handle(t, rig.message(msgGo))

	bridgetest.WaitFor(t, func() bool { return len(rig.chat.Turns()) == 1 })

	if bound := rig.store.BoundWorkspaces(); len(bound) != 1 || bound[0] != wsAcme {
		t.Errorf("bind workspaces = %v, want [%s]", bound, wsAcme)
	}

	if created := rig.chat.CreatedWorkspaces(); len(created) != 1 || created[0] != wsAcme {
		t.Errorf("session workspaces = %v, want [%s]", created, wsAcme)
	}

	if _, ok := rig.store.Binding(wsAcme, testTransport, testChatKey); !ok {
		t.Errorf("no binding under transport %q and chat key %q", testTransport, testChatKey)
	}
}

// TestCoreSendFailureCountedWithLabels proves a delivery that fails after the
// turn counts once, carries the transport and identity labels, and never re-runs
// the turn, which already persisted.
func TestCoreSendFailureCountedWithLabels(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, grantChat())
	rig.out.FailSends()

	rig.handle(t, rig.message(msgGo))

	bridgetest.WaitFor(t, func() bool { return len(rig.metrics.SendFailures()) == 1 })

	labels := rig.metrics.SendFailures()
	if labels[0].Transport != testTransport || labels[0].Identity != testIdentity {
		t.Errorf("send failure labels = %+v, want transport %q identity %q", labels[0], testTransport, testIdentity)
	}

	if turns := rig.chat.Turns(); len(turns) != 1 {
		t.Errorf("turn ran %d times, want 1 (no re-run)", len(turns))
	}

	turnsOK, turnsErr := rig.metrics.TurnCounts()
	if turnsOK != 1 || turnsErr != 0 {
		t.Errorf("turn counts = ok:%d err:%d, want ok:1 err:0", turnsOK, turnsErr)
	}
}
