package telegram

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/chadit/CairnOS/internal/bridge"
)

const (
	// transportName is the store binding's transport column value and the label
	// every counter this adapter records carries.
	transportName = "telegram"

	// identityName is the Metrics identity label. Telegram answers as a single
	// bot per serve, so it has no config identity to name and the label stays
	// empty rather than inventing one.
	identityName = ""

	// pollDeadlineMargin is added to the long-poll timeout for the client's
	// request deadline, so a hung connection unwedges instead of holding the
	// poll loop forever.
	pollDeadlineMargin = 10 * time.Second
)

// Config wires one Bridge.
type Config struct {
	// Core is the transport-neutral half every inbound is handed to. The serve
	// wiring builds it once and shares it across transports, so sessions and
	// pending proposals are common state.
	Core *bridge.Core

	// Metrics records the update outcomes this adapter refuses before the core
	// sees them; nil is not allowed.
	Metrics bridge.Metrics

	// API is the Bot API surface the adapter drives. A nil API means the real Bot
	// API client built by New against DefaultBaseURL, which is what production
	// always uses; a test injects its own BotAPI here instead (doc.go, base-URL
	// rationale).
	API BotAPI

	// Transport carries the real client's calls when API is nil, and nil means
	// the shared default. It is a round tripper rather than a base URL, so serve's
	// tests keep the poll off the network while the host stays fixed (doc.go,
	// base-URL rationale).
	Transport http.RoundTripper

	// Users is the allowlist keyed by numeric Telegram user id. Each entry is the
	// grant the core enforces for that sender.
	Users map[int64]bridge.Grant

	// Logger receives the adapter's structured logs; nil falls back to the slog
	// default.
	Logger *slog.Logger

	// Token is the bot credential, read from the environment at wiring.
	Token string

	// PollTimeout is the long-poll hold per getUpdates call.
	PollTimeout time.Duration

	// SendTimeout bounds each outbound Bot API call.
	SendTimeout time.Duration

	// BackoffBase is the first long-poll retry delay; each failure doubles it up
	// to a 1m ceiling. Zero falls back to bridge.DefaultBackoffBase. Tests shorten it
	// so retry assertions stay fast; production leaves it unset.
	BackoffBase time.Duration
}

// Bridge is the Telegram long-poll adapter. Run owns a single poll goroutine and
// hands every allowlisted update to the shared core; it is not safe to call Run
// twice.
type Bridge struct {
	api     BotAPI
	core    *bridge.Core
	metrics bridge.Metrics
	out     bridge.Outbound
	users   map[int64]bridge.Grant
	log     *slog.Logger

	pollTimeout time.Duration

	// backoffBase is the first retry delay for long-poll failures, taken from
	// Config.BackoffBase (bridge.DefaultBackoffBase when unset). Tests shorten it so
	// retry assertions stay fast.
	backoffBase time.Duration
}

// New builds a Bridge from cfg. When cfg.API is nil it constructs the real Bot
// API client against DefaultBaseURL over cfg.Transport, which is the production
// path; a test injects its own BotAPI through cfg.API instead. Nothing in
// production sets cfg.API, so a running bridge always addresses the fixed public
// host (doc.go, base-URL rationale).
func New(cfg *Config) *Bridge {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	api := cfg.API
	if api == nil {
		api = NewClientOver(cfg.Token, DefaultBaseURL, cfg.SendTimeout, cfg.PollTimeout, cfg.Transport)
	}

	backoffBase := cfg.BackoffBase
	if backoffBase <= 0 {
		backoffBase = bridge.DefaultBackoffBase
	}

	return &Bridge{
		api:         api,
		core:        cfg.Core,
		metrics:     cfg.Metrics,
		out:         outbound{api: api},
		users:       cfg.Users,
		log:         log,
		pollTimeout: cfg.PollTimeout,
		backoffBase: backoffBase,
	}
}

// Run drives the poll loop until ctx is canceled. A poll failure never returns:
// the loop retries with exponential backoff capped at bridge.BackoffCap, logging
// at warn only when the up/down state flips. Draining the core is the caller's
// job, because two transports share one core and the first Run to return would
// otherwise drain the other transport's conversation workers.
func (b *Bridge) Run(ctx context.Context) {
	timeoutSecs := int(b.pollTimeout.Seconds())
	delay := b.backoffBase

	var (
		offset int64
		down   bool
	)

	for ctx.Err() == nil {
		updates, err := b.api.GetUpdates(ctx, offset, timeoutSecs)
		if err != nil {
			down, delay = b.retryAfter(ctx, down, delay, err)

			continue
		}

		if down {
			b.log.WarnContext(ctx, "telegram long-poll recovered")

			down = false
		}

		delay = b.backoffBase

		for index := range updates {
			offset = updates[index].UpdateID + 1

			b.dispatch(ctx, &updates[index])
		}
	}
}

