package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/openaicompat"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/telemetry"
)

// attachOpenAICompatServe mounts the adapter and logs its endpoint, returning an
// exit code and false on a wiring failure so the serve flow stays under the
// complexity ceiling, the same shape attachChat uses. With openai_compat off,
// nothing mounts and it reports success.
func (a *App) attachOpenAICompatServe(ctx context.Context, cfg *config.Config, tel *telemetry.Telemetry, apiCfg *api.Config, deps *openaiCompatDeps) (int, bool) {
	mounted, err := attachOpenAICompat(cfg, deps, apiCfg)
	if err != nil {
		return a.failServe(ctx, tel, "openai-compat", err), false
	}

	if mounted {
		a.printOutf("openai-compatible chat available at %s/v1/chat/completions\n", "http://"+cfg.API.HTTP)
	}

	return ExitOK, true
}

// openaiCompatDeps groups the seams the OpenAI-compatible adapter shares with
// the rest of serve, so attachOpenAICompat takes one argument rather than a long
// parameter list. The agent and personalities come straight from attachChat, so
// the adapter and the native chat surface run over the same objects.
type openaiCompatDeps struct {
	agent         *chat.Agent
	personalities *chat.Personalities
	store         store.ChatStore
	authn         *auth.Authenticator
	logger        *slog.Logger
}

// attachOpenAICompat mounts the OpenAI-compatible chat completions adapter on
// the API server when [chat] openai_compat is set, reusing the chat agent, the
// personality set, the store, and the authenticator the rest of serve runs over
// (.agents/spec/parity/openai-compat.md). It returns whether it mounted, so the
// caller prints the banner, and an error only when the wiring is wrong.
//
// openai_compat with chat disabled is a configuration error: the adapter has no
// agent to drive, so serve fails fast rather than mounting a broken endpoint. A
// typed-nil authn is normalized to a true nil interface, the same care attachMCP
// takes, so "auth disabled" reads correctly inside the service.
func attachOpenAICompat(cfg *config.Config, deps *openaiCompatDeps, apiCfg *api.Config) (bool, error) {
	if !cfg.Chat.OpenAICompat {
		return false, nil
	}

	if deps.agent == nil {
		return false, fmt.Errorf("%w: chat.openai_compat requires chat.enabled", config.ErrInvalidConfig)
	}

	compatCfg := &openaicompat.Config{
		Agent:         deps.agent,
		Store:         deps.store,
		Personalities: deps.personalities,
		MaxBodyBytes:  cfg.API.MaxBodyBytes,
		Logger:        deps.logger,
	}

	if deps.authn != nil {
		compatCfg.Auth = deps.authn
	}

	service, err := openaicompat.NewService(compatCfg)
	if err != nil {
		return false, fmt.Errorf("build openai-compat service: %w", err)
	}

	apiCfg.Extensions = append(apiCfg.Extensions, service.Extension())

	return true, nil
}
