package a2a_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/chadit/CairnOS/internal/a2a"
	a2av1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/a2a/v1"
	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/bridge"
	"github.com/chadit/CairnOS/internal/bridge/bridgetest"
)

const (
	// contextOne and contextTwo are conversation handles a peer picks; the core
	// binds one chat session per handle.
	contextOne = "ctx-alpha"
	contextTwo = "ctx-beta"

	// msgHello drives an ordinary turn, msgPropose makes the fake agent raise a
	// D9 proposal, and msgSlow blocks until its context is canceled.
	msgHello   = "hello there"
	msgPropose = "propose"
	msgSlow    = "take your time"

	// verbApprove is the typed decision that resolves a live proposal on a
	// transport with no buttons to tap, which is every A2A peer.
	verbApprove = "approve"

	// stateCompleted and stateInputRequired are the wire states SendMessage
	// answers with. Package a2a keeps its own copies unexported, so they are
	// pinned here as the contract a peer reads.
	stateCompleted     = "completed"
	stateInputRequired = "input-required"

	// noGrantLine is the refusal the core posts when an approve arrives without
	// the approval grant. The bridge keeps it unexported, so it is pinned here as
	// the observable contract.
	noGrantLine = "Approval isn't granted for this user."

	// expiryTTL is short enough that an unanswered proposal lapses well inside
	// one case, and long enough that the raising call never races its own expiry.
	expiryTTL = 150 * time.Millisecond

	// turnDeadline bounds the canceled-turn case. It is short because the fake
	// agent blocks until the context ends, so the test waits exactly this long.
	turnDeadline = 200 * time.Millisecond
)

// send runs one SendMessage under a bearer token, failing the test on a
// transport error so each case reads as the assertion it is making.
func send(t *testing.T, fix *fixture, token, contextID, text string) *a2av1.SendMessageResponse {
	t.Helper()

	resp, err := fix.a2a.SendMessage(authedCtx(t, token), &a2av1.SendMessageRequest{
		ContextId: contextID,
		Text:      text,
	})
	if err != nil {
		t.Fatalf("SendMessage(%q, %q): %v", contextID, text, err)
	}

	return resp
}

// TestSendMessageKeepsOneSessionPerContext proves context_id is the
// conversation handle FR36 asks for: two calls under one id continue the same
// chat session, and a different id starts its own.
func TestSendMessageKeepsOneSessionPerContext(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{authEnabled: true, chatEnabled: true})
	token := fix.token(t, auth.ScopeA2ATask)

	first := send(t, fix, token, contextOne, msgHello)
	second := send(t, fix, token, contextOne, msgHello)
	other := send(t, fix, token, contextTwo, msgHello)

	if first.GetContextId() != contextOne || second.GetContextId() != contextOne {
		t.Fatalf("context ids = %q and %q, want both %q", first.GetContextId(), second.GetContextId(), contextOne)
	}

	if first.GetText() != bridgetest.ReplyText {
		t.Errorf("reply = %q, want the agent's answer %q", first.GetText(), bridgetest.ReplyText)
	}

	if first.GetState() != stateCompleted {
		t.Errorf("state = %q, want completed for an ordinary turn", first.GetState())
	}

	turns := fix.chat.agent.Turns()
	if len(turns) != 3 {
		t.Fatalf("recorded turns = %d, want 3", len(turns))
	}

	if turns[0].SessionID != turns[1].SessionID {
		t.Errorf("two calls on %q ran on sessions %q and %q, want one session",
			contextOne, turns[0].SessionID, turns[1].SessionID)
	}

	if turns[2].SessionID == turns[0].SessionID {
		t.Errorf("call on %q reused session %q from %q, want its own", contextTwo, turns[2].SessionID, contextOne)
	}

	if other.GetContextId() != contextTwo {
		t.Errorf("second conversation context id = %q, want %q", other.GetContextId(), contextTwo)
	}
}

