package builtins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chadit/CairnOS/pkg/runner"
)

// MCPClientName is the registry name of the generic MCP tool runner.
const MCPClientName = "mcp_client"

// MCP client config keys: a remote endpoint or a local command, and the tool
// to call on it.
const (
	cfgKeyServerURL = "server_url"
	cfgKeyCommand   = "command"
	cfgKeyTool      = "tool"
)

// codeMCPNotConfigured reports a runner block missing its required keys.
const codeMCPNotConfigured = MCPClientName + ".not_configured"

// describeTimeout bounds the schema fetch behind Describe, which carries no
// context of its own in the Describer contract.
const describeTimeout = 30 * time.Second

// clientName is the identity CairnOS announces in a handshake, to MCP servers
// and to the codex app-server alike. Only MCP also takes mcpClientVersion.
const (
	clientName       = "cairnos"
	mcpClientVersion = "0.1.0"
)

// The SDK reports transport- and session-level failures (connection refused,
// peer closing, session header mismatch) as wire errors in this
// implementation-defined band rather than as plain Go errors, so the band
// classifies as transport trouble, not a server verdict.
const (
	mcpTransportCodeMin = -32005 // rejected by transport
	mcpTransportCodeMax = -32001 // unknown error / header mismatch
)

// MCPClient calls one tool on a remote MCP server
// (.agents/spec/10-mcp-integration.md). One runner type covers every remote
// MCP tool; instances are configured per server/tool.
//
// Config: server_url (streamable HTTP) or command (stdio argv array), plus the
// tool name. Params pass through verbatim as the tool's arguments. Connections
// open and close per Execute; pooling waits until handshakes show up in
// latency numbers.
type MCPClient struct {
	// httpClient carries the shared egress guard for the streamable HTTP
	// transport, so registry-supplied server_url values cannot reach
	// loopback, link-local, or private addresses without an explicit
	// opt-in. Nil on the stdio path: its boundary is the process, not HTTP.
	httpClient *http.Client

	spec *runner.ToolSpec

	// name is the registered identifier this instance answers to: the
	// built-in registers as MCPClientName, while imported blocks
	// (MCPClientFactoryFor) register one instance per block name.
	name string

	serverURL string
	tool      string
	command   []string

	// mu guards spec, the schema cached by Describe after its first
	// successful fetch.
	mu sync.Mutex
}

// NewMCPClient builds the runner from its config block with no credential
// lookup. An empty block still builds (the registry constructs every built-in
// at startup); Validate reports the missing settings when a workflow references
// the runner. A credential key has no lookup to resolve against and fails the
// build.
func NewMCPClient(cfg map[string]any) (*MCPClient, error) {
	return buildMCPClient(MCPClientName, cfg, nil, nil)
}

// NewMCPClientWithCredentials builds the runner with an optional credential
// lookup. When the config sets a credential key, lookup must know it (Has) or
// construction fails, and the streamable HTTP transport carries a bearer token
// minted per request.
func NewMCPClientWithCredentials(cfg map[string]any, lookup TokenLookup) (*MCPClient, error) {
	return buildMCPClient(MCPClientName, cfg, lookup, nil)
}

// NewMCPClientOver is NewMCPClientWithCredentials over a caller-supplied round
// tripper. A nil transport keeps the guarded default. The bearer wrapper still
// goes on top, so a credential is minted the same way either side of the seam.
func NewMCPClientOver(cfg map[string]any, lookup TokenLookup, transport http.RoundTripper) (*MCPClient, error) {
	return buildMCPClient(MCPClientName, cfg, lookup, transport)
}

