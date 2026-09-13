package a2a

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/chadit/CairnOS/internal/api"
	a2av1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/a2a/v1"
	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/bridge"
	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/store"
)

const (
	// transportA2A is the bridge_sessions transport column value a conversation
	// held over A2A binds under, so the same context id on A2A and a chat
	// transport stay two conversations rather than one.
	transportA2A = "a2a"

	// stateCompleted and stateInputRequired are the two A2A states a turn can
	// end in: an ordinary answer, or a proposal waiting on the peer's approve or
	// deny (FR37).
	stateCompleted     = "completed"
	stateInputRequired = "input-required"

	// captureMaxChars is the per-message cap the capture reports. A2A answers in
	// one response body, so there is no wire limit to split for and the cap sits
	// past any reply a turn produces.
	captureMaxChars = 1_000_000

	// partSep joins the messages one turn produced. Each send is its own chat
	// message, so a blank line between them is what a chat client would show.
	partSep = "\n\n"

	// peerName labels an A2A sender in the core's logs. The peer's own name
	// never crosses the wire, so there is nothing finer to record.
	peerName = "a2a peer"

	// turnCeiling bounds the detached turn below. context.WithoutCancel strips the
	// request deadline along with cancellation and nothing downstream replaces it,
	// so a provider that accepts a connection and never answers would pin the
	// conversation worker and hang Drain. Ten minutes sits past any request timeout
	// a chat driver sets for itself, so a driver failure still reads as one.
	turnCeiling = 10 * time.Minute

	// keySep joins the peer subject to the context id in the chat key.
	keySep = ":"
)

// BridgeHandler is the slice of the chat bridge the conversation path drives;
// *bridge.Core satisfies it. Handle is asynchronous, which is why the capture
// below carries a completion signal.
type BridgeHandler interface {
	Handle(ctx context.Context, inbound *bridge.Inbound)
}

// capture is the bridge.Outbound one SendMessage call hands the core: instead
// of posting to a wire it buffers what the turn produced, so the reply goes
// back in the response body. It also implements bridge.TurnCompleter, which is
// how the handler learns the turn finished sending. Field order follows govet's
// fieldalignment check rather than reading order.
type capture struct {
	// done closes once, when the core reports the turn is over.
	done chan struct{}

	proposal *chat.Proposal
	parts    []string

	// mu guards proposal and parts, which the conversation worker writes and the
	// handler reads once the signal lands.
	mu     sync.Mutex
	closed bool
}

// newCapture builds the per-request send surface.
func newCapture() *capture {
	return &capture{done: make(chan struct{})}
}

// Send buffers one message. The empty reference says there is nothing to edit
// later, which is true of a response body.
func (c *capture) Send(_ context.Context, _ *bridge.Address, text string) (string, error) { //nolint:discardedcontext // the capture buffers in memory; nothing to cancel
	c.mu.Lock()
	defer c.mu.Unlock()

	c.parts = append(c.parts, text)

	return "", nil
}

// SendProposal buffers the rendered proposal and keeps the proposal itself, so
// the response can carry both the text a peer reads and the typed fields a peer
// decides on.
func (c *capture) SendProposal(
	_ context.Context, //nolint:discardedcontext // the capture buffers in memory; nothing to cancel
	_ *bridge.Address,
	_ string,
	proposal *chat.Proposal,
) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.proposal = proposal
	c.parts = append(c.parts, bridge.ProposalText(proposal))

	return "", nil
}

// Expired does nothing: the response body carrying the proposal was sent long
// before the TTL lapsed, and there is no posted message left to mark.
func (*capture) Expired(_ context.Context, _ *bridge.Address, _ string) error { //nolint:discardedcontext // nothing was posted, so nothing is cancelable
	return nil
}

// MaxChars reports the cap that keeps the core's splitter out of the way.
func (*capture) MaxChars() int {
	return captureMaxChars
}

// Done releases the handler waiting on this turn. The core calls it once per
// inbound, including one it drops, so a caller is never left waiting on a turn
// that will not run.
func (c *capture) Done() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return
	}

	c.closed = true
	close(c.done)
}

// Reply returns the turn's buffered text and the proposal it raised, if any.
// Callers read it after the completion signal, so every send has landed.
func (c *capture) Reply() (string, *chat.Proposal) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return strings.Join(c.parts, partSep), c.proposal
}

