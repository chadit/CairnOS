package mcp

import (
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/store"
)

// serverName and serverVersion identify this MCP server in the handshake.
const (
	serverName        = "cairnos"
	serverVersion     = "0.1.0"
	serverDescription = "CairnOS workflow and agent orchestration node."
)

// Tool and resource names, plus the resource URIs an external client lists and
// reads. The URIs use the cairnos scheme so they are absolute, which the SDK's
// AddResource requires.
const (
	toolRunWorkflow      = "run_workflow"
	toolQueryMemory      = "query_memory"
	toolGetLanguageRules = "get_language_rules"

	resourceWorkflowsName = "workflows"
	resourceWorkflowsURI  = "cairnos://workflows"
	resourceRunsName      = "runs"
	resourceRunsURI       = "cairnos://runs"
)

// Tool descriptions, one source for both build (which registers them with the
// SDK) and Catalog (which publishes them to the registry), so a published
// record never drifts from the description an MCP client actually sees.
const (
	descRunWorkflow      = "Start a CairnOS run by workflow name with inputs. Returns the created run id and its status; stream progress over A2A or gRPC."
	descQueryMemory      = "Search CairnOS memory for entries matching text, optionally narrowed by scope and tags."
	descGetLanguageRules = "Get the merged coding rules for a language, optionally narrowed to a project. Project rules override language defaults."
)

// triggerMCP marks runs started over MCP in run history. The store reserves the
// trigger vocabulary (cli|schedule|api|a2a|webhook); MCP rides "api" because an
// MCP run_workflow is the same "external client started a run by name" shape the
// API's RunWorkflow is, and adding a new enum value would ripple through every
// store backend's check constraint for no behavioral gain at 7a.
const triggerMCP = "api"

// Config wires one MCP Service. The seams are the same ones the gRPC API and
// the A2A adapter reuse; the MCP server owns none of them.
type Config struct {
	// Store is the durable run store the runs resource lists and run_workflow
	// reads a created run back from.
	Store store.Store

	// Memory backs the query_memory tool.
	Memory memory.Store

	// Rules backs the get_language_rules tool. Nil leaves that tool out of the
	// server, so a node without a rules store exposes an honestly absent tool
	// rather than one that always errors (the stance 7a took before the rules
	// service existed) (.agents/spec/10-mcp-integration.md, get_language_rules).
	Rules RulesLookup

	// Auth turns on token enforcement. Nil disables auth: every call passes on
	// the default workspace, matching the rest of the API.
	Auth Authenticator

	// Manager owns run starts and event fan-out; run_workflow starts runs
	// through it, the same path the API and A2A use.
	Manager *api.Manager

	// Resolver turns a workflow name into a loaded flow for run_workflow,
	// freshly on every call so edits apply without a restart.
	Resolver *api.WorkflowResolver

	// Logger receives the server's operational events; nil means slog.Default.
	Logger *slog.Logger

	// WorkflowsDir holds the workflow files the workflows resource lists.
	WorkflowsDir string
}

// Service is the MCP protocol surface over the shared CairnOS seams. One
// Service builds many *mcp.Server values (one per HTTP request, one per stdio
// process), each closing over the bearer token for that connection, so the
// tool and resource handlers enforce scopes without threading a request context
// through the SDK's typed handler signatures.
type Service struct {
	store         store.Store
	memory        memory.Store
	rules         RulesLookup
	authenticator Authenticator
	manager       *api.Manager
	resolver      *api.WorkflowResolver
	logger        *slog.Logger
	workflowsDir  string
}

// NewService validates the wiring and returns the Service, failing fast when a
// required seam is missing so a misconfigured node never serves an MCP surface
// it cannot back. A nil authenticator is allowed and means auth is disabled.
func NewService(cfg *Config) (*Service, error) {
	if cfg.Manager == nil {
		return nil, ErrNoManager
	}

	if cfg.Resolver == nil {
		return nil, ErrNoResolver
	}

	if cfg.Store == nil {
		return nil, ErrNoStore
	}

	if cfg.Memory == nil {
		return nil, ErrNoMemory
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Service{
		manager:       cfg.Manager,
		resolver:      cfg.Resolver,
		store:         cfg.Store,
		memory:        cfg.Memory,
		rules:         cfg.Rules,
		workflowsDir:  cfg.WorkflowsDir,
		authenticator: cfg.Auth,
		logger:        logger,
	}, nil
}

// AuthEnabled reports whether token enforcement is on, so the transport layer
// can decide whether to reject a tokenless HTTP request before it ever reaches
// the SDK. Stdio never asks: there, auth is the process boundary.
func (s *Service) AuthEnabled() bool {
	return s.authenticator != nil
}

// Catalog returns the tool surface this node serves, the same gating build
// applies: get_language_rules appears only when a rules store is wired. It
// shares ServerRecord's tool list so a published record never drifts from the
// running server (.agents/spec/10-mcp-integration.md, "MCP Registry").
func (s *Service) Catalog() []ToolEntry {
	return ServerRecord("", s.rules != nil).Tools
}

// build assembles an *mcp.Server bound to one connection's token. Every
// handler closes over token because the SDK carries no request context: HTTP
// extracts it per request, and stdio passes the empty string since auth there
// is the process boundary.
//
// get_language_rules registers only with a rules store wired. Without one the
// tool stays absent rather than erroring on every call
// (.agents/spec/10-mcp-integration.md).
func (s *Service) build(token string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: serverVersion}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        toolRunWorkflow,
		Description: descRunWorkflow,
	}, s.runWorkflowHandler(token))

	mcp.AddTool(server, &mcp.Tool{
		Name:        toolQueryMemory,
		Description: descQueryMemory,
	}, s.queryMemoryHandler(token))

	if s.rules != nil {
		mcp.AddTool(server, &mcp.Tool{
			Name:        toolGetLanguageRules,
			Description: descGetLanguageRules,
		}, s.getLanguageRulesHandler(token))
	}

	server.AddResource(&mcp.Resource{
		Name:        resourceWorkflowsName,
		URI:         resourceWorkflowsURI,
		MIMEType:    mimeJSON,
		Description: "Available workflows and their declared inputs.",
	}, s.workflowsResource(token))

	server.AddResource(&mcp.Resource{
		Name:        resourceRunsName,
		URI:         resourceRunsURI,
		MIMEType:    mimeJSON,
		Description: "Recent run state, newest first.",
	}, s.runsResource(token))

	return server
}

// Compile-time proof that the real authenticator satisfies the narrow interface
// the server depends on, so a signature drift in internal/auth surfaces here
// rather than at the call site in the serve wiring.
var _ Authenticator = (*auth.Authenticator)(nil)