// buildMCPClient is the shared constructor. name is the registered identifier
// the instance answers to; error codes and Meta always keep the mcp_client
// prefix. lookup is nil on the no-credential path, transport on the guarded
// one.
func buildMCPClient(
	name string,
	cfg map[string]any,
	lookup TokenLookup,
	transport http.RoundTripper,
) (*MCPClient, error) {
	serverURL, err := configValue(cfg, cfgKeyServerURL, "")
	if err != nil {
		return nil, wrapConfigErr(MCPClientName, err)
	}

	command, err := configStrings(cfg, cfgKeyCommand)
	if err != nil {
		return nil, wrapConfigErr(MCPClientName, err)
	}

	tool, err := configValue(cfg, cfgKeyTool, "")
	if err != nil {
		return nil, wrapConfigErr(MCPClientName, err)
	}

	allowPrivate, err := configValue(cfg, cfgKeyAllowPrivateTargets, false)
	if err != nil {
		return nil, wrapConfigErr(MCPClientName, err)
	}

	if serverURL != "" && len(command) > 0 {
		return nil, fmt.Errorf("%w: set server_url or command, not both", ErrConfigValue)
	}

	credential, err := requireCredential(cfg, lookup)
	if err != nil {
		return nil, wrapConfigErr(MCPClientName, err)
	}

	// A credential targets a request Authorization header, which only the
	// streamable HTTP transport has. A stdio command child gets its environment
	// from the process, so pairing credential with command is a config error
	// rather than a silently ignored key.
	if credential != "" && len(command) > 0 {
		return nil, fmt.Errorf("%w: credential requires server_url; a stdio command has no request headers for a bearer token", ErrConfigValue)
	}

	client := &MCPClient{name: name, serverURL: serverURL, command: command, tool: tool}

	if serverURL != "" {
		// Timeout 0 is deliberate: streamable HTTP holds a stream open for
		// the session, and a hard http.Client.Timeout would cut long tool
		// calls mid-stream. Every call is already ctx-bounded: Execute runs
		// under the engine's action timeout and Describe under
		// describeTimeout.
		client.httpClient = safeHTTPClient(MCPClientName, 0, allowPrivate)

		if transport != nil {
			client.httpClient.Transport = transport
		}

		if credential != "" {
			// The bearer round-tripper wraps the guarded transport, so every
			// request the session makes carries the token while the client's
			// dial and redirect guards stay in force.
			client.httpClient.Transport = &bearerRoundTripper{
				base:       client.httpClient.Transport,
				lookup:     lookup,
				credential: credential,
			}
		}
	}

	return client, nil
}

// MCPClientFactory adapts buildMCPClient to the registry contract with no
// credential lookup. It is MCPClientFactoryWithCredentials with a nil lookup.
func MCPClientFactory() runner.Factory {
	return MCPClientFactoryWithCredentials(nil)
}

// MCPClientFactoryWithCredentials returns a registry factory that wires lookup
// into the built-in mcp_client instance, so a block with a credential key sends
// Authorization: Bearer over the streamable HTTP transport.
func MCPClientFactoryWithCredentials(lookup TokenLookup) runner.Factory {
	return func(cfg map[string]any) (runner.Runner, error) {
		return buildMCPClient(MCPClientName, cfg, lookup, nil)
	}
}

// MCPClientFactoryFor builds MCP client instances registered under name with no
// credential lookup, the path CLI wiring takes for [runner.<name>] blocks with
// type = "mcp_client" (the shape `cairnos mcp import` emits). Only the reported
// Name changes; error codes and Meta keep the mcp_client prefix, matching the
// other builtins' type-level codes.
func MCPClientFactoryFor(name string) runner.Factory {
	return MCPClientFactoryForWithCredentials(name, nil)
}

// MCPClientFactoryForWithCredentials is MCPClientFactoryFor with a credential
// lookup, so an imported mcp_client block can bind a credential too.
func MCPClientFactoryForWithCredentials(name string, lookup TokenLookup) runner.Factory {
	return func(cfg map[string]any) (runner.Runner, error) {
		return buildMCPClient(name, cfg, lookup, nil)
	}
}

// Name implements runner.Runner: the registered identifier workflow files
// reference, so an imported instance answers with its block name, not the
// shared built-in name.
func (m *MCPClient) Name() string {
	return m.name
}

