package builtins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// mcpClientVersion is the client identity sent during the MCP handshake.
const mcpClientVersion = "0.1.0"

// The SDK reports transport- and session-level failures (connection refused,
// peer closing, session header mismatch) as wire errors in this
// implementation-defined band rather than as plain Go errors, so the band
// classifies as transport trouble, not a server verdict.
const (
	mcpTransportCodeMin = -32005 // rejected by transport
	mcpTransportCodeMax = -32001 // unknown error / header mismatch
)

// MCPClient calls one tool on a remote MCP server, the generic connector in
// the built-ins table (.agents/spec/10-mcp-integration.md). One runner type
// covers every remote MCP tool; instances are configured per server/tool.
//
// Config: server_url (streamable HTTP) or command (stdio argv array), plus
// the required tool name. Params pass through verbatim as the tool's
// arguments. Connections are opened and closed per Execute for Phase 1a
// simplicity; session pooling across calls is future work for when
// per-call handshakes show up in latency numbers.
type MCPClient struct {
	serverURL string
	command   []string
	tool      string

	// mu guards spec, the schema cached by Describe after its first
	// successful fetch.
	mu   sync.Mutex
	spec *runner.ToolSpec
}

// NewMCPClient builds the runner from its config block. An empty block still
// builds (the registry constructs every built-in at startup); Validate
// reports the missing settings when a workflow references the runner.
func NewMCPClient(cfg map[string]any) (*MCPClient, error) {
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

	if serverURL != "" && len(command) > 0 {
		return nil, fmt.Errorf("%w: set server_url or command, not both", ErrConfigValue)
	}

	return &MCPClient{serverURL: serverURL, command: command, tool: tool}, nil
}

// MCPClientFactory adapts NewMCPClient to the registry contract.
func MCPClientFactory(cfg map[string]any) (runner.Runner, error) {
	return NewMCPClient(cfg)
}

// Name implements runner.Runner.
func (*MCPClient) Name() string {
	return MCPClientName
}

// Validate implements runner.Runner. Params carry no fixed schema here: they
// pass through to the remote tool, which owns the contract, so only the
// runner's own config is checkable.
func (m *MCPClient) Validate(_ *runner.Task) error {
	if m.tool == "" {
		return runner.NewError(runner.ClassPermanent, codeMCPNotConfigured,
			"runner config must set tool")
	}

	if m.serverURL == "" && len(m.command) == 0 {
		return runner.NewError(runner.ClassPermanent, codeMCPNotConfigured,
			"runner config must set server_url or command")
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

// connect opens one session against the configured transport. Connection
// failures classify as transport errors, since an unreachable server is the
// retryable case.
func (m *MCPClient) connect(ctx context.Context) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "cairnos", Version: mcpClientVersion}, nil)

	session, err := client.Connect(ctx, m.transport(ctx), nil)
	if err != nil {
		return nil, classifyMCPError(ctx, err)
	}

	return session, nil
}

// transport picks streamable HTTP for server_url and stdio for command.
func (m *MCPClient) transport(ctx context.Context) mcp.Transport {
	if m.serverURL != "" {
		return &mcp.StreamableClientTransport{Endpoint: m.serverURL}
	}

	// #nosec G204 -- the argv array comes from the operator's own runner
	// config block, not from workflow params or network input.
	return &mcp.CommandTransport{Command: exec.CommandContext(ctx, m.command[0], m.command[1:]...)}
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

	return runner.ToolSpec{}, runner.NewError(runner.ClassPermanent, MCPClientName+".unknown_tool",
		fmt.Sprintf("server does not list tool %q", m.tool))
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

// classifyMCPError maps a protocol-level failure. Wire errors came from the
// server, so retrying only helps when the message signals throttling;
// -32602 is the spec's invalid-params code. Everything else is transport,
// classified by the shared network/timeout rules.
func classifyMCPError(ctx context.Context, err error) error {
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
		return runner.NewError(runner.ClassRateLimited, MCPClientName+codeSuffixRateLimited, message)
	}

	return runner.NewError(runner.ClassPermanent, MCPClientName+".tool_error", message)
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
