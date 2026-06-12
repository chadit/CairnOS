package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/telemetry"
	"github.com/chadit/CairnOS/internal/vault"
	"github.com/chadit/CairnOS/pkg/runner"
)

// failServe reports a serve setup failure, flushes telemetry, and returns the
// failure exit code, so the runServe wiring stays under the complexity ceiling
// instead of repeating the print-flush-return triple at every setup step.
func (a *App) failServe(ctx context.Context, tel *telemetry.Telemetry, stage string, err error) int {
	a.printErrf("serve: %s: %v\n", stage, err)
	a.closeTelemetry(ctx, tel)

	return ExitFailure
}

// attachChat builds the chat agent and, when chat is enabled, mounts it onto
// the API config and logs its endpoint. It returns an exit code and false on a
// wiring failure, so the serve flow stays under the complexity ceiling. With
// chat disabled, the agent is nil and ChatService stays unmounted.
func (a *App) attachChat(ctx context.Context, cfg *config.Config, tel *telemetry.Telemetry, apiCfg *api.Config, deps *chatDeps) (int, bool) {
	agent, err := buildChatAgent(cfg, deps)
	if err != nil {
		return a.failServe(ctx, tel, "chat", err), false
	}

	if agent != nil {
		apiCfg.Chat = agent

		a.printOutf("chat available at %s/v1/chat\n", "http://"+cfg.API.HTTP)
	}

	return ExitOK, true
}

// buildChatAgent wires the Phase 9 chat agent for serve when [chat] is enabled
// (.agents/spec/13-roadmap.md, Phase 9). It resolves the configured driver to
// a runner.ToolCaller (the same capability an agent action's driver needs),
// builds the memory and vault read tools plus the gated run_workflow
// descriptor, and hands the chat agent a workflow trigger over the run manager
// so a confirmed proposal fires a real run. A disabled [chat] returns a nil
// agent, which keeps ChatService unmounted.
func buildChatAgent(cfg *config.Config, deps *chatDeps) (*chat.Agent, error) {
	if !cfg.Chat.Enabled {
		return nil, nil //nolint:nilnil // a nil agent is the documented "chat disabled" state, not an error
	}

	driver, err := chatDriver(deps.registry, cfg.Chat.Driver)
	if err != nil {
		return nil, err
	}

	tools := chat.BuildToolSet(chatReadTools(deps.memory, deps.vault))

	trigger := api.NewChatWorkflowTrigger(deps.manager, api.NewWorkflowResolver(deps.workflowsDir, deps.logger))

	agent := chat.New(&chat.Config{
		Store:         deps.store,
		Driver:        driver,
		Tools:         tools.Tools,
		Specs:         tools.Specs,
		Personalities: chat.NewPersonalities(cfg.Chat.Personalities, cfg.Chat.DefaultPersonality),
		Trigger:       trigger,
		TurnBudgetUSD: cfg.Chat.TurnBudgetUSD,
		NewID:         store.NewID,
	})

	return agent, nil
}

// chatDeps groups the chat agent's wiring dependencies, so buildChatAgent takes
// one argument instead of a long parameter list.
type chatDeps struct {
	store        store.ChatStore
	memory       memory.Store
	vault        vault.Store
	registry     *runner.Registry
	manager      *api.Manager
	workflowsDir string
	logger       *slog.Logger
}

// chatDriver resolves the configured driver name to a runner.ToolCaller, the
// run-start check from the agent loop carried to chat: a driver that is not a
// registered ToolCaller fails wiring rather than failing the first turn.
func chatDriver(registry *runner.Registry, name string) (runner.ToolCaller, error) {
	if name == "" {
		return nil, fmt.Errorf("%w: chat.driver must name an LLM runner when chat is enabled", config.ErrInvalidConfig)
	}

	rnr, registered := registry.Get(name)
	if !registered {
		return nil, fmt.Errorf("%w: chat.driver %q is not a registered runner", config.ErrInvalidConfig, name)
	}

	caller, capable := rnr.(runner.ToolCaller)
	if !capable {
		return nil, fmt.Errorf("%w: chat.driver %q does not support tool calling; use anthropic or openai",
			config.ErrInvalidConfig, name)
	}

	return caller, nil
}

// chatReadTools builds the read tools the chat agent runs inline: the memory
// tool when a memory store is configured, and the vault query tool when a vault
// store is configured. A nil store leaves its tool out, so a node without a
// vault still runs chat with memory and the gate.
func chatReadTools(mem memory.Store, vaultStore vault.Store) map[string]runner.Runner {
	tools := map[string]runner.Runner{}

	if mem != nil {
		tools[chat.ToolMemory] = builtins.NewMemoryTool(mem)
	}

	if vaultStore != nil {
		tools[chat.ToolVaultQuery] = builtins.NewVaultQueryTool(vaultStore)
	}

	return tools
}