// TestSendMessageProposalRoundTrip proves FR37 end to end: a turn that ends on
// a proposal answers input-required with the proposal filled, and a follow-up
// approve on the same context id resolves it.
func TestSendMessageProposalRoundTrip(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{authEnabled: true, chatEnabled: true})
	fix.chat.agent.ProposeOn(msgPropose)

	token := fix.token(t, auth.ScopeA2ATask, auth.ScopeWriteWorkflow)

	raised := send(t, fix, token, contextOne, msgPropose)

	if raised.GetState() != stateInputRequired {
		t.Fatalf("state = %q, want input-required for a raised proposal", raised.GetState())
	}

	proposal := raised.GetProposal()
	if proposal == nil {
		t.Fatal("input-required answer carried no proposal")
	}

	if proposal.GetWorkflow() != bridgetest.WorkflowName {
		t.Errorf("proposal workflow = %q, want %q", proposal.GetWorkflow(), bridgetest.WorkflowName)
	}

	if got := proposal.GetInputs().AsMap()["env"]; got != "prod" {
		t.Errorf("proposal inputs env = %v, want prod", got)
	}

	if proposal.GetEstimatedBudgetUsd() != 1.5 {
		t.Errorf("proposal budget = %v, want 1.5", proposal.GetEstimatedBudgetUsd())
	}

	resolved := send(t, fix, token, contextOne, verbApprove)

	if resolved.GetState() != stateCompleted {
		t.Errorf("state after approve = %q, want completed", resolved.GetState())
	}

	if resolved.GetProposal() != nil {
		t.Errorf("resolved answer still carried a proposal: %+v", resolved.GetProposal())
	}

	if !strings.Contains(resolved.GetText(), bridgetest.RunID) {
		t.Errorf("approve reply = %q, want the started run %q in it", resolved.GetText(), bridgetest.RunID)
	}

	confirms := fix.chat.agent.Confirms()
	if len(confirms) != 1 || !confirms[0].Approve {
		t.Fatalf("confirm calls = %+v, want one approve", confirms)
	}
}

// TestSendMessageApproveNeedsWriteWorkflow proves the second, finer scope check
// FR37 asks for: a2a:task alone reaches the conversation but cannot approve a
// workflow, and the proposal stays pending rather than resolving.
func TestSendMessageApproveNeedsWriteWorkflow(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{authEnabled: true, chatEnabled: true})
	fix.chat.agent.ProposeOn(msgPropose)

	token := fix.token(t, auth.ScopeA2ATask)

	if state := send(t, fix, token, contextOne, msgPropose).GetState(); state != stateInputRequired {
		t.Fatalf("state = %q, want input-required for a raised proposal", state)
	}

	refused := send(t, fix, token, contextOne, verbApprove)

	if refused.GetText() != noGrantLine {
		t.Errorf("approve reply = %q, want the refusal %q", refused.GetText(), noGrantLine)
	}

	if confirms := fix.chat.agent.Confirms(); len(confirms) != 0 {
		t.Fatalf("refused approve still reached the agent: %+v", confirms)
	}
}

// TestSendMessageMintsContextID proves a peer may open a conversation without
// naming it: the node mints the handle and returns it for the follow-up.
func TestSendMessageMintsContextID(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{authEnabled: true, chatEnabled: true})
	token := fix.token(t, auth.ScopeA2ATask)

	minted := send(t, fix, token, "", msgHello).GetContextId()
	if minted == "" {
		t.Fatal("a call with no context id came back with none")
	}

	send(t, fix, token, minted, msgHello)

	turns := fix.chat.agent.Turns()
	if len(turns) != 2 || turns[0].SessionID != turns[1].SessionID {
		t.Fatalf("turns = %+v, want two on one session under the minted id", turns)
	}
}

// TestSendMessageRejectsEmptyText proves a blank turn is refused before any
// session work, since there is nothing for the agent to answer.
func TestSendMessageRejectsEmptyText(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{authEnabled: true, chatEnabled: true})
	token := fix.token(t, auth.ScopeA2ATask)

	_, err := fix.a2a.SendMessage(authedCtx(t, token), &a2av1.SendMessageRequest{
		ContextId: contextOne,
		Text:      "   ",
	})

	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("SendMessage with blank text = %v, want InvalidArgument", status.Code(err))
	}

	if turns := fix.chat.agent.Turns(); len(turns) != 0 {
		t.Fatalf("blank turn reached the agent: %+v", turns)
	}
}

