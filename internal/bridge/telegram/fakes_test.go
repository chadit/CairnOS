package telegram_test

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
	"github.com/chadit/CairnOS/internal/bridge/bridgetest"
	"github.com/chadit/CairnOS/internal/bridge/telegram"
)

const (
	// testToken carries a colon and dashes the way a real bot token does, so the
	// redaction assertions exercise the substring the Bot API embeds in URLs.
	testToken = "111222333:AAF-secretTOKENvalue"

	// wantRedaction is the marker the client swaps the token for. It mirrors the
	// adapter's unexported redactedToken, pinned here as the observable contract.
	wantRedaction = "[redacted]"

	// pollHoldEmpty throttles the fake's empty long-poll replies so the poll loop
	// does not spin against an exhausted script.
	pollHoldEmpty = 20 * time.Millisecond

	// testUserID and testChatID identify the single allowlisted operator these
	// tests drive; a private Telegram chat keys on the same id.
	testUserID = 1
	testChatID = 100

	// otherUserID is a second allowlisted operator, used to prove a non-opener tap
	// is refused.
	otherUserID = 2

	// wantMessageID is the sent message id the fake Bot API returns, echoed by the
	// round-trip client test.
	wantMessageID int64 = 555

	// wsDefault and wsAcme are the workspace ids the tests stamp turns with.
	wsDefault = "default"
	wsAcme    = "acme"

	// msgPropose makes the fake agent raise a D9 proposal, msgGo drives a plain
	// turn.
	msgPropose = "propose"
	msgGo      = "go"

	// actionApprove and actionDeny are the callback_data verbs a proposal tap
	// carries, the wire contract the adapter parses.
	actionApprove = "approve"
	actionDeny    = "deny"

	// transportName is the label every counter this adapter records carries, and
	// the store binding's transport column value.
	transportName = "telegram"

	// identityName is the empty identity label Telegram records, since it answers
	// as a single bot per serve.
	identityName = ""

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
	msgExpired      = "This proposal expired; nothing was run."

	// maxMessageChars mirrors the Bot API per-message cap the core splits on.
	maxMessageChars = 4096
)

// Sentinel errors the fakes return; err113 is disabled for test files.
var (
	errGetUpdates  = errors.New("fake get updates failed")
	errSendMessage = errors.New("fake send message failed")
)

// confirmData rebuilds a proposal button's callback_data, the wire contract the
// adapter parses a tap from: "confirm:<sessionID>:<action>". The tests build taps
// through it rather than reaching for the adapter's unexported constructor.
func confirmData(sessionID, action string) string {
	return "confirm:" + sessionID + ":" + action
}

// sentMessage records one outbound Bot API send the fake received.
type sentMessage struct {
	keyboard    *telegram.InlineKeyboardMarkup
	text        string
	chatID      int64
	hasKeyboard bool
}

// editedMessage records one EditMessageText the fake received, so the expiry
// tests can assert the proposal message was rewritten in place.
type editedMessage struct {
	text      string
	chatID    int64
	messageID int64
}

// fakeBotAPI is an in-memory telegram.BotAPI: it serves scripted getUpdates
// batches (honoring the acked offset), records outbound sends, edits, and
// answered callbacks, and can force leading getUpdates failures or a failing
// SendMessage. A test injects it through telegram.Config.API, which is the seam
// that lets these tests run black-box with no white-box client access.
type fakeBotAPI struct {
	batches  [][]telegram.Update
	offsets  []int64
	sent     []sentMessage
	edited   []editedMessage
	answered []string

	batchIndex  int
	getFailures int
	mu          sync.Mutex
	sendFail    bool
}

// newFakeBotAPI builds an empty recorder with no scripted updates.
func newFakeBotAPI() *fakeBotAPI {
	return &fakeBotAPI{}
}

// GetUpdates records the acked offset, forces any scheduled failure, and serves
// the next scripted batch whose updates sit at or beyond the offset. An empty
// poll holds briefly (or until ctx ends) so the poll loop does not spin.
func (f *fakeBotAPI) GetUpdates(ctx context.Context, offset int64, _ int) ([]telegram.Update, error) {
	f.mu.Lock()
	f.offsets = append(f.offsets, offset)

	if f.getFailures > 0 {
		f.getFailures--
		f.mu.Unlock()

		return nil, errGetUpdates
	}

	var batch []telegram.Update

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
func (f *fakeBotAPI) SendMessage(
	_ context.Context,
	chatID int64,
	text string,
	keyboard *telegram.InlineKeyboardMarkup,
) (int64, error) {
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

// seenOffsets returns a copy of the offsets the poll loop used, in order.
func (f *fakeBotAPI) seenOffsets() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]int64(nil), f.offsets...)
}

