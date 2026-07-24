package bridge_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/bridge"
	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/store"
)

const (
	// testToken carries a colon and dashes the way a real bot token does, so the
	// redaction assertions exercise the substring the Bot API embeds in URLs.
	testToken = "111222333:AAF-secretTOKENvalue"

	// wantRedaction is the marker the client swaps the token for. It mirrors the
	// bridge's unexported redactedToken, pinned here as the observable contract.
	wantRedaction = "[redacted]"

	// replyText is the canned assistant reply the fake agent streams.
	replyText = "acknowledged"

	// pollHoldEmpty throttles the fake's empty long-poll replies so the bridge's
	// poll loop does not spin against an exhausted script.
	pollHoldEmpty = 20 * time.Millisecond

	// testUserID and testChatID identify the single allowlisted operator the
	// bridge tests drive; a private Telegram chat keys on the same id.
	testUserID = 1
	testChatID = 100

	// testSessionID is the session the fake agent mints first, so a proposal
	// raised on the opening turn routes under this id.
	testSessionID = "sess-1"

	// wantMessageID is the sent message id the fake Bot API returns, echoed by the
	// round-trip client test.
	wantMessageID int64 = 555

	// confirmRunID and confirmRunStatus are the run the fake agent reports when a
	// proposal is approved, so the confirm tests can assert the relayed lines.
	confirmRunID     = "run-1"
	confirmRunStatus = "succeeded"

	// denialText is the assistant line the fake agent streams on a deny, mirroring
	// chat.Agent's own denial message.
	denialText = "Workflow trigger denied; nothing was run."

	// wsDefault and wsAcme are the workspace ids the tests stamp turns with.
	wsDefault = "default"
	wsAcme    = "acme"

	// workflowDeploy is the proposed workflow name the confirm tests render.
	workflowDeploy = "deploy"

	// msgPropose and msgGo are canned inbound texts: msgPropose makes the fake
	// agent raise a D9 proposal, msgGo drives a plain turn.
	msgPropose = "propose"
	msgGo      = "go"

	// actionApprove and actionDeny are the callback_data verbs a proposal tap
	// carries, the wire contract the bridge parses.
	actionApprove = "approve"
	actionDeny    = "deny"

	// Metric outcome labels the bridge passes to Metrics. They stay unexported in
	// the bridge, so the tests pin the strings here as the counter contract.
	outcomeUnauthorized = "unauthorized"
	confirmApproved     = "approved"
	confirmDenied       = "denied"
	confirmExpired      = "expired"
	confirmRefused      = "refused"

	// Operator-facing lines the confirm flow sends, asserted as observable text.
	msgNoGrant      = "Approval isn't granted for this user."
	msgNotOpener    = "Only the operator who started this proposal can answer it."
	msgStaleExpired = "This proposal expired."
	msgExpired      = "This proposal expired; nothing was run."

	// maxMessageChars mirrors the Bot API per-message cap the bridge splits on.
	maxMessageChars = 4096
)

// Sentinel errors the fakes return; err113 is disabled for test files.
var (
	errTurnFailed  = errors.New("fake turn failed")
	errNoSession   = errors.New("fake session not found")
	errGetUpdates  = errors.New("fake get updates failed")
	errSendMessage = errors.New("fake send message failed")
)

// confirmData rebuilds a proposal button's callback_data, the wire contract the
// bridge parses a tap from: "confirm:<sessionID>:<action>". The tests build taps
// through it rather than reaching for the bridge's unexported constructor.
func confirmData(sessionID, action string) string {
	return "confirm:" + sessionID + ":" + action
}

// sentMessage records one outbound Bot API send the fake received.
type sentMessage struct {
	chatID      int64
	text        string
	hasKeyboard bool
	keyboard    *bridge.InlineKeyboardMarkup
}

// editedMessage records one EditMessageText the fake received, so the expiry
// tests can assert the proposal message was rewritten in place.
type editedMessage struct {
	chatID    int64
	messageID int64
	text      string
}

// fakeBotAPI is an in-memory bridge.BotAPI: it serves scripted getUpdates
// batches (honoring the acked offset), records outbound sends, edits, and
// answered callbacks, and can force leading getUpdates failures or a failing
// SendMessage. A bridge test injects it through bridge.Config.API, which is the
// seam that lets these tests run black-box with no white-box client access.
type fakeBotAPI struct {
	mu sync.Mutex

	batches    [][]bridge.Update
	batchIndex int

	getFailures int
	sendFail    bool

	offsets  []int64
	sent     []sentMessage
	edited   []editedMessage
	answered []string
}

