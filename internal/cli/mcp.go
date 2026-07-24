package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/mcp"
	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/rules"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/telemetry"
)

// attachMCPServe mounts the MCP http transport and logs its endpoint, returning
// an exit code and false on a wiring failure so the serve flow stays under the
// complexity ceiling, the same shape attachChat uses. With MCP disabled or set
// to stdio, nothing mounts and it reports success.
func (a *App) attachMCPServe(ctx context.Context, cfg *config.Config, tel *telemetry.Telemetry, apiCfg *api.Config, deps *mcpServeDeps) (int, bool) {
	path, err := attachMCP(cfg, deps, apiCfg)
	if err != nil {
		return a.failServe(ctx, tel, "mcp", err), false
	}

	if path != "" {
		a.printOutf("mcp server available at %s%s\n", "http://"+cfg.API.HTTP, path)
	}

	return ExitOK, true
}

// mcpServeDeps groups the seams the MCP http transport shares with the rest of
// the serve process, so attachMCP takes one argument rather than a long
// parameter list (gocritic paramTypeCombine, varnamelen on a wide signature).
type mcpServeDeps struct {
	manager      *api.Manager
	store        store.Store
	memory       memory.Store
	rules        rules.Service
	workflowsDir string
	authn        *auth.Authenticator
	logger       *slog.Logger
}

// attachMCP mounts the MCP http transport on the API server when [mcp] is
// enabled with the http transport, reusing the same store, memory, manager, and
// authenticator the rest of serve runs over. It returns the mounted path for
// the serve banner, the empty string when MCP is disabled or set to the stdio
// transport (which serve does not mount), and an error only when the service
// cannot be built.
//
// The authenticator is threaded so MCP enforces the same scopes the gRPC
// interceptor does. A typed-nil authn is normalized to a true nil interface, so
// "auth disabled" reads correctly inside the service rather than as a non-nil
// interface wrapping a nil pointer.
func attachMCP(cfg *config.Config, deps *mcpServeDeps, apiCfg *api.Config) (string, error) {
	if !cfg.MCP.Enabled || transportIsStdio(cfg.MCP.Transport) {
		return "", nil
	}

	mcpCfg := &mcp.Config{
		Manager:      deps.manager,
		Resolver:     api.NewWorkflowResolver(deps.workflowsDir, deps.logger),
		Store:        deps.store,
		Memory:       deps.memory,
		WorkflowsDir: deps.workflowsDir,
		Logger:       deps.logger,
	}

	// The get_language_rules tool mounts only when a rules store is wired, so
	// the bridge wraps it here; a nil rules store leaves the tool absent.
	if deps.rules != nil {
		mcpCfg.Rules = newRulesLookup(deps.rules)
	}

	if deps.authn != nil {
		mcpCfg.Auth = deps.authn
	}

	service, err := mcp.NewService(mcpCfg)
	if err != nil {
		return "", fmt.Errorf("build mcp service: %w", err)
	}

	path := mcpHTTPPath(cfg.MCP.Path)
	apiCfg.Extensions = append(apiCfg.Extensions, service.Extension(path))

	return path, nil
}

// transportIsStdio reports whether the configured transport is stdio, the one
// value serve does not mount (the mcp serve subcommand serves it instead).
func transportIsStdio(transport string) bool {
	return transport == config.MCPTransportStdio
}

// mcpHTTPPath resolves the mount path, defaulting an empty config to the
// package default so the banner and the mount agree.
func mcpHTTPPath(path string) string {
	if path == "" {
		return mcp.DefaultHTTPPath
	}

	return path
}
