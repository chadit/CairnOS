package telegram_test

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/bridge"
	"github.com/chadit/CairnOS/internal/bridge/bridgetest"
	"github.com/chadit/CairnOS/internal/bridge/telegram"
	"github.com/chadit/CairnOS/internal/nettest"
)

// TestBridgeMultiTurnPersistsAcrossRestart proves an allowlisted chat holds a
// multi-turn conversation on one session and a fresh adapter over the same store
// re-resolves that binding, because it lives in bridge_sessions, not memory.
func TestBridgeMultiTurnPersistsAcrossRestart(t *testing.T) {
	t.Parallel()

	users := map[int64]bridge.Grant{1: {Workspace: wsDefault}}
	rig := newFixture(t, users)

	scriptSingles(
		rig.tg,
		messageUpdate(1, testChatID, "hello"),
		messageUpdate(1, testChatID, "again"),
	)

	stop := runBridge(t, rig.br)

	bridgetest.WaitFor(t, func() bool { return len(rig.chat.Turns()) == 2 })

	stop()

	turns := rig.chat.Turns()
	if turns[0].SessionID != turns[1].SessionID {
		t.Fatalf("turns used different sessions: %q vs %q", turns[0].SessionID, turns[1].SessionID)
	}

	// A fresh adapter over the same store must re-resolve the binding from the
	// store rather than any in-memory state the first core held.
	restartBot := newFakeBotAPI()
	scriptSingles(restartBot, messageUpdate(1, testChatID, "after restart"))

	restarted := telegram.New(&telegram.Config{
		Core: bridge.NewCore(&bridge.CoreConfig{
			Chat:       rig.chat,
			Store:      rig.store,
			Metrics:    rig.metrics,
			ConfirmTTL: time.Minute,
		}),
		Metrics:     rig.metrics,
		API:         restartBot,
		Users:       users,
		Token:       testToken,
		PollTimeout: time.Second,
		SendTimeout: time.Second,
		BackoffBase: time.Millisecond,
	})

	stopRestart := runBridge(t, restarted)

	bridgetest.WaitFor(t, func() bool { return len(rig.chat.Turns()) == 3 })

	stopRestart()

	turns = rig.chat.Turns()
	if turns[2].SessionID != turns[0].SessionID {
		t.Errorf("post-restart turn used session %q, want %q", turns[2].SessionID, turns[0].SessionID)
	}
}

// TestBridgeUnauthorizedSenderGetsNoReply proves a non-allowlisted sender gets
// no reply and no turn, and increments updates_total{unauthorized}
// (acceptance 4).
func TestBridgeUnauthorizedSenderGetsNoReply(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, map[int64]bridge.Grant{1: {Workspace: wsDefault}})

	scriptSingles(rig.tg, messageUpdate(999, testChatID, msgGo))

	runBridge(t, rig.br)

	bridgetest.WaitFor(t, func() bool { return rig.updateCount(bridge.OutcomeUnauthorized) == 1 })

	if sent := rig.tg.sentMessages(); len(sent) != 0 {
		t.Errorf("unauthorized sender got a reply: %+v", sent)
	}

	if turns := rig.chat.Turns(); len(turns) != 0 {
		t.Errorf("unauthorized sender ran a turn: %+v", turns)
	}
}

// TestBridgeNewRebindsAndSessionReplies proves /new starts a fresh session and
// /session answers the current session id.
func TestBridgeNewRebindsAndSessionReplies(t *testing.T) {
	t.Parallel()

	users := map[int64]bridge.Grant{1: {Workspace: wsDefault}}
	rig := newFixture(t, users)

	scriptSingles(
		rig.tg,
		messageUpdate(1, testChatID, "hello"),
		messageUpdate(1, testChatID, "/new"),
		messageUpdate(1, testChatID, "/session"),
	)

	runBridge(t, rig.br)

	bridgetest.WaitFor(t, func() bool { return len(rig.tg.sentMessages()) == 3 })

	sent := rig.tg.sentMessages()
	first := rig.chat.Turns()[0].SessionID

	newReply := sent[1].text
	if !strings.HasPrefix(newReply, "Started a new session: ") {
		t.Fatalf("/new reply = %q", newReply)
	}

	rebound := strings.TrimPrefix(newReply, "Started a new session: ")
	if rebound == first {
		t.Errorf("/new did not rebind: still %q", first)
	}

	if want := "Session: " + rebound; sent[2].text != want {
		t.Errorf("/session reply = %q, want %q", sent[2].text, want)
	}
}

// TestBridgeLongReplySplitsOnParagraphs proves a turn over the 4096-character cap
// the adapter reports splits on the paragraph boundary into ordered messages.
func TestBridgeLongReplySplitsOnParagraphs(t *testing.T) {
	t.Parallel()

	users := map[int64]bridge.Grant{1: {Workspace: wsDefault}}
	rig := newFixture(t, users)

	first := strings.Repeat("a", 3000)
	second := strings.Repeat("b", 3000)
	rig.chat.SetReply(first + "\n\n" + second)

	scriptSingles(rig.tg, messageUpdate(1, testChatID, msgGo))

	runBridge(t, rig.br)

	bridgetest.WaitFor(t, func() bool { return len(rig.tg.sentMessages()) == 2 })

	sent := rig.tg.sentMessages()
	if sent[0].text != first || sent[1].text != second {
		t.Errorf("split boundaries wrong: %d / %d", len(sent[0].text), len(sent[1].text))
	}

	for _, part := range sent {
		if len(part.text) > maxMessageChars {
			t.Errorf("part exceeds cap: %d", len(part.text))
		}
	}
}

