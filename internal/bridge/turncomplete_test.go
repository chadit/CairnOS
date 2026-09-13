package bridge_test

import (
	"sync"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/bridge"
	"github.com/chadit/CairnOS/internal/bridge/bridgetest"
)

// completeTimeout bounds the wait for a turn's completion signal. The core runs
// the turn on a conversation worker, so the signal arrives off a real goroutine.
const completeTimeout = 2 * time.Second

// completingOut is a recording outbound that also implements the optional
// bridge.TurnCompleter hook. It snapshots how many sends had landed when Done
// fired, which is what separates "Done was called" from "Done was called after
// the turn's last send".
type completingOut struct {
	*bridgetest.Out

	done chan struct{}

	mu         sync.Mutex
	calls      int
	sentAtDone int
}

// completingOut has to satisfy both halves the core looks for: the required
// send surface and the optional completion hook. A wrapper that quietly stopped
// satisfying either would make every assertion below vacuous.
var (
	_ bridge.Outbound      = (*completingOut)(nil)
	_ bridge.TurnCompleter = (*completingOut)(nil)
)

// newCompletingOut wraps a fresh recorder with the completion hook.
func newCompletingOut() *completingOut {
	return &completingOut{Out: bridgetest.NewOut(testMaxChars), done: make(chan struct{})}
}

// Done records the call and the send count at that moment, then releases every
// waiter. Later calls are counted so a test can prove the core signals once.
func (o *completingOut) Done() {
	o.mu.Lock()
	o.calls++
	first := o.calls == 1

	if first {
		o.sentAtDone = len(o.Sent())
	}

	o.mu.Unlock()

	if first {
		close(o.done)
	}
}

// SentAtDone reports how many sends had landed when the first Done arrived.
func (o *completingOut) SentAtDone() int {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.sentAtDone
}

// Calls reports how many times the core signaled completion.
func (o *completingOut) Calls() int {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.calls
}

// awaitDone blocks until the core signals the turn is over, failing the test
// rather than hanging when the signal never comes. A caller that waits on this
// signal for its reply, as the A2A service does, hangs without it.
func awaitDone(t *testing.T, out *completingOut) {
	t.Helper()

	select {
	case <-out.done:
	case <-time.After(completeTimeout):
		t.Fatal("core never signaled turn completion")
	}
}

// TestCoreSignalsCompletionAfterReply proves the completion hook fires once a
// plain turn has posted its reply, not before it. A request-scoped transport
// reads the buffered reply the moment the signal lands, so a signal that
// arrived first would hand back an empty answer.
func TestCoreSignalsCompletionAfterReply(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, grantChat())
	out := newCompletingOut()

	inbound := rig.message(msgGo)
	inbound.Out = out

	rig.core.Handle(t.Context(), inbound)
	awaitDone(t, out)

	if got := out.SentAtDone(); got != 1 {
		t.Errorf("sends landed before completion = %d, want 1 (the turn's reply)", got)
	}

	if got := out.Calls(); got != 1 {
		t.Errorf("completion signals = %d, want 1", got)
	}

	if !out.HasText(bridgetest.ReplyText) {
		t.Errorf("recorded sends = %v, want the canned reply", out.Texts())
	}
}

// TestCoreSignalsCompletionAfterDecision proves a resolved approve signals
// completion too, after the run lines it relays. The decision is a second
// request on a request-scoped transport, so it needs its own signal.
func TestCoreSignalsCompletionAfterDecision(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, grantConfirm())
	rig.chat.ProposeOn(msgPropose)

	proposeOut := newCompletingOut()
	propose := rig.message(msgPropose)
	propose.Out = proposeOut

	rig.core.Handle(t.Context(), propose)
	awaitDone(t, proposeOut)

	decideOut := newCompletingOut()
	decide := rig.decision(testSenderID, bridgetest.FirstSessionID, true)
	decide.Out = decideOut

	rig.core.Handle(t.Context(), decide)
	awaitDone(t, decideOut)

	// The approve relays a run-started and a run-finished line before the core
	// signals, so both are readable by a caller waiting on the signal.
	if got := decideOut.SentAtDone(); got != 2 {
		t.Errorf("sends landed before completion = %d, want 2 (the two run lines)", got)
	}

	if got := decideOut.Calls(); got != 1 {
		t.Errorf("completion signals = %d, want 1", got)
	}
}

// TestCoreSignalsCompletionOnDrainedDrop proves an inbound dropped because the
// core is drained still signals. A caller blocked on the signal would otherwise
// wait out its whole request deadline for a turn that was never going to run.
func TestCoreSignalsCompletionOnDrainedDrop(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, grantChat())
	rig.core.Drain()

	out := newCompletingOut()
	inbound := rig.message(msgGo)
	inbound.Out = out

	rig.core.Handle(t.Context(), inbound)
	awaitDone(t, out)

	if got := out.SentAtDone(); got != 0 {
		t.Errorf("sends landed before completion = %d, want 0 for a dropped inbound", got)
	}

	if rig.chat.HasTurn(msgGo) {
		t.Error("drained core ran the dropped inbound as a turn")
	}
}

// TestCoreHandlesOutboundWithoutCompletionHook proves the hook stays optional:
// a transport whose Outbound does not implement it, which is every long-lived
// poll-loop transport, runs its turn unchanged instead of panicking on a
// missing method.
func TestCoreHandlesOutboundWithoutCompletionHook(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, grantChat())

	rig.handle(t, rig.message(msgGo))

	bridgetest.WaitFor(t, func() bool { return rig.out.HasText(bridgetest.ReplyText) })
}

// TestCoreSignalsCompletionOnFullInbox proves the other drop path signals too.
// The full-inbox branch is the one that fires under load rather than only at
// shutdown, so it is the one a request-scoped caller actually meets while the
// node is busy (FR20).
func TestCoreSignalsCompletionOnFullInbox(t *testing.T) {
	t.Parallel()

	const blockText = "BLOCK"

	rig := newFixture(t, grantChat())
	rig.chat.GateOn(blockText)

	rig.handle(t, rig.message(blockText))

	// The blocked turn has to be off the inbox and running before the fill
	// starts, or the fill would be short by one and nothing would overflow.
	bridgetest.WaitFor(t, func() bool { return len(rig.chat.Turns()) == 1 })

	for range testInboxSize {
		rig.handle(t, rig.message(msgGo))
	}

	out := newCompletingOut()
	overflowing := rig.message(msgGo)
	overflowing.Out = out

	rig.core.Handle(t.Context(), overflowing)
	awaitDone(t, out)

	if got := out.SentAtDone(); got != 0 {
		t.Errorf("sends landed before completion = %d, want 0 for a dropped inbound", got)
	}

	if got := rig.updateCount(bridge.OutcomeError); got != 1 {
		t.Errorf("overflow count = %d, want 1", got)
	}

	rig.chat.Release()
}
