package bridgetest

import (
	"context"
	"slices"
	"strconv"
	"sync"

	"github.com/chadit/CairnOS/internal/bridge"
	"github.com/chadit/CairnOS/internal/chat"
)

// Sent records one outbound the recorder received. Proposal is nil for a plain
// send and set for a rendered proposal, which is how a test tells the two calls
// apart with no wire format in sight.
type Sent struct {
	Proposal  *chat.Proposal
	Addr      bridge.Address
	Text      string
	SessionID string
}

// Out is a recording bridge.Outbound: it keeps every send, hands back a reference
// the expiry path later quotes, and can fail every send. It is the seam that lets
// the core be exercised with no transport at all.
type Out struct {
	sent     []Sent
	expired  []string
	refSeq   int
	maxChars int
	mu       sync.Mutex
	failSend bool
}

// NewOut builds a recorder reporting maxChars as the transport's per-message cap.
func NewOut(maxChars int) *Out {
	return &Out{maxChars: maxChars}
}

// Send records one plain message and returns a fresh reference.
func (o *Out) Send(_ context.Context, addr *bridge.Address, text string) (string, error) { //nolint:discardedcontext // a recorder appends in memory; nothing to cancel
	return o.record(&Sent{Addr: *addr, Text: text})
}

// SendProposal records one rendered proposal and returns a fresh reference. It
// renders through bridge.ProposalText so a test reads the same body a real
// adapter would post.
func (o *Out) SendProposal(
	_ context.Context, //nolint:discardedcontext // a recorder appends in memory; nothing to cancel
	addr *bridge.Address,
	sessionID string,
	proposal *chat.Proposal,
) (string, error) {
	return o.record(&Sent{
		Proposal:  proposal,
		Addr:      *addr,
		Text:      bridge.ProposalText(proposal),
		SessionID: sessionID,
	})
}

// Expired records the reference the core asked to mark lapsed.
func (o *Out) Expired(_ context.Context, _ *bridge.Address, ref string) error { //nolint:discardedcontext // a recorder appends in memory; nothing to cancel
	o.mu.Lock()
	defer o.mu.Unlock()

	o.expired = append(o.expired, ref)

	return nil
}

// MaxChars reports the per-message cap the split assertions drive.
func (o *Out) MaxChars() int {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.maxChars
}

// Sent returns a copy of the recorded sends in order.
func (o *Out) Sent() []Sent {
	o.mu.Lock()
	defer o.mu.Unlock()

	return append([]Sent(nil), o.sent...)
}

// Texts returns just the recorded send texts in order.
func (o *Out) Texts() []string {
	sent := o.Sent()
	texts := make([]string, 0, len(sent))

	for _, message := range sent {
		texts = append(texts, message.Text)
	}

	return texts
}

// HasText reports whether any recorded send carried exactly text.
func (o *Out) HasText(want string) bool {
	return slices.Contains(o.Texts(), want)
}

// ExpiredRefs returns a copy of the references marked expired, in order.
func (o *Out) ExpiredRefs() []string {
	o.mu.Lock()
	defer o.mu.Unlock()

	return append([]string(nil), o.expired...)
}

// FailSends makes every later send fail with ErrSend.
func (o *Out) FailSends() {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.failSend = true
}

// record appends one send under the lock and mints its reference, failing when
// the recorder is configured to.
func (o *Out) record(message *Sent) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.sent = append(o.sent, *message)

	if o.failSend {
		return "", ErrSend
	}

	o.refSeq++

	return "ref-" + strconv.Itoa(o.refSeq), nil
}
