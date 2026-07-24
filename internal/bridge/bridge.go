package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/store"
)

const (
	// transportTelegram is the store binding's transport column value; a second
	// transport is additive (.agents/spec/parity/messaging-bridge.md, decision 1).
	transportTelegram = "telegram"

	// Update outcome labels for the updates_total counter.
	outcomeHandled      = "handled"
	outcomeUnauthorized = "unauthorized"
	outcomeError        = "error"

	// maxMessageChars is the Bot API per-message text cap; a longer turn splits
	// on paragraph boundaries.
	maxMessageChars = 4096

	// inboxSize bounds a per-chat worker's backlog. A full inbox drops the update
	// rather than blocking the poll loop and stalling every other chat.
	inboxSize = 16

	// turnEventBuffer bounds the per-turn event channel the agent streams into.
	turnEventBuffer = 32

	// Bridge commands a message can carry instead of chat text.
	cmdNew     = "/new"
	cmdSession = "/session"

	// pollDeadlineMargin is added to the long-poll timeout for the client's
	// request deadline, so a hung connection unwedges instead of holding the
	// poll loop forever.
	pollDeadlineMargin = 10 * time.Second

	// defaultBackoffBase is the first long-poll retry delay; it doubles up to
	// backoffCap. The cap is the spec's 1m ceiling.
	defaultBackoffBase = 1 * time.Second
	backoffCap         = 1 * time.Minute
)

// ChatService is the slice of the chat agent the bridge drives; *chat.Agent
// satisfies it. Defined here so the bridge tests run against a fake agent and
// the dependency points at a narrow surface, mirroring internal/api's ChatAgent.
type ChatService interface {
	CreateSession(ctx context.Context, personality string) (*store.ChatSession, error)
	GetSession(ctx context.Context, id string) (*store.ChatSession, error)
	SendMessage(ctx context.Context, sessionID, userMessage string, out chan<- chat.Event) error
	ConfirmAction(ctx context.Context, sessionID string, approve bool, out chan<- chat.Event) error
}

// Metrics is the telemetry surface the bridge records through, kept an interface
// so the package stays telemetry-free; the cli wiring adapts telemetry.Recorder
// the way queueMetricsAdapter does.
type Metrics interface {
	// Update counts one poll-loop update by outcome: handled, unauthorized, or
	// error.
	Update(outcome string)
	// Turn counts one delivered chat turn, ok false on a turn failure.
	Turn(ok bool)
	// Confirm counts one resolved D9 confirmation by outcome. The confirm slice
	// (confirm.go) drives it.
	Confirm(outcome string)
	// SendFailure counts one outbound send that failed after the turn completed.
	SendFailure()
}

// AllowedUser is one allowlisted sender's binding: the workspace its turns are
// stamped with, the personality a new session gets, and whether it may approve a
// D9 proposal. The cli wiring builds this map from [bridge.telegram.users]; the
// bridge keys it by the numeric Telegram user id it matches against each update.
type AllowedUser struct {
	Workspace    string
	Personality  string
	AllowConfirm bool
}

// Config wires one Bridge.
type Config struct {
	// Chat is the agent the bridge drives; the shipped *chat.Agent satisfies it.
	Chat ChatService

	// Store holds the chat-to-session bindings that survive a serve restart.
	Store store.BridgeSessionStore

	// Metrics records the four bridge counters; nil is not allowed.
	Metrics Metrics

	// API is the Bot API surface the bridge drives. A nil API means the real Bot
	// API client built by New against DefaultBaseURL, which is what production
	// always uses; a test injects its own BotAPI here instead (doc.go, base-URL
	// rationale).
	API BotAPI

	// Users is the allowlist keyed by numeric Telegram user id.
	Users map[int64]AllowedUser

	// Token is the bot credential, read from the environment at wiring.
	Token string

	// PollTimeout is the long-poll hold per getUpdates call.
	PollTimeout time.Duration

	// SendTimeout bounds each outbound Bot API call.
	SendTimeout time.Duration

	// BackoffBase is the first long-poll retry delay; each failure doubles it up
	// to a 1m ceiling. Zero falls back to the built-in default. Tests shorten it
	// so retry assertions stay fast; production leaves it unset.
	BackoffBase time.Duration

	// ConfirmTTL is how long an unanswered D9 proposal stays pending; the confirm
	// slice (confirm.go) arms the timer.
	ConfirmTTL time.Duration

	// Verbose relays tool-call status lines when true.
	Verbose bool

	// Logger receives the bridge's structured logs; nil falls back to the slog
	// default.
	Logger *slog.Logger
}

