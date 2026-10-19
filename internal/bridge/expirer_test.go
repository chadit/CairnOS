package bridge_test

import (
	"context"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/bridge"
	"github.com/chadit/CairnOS/internal/bridge/bridgetest"
	"github.com/chadit/CairnOS/internal/chat"
)

// unmarkedOut sends through a recorder but leaves out Expired, the way a
// request-scoped transport does. The recorder is a named field rather than
// embedded so its Expired is not promoted.
type unmarkedOut struct {
	rec *bridgetest.Out
}

// The recorder must keep satisfying Expirer, or the expiry case in
// core_test.go would pass without the core ever calling the hook.
var (
	_ bridge.Outbound = (*unmarkedOut)(nil)
	_ bridge.Expirer  = (*bridgetest.Out)(nil)
)

func (o *unmarkedOut) Send(ctx context.Context, addr *bridge.Address, text string) (string, error) {
	return o.rec.Send(ctx, addr, text)
}

func (o *unmarkedOut) SendProposal(
	ctx context.Context,
	addr *bridge.Address,
	sessionID string,
	proposal *chat.Proposal,
) (string, error) {
	return o.rec.SendProposal(ctx, addr, sessionID, proposal)
}

func (o *unmarkedOut) MaxChars() int {
	return o.rec.MaxChars()
}

// TestCoreExpiresProposalWithoutExpirer proves the hook stays optional: a
// transport with nothing to mark still has its lapsed proposal denied, and the
// core keeps serving the conversation afterward.
func TestCoreExpiresProposalWithoutExpirer(t *testing.T) {
	t.Parallel()

	rig := newTunedFixture(t, grantConfirm(), 20*time.Millisecond, false)
	rig.chat.ProposeOn(msgPropose)

	out := &unmarkedOut{rec: bridgetest.NewOut(testMaxChars)}
	propose := rig.message(msgPropose)
	propose.Out = out

	rig.handle(t, propose)

	bridgetest.WaitFor(t, func() bool { return rig.confirmCount(confirmExpired) == 1 })

	calls := rig.chat.Confirms()
	if len(calls) != 1 || calls[0].Approve {
		t.Fatalf("confirm calls = %+v, want one deny from expiry", calls)
	}

	follow := rig.message(msgGo)
	follow.Out = out

	rig.handle(t, follow)

	bridgetest.WaitFor(t, func() bool { return out.rec.HasText(bridgetest.ReplyText) })

	if refs := out.rec.ExpiredRefs(); len(refs) != 0 {
		t.Errorf("expired references = %v, want none through a transport without Expired", refs)
	}
}