// TestBridgeFailedSendCountsOnceNoRerun proves a failed delivery counts on
// send_failures_total and does not re-run the turn, which already persisted.
func TestBridgeFailedSendCountsOnceNoRerun(t *testing.T) {
	t.Parallel()

	users := map[int64]bridge.Grant{1: {Workspace: wsDefault}}
	rig := newFixture(t, users)
	rig.tg.failSends()

	scriptSingles(rig.tg, messageUpdate(1, testChatID, msgGo))

	runBridge(t, rig.br)

	bridgetest.WaitFor(t, func() bool { return len(rig.metrics.SendFailures()) == 1 })

	if turns := rig.chat.Turns(); len(turns) != 1 {
		t.Errorf("turn ran %d times, want 1 (no re-run)", len(turns))
	}

	turnsOK, turnsErr := rig.metrics.TurnCounts()
	if turnsOK != 1 || turnsErr != 0 {
		t.Errorf("turn counts = ok:%d err:%d, want ok:1 err:0", turnsOK, turnsErr)
	}

	// The failure carries the adapter's own labels, so a dashboard attributes a
	// missed reply to Telegram rather than to an unlabeled series (FR42).
	labels := rig.metrics.SendFailures()
	if labels[0].Transport != transportName || labels[0].Identity != identityName {
		t.Errorf("send failure labels = %+v, want transport %q", labels[0], transportName)
	}
}

// TestBridgeWorkspaceStampedOnEveryTurn proves the bridge stamps the mapped
// workspace onto the store and the agent, exactly as the gRPC interceptor would
// (security amendment: workspace stamping).
func TestBridgeWorkspaceStampedOnEveryTurn(t *testing.T) {
	t.Parallel()

	users := map[int64]bridge.Grant{1: {Workspace: wsAcme}}
	rig := newFixture(t, users)

	scriptSingles(rig.tg, messageUpdate(1, testChatID, msgGo))

	runBridge(t, rig.br)

	bridgetest.WaitFor(t, func() bool { return len(rig.chat.Turns()) == 1 })

	if bound := rig.store.BoundWorkspaces(); len(bound) != 1 || bound[0] != wsAcme {
		t.Errorf("bind workspaces = %v, want [acme]", bound)
	}

	if created := rig.chat.CreatedWorkspaces(); len(created) != 1 || created[0] != wsAcme {
		t.Errorf("session workspaces = %v, want [acme]", created)
	}

	// The binding is keyed by the adapter's transport name, which is the value the
	// store column has carried since the bridge shipped.
	if _, ok := rig.store.Binding(wsAcme, transportName, "100"); !ok {
		t.Errorf("no binding under transport %q", transportName)
	}
}

// TestBridgeOffsetAckingSkipsAckedUpdates proves the ack advances past delivered
// updates so a batch is not replayed on the next poll (acceptance 1, dispatch).
func TestBridgeOffsetAckingSkipsAckedUpdates(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, map[int64]bridge.Grant{1: {Workspace: wsDefault}})

	first := messageUpdate(1, testChatID, "one")
	first.UpdateID = 5
	second := messageUpdate(1, testChatID, "two")
	second.UpdateID = 6
	rig.tg.script([]telegram.Update{*first}, []telegram.Update{*second})

	stop := runBridge(t, rig.br)

	// Wait for both turns to run and for the ack of update 6 (offset 7) to be
	// polled. Stopping on the turn count alone races the poll loop's final
	// getUpdates(7) against the cancel, which flakes under load.
	bridgetest.WaitFor(t, func() bool {
		return len(rig.chat.Turns()) == 2 && slices.Contains(rig.tg.seenOffsets(), int64(7))
	})

	stop()

	if turns := rig.chat.Turns(); len(turns) != 2 {
		t.Fatalf("processed %d turns, want 2 (no replay)", len(turns))
	}

	offsets := rig.tg.seenOffsets()
	if !slices.Contains(offsets, int64(6)) || !slices.Contains(offsets, int64(7)) {
		t.Errorf("offsets %v missing acks 6 and 7", offsets)
	}

	if slices.Contains(offsets, int64(5)) {
		t.Errorf("offsets %v re-requested an acked update", offsets)
	}
}