// Bridge is the Telegram long-poll bridge. Run owns a single poll goroutine and
// one worker goroutine per chat; it is not safe to call Run twice.
type Bridge struct {
	api     BotAPI
	chat    ChatService
	store   store.BridgeSessionStore
	metrics Metrics
	users   map[int64]AllowedUser
	verbose bool
	log     *slog.Logger

	pollTimeout time.Duration

	// backoffBase is the first retry delay for long-poll failures, taken from
	// Config.BackoffBase (defaultBackoffBase when unset). Tests shorten it so
	// retry assertions stay fast.
	backoffBase time.Duration

	// confirmTTL is how long an unanswered D9 proposal stays pending before the
	// confirm slice denies it as expired.
	confirmTTL time.Duration

	// confirmMu guards pending, which the per-chat workers and the TTL timer
	// callbacks touch concurrently.
	confirmMu sync.Mutex

	// pending is the confirm routing map: session id -> chat, message, and opener
	// of a live proposal keyboard, plus its expiry timer. The D9 gate state itself
	// stays inside chat.Agent; this holds only what the bridge needs to route a
	// tap and edit the message on expiry (confirm.go).
	pending map[string]*proposalRoute
}

// New builds a Bridge from cfg. When cfg.API is nil it constructs the real Bot
// API client against DefaultBaseURL, which is the production path; a test
// injects its own BotAPI through cfg.API instead. Nothing in production sets
// cfg.API, so a running bridge always talks to the fixed public host (doc.go,
// base-URL rationale).
func New(cfg *Config) *Bridge {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	api := cfg.API
	if api == nil {
		api = NewClient(cfg.Token, DefaultBaseURL, cfg.SendTimeout, cfg.PollTimeout)
	}

	backoffBase := cfg.BackoffBase
	if backoffBase <= 0 {
		backoffBase = defaultBackoffBase
	}

	return &Bridge{
		api:         api,
		chat:        cfg.Chat,
		store:       cfg.Store,
		metrics:     cfg.Metrics,
		users:       cfg.Users,
		verbose:     cfg.Verbose,
		log:         log,
		pollTimeout: cfg.PollTimeout,
		backoffBase: backoffBase,
		confirmTTL:  cfg.ConfirmTTL,
		pending:     map[string]*proposalRoute{},
	}
}

// worker is one chat's serial processor: updates for a single chat run FIFO on
// its own goroutine, so a slow turn occupies only its chat while the poll loop
// and every other chat keep moving (.agents/spec/parity/messaging-bridge.md,
// decision 2).
type worker struct {
	inbox chan inbound
}

// inbound is one routed update paired with the resolved sender binding and the
// numeric chat key its worker is addressed by.
type inbound struct {
	update  *Update
	user    AllowedUser
	chatKey int64
}