// newFakeBotAPI builds an empty recorder with no scripted updates.
func newFakeBotAPI() *fakeBotAPI {
	return &fakeBotAPI{}
}

// GetUpdates records the acked offset, forces any scheduled failure, and serves
// the next scripted batch whose updates sit at or beyond the offset. An empty
// poll holds briefly (or until ctx ends) so the bridge loop does not spin.
func (f *fakeBotAPI) GetUpdates(ctx context.Context, offset int64, _ int) ([]bridge.Update, error) {
	f.mu.Lock()
	f.offsets = append(f.offsets, offset)

	if f.getFailures > 0 {
		f.getFailures--
		f.mu.Unlock()

		return nil, errGetUpdates
	}

	var batch []bridge.Update

	if f.batchIndex < len(f.batches) && f.batches[f.batchIndex][0].UpdateID >= offset {
		batch = f.batches[f.batchIndex]
		f.batchIndex++
	}

	f.mu.Unlock()

	if len(batch) == 0 {
		select {
		case <-time.After(pollHoldEmpty):
		case <-ctx.Done():
			return nil, fmt.Errorf("fake get updates canceled: %w", ctx.Err())
		}
	}

	return batch, nil
}

// SendMessage records the outbound message and returns the fake message id,
// failing when so configured.
func (f *fakeBotAPI) SendMessage(_ context.Context, chatID int64, text string, keyboard *bridge.InlineKeyboardMarkup) (int64, error) {
	f.mu.Lock()
	f.sent = append(f.sent, sentMessage{
		chatID:      chatID,
		text:        text,
		hasKeyboard: keyboard != nil,
		keyboard:    keyboard,
	})
	fail := f.sendFail
	f.mu.Unlock()

	if fail {
		return 0, errSendMessage
	}

	return wantMessageID, nil
}

// EditMessageText records one edit-in-place.
func (f *fakeBotAPI) EditMessageText(_ context.Context, chatID, messageID int64, text string) error {
	f.mu.Lock()
	f.edited = append(f.edited, editedMessage{chatID: chatID, messageID: messageID, text: text})
	f.mu.Unlock()

	return nil
}

// AnswerCallbackQuery records one answered callback query.
func (f *fakeBotAPI) AnswerCallbackQuery(_ context.Context, callbackID, _ string) error {
	f.mu.Lock()
	f.answered = append(f.answered, callbackID)
	f.mu.Unlock()

	return nil
}

// sentMessages returns a copy of the recorded outbound sends.
func (f *fakeBotAPI) sentMessages() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]sentMessage(nil), f.sent...)
}

// answeredCallbacks returns a copy of the answered callback query ids.
func (f *fakeBotAPI) answeredCallbacks() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.answered...)
}

// editedMessages returns a copy of the recorded EditMessageText calls.
func (f *fakeBotAPI) editedMessages() []editedMessage {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]editedMessage(nil), f.edited...)
}

// seenOffsets returns a copy of the offsets the bridge polled with, in order.
func (f *fakeBotAPI) seenOffsets() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]int64(nil), f.offsets...)
}

// script sets the getUpdates batches under the lock so the poll-loop goroutine
// reads them without a data race.
func (f *fakeBotAPI) script(batches ...[]bridge.Update) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.batches = batches
}

// failGetUpdates makes the next count getUpdates calls fail.
func (f *fakeBotAPI) failGetUpdates(count int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.getFailures = count
}

// failSends makes every SendMessage fail.
func (f *fakeBotAPI) failSends() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.sendFail = true
}

// sentTurn records one SendMessage the fake agent handled.
type sentTurn struct {
	sessionID string
	message   string
}

// confirmCall records one ConfirmAction the fake agent handled: which session and
// whether it was an approve, so the confirm tests can assert the exact calls.
type confirmCall struct {
	sessionID string
	approve   bool
}

