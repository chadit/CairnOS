package cli

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"

	"github.com/chadit/CairnOS/internal/bridge"
	"github.com/chadit/CairnOS/internal/bridge/mattermost"
	"github.com/chadit/CairnOS/internal/bridge/telegram"
	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/messaging"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/telemetry"
)

// bridgeTelegramTokenEnv holds the Telegram bot token. The token is a credential
// embedded in every Bot API URL path (/bot<token>/method), so it is read only
// from the environment at serve wiring, never a config-file key; an unset token
// treats the bridge as off, the same posture as the Discord webhook secret
// (.agents/spec/parity/messaging-bridge.md, decision 3).
const bridgeTelegramTokenEnv = "CAIRNOS_BRIDGE_TELEGRAM_TOKEN" // #nosec G101 -- environment variable name, not a credential

// bridgeDeps groups what the bridge builders wire their adapter over, so each
// helper takes one argument instead of a long parameter list.
type bridgeDeps struct {
	// core is the transport-neutral half both adapters hand their inbounds to.
	// It is built once in runServe so Telegram and Mattermost share one session
	// map and one set of pending proposals.
	core *bridge.Core

	// chat is the shipped chat agent the core drives, reused rather than forked.
	// It is nil when [chat] is disabled, which is the cross-check both builders
	// fail on.
	chat *chat.Agent

	// store holds the chat-to-session bindings that survive a serve restart.
	store store.BridgeSessionStore

	// metrics forwards the adapters' four counters to the telemetry recorder.
	metrics bridge.Metrics

	logger *slog.Logger
}

// newBridgeCore builds the core both transports share, or nil when [chat] is
// off: each builder fails that cross-check first. The confirm TTL and verbose
// flag are per-transport config while the core holds one of each, so an enabled
// Telegram bridge supplies them and Mattermost does otherwise.
func newBridgeCore(cfg *config.Config, deps *bridgeDeps) *bridge.Core {
	if deps.chat == nil {
		return nil
	}

	confirmTTL := cfg.Bridge.Mattermost.ConfirmTTL

	var verbose bool

	if cfg.Bridge.Telegram.Enabled {
		confirmTTL = cfg.Bridge.Telegram.ConfirmTTL
		verbose = cfg.Bridge.Telegram.Verbose

		noteConfirmTTLConflict(cfg, deps)
	}

	return bridge.NewCore(&bridge.CoreConfig{
		Chat:       deps.chat,
		Store:      deps.store,
		Metrics:    deps.metrics,
		Logger:     deps.logger,
		ConfirmTTL: confirmTTL,
		Verbose:    verbose,
	})
}

// noteConfirmTTLConflict says once which TTL won. The core holds one, Telegram
// supplies it when both bridges are live, and nothing else tells an operator who
// set confirm_ttl = "30m" for slower Mattermost approvers that they got
// Telegram's value instead.
func noteConfirmTTLConflict(cfg *config.Config, deps *bridgeDeps) {
	if len(cfg.Bridge.Mattermost.Identity) == 0 {
		return
	}

	if cfg.Bridge.Mattermost.ConfirmTTL == cfg.Bridge.Telegram.ConfirmTTL {
		return
	}

	deps.logger.Warn("both bridges are live and the core holds one confirm ttl; telegram's wins",
		"telegram_confirm_ttl", cfg.Bridge.Telegram.ConfirmTTL,
		"mattermost_confirm_ttl", cfg.Bridge.Mattermost.ConfirmTTL)
}

// buildTelegramBridge builds the Telegram long-poll bridge when
// [bridge.telegram] is enabled and its token is set. A disabled bridge or unset
// token returns a nil bridge with ExitOK and one log line, so the default
// posture starts no goroutine. Enabled while [chat] is off fails with
// config.ErrInvalidConfig: the bridge would have no agent to drive.
func (a *App) buildTelegramBridge(
	ctx context.Context,
	cfg *config.Config,
	tel *telemetry.Telemetry,
	deps *bridgeDeps,
) (*telegram.Bridge, int, bool) {
	settings := &cfg.Bridge.Telegram

	if !settings.Enabled {
		deps.logger.Info("telegram bridge disabled")

		return nil, ExitOK, true
	}

	token := a.envLookup().Get(bridgeTelegramTokenEnv)
	if token == "" {
		deps.logger.Info("telegram bridge disabled: token env unset", "env", bridgeTelegramTokenEnv)

		return nil, ExitOK, true
	}

	// The bridge drives the chat agent, so an enabled bridge with chat off has
	// nothing to run. The cross-check lives here, where both blocks are visible,
	// rather than in config.Validate (decision 6). A typed-nil *chat.Agent never
	// reaches the core's interface field, so its own nil check stays meaningful.
	if deps.chat == nil {
		return nil, a.failServe(ctx, tel, bridgeStage,
			fmt.Errorf("%w: bridge.telegram.enabled requires [chat] enabled", config.ErrInvalidConfig)), false
	}

	users, err := bridgeUsers(settings.Users)
	if err != nil {
		return nil, a.failServe(ctx, tel, bridgeStage, err), false
	}

	built := telegram.New(&telegram.Config{
		Core:        deps.core,
		Metrics:     deps.metrics,
		Users:       users,
		Logger:      deps.logger,
		Token:       token,
		PollTimeout: settings.PollTimeout,
		SendTimeout: settings.SendTimeout,
	})

	return built, ExitOK, true
}