// Run drives the poll loop until ctx is canceled. It owns one poll goroutine and
// starts one worker goroutine per chat lazily; on return it closes every worker
// inbox and waits for the workers to drain, so no goroutine outlives Run. A poll
// failure never returns: the loop retries with exponential backoff capped at
// backoffCap, logging at warn only when the up/down state flips.
func (b *Bridge) Run(ctx context.Context) {
	var waiters sync.WaitGroup

	workers := map[int64]*worker{}

	defer func() {
		for _, active := range workers {
			close(active.inbox)
		}

		waiters.Wait()
		b.stopAllProposals()
	}()

	timeoutSecs := int(b.pollTimeout.Seconds())
	delay := b.backoffBase

	var (
		offset int64
		down   bool
	)

	for {
		if ctx.Err() != nil {
			return
		}

		updates, err := b.api.GetUpdates(ctx, offset, timeoutSecs)
		if err != nil {
			if ctx.Err() != nil {
				return
			}

			if !down {
				b.log.WarnContext(ctx, "telegram long-poll down", "error", err)

				down = true
			}

			if !sleepCtx(ctx, delay) {
				return
			}

			delay = nextBackoff(delay)

			continue
		}

		if down {
			b.log.WarnContext(ctx, "telegram long-poll recovered")

			down = false
		}

		delay = b.backoffBase

		for index := range updates {
			offset = updates[index].UpdateID + 1

			b.dispatch(ctx, &waiters, workers, &updates[index])
		}
	}
}