// Validate implements runner.Runner. Params carry no fixed schema here: they
// pass through to the remote tool, which owns the contract, so only the
// runner's own config is checkable.
func (m *MCPClient) Validate(_ *runner.Task) error {
	if m.tool == "" {
		return taskError(runner.ClassPermanent, codeMCPNotConfigured,
			"runner config must set tool")
	}

	if m.serverURL == "" && len(m.command) == 0 {
		return taskError(runner.ClassPermanent, codeMCPNotConfigured,
			"runner config must set server_url or command")
	}

	// The scheme allowlist runs here so a file:// or gopher:// endpoint
	// fails as a permanent blocked_target before any dial happens.
	if m.serverURL != "" {
		if err := guardTarget(MCPClientName, m.serverURL); err != nil {
			return err
		}
	}

	return nil
}

// Execute implements runner.Runner: connect, call the configured tool with
// the task params as arguments, and surface the result content as Output.
func (m *MCPClient) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	if err := m.Validate(task); err != nil {
		return runner.Result{}, err
	}

	start := time.Now()

	session, err := m.connect(ctx)
	if err != nil {
		return runner.Result{}, err
	}

	defer func() { _ = session.Close() }()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: m.tool, Arguments: task.Params})
	if err != nil {
		return runner.Result{}, classifyMCPError(ctx, err)
	}

	if result.IsError {
		return runner.Result{}, classifyMCPToolError(joinMCPText(result.Content))
	}

	return runner.Result{
		Output: mcpOutput(result),
		Usage:  runner.Usage{Latency: time.Since(start)},
		Meta:   map[string]string{cfgKeyTool: m.tool},
	}, nil
}

// Describe implements runner.Describer: the named tool's schema, fetched
// from the server's tool list and cached after the first success so agent
// loops do not re-handshake per planning step. Fetch failures return a
// minimal uncached spec, so the next call retries.
func (m *MCPClient) Describe() runner.ToolSpec {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.spec != nil {
		return *m.spec
	}

	spec, err := m.fetchSpec()
	if err != nil {
		return runner.ToolSpec{
			Name:        m.tool,
			Description: "remote MCP tool (schema fetch failed: " + err.Error() + ")",
		}
	}

	m.spec = &spec

	return spec
}

// connect opens one session over streamable HTTP for server_url or stdio for
// command. The HTTP path rides the guarded client so every dial, including
// redirect hops, passes the egress checks. Connection failures classify as
// transport errors, since an unreachable server is the retryable case.
func (m *MCPClient) connect(ctx context.Context) (*mcp.ClientSession, error) {
	if m.serverURL != "" {
		return openSession(ctx, &mcp.StreamableClientTransport{Endpoint: m.serverURL, HTTPClient: m.httpClient})
	}

	// #nosec G204 -- the argv array comes from the operator's own runner
	// config block, not from workflow params or network input.
	return openSession(ctx, &mcp.CommandTransport{Command: exec.CommandContext(ctx, m.command[0], m.command[1:]...)})
}

// openSession runs the MCP handshake over transport, whichever kind connect
// picked, so both paths classify a failed connect the same way.
func openSession(ctx context.Context, transport mcp.Transport) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: clientName, Version: mcpClientVersion}, nil)

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, classifyMCPError(ctx, err)
	}

	return session, nil
}

// fetchSpec lists the server's tools and translates the configured one. The
// Describer contract carries no context, so the fetch runs under its own
// bounded one.
func (m *MCPClient) fetchSpec() (runner.ToolSpec, error) {
	ctx, cancel := context.WithTimeout(context.Background(), describeTimeout)
	defer cancel()

	session, err := m.connect(ctx)
	if err != nil {
		return runner.ToolSpec{}, err
	}

	defer func() { _ = session.Close() }()

	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return runner.ToolSpec{}, classifyMCPError(ctx, err)
		}

		if tool.Name == m.tool {
			return runner.ToolSpec{
				Name:        tool.Name,
				Description: tool.Description,
				InputSchema: schemaMap(tool.InputSchema),
			}, nil
		}
	}

	return runner.ToolSpec{}, taskError(runner.ClassPermanent, MCPClientName+".unknown_tool",
		"server does not list tool %q", m.tool)
}