// fakeChat is a bridge.ChatService stand-in: it mints sessions, records turns and
// the workspace each session was created under, can gate one message on a release
// channel to prove per-chat serialization, can fail a named message, and can
// raise a D9 proposal on a named message so the confirm flow runs end to end.
type fakeChat struct {
	mu sync.Mutex

	sessions map[string]*store.ChatSession
	seq      int
	turns    []sentTurn
	created  []string

	// pending holds session ids with an armed proposal. ConfirmAction clears one
	// under the lock, so two concurrent confirmations arbitrate exactly as
	// chat.Agent's mutex does: the loser reads CodeNoPendingAction.
	pending  map[string]bool
	confirms []confirmCall

	gateOn  string
	release chan struct{}
	failOn  string
	reply   string

	// proposeOn names the inbound text that makes a turn raise a proposal; armGate
	// says whether raising it also arms the pending gate. A raised proposal with an
	// unarmed gate models a proposal whose D9 gate a concurrent path already
	// cleared, so ConfirmAction reports CodeNoPendingAction.
	proposeOn string
	armGate   bool
}

// newFakeChat builds a fake agent with the canned reply and an armed gate.
func newFakeChat() *fakeChat {
	return &fakeChat{
		sessions: map[string]*store.ChatSession{},
		pending:  map[string]bool{},
		release:  make(chan struct{}),
		reply:    replyText,
		armGate:  true,
	}
}

// CreateSession mints a session stamped with the request workspace.
func (f *fakeChat) CreateSession(ctx context.Context, personality string) (*store.ChatSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.seq++
	workspace := store.WorkspaceFromContext(ctx)
	session := &store.ChatSession{
		ID:          "sess-" + strconv.Itoa(f.seq),
		WorkspaceID: workspace,
		Personality: personality,
	}
	f.sessions[session.ID] = session
	f.created = append(f.created, workspace)

	return session, nil
}

// GetSession returns a minted session; the bridge does not call it in this
// slice, but ChatService requires it.
func (f *fakeChat) GetSession(_ context.Context, id string) (*store.ChatSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	session, ok := f.sessions[id]
	if !ok {
		return nil, errNoSession
	}

	return session, nil
}

// SendMessage records the turn, optionally blocking a gated message until
// release, then streams the canned reply or raises a proposal.
func (f *fakeChat) SendMessage(ctx context.Context, sessionID, userMessage string, out chan<- chat.Event) error {
	defer close(out)

	f.recordTurn(sessionID, userMessage)

	if userMessage == f.gateOn {
		select {
		case <-f.release:
		case <-ctx.Done():
			return fmt.Errorf("fake chat canceled: %w", ctx.Err())
		}
	}

	if userMessage == f.failOn {
		return errTurnFailed
	}

	if f.proposeOn != "" && userMessage == f.proposeOn {
		if f.armGate {
			f.arm(sessionID)
		}

		out <- chat.Event{Type: chat.EventConfirmationRequired, Proposal: testProposal()}

		out <- chat.Event{Type: chat.EventTurnDone}

		return nil
	}

	out <- chat.Event{Type: chat.EventAssistantChunk, Text: f.reply}

	out <- chat.Event{Type: chat.EventTurnDone, Text: f.reply}

	return nil
}

// ConfirmAction models the D9 gate: it records the call, clears the pending flag
// under the lock, and either streams run events (approve) or a denial line (deny).
// An unarmed session reports CodeNoPendingAction, so a concurrent tap and expiry
// race exactly as chat.Agent does and the loser gets that code.
func (f *fakeChat) ConfirmAction(_ context.Context, sessionID string, approve bool, out chan<- chat.Event) error {
	defer close(out)

	f.mu.Lock()
	armed := f.pending[sessionID]
	delete(f.pending, sessionID)
	f.confirms = append(f.confirms, confirmCall{sessionID: sessionID, approve: approve})
	f.mu.Unlock()

	if !armed {
		return &cairnerr.Error{Code: chat.CodeNoPendingAction, Message: "no pending action"}
	}

	if approve {
		out <- chat.Event{Type: chat.EventRunStarted, RunID: confirmRunID}

		out <- chat.Event{Type: chat.EventRunFinished, RunID: confirmRunID, RunStatus: confirmRunStatus}

		out <- chat.Event{Type: chat.EventTurnDone}

		return nil
	}

	out <- chat.Event{Type: chat.EventTurnDone, Text: denialText}

	return nil
}

// arm marks a session as having a proposal awaiting confirmation, so the next
// ConfirmAction for it resolves rather than reporting no pending action.
func (f *fakeChat) arm(sessionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.pending[sessionID] = true
}

