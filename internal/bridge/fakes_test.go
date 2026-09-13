package bridge_test

import (
	"bytes"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/bridge"
	"github.com/chadit/CairnOS/internal/bridge/bridgetest"
)

const (
	// The fake transport the core tests address. The core treats both strings as
	// opaque, so they only have to be stable across a test.
	testTransport = "fake"
	testIdentity  = "bot"

	// testChatKey is the one conversation these tests drive; testChannel is the
	// transport's reply coordinate for it.
	testChatKey = "chat-1"
	testChannel = "channel-1"

	// testSenderID is the operator every test sends as, and the opener a proposal
	// is matched against. otherSenderID is a second operator, used to prove a
	// decision from anyone but the opener is refused.
	testSenderID  = "user-1"
	otherSenderID = "user-2"

	// wsDefault and wsAcme are the workspace ids the tests stamp turns with.
	wsDefault = "default"
	wsAcme    = "acme"

	// msgPropose makes the fake agent raise a D9 proposal, msgTool makes it emit a
	// tool-call event, and msgGo drives a plain turn.
	msgPropose = "propose"
	msgTool    = "tool-please"
	msgGo      = "go"

	// Confirm outcome labels, read off the bridge rather than re-typed, so a
	// rename breaks this test instead of a dashboard query.
	confirmApproved = bridge.ConfirmApproved
	confirmDenied   = bridge.ConfirmDenied
	confirmExpired  = bridge.ConfirmExpired
	confirmRefused  = bridge.ConfirmRefused

	// Operator-facing lines the confirm flow sends, asserted as observable text.
	msgNoGrant      = "Approval isn't granted for this user."
	msgNotOpener    = "Only the operator who started this proposal can answer it."
	msgStaleExpired = "This proposal expired."

	// testMaxChars is the per-message cap the recording outbound reports. It is
	// small so a split assertion needs a short reply rather than a 4096-character
	// one.
	testMaxChars = 40

	// testInboxSize mirrors the core's unexported per-conversation inbox bound,
	// pinned here as the observable contract the overflow test drives.
	testInboxSize = 16

	// toolPrefix is the status line a verbose turn relays for a tool call.
	toolPrefix = "tool: "
)

// syncBuffer is a concurrency-safe log sink so assertions can read what the
// workers and timers wrote from several goroutines.
type syncBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

// Write appends log bytes under the lock. bytes.Buffer.Write never errors, so
// the discarded error is safe and the nil keeps the io.Writer contract clean.
func (s *syncBuffer) Write(data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	written, _ := s.buf.Write(data)

	return written, nil
}

// String returns the captured log text.
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.buf.String()
}

// grantChat is a sender who may talk to the agent but not approve a workflow.
// It is a function rather than a package variable so no suite can mutate the
// grant another suite reads.
func grantChat() bridge.Grant {
	return bridge.Grant{Workspace: wsDefault}
}

// grantConfirm is grantChat plus the approval grant the confirm paths check.
func grantConfirm() bridge.Grant {
	return bridge.Grant{Workspace: wsDefault, AllowConfirm: true}
}

// fixture bundles a core with its fakes for one test.
type fixture struct {
	core    *bridge.Core
	chat    *bridgetest.Chat
	store   *bridgetest.Store
	metrics *bridgetest.Metrics
	out     *bridgetest.Out
	logs    *syncBuffer
	grant   bridge.Grant
}

// newFixture builds a core wired to fresh fakes with a long confirm TTL, so a
// proposal stays live for the whole test.
func newFixture(t *testing.T, grant bridge.Grant) *fixture {
	t.Helper()

	return newTunedFixture(t, grant, time.Minute, false)
}

// newTunedFixture is newFixture with a caller-chosen confirm TTL and verbosity.
// The expiry case arms a short timer; the tool-call case turns verbose on.
func newTunedFixture(t *testing.T, grant bridge.Grant, confirmTTL time.Duration, verbose bool) *fixture {
	t.Helper()

	agent := bridgetest.NewChat()
	bindings := bridgetest.NewStore()
	metrics := bridgetest.NewMetrics()
	out := bridgetest.NewOut(testMaxChars)
	logs := &syncBuffer{}

	core := bridge.NewCore(&bridge.CoreConfig{
		Chat:       agent,
		Store:      bindings,
		Metrics:    metrics,
		Logger:     slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		ConfirmTTL: confirmTTL,
		Verbose:    verbose,
	})

	// Draining on cleanup stops every armed proposal timer and waits for the
	// conversation workers, so nothing fires into the fakes after the test.
	t.Cleanup(core.Drain)

	return &fixture{
		core:    core,
		chat:    agent,
		store:   bindings,
		metrics: metrics,
		out:     out,
		logs:    logs,
		grant:   grant,
	}
}

// message builds a text inbound from the test operator.
func (f *fixture) message(text string) *bridge.Inbound {
	return f.messageFrom(testSenderID, text)
}

// messageFrom builds a text inbound from a named sender, so a test can prove a
// decision from someone other than the opener is refused.
func (f *fixture) messageFrom(sender, text string) *bridge.Inbound {
	return &bridge.Inbound{
		Addr: bridge.Address{
			Transport: testTransport,
			Identity:  testIdentity,
			ChatKey:   testChatKey,
			Channel:   testChannel,
		},
		Sender: bridge.Sender{ID: sender},
		Grant:  f.grant,
		Text:   text,
		Out:    f.out,
		Kind:   bridge.KindMessage,
	}
}

// decision builds an inbound the transport already resolved into an approve or a
// deny, the shape a Telegram keyboard tap arrives as.
func (f *fixture) decision(sender, sessionID string, approve bool) *bridge.Inbound {
	item := f.messageFrom(sender, "")
	item.Kind = bridge.KindDecision
	item.Session = sessionID
	item.Approve = approve

	return item
}

// handle routes each inbound in order. One conversation's worker runs them FIFO,
// so a later inbound always observes what an earlier one settled.
func (f *fixture) handle(t *testing.T, inbounds ...*bridge.Inbound) {
	t.Helper()

	for _, item := range inbounds {
		f.core.Handle(t.Context(), item)
	}
}

// updateCount reads one update outcome count under the test transport's labels.
func (f *fixture) updateCount(outcome string) int {
	return f.metrics.UpdateCount(testTransport, testIdentity, outcome)
}

// confirmCount reads one confirm outcome count under the test transport's labels.
func (f *fixture) confirmCount(outcome string) int {
	return f.metrics.ConfirmCount(testTransport, testIdentity, outcome)
}