// SendMessage runs one conversational turn and answers with the reply. The
// conversation is keyed by context_id, so a peer holding the same id across
// calls continues the same chat session, and a turn that ends on a proposal
// answers input-required for the peer to resolve with a later approve or deny
// (FR36, FR37).
func (s *Service) SendMessage(ctx context.Context, req *a2av1.SendMessageRequest) (*a2av1.SendMessageResponse, error) {
	if s.bridge == nil {
		return nil, wireChatDisabled()
	}

	text := strings.TrimSpace(req.GetText())
	if text == "" {
		return nil, wireInvalidArgument("text is required")
	}

	contextID := strings.TrimSpace(req.GetContextId())
	if contextID == "" {
		contextID = store.NewID()
	}

	out := newCapture()

	// The turn runs on a context detached from the request so an unanswered
	// proposal's expiry timer, and the ConfirmAction it fires, outlive the
	// response carrying the proposal. The handler still waits on the request
	// context below, so a peer that gives up is answered at once. CreateTask
	// detaches a run for the same reason.
	turnCtx, cancelTurn := context.WithTimeout(context.WithoutCancel(ctx), turnCeiling)

	go releaseCeiling(out.done, cancelTurn)

	s.bridge.Handle(turnCtx, inboundFor(ctx, contextID, text, out))

	select {
	case <-out.done:
	case <-ctx.Done():
		return nil, wireContext(ctx.Err())
	}

	reply, proposal := out.Reply()

	return &a2av1.SendMessageResponse{
		ContextId: contextID,
		Text:      reply,
		State:     turnState(proposal),
		Proposal:  protoProposal(proposal),
	}, nil
}

// releaseCeiling frees the detached turn's deadline once the core reports the
// turn over, so an ordinary turn holds no timer for the whole ceiling. It waits
// on the turn, not the response, because a peer that gives up must not cancel
// work in flight. A wedged turn ends when the ceiling fires, so the wait is
// bounded.
func releaseCeiling(done <-chan struct{}, cancel context.CancelFunc) {
	<-done
	cancel()
}

// inboundFor builds the core's view of one A2A turn. The workspace and the
// approval grant both come off the verified token the interceptor stamped, so
// an A2A peer is authorized exactly like a gRPC caller rather than by a second
// allowlist (FR37).
func inboundFor(ctx context.Context, contextID, text string, out *capture) *bridge.Inbound {
	peer := peerID(ctx)

	return &bridge.Inbound{
		Out:    out,
		Addr:   bridge.Address{Transport: transportA2A, ChatKey: chatKey(peer, contextID)},
		Text:   text,
		Sender: bridge.Sender{ID: peer, Name: peerName},
		Grant: bridge.Grant{
			Workspace:    store.WorkspaceFromContext(ctx),
			AllowConfirm: api.ContextHasScope(ctx, auth.ScopeWriteWorkflow),
		},
		Kind: bridge.KindMessage,
	}
}

// chatKey scopes the session binding to the calling peer, not just to the peer's
// chosen context id. The binding is (workspace, transport, chat_key), so an
// unscoped key lets any other key in the workspace resume a conversation by
// guessing an id as ordinary as "ops" and read the transcript back (FR36).
func chatKey(peer, contextID string) string {
	return peer + keySep + contextID
}

// peerID names the peer a proposal is matched against: the subject of the token
// that made the call, so two keys in one workspace stay two peers. With auth
// off no subject is stamped and every caller in a workspace reads as one peer,
// which is the right answer for a node that is not identifying anyone.
func peerID(ctx context.Context) string {
	if subject, ok := api.ContextSubject(ctx); ok && subject != "" {
		return subject
	}

	return store.WorkspaceFromContext(ctx)
}

// turnState reads the A2A state off the turn's outcome: a captured proposal
// means the peer still owes a decision.
func turnState(proposal *chat.Proposal) string {
	if proposal == nil {
		return stateCompleted
	}

	return stateInputRequired
}

// protoProposal renders a captured proposal on the wire, nil for a turn that
// raised none.
func protoProposal(proposal *chat.Proposal) *a2av1.Proposal {
	if proposal == nil {
		return nil
	}

	return &a2av1.Proposal{
		Workflow:           proposal.WorkflowName,
		Inputs:             protoStruct(proposal.Inputs),
		EstimatedBudgetUsd: proposal.EstimatedBudgetUSD,
	}
}