// script sets the getUpdates batches under the lock so the poll-loop goroutine
// reads them without a data race.
func (f *fakeBotAPI) script(batches ...[]telegram.Update) {
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

// syncBuffer is a concurrency-safe log sink so the redaction assertions can read
// what the poll loop and workers wrote from several goroutines.
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

// fixture bundles an adapter, the core behind it, and their fakes for one test.
type fixture struct {
	br      *telegram.Bridge
	tg      *fakeBotAPI
	chat    *bridgetest.Chat
	store   *bridgetest.Store
	metrics *bridgetest.Metrics
	logs    *syncBuffer
}

// newFixture builds an adapter over a fresh core with a long confirm TTL, its Bot
// API surface injected as a fake, and its backoff shortened so retry tests stay
// fast.
func newFixture(t *testing.T, users map[int64]bridge.Grant) *fixture {
	t.Helper()

	return newTunedFixture(t, users, time.Minute)
}

// newTunedFixture is newFixture with a caller-chosen confirm TTL. The expiry case
// arms a short timer; the others keep a proposal live for the whole run.
func newTunedFixture(t *testing.T, users map[int64]bridge.Grant, confirmTTL time.Duration) *fixture {
	t.Helper()

	bot := newFakeBotAPI()
	agent := bridgetest.NewChat()
	bindings := bridgetest.NewStore()
	metrics := bridgetest.NewMetrics()
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	built := telegram.New(&telegram.Config{
		Core: bridge.NewCore(&bridge.CoreConfig{
			Chat:       agent,
			Store:      bindings,
			Metrics:    metrics,
			Logger:     logger,
			ConfirmTTL: confirmTTL,
		}),
		Metrics:     metrics,
		API:         bot,
		Users:       users,
		Logger:      logger,
		Token:       testToken,
		PollTimeout: time.Second,
		SendTimeout: time.Second,
		BackoffBase: time.Millisecond,
	})

	return &fixture{br: built, tg: bot, chat: agent, store: bindings, metrics: metrics, logs: logs}
}

// updateCount reads one update outcome count under the adapter's labels.
func (f *fixture) updateCount(outcome string) int {
	return f.metrics.UpdateCount(transportName, identityName, outcome)
}

// confirmCount reads one confirm outcome count under the adapter's labels.
func (f *fixture) confirmCount(outcome string) int {
	return f.metrics.ConfirmCount(transportName, identityName, outcome)
}

// runBridge starts the poll loop and registers a stop that cancels and waits for
// Run to return, so a proposal timer never fires into the fakes after the test
// and the poll goroutine never outlives it.
func runBridge(t *testing.T, built *telegram.Bridge) func() {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		built.Run(ctx)
		close(done)
	}()

	var once sync.Once

	stop := func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}

	t.Cleanup(stop)

	return stop
}

// messageUpdate builds a text-message update; UpdateID defaults to the chat id
// and callers or scriptSingles override it when a test needs explicit ids.
func messageUpdate(userID, chatID int64, text string) *telegram.Update {
	return &telegram.Update{
		UpdateID: chatID,
		Message: &telegram.Message{
			MessageID: 1,
			From:      &telegram.User{ID: userID},
			Chat:      telegram.Chat{ID: chatID},
			Text:      text,
		},
	}
}

// callbackUpdate builds an inline-keyboard tap update.
func callbackUpdate(updateID, userID, chatID int64, data string) *telegram.Update {
	return &telegram.Update{
		UpdateID: updateID,
		CallbackQuery: &telegram.CallbackQuery{
			ID:      "cb-" + strconv.FormatInt(updateID, 10),
			From:    &telegram.User{ID: userID},
			Message: &telegram.Message{Chat: telegram.Chat{ID: chatID}},
			Data:    data,
		},
	}
}

// scriptSingles scripts each update as its own batch, numbering update ids in
// order so the poll loop's offset acking serves them one after another and a
// batch is never replayed.
func scriptSingles(bot *fakeBotAPI, updates ...*telegram.Update) {
	batches := make([][]telegram.Update, len(updates))

	for index, update := range updates {
		update.UpdateID = int64(index + 1)
		batches[index] = []telegram.Update{*update}
	}

	bot.script(batches...)
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