// sleepCtx waits for delay or ctx cancellation, reporting false when ctx ended
// so the caller stops instead of continuing.
func sleepCtx(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// nextBackoff doubles the retry delay up to backoffCap.
func nextBackoff(current time.Duration) time.Duration {
	next := current * 2
	if next > backoffCap {
		return backoffCap
	}

	return next
}

// dispatch does the poll loop's cheap per-update work: allowlist the sender,
// answer a callback tap immediately so the client spinner clears no matter how
// busy the target chat is, then hand the update to its per-chat worker. A miss
// or a full inbox is dropped here rather than blocking the loop.
func (b *Bridge) dispatch(ctx context.Context, waiters *sync.WaitGroup, workers map[int64]*worker, update *Update) {
	userID, chatKey, ok := route(update)
	if !ok {
		b.metrics.Update(outcomeError)
		b.log.DebugContext(ctx, "dropping unroutable telegram update", "update_id", update.UpdateID)

		return
	}

	user, allowed := b.users[userID]
	if !allowed {
		b.metrics.Update(outcomeUnauthorized)
		b.log.DebugContext(ctx, "dropping update from unauthorized sender", "user_id", userID)

		return
	}

	if update.CallbackQuery != nil {
		if err := b.api.AnswerCallbackQuery(ctx, update.CallbackQuery.ID, ""); err != nil {
			b.log.WarnContext(ctx, "answer callback failed", "error", err)
		}
	}

	target := workers[chatKey]
	if target == nil {
		target = &worker{inbox: make(chan inbound, inboxSize)}
		workers[chatKey] = target

		waiters.Add(1)

		go b.runWorker(ctx, target, waiters)
	}

	select {
	case target.inbox <- inbound{update: update, user: user, chatKey: chatKey}:
	default:
		b.metrics.Update(outcomeError)
		b.log.WarnContext(ctx, "chat inbox full, dropping update", "chat_key", chatKey)
	}
}

// route extracts the sender id and chat key an update is addressed by, for both
// message and callback updates, returning them in that order with a final ok
// flag. ok is false for an update the bridge cannot place (no sender or no
// chat), which the poll loop drops.
func route(update *Update) (int64, int64, bool) {
	switch {
	case update.Message != nil && update.Message.From != nil:
		return update.Message.From.ID, update.Message.Chat.ID, true
	case update.CallbackQuery != nil && update.CallbackQuery.From != nil && update.CallbackQuery.Message != nil:
		return update.CallbackQuery.From.ID, update.CallbackQuery.Message.Chat.ID, true
	default:
		return 0, 0, false
	}
}

// runWorker processes one chat's updates in arrival order until its inbox closes
// or ctx is canceled.
func (b *Bridge) runWorker(ctx context.Context, active *worker, waiters *sync.WaitGroup) {
	defer waiters.Done()

	for item := range active.inbox {
		if ctx.Err() != nil {
			return
		}

		b.handle(ctx, &item)
	}
}

// handle processes one routed update: a chat message runs a turn, a callback tap
// routes into the confirm flow. Either way the update counts handled.
func (b *Bridge) handle(ctx context.Context, item *inbound) {
	b.metrics.Update(outcomeHandled)

	switch {
	case item.update.Message != nil:
		b.handleMessage(ctx, item)
	case item.update.CallbackQuery != nil:
		b.handleCallback(ctx, item)
	}
}

// handleMessage stamps the sender's workspace onto the context exactly as the
// gRPC interceptor would, then handles a bridge command or runs a chat turn.
func (b *Bridge) handleMessage(ctx context.Context, item *inbound) {
	ctx = store.ContextWithWorkspace(ctx, item.user.Workspace)

	message := item.update.Message
	chatKey := strconv.FormatInt(message.Chat.ID, 10)

	switch strings.TrimSpace(message.Text) {
	case cmdNew:
		b.startNewSession(ctx, message.Chat.ID, chatKey, item.user)
	case cmdSession:
		b.replySessionID(ctx, message.Chat.ID, chatKey, item.user)
	default:
		b.runTurn(ctx, message.Chat.ID, message.From.ID, chatKey, item.user, message.Text)
	}
}

// ensureSession resolves the chat's bound session, creating and binding a fresh
// one on first contact (ErrBridgeSessionNotFound). The binding lives in the
// store so it survives a serve restart.
func (b *Bridge) ensureSession(ctx context.Context, chatKey string, user AllowedUser) (string, error) {
	sessionID, err := b.store.LookupBridgeSession(ctx, transportTelegram, chatKey)
	if err == nil {
		return sessionID, nil
	}

	if !errors.Is(err, store.ErrBridgeSessionNotFound) {
		return "", fmt.Errorf("lookup bridge session: %w", err)
	}

	return b.newSession(ctx, chatKey, user)
}

// newSession creates a session with the sender's personality and binds the chat
// to it, overwriting any prior binding (the /new path).
func (b *Bridge) newSession(ctx context.Context, chatKey string, user AllowedUser) (string, error) {
	session, err := b.chat.CreateSession(ctx, user.Personality)
	if err != nil {
		return "", fmt.Errorf("create chat session: %w", err)
	}

	if err := b.store.BindBridgeSession(ctx, transportTelegram, chatKey, session.ID); err != nil {
		return "", fmt.Errorf("bind bridge session: %w", err)
	}

	return session.ID, nil
}

// startNewSession rebinds the chat to a fresh session and confirms it, so /new
// starts a clean conversation while the prior transcript stays readable by its
// own session id.
func (b *Bridge) startNewSession(ctx context.Context, chatID int64, chatKey string, user AllowedUser) {
	sessionID, err := b.newSession(ctx, chatKey, user)
	if err != nil {
		b.log.ErrorContext(ctx, "bridge /new failed", "error", err)
		b.reply(ctx, chatID, "Could not start a new session.")

		return
	}

	b.reply(ctx, chatID, "Started a new session: "+sessionID)
}

// replySessionID answers /session with the chat's current session id so an
// operator can pick the same conversation up from cairnos chat --session.
func (b *Bridge) replySessionID(ctx context.Context, chatID int64, chatKey string, user AllowedUser) {
	sessionID, err := b.ensureSession(ctx, chatKey, user)
	if err != nil {
		b.log.ErrorContext(ctx, "bridge /session failed", "error", err)
		b.reply(ctx, chatID, "Could not resolve this chat's session.")

		return
	}

	b.reply(ctx, chatID, "Session: "+sessionID)
}

// runTurn resolves the chat's session, runs one chat turn, and delivers the
// reply. The turn's outcome counts on turns_total; a failed delivery counts on
// send_failures_total and is never retried, since the reply is already in
// session history. openerID is the numeric sender id, recorded on any proposal
// this turn raises so only that operator can approve it (confirm.go).
func (b *Bridge) runTurn(ctx context.Context, chatID, openerID int64, chatKey string, user AllowedUser, userMessage string) {
	sessionID, err := b.ensureSession(ctx, chatKey, user)
	if err != nil {
		b.metrics.Turn(false)
		b.log.ErrorContext(ctx, "bridge session ensure failed", "error", err)

		return
	}

	events := make(chan chat.Event, turnEventBuffer)
	result := make(chan error, 1)

	go func() {
		result <- b.chat.SendMessage(ctx, sessionID, userMessage, events)
	}()

	var reply strings.Builder

	for event := range events {
		b.consume(ctx, chatID, sessionID, openerID, &reply, &event)
	}

	turnErr := <-result

	b.metrics.Turn(turnErr == nil)

	if turnErr != nil {
		b.log.ErrorContext(ctx, "bridge chat turn failed", "error", turnErr)

		return
	}

	if reply.Len() > 0 {
		b.deliver(ctx, chatID, reply.String())
	}
}

// consume folds one turn event toward the outbound reply: assistant text buffers
// into one message, a tool call is relayed as a status line only in verbose
// mode, and a workflow proposal renders the D9 confirmation keyboard. Run-lifecycle
// and turn-done events carry nothing new to send here.
func (b *Bridge) consume(ctx context.Context, chatID int64, sessionID string, openerID int64, reply *strings.Builder, event *chat.Event) {
	switch event.Type {
	case chat.EventAssistantChunk:
		reply.WriteString(event.Text)
	case chat.EventToolCall:
		if b.verbose {
			b.reply(ctx, chatID, "tool: "+event.Tool)
		}
	case chat.EventConfirmationRequired:
		b.sendProposal(ctx, chatID, sessionID, openerID, event.Proposal)
	case chat.EventRunStarted, chat.EventRunFinished, chat.EventTurnDone:
	}
}

// deliver sends one turn's reply, splitting anything over the Bot API's
// per-message cap on paragraph boundaries so a long answer arrives as ordered
// messages rather than a rejected one.
func (b *Bridge) deliver(ctx context.Context, chatID int64, text string) {
	for _, part := range splitMessage(text, maxMessageChars) {
		b.reply(ctx, chatID, part)
	}
}

// reply sends one plain-text message, counting and logging a delivery failure
// without retrying: the turn already ran and its reply is in session history.
// The failure is stamped CodeSendFailed over the client's already-redacted
// cause, so the log carries no token.
func (b *Bridge) reply(ctx context.Context, chatID int64, text string) {
	if _, err := b.api.SendMessage(ctx, chatID, text, nil); err != nil {
		b.metrics.SendFailure()
		b.log.ErrorContext(ctx, "bridge send failed",
			"error", errf(CodeSendFailed, "deliver to chat %d: %v", chatID, err))
	}
}

// paragraphBreak is the blank-line boundary splitMessage prefers to break on.
const paragraphBreak = "\n\n"

// splitMessage breaks text into chunks no longer than limit, preferring
// paragraph boundaries (a blank line) and hard-cutting a single paragraph that
// exceeds the limit on its own.
func splitMessage(text string, limit int) []string {
	if len(text) <= limit {
		return []string{text}
	}

	return packUnits(paragraphUnits(text, limit), limit)
}

// paragraphUnits splits text on blank lines and hard-cuts any paragraph longer
// than limit, so every returned unit fits in one message.
func paragraphUnits(text string, limit int) []string {
	var units []string

	for paragraph := range strings.SplitSeq(text, paragraphBreak) {
		rest := paragraph

		for len(rest) > limit {
			units = append(units, rest[:limit])
			rest = rest[limit:]
		}

		if rest != "" {
			units = append(units, rest)
		}
	}

	return units
}

// packUnits greedily joins units back with blank lines while they fit under
// limit, so a long turn arrives as the fewest ordered messages.
func packUnits(units []string, limit int) []string {
	var (
		parts   []string
		current string
	)

	for _, unit := range units {
		candidate := joinChunk(current, unit)

		if len(candidate) > limit {
			parts = append(parts, current)
			current = unit

			continue
		}

		current = candidate
	}

	if current != "" {
		parts = append(parts, current)
	}

	return parts
}

// joinChunk appends unit to current with a blank-line separator, or returns unit
// alone when current is empty.
func joinChunk(current, unit string) string {
	if current == "" {
		return unit
	}

	return current + paragraphBreak + unit
}
