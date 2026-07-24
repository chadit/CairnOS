package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/chadit/CairnOS/internal/bridge"
	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/telemetry"
)

// bridgeTelegramTokenEnv holds the Telegram bot token. The token is a credential
// embedded in every Bot API URL path (/bot<token>/method), so it is read only
// from the environment at serve wiring, never a config-file key; an unset token
// treats the bridge as off, the same posture as the Discord webhook secret
// (.agents/spec/parity/messaging-bridge.md, decision 3).
const bridgeTelegramTokenEnv = "CAIRNOS_BRIDGE_TELEGRAM_TOKEN" // #nosec G101 -- environment variable name, not a credential

// bridgeDeps groups what buildTelegramBridge wires the Telegram bridge over, so
// the helper takes one argument instead of a long parameter list.
type bridgeDeps struct {
	// chat is the shipped chat agent the bridge drives, reused rather than
	// forked. It is nil when [chat] is disabled, which is the cross-check
	// buildTelegramBridge fails on.
	chat *chat.Agent

	// store holds the chat-to-session bindings that survive a serve restart.
	store store.BridgeSessionStore

	logger *slog.Logger
}

// buildTelegramBridge builds the Telegram long-poll bridge when [bridge.telegram]
// is enabled and its token is set. It returns a nil bridge with ExitOK and one
// log line when the bridge is disabled or the token env is unset, so the default
// posture starts no goroutine. It fails fast with config.ErrInvalidConfig (via
// failServe) when the bridge is enabled while [chat] is disabled: the bridge has
// no agent to drive. The empty-users case already failed at config load
// (validateBridge), so it never reaches here. Kept out of runServe so that
// function stays under the complexity ceiling.
func (a *App) buildTelegramBridge(ctx context.Context, cfg *config.Config, tel *telemetry.Telemetry, deps *bridgeDeps) (*bridge.Bridge, int, bool) {
	telegram := &cfg.Bridge.Telegram

	if !telegram.Enabled {
		deps.logger.Info("telegram bridge disabled")

		return nil, ExitOK, true
	}

	token := os.Getenv(bridgeTelegramTokenEnv)
	if token == "" {
		deps.logger.Info("telegram bridge disabled: token env unset", "env", bridgeTelegramTokenEnv)

		return nil, ExitOK, true
	}

	// The bridge drives the chat agent, so an enabled bridge with chat off has
	// nothing to run. The cross-check lives here, where both blocks are visible,
	// rather than in config.Validate (decision 6). A typed-nil *chat.Agent never
	// reaches the bridge's interface field, so its own nil check stays meaningful.
	if deps.chat == nil {
		return nil, a.failServe(ctx, tel, bridgeStage,
			fmt.Errorf("%w: bridge.telegram.enabled requires [chat] enabled", config.ErrInvalidConfig)), false
	}

	users, err := bridgeUsers(telegram.Users)
	if err != nil {
		return nil, a.failServe(ctx, tel, bridgeStage, err), false
	}

	built := bridge.New(&bridge.Config{
		Chat:        deps.chat,
		Store:       deps.store,
		Metrics:     &bridgeMetricsAdapter{rec: tel.Recorder()},
		Users:       users,
		Token:       token,
		PollTimeout: telegram.PollTimeout,
		SendTimeout: telegram.SendTimeout,
		ConfirmTTL:  telegram.ConfirmTTL,
		Verbose:     telegram.Verbose,
		Logger:      deps.logger,
	})

	return built, ExitOK, true
}

// bridgeStage names the serve setup step in failServe diagnostics.
const bridgeStage = "bridge"

// bridgeUsers converts the config allowlist, keyed by the Telegram user ID as a
// string, to the bridge's int64-keyed map. A non-numeric key is a config error:
// the bridge matches each update's numeric sender ID, so a key that could never
// be a Telegram user ID is rejected rather than silently accepting nobody.
func bridgeUsers(users map[string]config.TelegramUser) (map[int64]bridge.AllowedUser, error) {
	resolved := make(map[int64]bridge.AllowedUser, len(users))

	for id, user := range users {
		userID, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: bridge.telegram.users key %q is not a numeric Telegram user id",
				config.ErrInvalidConfig, id)
		}

		resolved[userID] = bridge.AllowedUser{
			Workspace:    user.Workspace,
			Personality:  user.Personality,
			AllowConfirm: user.AllowConfirm,
		}
	}

	return resolved, nil
}

// startTelegramBridge runs the bridge's long-poll loop on its own goroutine and
// prints its banner when the bridge is configured, closing the returned channel
// when the loop exits so shutdown can wait for the workers to drain, the same
// supervised-goroutine shape as startSweep and startHiveSubscribe. A nil bridge
// (disabled or token-less) starts nothing and returns an already-closed channel,
// so the drain in runServe needs no nil guard. A panic in the poll loop is
// recovered and logged so a bridge crash never takes serve down (decision 6);
// the loop already retries poll failures with capped backoff and returns only on
// ctx cancellation.
func (a *App) startTelegramBridge(ctx context.Context, telegramBridge *bridge.Bridge, logger *slog.Logger) <-chan struct{} {
	done := make(chan struct{})

	if telegramBridge == nil {
		close(done)

		return done
	}

	a.printOutf("telegram bridge running\n")

	go func() {
		defer close(done)

		defer func() {
			if recovered := recover(); recovered != nil {
				logger.ErrorContext(ctx, "telegram bridge crashed", "panic", recovered)
			}
		}()

		telegramBridge.Run(ctx)
	}()

	return done
}

// bridgeMetricsAdapter forwards the bridge's four counters to the telemetry
// recorder, keeping internal/bridge telemetry-free behind its Metrics interface,
// the same split queueMetricsAdapter uses for the work queue. Metric recording
// is fire-and-forget, so it carries no request context.
type bridgeMetricsAdapter struct {
	rec *telemetry.Recorder
}

// Update records one poll-loop update by outcome: handled, unauthorized, or error.
func (m *bridgeMetricsAdapter) Update(outcome string) {
	m.rec.BridgeUpdate(context.Background(), outcome)
}

// Turn records one delivered chat turn, ok false on a turn failure.
func (m *bridgeMetricsAdapter) Turn(ok bool) {
	m.rec.BridgeTurn(context.Background(), ok)
}

// Confirm records one resolved D9 confirmation by outcome: approved, denied,
// expired, or refused.
func (m *bridgeMetricsAdapter) Confirm(outcome string) {
	m.rec.BridgeConfirm(context.Background(), outcome)
}

// SendFailure records one outbound send that failed after the turn completed.
func (m *bridgeMetricsAdapter) SendFailure() {
	m.rec.BridgeSendFailure(context.Background())
}