// confirmCalls returns a copy of the recorded ConfirmAction calls in order.
func (f *fakeChat) confirmCalls() []confirmCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]confirmCall(nil), f.confirms...)
}

// recordTurn appends one handled turn under the lock.
func (f *fakeChat) recordTurn(sessionID, userMessage string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.turns = append(f.turns, sentTurn{sessionID: sessionID, message: userMessage})
}

// sentTurns returns a copy of the handled turns in order.
func (f *fakeChat) sentTurns() []sentTurn {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]sentTurn(nil), f.turns...)
}

// createdWorkspaces returns the workspaces sessions were created under.
func (f *fakeChat) createdWorkspaces() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.created...)
}

// testProposal builds the proposal a raised turn emits and the confirm tests
// render.
func testProposal() *chat.Proposal {
	return &chat.Proposal{
		WorkflowName:       workflowDeploy,
		Inputs:             map[string]any{"env": "prod"},
		EstimatedBudgetUSD: 1.5,
	}
}

// fakeStore is an in-memory store.BridgeSessionStore keyed by workspace so the
// workspace-stamping and cross-restart assertions can read what the bridge
// bound and looked up.
type fakeStore struct {
	mu sync.Mutex

	bindings map[string]string
	bound    []string
	looked   []string
}

// newFakeStore builds an empty binding store.
func newFakeStore() *fakeStore {
	return &fakeStore{bindings: map[string]string{}}
}

// bindingKey namespaces a binding by workspace so tenants stay isolated.
func bindingKey(workspace, transport, chatKey string) string {
	return workspace + "|" + transport + "|" + chatKey
}

// BindBridgeSession upserts the workspace-scoped binding.
func (s *fakeStore) BindBridgeSession(ctx context.Context, transport, chatKey, sessionID string) error {
	workspace := store.WorkspaceFromContext(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.bindings[bindingKey(workspace, transport, chatKey)] = sessionID
	s.bound = append(s.bound, workspace)

	return nil
}

// LookupBridgeSession reads the workspace-scoped binding or reports the
// not-found sentinel.
func (s *fakeStore) LookupBridgeSession(ctx context.Context, transport, chatKey string) (string, error) {
	workspace := store.WorkspaceFromContext(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.looked = append(s.looked, workspace)

	sessionID, ok := s.bindings[bindingKey(workspace, transport, chatKey)]
	if !ok {
		return "", store.ErrBridgeSessionNotFound
	}

	return sessionID, nil
}

// boundWorkspaces returns the workspaces the bridge bound under.
func (s *fakeStore) boundWorkspaces() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.bound...)
}

// fakeMetrics counts the four bridge counters under a lock.
type fakeMetrics struct {
	mu sync.Mutex

	updates      map[string]int
	turnsOK      int
	turnsError   int
	confirms     map[string]int
	sendFailures int
}

// newFakeMetrics builds a zeroed metrics recorder.
func newFakeMetrics() *fakeMetrics {
	return &fakeMetrics{updates: map[string]int{}, confirms: map[string]int{}}
}

// Update counts one poll-loop update outcome.
func (m *fakeMetrics) Update(outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.updates[outcome]++
}

// Turn counts one delivered turn.
func (m *fakeMetrics) Turn(ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if ok {
		m.turnsOK++

		return
	}

	m.turnsError++
}

// Confirm counts one resolved confirmation; the confirm slice drives it.
func (m *fakeMetrics) Confirm(outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.confirms[outcome]++
}

// SendFailure counts one failed outbound send.
func (m *fakeMetrics) SendFailure() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sendFailures++
}

// updateCount reads one update outcome count.
func (m *fakeMetrics) updateCount(outcome string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.updates[outcome]
}

// turnCounts reads the ok and error turn tallies.
func (m *fakeMetrics) turnCounts() (int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.turnsOK, m.turnsError
}

// sendFailureCount reads the send-failure tally.
func (m *fakeMetrics) sendFailureCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.sendFailures
}

// confirmCount reads one confirm outcome count.
func (m *fakeMetrics) confirmCount(outcome string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.confirms[outcome]
}

// syncBuffer is a concurrency-safe log sink so the redaction assertions can read
// what the poll loop and workers wrote from several goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
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