// schemaMap normalizes a tool's input schema. The SDK documents the
// client-side value as map[string]any already; the marshal round trip covers
// any other JSON-marshalable shape a server might hand back.
func schemaMap(schema any) map[string]any {
	if mapped, isMap := schema.(map[string]any); isMap {
		return mapped
	}

	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return nil
	}

	return decoded
}

// mcpOutput picks the result shape: structured content passes through
// untouched, otherwise the text blocks join into one string.
func mcpOutput(result *mcp.CallToolResult) any {
	if result.StructuredContent != nil {
		return result.StructuredContent
	}

	return joinMCPText(result.Content)
}

// joinMCPText collects the text blocks of a tool result, newline-joined.
// Non-text content (images, audio) has no string form and is skipped.
func joinMCPText(blocks []mcp.Content) string {
	texts := make([]string, 0, len(blocks))

	for _, block := range blocks {
		if text, isText := block.(*mcp.TextContent); isText {
			texts = append(texts, text.Text)
		}
	}

	return strings.Join(texts, "\n")
}

// classifyMCPError maps a protocol-level failure. A dial the egress guard
// refused is permanent and checked first. Wire errors came from the
// server, so retrying only helps when the message signals throttling;
// -32602 is the spec's invalid-params code. Everything else is transport,
// classified by the shared network/timeout rules.
func classifyMCPError(ctx context.Context, err error) error {
	// The guard's refused dial rides under the SDK's "rejected by
	// transport" wire error (-32005), so the blocked-dial check has to run
	// before the wire split or it would classify as a retryable transport
	// failure that never succeeds.
	if blocked := classifyBlockedDial(MCPClientName, err); blocked != nil {
		return blocked
	}

	wireErr, isWire := errors.AsType[*jsonrpc.Error](err)
	if !isWire {
		if looksRateLimited(err.Error()) {
			return &runner.Error{
				Class: runner.ClassRateLimited, Code: MCPClientName + codeSuffixRateLimited,
				Message: err.Error(), Cause: err,
			}
		}

		return classifyAPITransport(ctx, MCPClientName, err)
	}

	switch {
	case wireErr.Code == jsonrpc.CodeInvalidParams:
		return &runner.Error{
			Class: runner.ClassPermanent, Code: MCPClientName + ".invalid_params",
			Message: wireErr.Message, Cause: err,
		}
	case wireErr.Code >= mcpTransportCodeMin && wireErr.Code <= mcpTransportCodeMax:
		return classifyAPITransport(ctx, MCPClientName, err)
	case looksRateLimited(wireErr.Message):
		return &runner.Error{
			Class: runner.ClassRateLimited, Code: MCPClientName + codeSuffixRateLimited,
			Message: wireErr.Message, Cause: err,
		}
	default:
		return &runner.Error{
			Class: runner.ClassPermanent, Code: MCPClientName + ".remote",
			Message: wireErr.Message, Cause: err,
		}
	}
}

// classifyMCPToolError maps a tool-level failure (IsError results). The
// remote tool ran and rejected the call, which is permanent unless its
// message signals throttling.
func classifyMCPToolError(message string) error {
	if looksRateLimited(message) {
		return taskError(runner.ClassRateLimited, MCPClientName+codeSuffixRateLimited, "%s", message)
	}

	return taskError(runner.ClassPermanent, MCPClientName+".tool_error", "%s", message)
}

// looksRateLimited spots throttling language in a remote error message. MCP
// carries no structured rate-limit signal, so a text heuristic is the best
// available mapping to the RateLimited class.
func looksRateLimited(message string) bool {
	lowered := strings.ToLower(message)

	return strings.Contains(lowered, "rate limit") ||
		strings.Contains(lowered, "too many requests") ||
		strings.Contains(lowered, "429")
}
