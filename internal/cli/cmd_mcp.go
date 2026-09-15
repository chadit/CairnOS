package cli

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"

	"github.com/chadit/CairnOS/internal/agentloop"
	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/hostenv"
	"github.com/chadit/CairnOS/internal/mcp"
	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/rules"
	"github.com/chadit/CairnOS/internal/store"
)

const mcpUsage = `usage: cairnos mcp <serve|publish|import> [--dir D]

  serve    Run CairnOS as a stdio MCP server, so an MCP client (Claude, ChatGPT,
           others) can spawn this process and call its tools and resources over
           stdin and stdout. Auth is the process boundary here, not a token.
  publish  Register this node's MCP tools with the configured registry so other
           nodes can discover them. Needs [mcp] registry_url and advertise_url;
           the registry token comes from CAIRNOS_MCP_REGISTRY_TOKEN. Use
           --dry-run to print the record without contacting the registry.
  import   Pull registry records and print mcp_client runner config blocks on
           stdout to paste into your config. Needs [mcp] registry_url.
`

// mcpRegistryTokenEnv holds the bearer token for the MCP registry. It is a
// secret, so it rides the environment rather than the config file, the same
// convention runner API keys and the store DSN follow.
const mcpRegistryTokenEnv = "CAIRNOS_MCP_REGISTRY_TOKEN" // #nosec G101 -- environment variable name, not a credential

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
	case "publish":
		return a.cmdMCPPublish(ctx, args[1:])
	case subcmdImport:
		return a.cmdMCPImport(ctx, args[1:])
	default:
		a.printErrf("unknown mcp subcommand %q\n", args[0])
		a.printErrf("%s", mcpUsage)

		return ExitUsage
	}
}

// cmdMCPServe runs the stdio MCP server until the client disconnects or the
// process is interrupted, wiring the same store, memory, registry, and engine
// the API serves over. The authenticator is deliberately nil: the client spawns
// this server as a local subprocess, so the process boundary is the access
// control, and there is no HTTP header to carry a token.
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
	// The source is built before the registry so the credential manager and the
	// runners share one secret chain.
	source, closeSource, err := a.buildSecretSource(ctx, dir, cfg)
	if err != nil {
		a.printErrf("mcp serve: %v\n", err)

		return ExitFailure
	}
	defer func() { _ = closeSource() }()

	// Outbound credentials resolve their secret refs through the source so the
	// http and mcp_client runners mint bearer tokens. Construction does no
	// network and fails fast on a missing ref.
	credManager, err := buildCredentialManager(ctx, cfg, source)
	if err != nil {
		a.printErrf("mcp serve: %v\n", err)

		return ExitFailure
	}

	registry, err := newRegistry(ctx, cfg.Runners, sourceSpecs(cfg.Sources), hostenv.Render(cfg.Hosts), mem, rulesSvc, credManager, nil)
	if err != nil {
		a.printErrf("mcp serve: %v\n", err)

		return ExitFailure
	}
	defer a.closeRegistry(registry)

	manager := api.NewManager(a.logger)

	engCfg := engineConfig(cfg, source)
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

// cmdMCPPublish registers this node's MCP tool catalog with the configured
// registry so other nodes can discover and import it
// (.agents/spec/10-mcp-integration.md, "MCP Registry"). It reads only the config
// and the network, opening no store: the catalog is built from the same
// constants the server registers, so publish works while serve holds the
// database. --dry-run prints the record without contacting the registry.
func (a *App) cmdMCPPublish(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("mcp publish", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)
	dryRun := flags.Bool("dry-run", false, "print the server record as JSON without contacting the registry")

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	if flags.NArg() > 0 {
		a.printErrf("%s", mcpUsage)

		return ExitUsage
	}

	cfg, code, proceed := a.loadConfigAt(*dirFlag, "mcp publish")
	if !proceed {
		return code
	}

	if cfg.MCP.AdvertiseURL == "" {
		a.printErrf("mcp publish: set [mcp] advertise_url (or CAIRNOS_MCP_ADVERTISE_URL) to this node's reachable MCP endpoint\n")

		return ExitUsage
	}

	// Every CairnOS node wires a rules store, so the served surface always
	// includes get_language_rules; the published record reflects that.
	record := mcp.ServerRecord(cfg.MCP.AdvertiseURL, true)

	if *dryRun {
		return a.printMCPRecord(&record)
	}

	if cfg.MCP.RegistryURL == "" {
		a.printErrf("mcp publish: set [mcp] registry_url (or CAIRNOS_MCP_REGISTRY_URL) first, or use --dry-run\n")

		return ExitUsage
	}

	client, err := mcp.NewRegistryClient(cfg.MCP.RegistryURL, os.Getenv(mcpRegistryTokenEnv), nil)
	if err != nil {
		a.printErrf("mcp publish: %v\n", err)

		return ExitFailure
	}

	if err := client.Publish(ctx, &record); err != nil {
		a.printErrf("mcp publish: %v\n", err)

		return ExitFailure
	}

	a.printOutf("published %d tools to %s\n", len(record.Tools), cfg.MCP.RegistryURL)

	return ExitOK
}

// printMCPRecord renders the server record as indented JSON for the --dry-run
// path, so an operator sees what publish would send before sending it.
func (a *App) printMCPRecord(record *mcp.ServerEntry) int {
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		a.printErrf("mcp publish: %v\n", err)

		return ExitFailure
	}

	a.printOutf("%s\n", encoded)

	return ExitOK
}

// cmdMCPImport pulls registry records and prints mcp_client runner config blocks
// on stdout, one per remote tool, for an operator to paste into their config
// (.agents/spec/10-mcp-integration.md, "MCP Registry"). The status line goes to
// stderr, so `cairnos mcp import >> config.toml` captures only the config.
func (a *App) cmdMCPImport(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("mcp import", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	if flags.NArg() > 0 {
		a.printErrf("%s", mcpUsage)

		return ExitUsage
	}

	cfg, code, proceed := a.loadConfigAt(*dirFlag, "mcp import")
	if !proceed {
		return code
	}

	if cfg.MCP.RegistryURL == "" {
		a.printErrf("mcp import: set [mcp] registry_url (or CAIRNOS_MCP_REGISTRY_URL) first\n")

		return ExitUsage
	}

	client, err := mcp.NewRegistryClient(cfg.MCP.RegistryURL, os.Getenv(mcpRegistryTokenEnv), nil)
	if err != nil {
		a.printErrf("mcp import: %v\n", err)

		return ExitFailure
	}

	servers, err := client.List(ctx)
	if err != nil {
		a.printErrf("mcp import: %v\n", err)

		return ExitFailure
	}

	rendered, names := mcp.ImportConfig(servers, cfg.MCP.AdvertiseURL)
	a.printOutf("%s", rendered)

	if len(names) == 0 {
		a.printErrf("mcp import: registry returned no importable tools\n")

		return ExitOK
	}

	a.printErrf("imported %d runner block(s) from %s\n", len(names), cfg.MCP.RegistryURL)

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