// TestBridgeBackoffStateChangeLogsOnly proves long-poll failures retry and log
// at warn only when the up/down state flips (acceptance 6).
func TestBridgeBackoffStateChangeLogsOnly(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, map[int64]bridge.Grant{1: {Workspace: wsDefault}})
	rig.tg.failGetUpdates(2)

	only := messageUpdate(1, testChatID, msgGo)
	only.UpdateID = 10
	rig.tg.script([]telegram.Update{*only})

	stop := runBridge(t, rig.br)

	bridgetest.WaitFor(t, func() bool { return len(rig.chat.Turns()) == 1 })

	stop()

	logs := rig.logs.String()
	if down := strings.Count(logs, "telegram long-poll down"); down != 1 {
		t.Errorf("down logs = %d, want 1 (state change only)", down)
	}

	if recovered := strings.Count(logs, "telegram long-poll recovered"); recovered != 1 {
		t.Errorf("recovered logs = %d, want 1", recovered)
	}
}

// TestBridgeNoTokenInLogsOnTransportError proves the token, which a transport
// error carries in its URL, never reaches the poll loop's logs. It injects the
// real client through the NewClientOver seam over a failing tripper, so the
// redaction under test is the shipped client's, not a fake's (acceptance 6;
// security amendment: no token in errors or logs).
func TestBridgeNoTokenInLogsOnTransportError(t *testing.T) {
	t.Parallel()

	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	metrics := bridgetest.NewMetrics()

	built := telegram.New(&telegram.Config{
		Core: bridge.NewCore(&bridge.CoreConfig{
			Chat:       bridgetest.NewChat(),
			Store:      bridgetest.NewStore(),
			Metrics:    metrics,
			Logger:     logger,
			ConfirmTTL: time.Minute,
		}),
		Metrics:     metrics,
		API:         telegram.NewClientOver(testToken, botHost, time.Second, time.Second, nettest.Unreachable(errDialRefused)),
		Users:       map[int64]bridge.Grant{1: {Workspace: wsDefault}},
		Logger:      logger,
		Token:       testToken,
		PollTimeout: time.Second,
		SendTimeout: time.Second,
		BackoffBase: time.Millisecond,
	})

	stop := runBridge(t, built)

	bridgetest.WaitFor(t, func() bool { return strings.Contains(logs.String(), "telegram long-poll down") })

	stop()

	got := logs.String()
	if strings.Contains(got, testToken) {
		t.Errorf("token leaked into logs: %s", got)
	}

	if !strings.Contains(got, wantRedaction) {
		t.Errorf("expected redaction marker in logs: %s", got)
	}
}

// TestBridgeDispatchFIFOAndConcurrent proves the dispatch model: a blocked turn
// occupies only its chat while another chat runs concurrently, a second update
// for the blocked chat waits and then runs FIFO, and a callback tap still gets an
// immediate answerCallbackQuery from the poll loop (acceptance 1, S4 dispatch).
func TestBridgeDispatchFIFOAndConcurrent(t *testing.T) {
	t.Parallel()

	const (
		blockText  = "BLOCK"
		quickText  = "quick"
		secondText = "second"
	)

	users := map[int64]bridge.Grant{
		1: {Workspace: "w1"},
		2: {Workspace: "w2"},
	}
	rig := newFixture(t, users)
	rig.chat.GateOn(blockText)

	blocked := messageUpdate(1, 1, blockText)
	other := messageUpdate(2, 2, quickText)
	queued := messageUpdate(1, 1, secondText)
	tap := callbackUpdate(4, 1, 1, confirmData("x", actionApprove))

	scriptSingles(rig.tg, blocked, other, queued, tap)

	runBridge(t, rig.br)

	bridgetest.WaitFor(t, func() bool { return rig.chat.HasTurn(quickText) })

	bridgetest.WaitFor(t, func() bool { return len(rig.tg.answeredCallbacks()) == 1 })

	if rig.chat.HasTurn(secondText) {
		t.Fatal("queued update ran before the blocked turn released; FIFO broken")
	}

	rig.chat.Release()

	bridgetest.WaitFor(t, func() bool { return rig.chat.HasTurn(secondText) })

	if rig.chat.TurnIndex(blockText) > rig.chat.TurnIndex(secondText) {
		t.Errorf("first chat processed out of order: %+v", rig.chat.Turns())
	}
}

// TestBridgeBadCallbackDataDropped proves a tap whose callback_data does not
// parse is logged and dropped without reaching the core, while still counting as
// one allowlisted update the adapter accepted.
func TestBridgeBadCallbackDataDropped(t *testing.T) {
	t.Parallel()

	rig := newFixture(t, map[int64]bridge.Grant{testUserID: {Workspace: wsDefault}})

	scriptSingles(rig.tg, callbackUpdate(1, testUserID, testChatID, "not-a-confirm"))

	runBridge(t, rig.br)

	bridgetest.WaitFor(t, func() bool { return rig.updateCount(bridge.OutcomeHandled) == 1 })

	if sent := rig.tg.sentMessages(); len(sent) != 0 {
		t.Errorf("bad callback produced a reply: %+v", sent)
	}

	if calls := rig.chat.Confirms(); len(calls) != 0 {
		t.Errorf("bad callback reached the agent: %+v", calls)
	}

	if !strings.Contains(rig.logs.String(), "bridge bad callback") {
		t.Errorf("bad callback not logged: %s", rig.logs.String())
	}
}