// TestSendMessageChatDisabled proves a node with no chat surface answers a
// precondition rather than an empty reply, so a peer can tell a misconfigured
// node from an agent with nothing to say.
func TestSendMessageChatDisabled(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{authEnabled: true})
	token := fix.token(t, auth.ScopeA2ATask)

	_, err := fix.a2a.SendMessage(authedCtx(t, token), &a2av1.SendMessageRequest{
		ContextId: contextOne,
		Text:      msgHello,
	})

	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("SendMessage with chat off = %v, want FailedPrecondition", status.Code(err))
	}
}

// TestSendMessageHonorsRequestDeadline proves the handler waits on the request
// context as well as the turn: a turn still running when the peer's deadline
// lapses answers DeadlineExceeded instead of blocking on a reply that will
// never arrive.
func TestSendMessageHonorsRequestDeadline(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{authEnabled: true, chatEnabled: true})
	fix.chat.agent.GateOn(msgSlow)

	token := fix.token(t, auth.ScopeA2ATask)

	ctx, cancel := context.WithTimeout(t.Context(), turnDeadline)
	defer cancel()

	authed := metadata.AppendToOutgoingContext(ctx, "authorization", bearerPrefix+token)

	_, err := fix.a2a.SendMessage(authed, &a2av1.SendMessageRequest{ContextId: contextOne, Text: msgSlow})

	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("SendMessage on a lapsed deadline = %v, want DeadlineExceeded", status.Code(err))
	}

	// The turn runs on a context detached from the request, so the peer's
	// deadline does not end it. Releasing the gate lets the conversation worker
	// finish, which is what the core's drain on cleanup waits for.
	fix.chat.agent.Release()
}

// TestSendMessageRequiresA2AScope proves the conversational method sits behind
// the same scope the task methods do, so the scope table entry is enforced
// rather than only declared.
func TestSendMessageRequiresA2AScope(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{authEnabled: true, chatEnabled: true})
	token := fix.token(t, auth.ScopeReadRun)

	_, err := fix.a2a.SendMessage(authedCtx(t, token), &a2av1.SendMessageRequest{
		ContextId: contextOne,
		Text:      msgHello,
	})

	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("SendMessage without a2a:task = %v, want PermissionDenied", status.Code(err))
	}
}

// TestSendMessageScopesTheConversationToItsPeer proves two keys in one workspace
// holding the same context_id hold two conversations. The chat key used to be
// the peer's own context_id, so a second key could resume the first's session by
// naming an id as ordinary as "ops" and read the transcript back (FR36).
func TestSendMessageScopesTheConversationToItsPeer(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{authEnabled: true, chatEnabled: true})

	first := fix.token(t, auth.ScopeA2ATask)
	second := fix.token(t, auth.ScopeA2ATask)

	send(t, fix, first, contextOne, msgHello)
	send(t, fix, second, contextOne, msgHello)

	turns := fix.chat.agent.Turns()
	if len(turns) != 2 {
		t.Fatalf("recorded turns = %d, want one per peer", len(turns))
	}

	if turns[0].SessionID == turns[1].SessionID {
		t.Fatalf("two peers on %q shared session %q, want one session each", contextOne, turns[0].SessionID)
	}
}

// TestSendMessageKeepsAProposalWithItsPeer proves a second key in the workspace
// cannot answer, or even reach, another peer's live proposal. Both tokens carry
// write:workflow, so nothing here turns on the approval grant.
func TestSendMessageKeepsAProposalWithItsPeer(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{authEnabled: true, chatEnabled: true})
	fix.chat.agent.ProposeOn(msgPropose)

	opener := fix.token(t, auth.ScopeA2ATask, auth.ScopeWriteWorkflow)
	stranger := fix.token(t, auth.ScopeA2ATask, auth.ScopeWriteWorkflow)

	if state := send(t, fix, opener, contextOne, msgPropose).GetState(); state != stateInputRequired {
		t.Fatalf("state = %q, want input-required for a raised proposal", state)
	}

	// The stranger's approve is ordinary text in a conversation of their own:
	// nothing is pending there, so it never reaches the agent's confirm gate.
	stray := send(t, fix, stranger, contextOne, verbApprove)

	if stray.GetText() != bridgetest.ReplyText {
		t.Errorf("stranger's approve reply = %q, want an ordinary turn %q", stray.GetText(), bridgetest.ReplyText)
	}

	if confirms := fix.chat.agent.Confirms(); len(confirms) != 0 {
		t.Fatalf("a stranger's approve reached the agent: %+v", confirms)
	}

	// The proposal is still pending, so its own peer can still answer it.
	if state := send(t, fix, opener, contextOne, verbApprove).GetState(); state != stateCompleted {
		t.Fatalf("opener's approve state = %q, want completed", state)
	}

	confirms := fix.chat.agent.Confirms()
	if len(confirms) != 1 || !confirms[0].Approve {
		t.Fatalf("confirm calls = %+v, want one approve from the opener", confirms)
	}
}

