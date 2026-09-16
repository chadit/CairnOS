package cli

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/chadit/CairnOS/internal/bridge/mattermost"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/messaging"
	"github.com/chadit/CairnOS/internal/telemetry"
)

// mattermostStage names the Mattermost setup step in failServe diagnostics.
const mattermostStage = "mattermost bridge"

// buildMattermostBridge builds the bridge from the declared identities and
// registers each send surface, so the send service, CLI, message runner, and
// notify sink post through the client the bridge answers on (FR33).
//
// A nil bridge with ExitOK means nothing to run: no identity, or none with a
// token (FR2). An identity with [chat] off fails with config.ErrInvalidConfig
// (FR4).
func (a *App) buildMattermostBridge(
	ctx context.Context,
	cfg *config.Config,
	tel *telemetry.Telemetry,
	deps *bridgeDeps,
	registry *messaging.Registry,
) (*mattermost.Bridge, int, bool) {
	settings := &cfg.Bridge.Mattermost

	if len(settings.Identity) == 0 {
		deps.logger.Info("mattermost bridge disabled: no identity declared")

		return nil, ExitOK, true
	}

	// The bridge drives the chat agent, so a declared identity with chat off has
	// nothing to run. A typed-nil *chat.Agent never reaches the core's interface
	// field, which is what keeps this nil check meaningful.
	if deps.chat == nil {
		return nil, a.failServe(ctx, tel, mattermostStage,
			fmt.Errorf("%w: bridge.mattermost.identity requires [chat] enabled", config.ErrInvalidConfig)), false
	}

	identities, err := mattermostIdentities(settings, deps.logger)
	if err != nil {
		return nil, a.failServe(ctx, tel, mattermostStage, err), false
	}

	if len(identities) == 0 {
		return nil, ExitOK, true
	}

	built := mattermost.New(&mattermost.Config{
		Core:       deps.core,
		Metrics:    deps.metrics,
		Logger:     deps.logger,
		Identities: identities,
		Server:     *settings,
	})

	for name, sender := range built.Senders() {
		registry.Add(name, sender)
	}

	return built, ExitOK, true
}

// mattermostIdentities builds one client per identity whose token env is set,
// logging one info line for each it skips (FR2). Every client shares one rate
// bucket because Mattermost keys its own limit by source IP (decision D5). Name
// order makes the log lines and any failure reproducible across starts.
func mattermostIdentities(settings *config.Mattermost, logger *slog.Logger) ([]mattermost.Identity, error) {
	limiter := mattermost.NewBucket(mattermost.BucketConfig{
		RatePerSecond: float64(settings.RatePerSec),
		Burst:         float64(settings.RateBurst),
	})

	identities := make([]mattermost.Identity, 0, len(settings.Identity))

	for _, name := range slices.Sorted(maps.Keys(settings.Identity)) {
		identity := settings.Identity[name]

		token, env := config.ResolveMattermostToken(&identity, name, os.Getenv)
		if token == "" {
			logger.Info("mattermost identity disabled: no token env set", "identity", name,
				"tried", strings.Join(config.MattermostTokenEnvs(&identity, name), " "))

			continue
		}

		logger.Debug("mattermost identity token resolved", "identity", name, "env", env)

		client, err := mattermost.NewClient(mattermost.ClientConfig{
			Limiter:      limiter,
			Log:          logger,
			URL:          settings.URL,
			Token:        token,
			Timeout:      settings.SendTimeout,
			InsecureHTTP: settings.InsecureHTTP,
		})
		if err != nil {
			return nil, fmt.Errorf("%w: bridge.mattermost.identity.%s: %w", config.ErrInvalidConfig, name, err)
		}

		identities = append(identities, mattermost.Identity{
			Client: client,
			Name:   name,
			Config: settings.Identity[name],
		})
	}

	return identities, nil
}
