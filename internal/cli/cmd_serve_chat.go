package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/skills"
	sourcesync "github.com/chadit/CairnOS/internal/source"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/telemetry"
	"github.com/chadit/CairnOS/internal/vault"
	"github.com/chadit/CairnOS/pkg/runner"
)

// failServe reports a serve setup failure, announces it to the configured
// notify destinations, flushes telemetry, and returns the failure exit code, so
// the runServe wiring stays under the complexity ceiling instead of repeating
// the same sequence at every setup step.
func (a *App) failServe(ctx context.Context, tel *telemetry.Telemetry, stage string, err error) int {
	a.printErrf("serve: %s: %v\n", stage, err)
	a.notifyServeFailure(ctx, stage, err)
	a.closeTelemetry(ctx, tel)

	return ExitFailure
}

// prepareSources brings every configured source up before anything reads one,
// and returns the specs the runner registry needs. A workflow can live inside
// the tree it syncs, so serve has no business starting on a host where the first
// clone failed or the tree on disk points at another repository.
func (a *App) prepareSources(ctx context.Context, cfg *config.Config, tel *telemetry.Telemetry) ([]sourcesync.Spec, int, bool) {
	specs := sourceSpecs(cfg.Sources)

	if err := sourcesync.EnsureAll(ctx, sourcesync.GitCommand, specs, a.logger); err != nil {
		return nil, a.failServe(ctx, tel, "sources", err), false
	}

	return specs, ExitOK, true
}

// reconcileSources runs workflow discovery once per source at start, after the
// checkouts exist, so a workflow committed while serve was down is scheduled
// before the first sync tick rather than after it.
func reconcileSources(ctx context.Context, specs []sourcesync.Spec, reconcile builtins.SourceReconciler) {
	for _, spec := range specs {
		reconcile(ctx, spec.Name)
	}
}

// attachChat builds the chat agent and, when chat is enabled, mounts it onto
// the API config and logs its endpoint. It returns an exit code and false on a
// wiring failure, so the serve flow stays under the complexity ceiling. With
// chat disabled, the agent is nil and ChatService stays unmounted.
func (a *App) attachChat(ctx context.Context, cfg *config.Config, tel *telemetry.Telemetry, apiCfg *api.Config, deps *chatDeps) (*chat.Agent, *chat.Personalities, int, bool) {
	agent, personalities, err := buildChatAgent(cfg, deps)
	if err != nil {
		return nil, nil, a.failServe(ctx, tel, "chat", err), false
	}

	if agent != nil {
		apiCfg.Chat = agent

		a.printOutf("chat available at %s/v1/chat\n", "http://"+cfg.API.HTTP)
	}

	return agent, personalities, ExitOK, true
}