// TestSendMessageDetachesTheTurnFromTheRequest proves the core runs the turn on
// a context that outlives the response. The proposal expiry timer fires long
// after the request that raised it returned, and it settles the proposal
// through the same context the turn ran on, so a context that died with the
// request would leave the denial unable to persist.
func TestSendMessageDetachesTheTurnFromTheRequest(t *testing.T) {
	t.Parallel()

	recorder := newRecordingBridge()
	service := a2a.NewService(nil, nil, nil, recorder)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	sent := make(chan struct{})

	go func() {
		defer close(sent)

		_, _ = service.SendMessage(ctx, &a2av1.SendMessageRequest{ContextId: contextOne, Text: msgHello})
	}()

	<-recorder.captured
	cancel()
	close(recorder.release)

	if err := recorder.TurnErr(t); err != nil {
		t.Fatalf("turn context ended with %v once the request was canceled, want it still live", err)
	}

	<-sent
}

// TestSendMessageExpiresAnUnansweredProposal proves the expiry path still runs
// after the request that raised the proposal is over: the timer fires, the core
// finds its route, and the denial reaches the agent.
func TestSendMessageExpiresAnUnansweredProposal(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{authEnabled: true, chatEnabled: true, confirmTTL: expiryTTL})
	fix.chat.agent.ProposeOn(msgPropose)

	token := fix.token(t, auth.ScopeA2ATask, auth.ScopeWriteWorkflow)

	if state := send(t, fix, token, contextOne, msgPropose).GetState(); state != stateInputRequired {
		t.Fatalf("state = %q, want input-required for a raised proposal", state)
	}

	// The request is over and its context canceled by now. Nobody answers, so the
	// TTL has to settle the proposal on its own.
	bridgetest.WaitFor(t, func() bool {
		confirms := fix.chat.agent.Confirms()

		return len(confirms) == 1 && !confirms[0].Approve
	})
}

// recordingBridge is a BridgeHandler that holds the turn open until the test
// releases it, then reads the turn context. Holding it open matters: the
// service cancels the detached context as soon as the turn reports itself over,
// so a reading taken after that says nothing about the request.
type recordingBridge struct {
	// captured closes once Handle holds the turn context, so the test cancels the
	// request against a turn that is already running.
	captured chan struct{}

	// release lets the turn finish, and the test closes it only after canceling
	// the request, which is what makes the reading below ordered rather than raced.
	release chan struct{}

	// recorded closes once err holds the reading, publishing it to the test.
	recorded chan struct{}

	// err is the turn context's state, read on the worker goroutine.
	err error
}

// newRecordingBridge builds the handshake channels the turn and the test use.
func newRecordingBridge() *recordingBridge {
	return &recordingBridge{
		captured: make(chan struct{}),
		release:  make(chan struct{}),
		recorded: make(chan struct{}),
	}
}

// Handle waits out the request's cancellation, records how the turn's context
// took it, then completes the turn.
func (b *recordingBridge) Handle(ctx context.Context, inbound *bridge.Inbound) {
	close(b.captured)
	<-b.release

	b.err = ctx.Err()
	close(b.recorded)

	if completer, ok := inbound.Out.(bridge.TurnCompleter); ok {
		completer.Done()
	}
}

// TurnErr reports the state of the context the service handed the core, failing
// the test when no turn ever arrived.
func (b *recordingBridge) TurnErr(t *testing.T) error {
	t.Helper()

	select {
	case <-b.recorded:
		return b.err
	case <-time.After(turnDeadline):
		t.Fatal("the service never handed a turn to the bridge")

		return nil
	}
}