// fixture bundles a bridge with its fakes for one test.
type fixture struct {
	br      *bridge.Bridge
	tg      *fakeBotAPI
	chat    *fakeChat
	store   *fakeStore
	metrics *fakeMetrics
	logs    *syncBuffer
}

// newFixture builds a bridge wired to fresh fakes with a long confirm TTL, its
// Bot API surface injected as a fake, and its backoff shortened so retry tests
// stay fast.
func newFixture(t *testing.T, users map[int64]bridge.AllowedUser) *fixture {
	t.Helper()

	return newTunedFixture(t, users, time.Minute)
}

// newTunedFixture is newFixture with a caller-chosen confirm TTL. The expiry
// case arms a short timer; the others keep a proposal live for the whole run.
func newTunedFixture(t *testing.T, users map[int64]bridge.AllowedUser, confirmTTL time.Duration) *fixture {
	t.Helper()

	bot := newFakeBotAPI()
	agent := newFakeChat()
	bindings := newFakeStore()
	metrics := newFakeMetrics()
	logs := &syncBuffer{}

	built := bridge.New(&bridge.Config{
		Chat:        agent,
		Store:       bindings,
		Metrics:     metrics,
		API:         bot,
		Users:       users,
		Token:       testToken,
		PollTimeout: time.Second,
		SendTimeout: time.Second,
		BackoffBase: time.Millisecond,
		ConfirmTTL:  confirmTTL,
		Logger:      slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	return &fixture{br: built, tg: bot, chat: agent, store: bindings, metrics: metrics, logs: logs}
}

// runBridge starts the bridge's poll loop and registers a stop that cancels and
// waits for Run to return, so a proposal timer never fires into the fakes after
// the test and the poll goroutine never outlives it.
func runBridge(t *testing.T, built *bridge.Bridge) func() {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		built.Run(ctx)
		close(done)
	}()

	stop := func() {
		cancel()
		<-done
	}

	t.Cleanup(stop)

	return stop
}

// messageUpdate builds a text-message update; UpdateID defaults to the chat id
// and callers or scriptSingles override it when a test needs explicit ids.
func messageUpdate(userID, chatID int64, text string) *bridge.Update {
	return &bridge.Update{
		UpdateID: chatID,
		Message: &bridge.Message{
			MessageID: 1,
			From:      &bridge.User{ID: userID},
			Chat:      bridge.Chat{ID: chatID},
			Text:      text,
		},
	}
}

// callbackUpdate builds an inline-keyboard tap update.
func callbackUpdate(updateID, userID, chatID int64, data string) *bridge.Update {
	return &bridge.Update{
		UpdateID: updateID,
		CallbackQuery: &bridge.CallbackQuery{
			ID:      "cb-" + strconv.FormatInt(updateID, 10),
			From:    &bridge.User{ID: userID},
			Message: &bridge.Message{Chat: bridge.Chat{ID: chatID}},
			Data:    data,
		},
	}
}

// scriptSingles scripts each update as its own batch, numbering update ids in
// order so the poll loop's offset acking serves them one after another and a
// batch is never replayed.
func scriptSingles(bot *fakeBotAPI, updates ...*bridge.Update) {
	batches := make([][]bridge.Update, len(updates))

	for index, update := range updates {
		update.UpdateID = int64(index + 1)
		batches[index] = []bridge.Update{*update}
	}

	bot.script(batches...)
}

// waitFor polls condition until it holds or a short deadline passes. The bridge
// drives real goroutines over a fake Bot API, so the wait ticks off a real timer
// rather than synthetic time.
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()

	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		if condition() {
			return
		}

		select {
		case <-timeout.C:
			t.Fatal("condition not met before deadline")
		case <-ticker.C:
		}
	}
}

// sentContains reports whether any recorded send carried substr in its text.
func sentContains(sent []sentMessage, substr string) bool {
	for _, message := range sent {
		if strings.Contains(message.text, substr) {
			return true
		}
	}

	return false
}

// containsTurn reports whether any recorded turn carried message.
func containsTurn(turns []sentTurn, message string) bool {
	for _, turn := range turns {
		if turn.message == message {
			return true
		}
	}

	return false
}

// turnIndex returns the position of the first turn carrying message, or -1.
func turnIndex(turns []sentTurn, message string) int {
	for index, turn := range turns {
		if turn.message == message {
			return index
		}
	}

	return -1
}
