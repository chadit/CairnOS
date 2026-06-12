package cli

import (
	"context"
	"flag"
	"path/filepath"

	"github.com/chadit/CairnOS/internal/agentloop"
	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/mcp"
	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/rules"
	"github.com/chadit/CairnOS/internal/store"
)

const mcpUsage = `usage: cairnos mcp serve [--dir D]

  serve  Run CairnOS as a stdio MCP server, so an MCP client (Claude, ChatGPT,
         others) can spawn this process and call its tools and resources over
         stdin and stdout. Auth is the process boundary here, not a token.
`

// cmdMCP dispatches the Phase 7a MCP subcommands
// (.agents/spec/10-mcp-integration.md, "MCP Server"). Only the stdio serve
// path lives here; the http transport mounts on the serve process's API mux.
func (a *App) cmdMCP(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.printErrf("%s", mcpUsage)

		return ExitUsage
	}

	switch args[0] {
	case "serve":
		return a.cmdMCPServe(ctx, args[1:])
	default:
		a.printErrf("unknown mcp subcommand %q\n", args[0])
		a.printErrf("%s", mcpUsage)

		return ExitUsage
	}
}

// cmdMCPServe runs the stdio MCP server until the client disconnects or the
// process is interrupted. It wires the same store, memory, registry, and engine
// the API serves over, then drives the MCP server over stdio. The authenticator
// is deliberately nil: a stdio MCP server is spawned by the client as a local
// subprocess, so the process boundary is the access control, and there is no
// HTTP header to carry a token on anyway.
func (a *App) cmdMCPServe(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("mcp serve", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	if flags.NArg() > 0 {
		a.printErrf("%s", mcpUsage)

		return ExitUsage
	}

	dir, err := resolveDir(*dirFlag)
	if err != nil {
		a.printErrf("mcp serve: %v\n", err)

		return ExitFailure
	}

	db, cfg, code, proceed := a.openStoreAt(ctx, "mcp serve", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	mem, code, proceed := a.openMemoryAt(ctx, "mcp serve", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(mem)

	rulesSvc, code, proceed := a.openRulesAt(ctx, "mcp serve", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(rulesSvc)

	return a.runMCPStdio(ctx, &cfg, dir, db, mem, rulesSvc)
}

// runMCPStdio builds the engine and the MCP service, then serves over stdio
// until ctx cancels. It mirrors the serve wiring's run-side spine (manager,
// engine, registry) without the telemetry, scheduler, and API listeners a stdio
// MCP server has no use for.
func (a *App) runMCPStdio(ctx context.Context, cfg *config.Config, dir string, db store.Store, mem memory.Store, rulesSvc rules.Service) int {
	registry, err := newRegistry(ctx, cfg.Runners, mem, rulesSvc)
	if err != nil {
		a.printErrf("mcp serve: %v\n", err)

		return ExitFailure
	}
	defer a.closeRegistry(registry)

	manager := api.NewManager(a.logger)

	engCfg := engineConfig(cfg)
	engCfg.NewID = manager.NewID
	engCfg.OnEvent = manager.Dispatch
	engCfg.AgentExecutor = agentloop.New(registry, mem, db, nil)
	engCfg.Budget = db
	engCfg.Pause = db

	manager.Bind(engine.New(engCfg, registry, db))

	workflowsDir := mcpWorkflowsDir(cfg, dir)

	service, err := mcp.NewService(&mcp.Config{
		Manager:      manager,
		Resolver:     api.NewWorkflowResolver(workflowsDir, a.logger),
		Store:        db,
		Memory:       mem,
		Rules:        newRulesLookup(rulesSvc),
		WorkflowsDir: workflowsDir,
		Logger:       a.logger,
	})
	if err != nil {
		a.printErrf("mcp serve: %v\n", err)

		return ExitFailure
	}

	if err := service.ServeStdio(ctx); err != nil {
		a.printErrf("mcp serve: %v\n", err)

		return ExitFailure
	}

	return ExitOK
}

// mcpWorkflowsDir picks the workflows directory the MCP surface lists and
// resolves names in: the [mcp].workflows_dir override when set, else the node's
// standard workflows directory under the home, the same one run and serve use.
func mcpWorkflowsDir(cfg *config.Config, dir string) string {
	if cfg.MCP.WorkflowsDir != "" {
		return cfg.MCP.WorkflowsDir
	}

	return filepath.Join(dir, workflowsDirName)
}
