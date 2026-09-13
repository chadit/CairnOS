package bridge_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/bridge"
	"github.com/chadit/CairnOS/internal/bridge/bridgetest"
)

// concurrentHandles is how many callers race Drain below. It is comfortably past
// the per-conversation inbox bound, so both the queued path and the dropped path
// run while the inboxes are being closed.
const concurrentHandles = 64

// TestHandleRacesDrainWithoutPanicking proves a Handle already inside the core
// cannot send into an inbox Drain just closed. A select with a default arm does
// not protect that send: it panics like a bare one. A2A calls Handle from a gRPC
// goroutine no transport loop knows about, so a shutdown racing a request used
// to kill serve outright.
func TestHandleRacesDrainWithoutPanicking(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, grantChat())

	var callers sync.WaitGroup

	for range concurrentHandles {
		callers.Go(func() { rig.core.Handle(context.WithoutCancel(t.Context()), rig.message(msgGo)) })
	}

	rig.core.Drain()
	callers.Wait()
}

// panicOut is a send surface whose sends panic, standing in for a formatter or
// an address that blows up part way through a turn.
type panicOut struct {
	*bridgetest.Out
}

// panicText is what the panicking send raises, so the recovery assertion can
// name the value it expects to see logged.
const panicText = "outbound exploded"

func (*panicOut) Send(_ context.Context, _ *bridge.Address, _ string) (string, error) {
	panic(panicText)
}

// panicOut still has to satisfy the send surface, or the case below would prove
// nothing about what the core does with a real Outbound.
var _ bridge.Outbound = (*panicOut)(nil)

// TestWorkerSurvivesAPanickingTurn proves a panic inside one turn costs that
// turn and nothing else. The worker goroutine carried no recover, so a value
// whose formatting panicked took the API server, the scheduler, and every other
// identity down with it, which is what NFR3 forbids.
func TestWorkerSurvivesAPanickingTurn(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, grantChat())

	exploding := rig.message(msgGo)
	exploding.Out = &panicOut{Out: bridgetest.NewOut(testMaxChars)}

	rig.handle(t, exploding)

	// The same conversation, so the assertion is about the worker surviving
	// rather than about a second worker starting.
	rig.handle(t, rig.message(msgGo))

	bridgetest.WaitFor(t, func() bool { return rig.out.HasText(bridgetest.ReplyText) })

	logs := rig.logs.String()
	if !strings.Contains(logs, panicText) {
		t.Errorf("the panic was not logged: %s", logs)
	}

	if !strings.Contains(logs, testChatKey) {
		t.Errorf("the panic log does not name the conversation that died: %s", logs)
	}
}

// TestSecondProposalReplacesTheFirstTimer proves a replacement proposal arms the
// only live timer. The first timer used to survive its own entry being
// overwritten, then fire early and deny the replacement while marking the
// original message lapsed, leaving the replacement denied with no expiry line
// and no timer of its own (FR23).
func TestSecondProposalReplacesTheFirstTimer(t *testing.T) {
	t.Parallel()

	const proposalTTL = 150 * time.Millisecond

	rig := newTunedFixture(t, grantConfirm(), proposalTTL, false)
	rig.chat.ProposeOn(msgPropose)

	rig.handle(t, rig.message(msgPropose))
	bridgetest.WaitFor(t, func() bool { return len(rig.out.Sent()) == 1 })

	rig.handle(t, rig.message(msgPropose))
	bridgetest.WaitFor(t, func() bool { return len(rig.out.Sent()) == 2 })

	bridgetest.WaitFor(t, func() bool { return rig.confirmCount(confirmExpired) == 1 })
	bridgetest.WaitFor(t, func() bool { return len(rig.out.ExpiredRefs()) == 1 })

	// ref-2 is the second proposal's own message. The first timer firing would
	// have marked ref-1 instead, settling a proposal that was already replaced.
	if refs := rig.out.ExpiredRefs(); refs[0] != "ref-2" {
		t.Fatalf("expired reference = %q, want the live proposal's own reference", refs[0])
	}

	// One timer, so one deny. A surviving first timer adds a second ConfirmAction
	// that finds nothing pending.
	if calls := rig.chat.Confirms(); len(calls) != 1 {
		t.Errorf("confirm calls = %+v, want one deny from expiry", calls)
	}
}

// TestDeliverySurvivesAZeroMaxChars proves a transport reporting a useless cap
// gets the reply whole. Cutting on a zero limit appended empty chunks forever
// inside a conversation worker, so the process allocated until it died and Drain
// blocked on a worker that was never going to return (FR18).
func TestDeliverySurvivesAZeroMaxChars(t *testing.T) {
	t.Parallel()

	const reply = "the server reported no usable post size"

	rig := newFixture(t, grantChat())
	rig.chat.SetReply(reply)

	uncapped := bridgetest.NewOut(0)

	inbound := rig.message(msgGo)
	inbound.Out = uncapped

	rig.handle(t, inbound)

	bridgetest.WaitFor(t, func() bool { return uncapped.HasText(reply) })

	if got := uncapped.Texts(); len(got) != 1 {
		t.Fatalf("delivered %d messages, want the reply whole: %q", len(got), got)
	}
}