// buildChatAgent wires the chat agent when [chat] is enabled. It resolves the
// configured driver to a runner.ToolCaller, builds the memory and vault read
// tools plus the gated run_workflow descriptor, and hands it a workflow trigger
// over the run manager so a confirmed proposal fires a run. A disabled [chat]
// returns a nil agent, keeping ChatService unmounted.
func buildChatAgent(cfg *config.Config, deps *chatDeps) (*chat.Agent, *chat.Personalities, error) {
	if !cfg.Chat.Enabled {
		// A nil agent is the documented "chat disabled" state, not an error.
		return nil, nil, nil
	}

	if cfg.Chat.Driver == "" {
		return nil, nil, fmt.Errorf("%w: chat.driver must name an LLM runner when chat is enabled", config.ErrInvalidConfig)
	}

	// The agent loop's run-start check, carried to chat: a driver that is not a
	// registered ToolCaller fails wiring rather than failing the first turn.
	registered, found := deps.registry.Get(cfg.Chat.Driver)
	if !found {
		return nil, nil, fmt.Errorf("%w: chat.driver %q is not a registered runner", config.ErrInvalidConfig, cfg.Chat.Driver)
	}

	driver, capable := registered.(runner.ToolCaller)
	if !capable {
		return nil, nil, fmt.Errorf("%w: chat.driver %q does not support tool calling; use anthropic or openai",
			config.ErrInvalidConfig, cfg.Chat.Driver)
	}

	// One adapter feeds both the system-prompt listing (via Config.Skills) and
	// the skill read tool (via chatReadTools), so the two description-match paths
	// see the same library.
	source := skillSourceAdapter{lib: skills.New(deps.skillsDir, deps.logger)}

	tools := chat.BuildToolSet(chatReadTools(deps.memory, deps.vault, source, deps.registry))

	trigger := api.NewChatWorkflowTrigger(deps.manager, api.NewWorkflowResolver(deps.workflowsDir, deps.logger))

	// Built once here so the OpenAI-compatible adapter and the chat agent share
	// the same resolved personality set rather than each building its own.
	personalities := chat.NewPersonalities(cfg.Chat.Personalities, cfg.Chat.DefaultPersonality)

	agent := chat.New(&chat.Config{
		Store:         deps.store,
		Driver:        driver,
		Tools:         tools.Tools,
		Specs:         tools.Specs,
		Personalities: personalities,
		Skills:        source,
		ContextDir:    deps.contextDir,
		FileAllowlist: cfg.Chat.FileAllowlist,
		MaxFileBytes:  cfg.Chat.MaxFileBytes,
		Trigger:       trigger,
		TurnBudgetUSD: cfg.Chat.TurnBudgetUSD,
		NewID:         store.NewID,
	})

	return agent, personalities, nil
}

// skillSourceAdapter adapts the skills library to chat.SkillSource, keeping
// internal/chat free of an internal/skills import (the WorkflowTrigger seam
// pattern). It maps skills.ErrNotFound onto chat.ErrSkillNotFound so the chat
// side matches on its own sentinel.
type skillSourceAdapter struct {
	lib *skills.Library
}

// List implements chat.SkillSource.
func (a skillSourceAdapter) List() []chat.SkillInfo {
	loaded := a.lib.List()
	infos := make([]chat.SkillInfo, len(loaded))

	for idx := range loaded {
		infos[idx] = chat.SkillInfo{Name: loaded[idx].Name, Description: loaded[idx].Description}
	}

	return infos
}

// Load implements chat.SkillSource.
func (a skillSourceAdapter) Load(name string) (string, error) {
	skill, err := a.lib.Load(name)
	if err != nil {
		if errors.Is(err, skills.ErrNotFound) {
			return "", chat.ErrSkillNotFound
		}

		return "", fmt.Errorf("load skill: %w", err)
	}

	return skill.Body, nil
}

// chatDeps groups the chat agent's wiring dependencies, so buildChatAgent takes
// one argument instead of a long parameter list.
type chatDeps struct {
	store        store.ChatStore
	memory       memory.Store
	vault        vault.Store
	registry     *runner.Registry
	manager      *api.Manager
	logger       *slog.Logger
	workflowsDir string
	skillsDir    string
	contextDir   string
}

// chatReadTools builds the read tools the chat agent runs inline: memory, vault
// query, skill, and the usage probe, each only when its dependency is present.
// The probe is the registry's own instance, so chat reads the same launchers and
// hosts a workflow action does.
func chatReadTools(
	mem memory.Store, vaultStore vault.Store, skillSource chat.SkillSource, registry *runner.Registry,
) map[string]runner.Runner {
	tools := map[string]runner.Runner{}

	if mem != nil {
		tools[chat.ToolMemory] = builtins.NewMemoryTool(mem)
	}

	if vaultStore != nil {
		tools[chat.ToolVaultQuery] = builtins.NewVaultQueryTool(vaultStore)
	}

	if skillSource != nil {
		tools[chat.SkillToolName] = chat.NewSkillTool(skillSource)
	}

	if registry == nil {
		return tools
	}

	if usage, built := registry.Get(builtins.UsageName); built {
		tools[chat.ToolUsage] = usage
	}

	return tools
}