// bridgeStage names the serve setup step in failServe diagnostics.
const bridgeStage = "bridge"

// bridgeUsers converts the config allowlist, keyed by the Telegram user ID as a
// string, to the adapter's int64-keyed grant map. A non-numeric key is a config
// error: the adapter matches each update's numeric sender ID, so a key that could
// never be a Telegram user ID is rejected rather than silently accepting nobody.
func bridgeUsers(users map[string]config.TelegramUser) (map[int64]bridge.Grant, error) {
	resolved := make(map[int64]bridge.Grant, len(users))

	for id, user := range users {
		userID, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: bridge.telegram.users key %q is not a numeric Telegram user id",
				config.ErrInvalidConfig, id)
		}

		resolved[userID] = bridge.Grant{
			Workspace:    user.Workspace,
			Personality:  user.Personality,
			AllowConfirm: user.AllowConfirm,
		}
	}

	return resolved, nil
}

// buildServeBridges builds the core both transports share, then each adapter,
// and registers every Mattermost identity's send surface in the registry. It
// returns the runtime startBridges later drives, or the failure exit code when a
// builder's cross-check refuses the config before any listener binds.
func (a *App) buildServeBridges(
	ctx context.Context,
	cfg *config.Config,
	tel *telemetry.Telemetry,
	registry *messaging.Registry,
	deps *bridgeDeps,
) (*bridgeRuntime, int, bool) {
	deps.core = newBridgeCore(cfg, deps)

	telegramBridge, code, mounted := a.buildTelegramBridge(ctx, cfg, tel, deps)
	if !mounted {
		return nil, code, false
	}

	mattermostBridge, code, mounted := a.buildMattermostBridge(ctx, cfg, tel, deps, registry)
	if !mounted {
		return nil, code, false
	}

	return &bridgeRuntime{
		core:       deps.core,
		telegram:   telegramBridge,
		mattermost: mattermostBridge,
	}, ExitOK, true
}

// bridgeRuntime is the set of transports serve started, plus the core they
// share, so shutdown drains all of it through one channel.
type bridgeRuntime struct {
	core       *bridge.Core
	telegram   *telegram.Bridge
	mattermost *mattermost.Bridge
}

// startBridges runs each configured transport on its own goroutine, closing the
// returned channel once all have stopped and the shared core has drained. The
// drain lives here because both transports share one core: whichever loop
// returned first would otherwise drain the other's conversation workers. With
// none configured it returns a closed channel, so runServe needs no nil guard.
func (a *App) startBridges(ctx context.Context, logger *slog.Logger, deps *bridgeRuntime) <-chan struct{} {
	done := make(chan struct{})
	loops := make([]func(context.Context), 0, 2)

	if deps.telegram != nil {
		a.printOutf("telegram bridge running\n")

		loops = append(loops, deps.telegram.Run)
	}

	if deps.mattermost != nil {
		a.printOutf("mattermost bridge running for %d identities\n", len(deps.mattermost.Senders()))

		loops = append(loops, deps.mattermost.Run)
	}

	if len(loops) == 0 {
		close(done)

		return done
	}

	go func() {
		defer close(done)
		defer deps.core.Drain()

		runBridgeLoops(ctx, logger, loops)
	}()

	return done
}

// runBridgeLoops runs each transport loop until it returns, then waits for all
// of them, so the caller drains the core only once every transport is finished
// with it.
func runBridgeLoops(ctx context.Context, logger *slog.Logger, loops []func(context.Context)) {
	var running sync.WaitGroup

	for _, loop := range loops {
		running.Go(func() { runBridgeLoop(ctx, logger, loop) })
	}

	running.Wait()
}

// runBridgeLoop runs one transport with a panic recovered and logged, so a
// bridge crash never takes serve down (decision 6). Each loop already retries
// its own connection failures with capped backoff and returns only when ctx is
// canceled.
func runBridgeLoop(ctx context.Context, logger *slog.Logger, loop func(context.Context)) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.ErrorContext(ctx, "bridge loop crashed", "panic", recovered)
		}
	}()

	loop(ctx)
}

// bridgeMetricsAdapter forwards the bridge's four counters to the telemetry
// recorder, keeping internal/bridge telemetry-free behind its Metrics interface,
// the same split queueMetricsAdapter uses for the work queue. Metric recording
// is fire-and-forget, so it carries no request context.
type bridgeMetricsAdapter struct {
	rec *telemetry.Recorder
}

// Update records one inbound message by outcome: handled, unauthorized, or error.
func (m *bridgeMetricsAdapter) Update(transport, identity, outcome string) {
	m.rec.BridgeUpdate(context.Background(), transport, identity, outcome)
}

// Turn records one delivered chat turn, ok false on a turn failure.
func (m *bridgeMetricsAdapter) Turn(transport, identity string, ok bool) {
	m.rec.BridgeTurn(context.Background(), transport, identity, ok)
}

// Confirm records one resolved D9 confirmation by outcome: approved, denied,
// expired, or refused.
func (m *bridgeMetricsAdapter) Confirm(transport, identity, outcome string) {
	m.rec.BridgeConfirm(context.Background(), transport, identity, outcome)
}

// SendFailure records one outbound send that failed after the turn completed.
func (m *bridgeMetricsAdapter) SendFailure(transport, identity string) {
	m.rec.BridgeSendFailure(context.Background(), transport, identity)
}