// retryAfter handles one failed poll and returns the outage state and the next
// delay. Only an outage's first failure logs, so a long outage writes one warn
// line instead of one per retry. A failure caused by ctx ending logs nothing, and
// Sleep's result is dropped because a ctx that ends mid-wait is what the poll
// loop's condition checks next.
func (b *Bridge) retryAfter(
	ctx context.Context,
	wasDown bool,
	delay time.Duration,
	cause error,
) (bool, time.Duration) {
	if ctx.Err() != nil {
		return wasDown, delay
	}

	if !wasDown {
		b.log.WarnContext(ctx, "telegram long-poll down", "error", cause)
	}

	bridge.Sleep(ctx, delay)

	return true, bridge.NextBackoff(delay)
}

// origin is one update's resolved addressing: the sender id the allowlist is
// keyed by and the chat id the conversation is addressed by.
type origin struct {
	sender int64
	chat   int64
}

// dispatch does the poll loop's cheap per-update work: allowlist the sender,
// answer a callback tap immediately so the client spinner clears no matter how
// busy the target conversation is, then hand the update to the core. A miss is
// dropped here rather than blocking the loop.
func (b *Bridge) dispatch(ctx context.Context, update *Update) {
	from, routable := route(update)
	if !routable {
		b.log.DebugContext(ctx, "dropping unroutable telegram update", "update_id", update.UpdateID)
		b.metrics.Update(transportName, identityName, bridge.OutcomeError)

		return
	}

	grant, allowed := b.users[from.sender]
	if !allowed {
		b.log.DebugContext(ctx, "dropping update from unauthorized sender", "user_id", from.sender)
		b.metrics.Update(transportName, identityName, bridge.OutcomeUnauthorized)

		return
	}

	if update.CallbackQuery != nil {
		if err := b.api.AnswerCallbackQuery(ctx, update.CallbackQuery.ID, ""); err != nil {
			b.log.WarnContext(ctx, "answer callback failed", "error", err)
		}
	}

	item, built := b.toInbound(update, from, grant)
	if !built {
		// A tap whose callback_data does not parse never reaches the core, and the
		// core is what counts an accepted update, so this branch counts it instead.
		// The count comes last: it is the only signal another goroutine can observe,
		// so counting after the log is what makes the count mean the drop is done.
		b.log.WarnContext(ctx, "bridge bad callback",
			"error", errf(CodeBadCallback, "callback data %q", update.CallbackQuery.Data))
		b.metrics.Update(transportName, identityName, bridge.OutcomeHandled)

		return
	}

	b.core.Handle(ctx, item)
}

// toInbound turns one routed update into the core's neutral inbound: a chat
// message becomes a turn, an inline-keyboard tap becomes a decision carrying the
// session its callback_data names. It reports false for a tap whose callback_data
// does not parse, which the caller drops.
func (b *Bridge) toInbound(update *Update, from origin, grant bridge.Grant) (*bridge.Inbound, bool) {
	chatKey := strconv.FormatInt(from.chat, idBase)

	item := &bridge.Inbound{
		Addr: bridge.Address{
			Transport: transportName,
			Identity:  identityName,
			ChatKey:   chatKey,
			Channel:   chatKey,
		},
		Sender: bridge.Sender{ID: strconv.FormatInt(from.sender, idBase)},
		Grant:  grant,
		Out:    b.out,
	}

	if update.CallbackQuery == nil {
		item.Kind = bridge.KindMessage
		item.Text = update.Message.Text

		return item, true
	}

	sessionID, action, valid := parseCallback(update.CallbackQuery.Data)
	if !valid {
		return nil, false
	}

	item.Kind = bridge.KindDecision
	item.Session = sessionID
	item.Approve = action == actionApprove

	return item, true
}

// route extracts the sender id and chat id an update is addressed by, for both
// message and callback updates. It reports false for an update the adapter cannot
// place (no sender or no chat), which the poll loop drops.
func route(update *Update) (origin, bool) {
	switch {
	case update.Message != nil && update.Message.From != nil:
		return origin{sender: update.Message.From.ID, chat: update.Message.Chat.ID}, true
	case update.CallbackQuery != nil && update.CallbackQuery.From != nil && update.CallbackQuery.Message != nil:
		return origin{sender: update.CallbackQuery.From.ID, chat: update.CallbackQuery.Message.Chat.ID}, true
	default:
		return origin{}, false
	}
}
