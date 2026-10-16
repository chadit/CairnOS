package builtins_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chadit/CairnOS/internal/nettest"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Shared fixture names for the MCP client tests.
const (
	serverURLKey = "server_url"
	commandKey   = "command"
	toolKey      = "tool"
	echoTool     = "echo"
	echoDesc     = "echoes text back"
	textKey      = "text"
	actionMCP    = "mcp"
	echoInput    = "hi"
	echoPrefix   = "echo: "
	echoOutput   = echoPrefix + echoInput
	exampleMCP   = "http://mcp.test"
	stubCommand  = "mcp-server"
)

// echoArgs is the echo tool's typed input; the SDK derives the input schema
// from it, which is what Describe must translate.
type echoArgs struct {
	Text string `json:"text" jsonschema:"the text to echo back"`
}

// newMCPHandler builds an in-process MCP server with one echo tool behind a
// streamable HTTP endpoint. egress_test.go puts it on a real loopback listener
// because the guard it exercises works at dial; everything else here serves it
// in memory.
func newMCPHandler() http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "cairnos-test", Version: "0.0.1"}, nil)

	mcp.AddTool(server, &mcp.Tool{Name: echoTool, Description: echoDesc},
		func(_ context.Context, _ *mcp.CallToolRequest, args echoArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: echoPrefix + args.Text}},
			}, nil, nil
		})

	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
}

// newMCPServer serves the echo tool over an in-memory listener. The streamable
// transport holds a response open while it writes, so this path needs a real
// server rather than a recorder.
func newMCPServer(t *testing.T) *nettest.Server {
	t.Helper()

	return nettest.NewServer(t, newMCPHandler())
}

// newMCPClient points the runner at server through the fixture transport, so
// the request never leaves the process. The guard the default transport
// carries has its own cases in egress_test.go.
func newMCPClient(t *testing.T, server *nettest.Server, tool string) *builtins.MCPClient {
	t.Helper()

	cfg := map[string]any{serverURLKey: server.URL, toolKey: tool}

	client, err := builtins.NewMCPClientOver(cfg, nil, server.Transport())
	if err != nil {
		t.Fatalf("new mcp client: %v", err)
	}

	return client
}

// errNoRoute is what the unreachable fixture transport fails with, standing in
// for a dial that never completed.
var errNoRoute = errors.New("fixture transport reached no host")

func mcpTask(params map[string]any) *runner.Task {
	return &runner.Task{RunID: testRunID, ActionName: actionMCP, Params: params, Resolved: true}
}

// TestMCPClientNameFollowsRegistration pins the Runner.Name contract: an
// instance answers with the name it was registered under, so an imported
// block like github_search must not answer mcp_client, while the built-in
// construction paths keep answering mcp_client.
func TestMCPClientNameFollowsRegistration(t *testing.T) {
	t.Parallel()

	cfg := map[string]any{serverURLKey: exampleMCP, toolKey: echoTool}

	cases := []struct {
		name     string
		build    func() (runner.Runner, error)
		wantName string
	}{
		{
			name:     "per-name factory reports its block name",
			build:    func() (runner.Runner, error) { return builtins.MCPClientFactoryFor("github_search")(cfg) },
			wantName: "github_search",
		},
		{
			name:     "built-in factory reports mcp_client",
			build:    func() (runner.Runner, error) { return builtins.MCPClientFactory()(cfg) },
			wantName: builtins.MCPClientName,
		},
		{
			name:     "direct constructor reports mcp_client",
			build:    func() (runner.Runner, error) { return builtins.NewMCPClient(cfg) },
			wantName: builtins.MCPClientName,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			instance, err := tt.build()
			if err != nil {
				t.Fatalf("build: %v", err)
			}

			if instance.Name() != tt.wantName {
				t.Fatalf("Name() = %q, want %q", instance.Name(), tt.wantName)
			}
		})
	}
}

// TestMCPClientFactoryForPropagatesConfigErrors keeps the per-name factory
// on the same construction contract as the built-in: a bad block fails the
// build instead of producing a half-configured runner.
func TestMCPClientFactoryForPropagatesConfigErrors(t *testing.T) {
	t.Parallel()

	_, err := builtins.MCPClientFactoryFor("github_search")(map[string]any{
		serverURLKey: exampleMCP,
		commandKey:   []any{stubCommand},
		toolKey:      echoTool,
	})
	if err == nil {
		t.Fatal("config with both server_url and command built")
	}
}

func TestMCPClientCallsRemoteTool(t *testing.T) {
	t.Parallel()

	client := newMCPClient(t, newMCPServer(t), echoTool)

	result, err := client.Execute(t.Context(), mcpTask(map[string]any{textKey: echoInput}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if result.Output != echoOutput {
		t.Fatalf("output = %#v", result.Output)
	}

	if result.Meta[toolKey] != echoTool {
		t.Fatalf("meta = %v", result.Meta)
	}
}

func TestMCPClientDescribeTranslatesSchema(t *testing.T) {
	t.Parallel()

	client := newMCPClient(t, newMCPServer(t), echoTool)

	spec := client.Describe()

	if spec.Name != echoTool || spec.Description != echoDesc {
		t.Fatalf("spec = %+v", spec)
	}

	properties, isMap := spec.InputSchema["properties"].(map[string]any)
	if !isMap {
		t.Fatalf("input schema = %#v", spec.InputSchema)
	}

	if _, hasText := properties[textKey]; !hasText {
		t.Fatalf("schema properties = %#v", properties)
	}

	// The second call must serve the cached spec; identical output is the
	// observable contract.
	if again := client.Describe(); again.Name != spec.Name {
		t.Fatalf("cached spec = %+v", again)
	}
}

func TestMCPClientUnknownToolIsInvalidParams(t *testing.T) {
	t.Parallel()

	client := newMCPClient(t, newMCPServer(t), "ghost")

	_, err := client.Execute(t.Context(), mcpTask(map[string]any{textKey: echoInput}))

	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified {
		t.Fatalf("error %v is not a runner.Error", err)
	}

	if classified.Class != runner.ClassPermanent || classified.Code != "mcp_client.invalid_params" {
		t.Fatalf("class/code = %v/%q, want permanent mcp_client.invalid_params", classified.Class, classified.Code)
	}
}

func TestMCPClientBadArgumentsIsPermanentToolError(t *testing.T) {
	t.Parallel()

	client := newMCPClient(t, newMCPServer(t), echoTool)

	// The schema wants a string; an integer fails the server-side argument
	// validation, which the SDK reports as a tool error result.
	_, err := client.Execute(t.Context(), mcpTask(map[string]any{textKey: int64(7)}))

	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified {
		t.Fatalf("error %v is not a runner.Error", err)
	}

	if classified.Class != runner.ClassPermanent || classified.Code != "mcp_client.tool_error" {
		t.Fatalf("class/code = %v/%q, want permanent mcp_client.tool_error", classified.Class, classified.Code)
	}
}

func TestMCPClientUnreachableServerIsTransient(t *testing.T) {
	t.Parallel()

	// A transport that never reaches the host is the transport-failure case,
	// and it needs no port that has to stay closed.
	cfg := map[string]any{serverURLKey: nettest.BaseURL, toolKey: echoTool}

	client, err := builtins.NewMCPClientOver(cfg, nil, nettest.Unreachable(errNoRoute))
	if err != nil {
		t.Fatalf("new mcp client: %v", err)
	}

	_, err = client.Execute(t.Context(), mcpTask(map[string]any{textKey: echoInput}))

	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified {
		t.Fatalf("error %v is not a runner.Error", err)
	}

	if classified.Class != runner.ClassTransient {
		t.Fatalf("class = %v, want transient", classified.Class)
	}
}

func TestMCPClientValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		cfg     map[string]any
		name    string
		wantErr bool
	}{
		{name: "empty config", cfg: nil, wantErr: true},
		{name: "tool without endpoint", cfg: map[string]any{toolKey: echoTool}, wantErr: true},
		{name: "endpoint without tool", cfg: map[string]any{serverURLKey: exampleMCP}, wantErr: true},
		{name: "server url and tool", cfg: map[string]any{serverURLKey: exampleMCP, toolKey: echoTool}, wantErr: false},
		{name: "command and tool", cfg: map[string]any{commandKey: []any{stubCommand}, toolKey: echoTool}, wantErr: false},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client, err := builtins.NewMCPClient(tt.cfg)
			if err != nil {
				t.Fatalf("new mcp client: %v", err)
			}

			err = client.Validate(mcpTask(map[string]any{textKey: echoInput}))

			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("validate error = %v, want error %v", err, tt.wantErr)
			}
		})
	}
}

func TestMCPClientRejectsConflictingEndpoints(t *testing.T) {
	t.Parallel()

	_, err := builtins.NewMCPClient(map[string]any{
		serverURLKey: exampleMCP,
		commandKey:   []any{stubCommand},
		toolKey:      echoTool,
	})
	if err == nil {
		t.Fatal("config with both server_url and command built")
	}
}

// TestMCPClientRejectsNonBoolAllowPrivateTargets pins the config contract: a
// mistyped opt-out fails the build instead of silently keeping the guard on,
// which would make the typo invisible until an internal call is refused.
func TestMCPClientRejectsNonBoolAllowPrivateTargets(t *testing.T) {
	t.Parallel()

	_, err := builtins.NewMCPClient(map[string]any{
		serverURLKey:    exampleMCP,
		toolKey:         echoTool,
		allowPrivateKey: "yes",
	})
	if !errors.Is(err, builtins.ErrConfigValue) {
		t.Fatalf("error = %v, want ErrConfigValue", err)
	}
}

// newMCPServerCapturingAuth serves the same echo MCP server behind an outer
// handler that asserts every request carries the wanted Authorization header,
// so the bearer round-tripper is proven end to end.
func newMCPServerCapturingAuth(t *testing.T, want string) *nettest.Server {
	t.Helper()

	inner := newMCPHandler()

	guarded := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get(authHeader); got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}

		inner.ServeHTTP(w, r)
	})

	return nettest.NewServer(t, guarded)
}

// TestMCPClientCredentialSendsBearer proves the streamable HTTP transport
// carries Authorization: Bearer minted from the credential on its requests.
func TestMCPClientCredentialSendsBearer(t *testing.T) {
	t.Parallel()

	server := newMCPServerCapturingAuth(t, bearerPrefix+credToken)
	lookup := &fakeTokenLookup{tokens: map[string]string{credName: credToken}}

	client, err := builtins.NewMCPClientOver(map[string]any{
		serverURLKey:  server.URL,
		toolKey:       echoTool,
		credentialKey: credName,
	}, lookup, server.Transport())
	if err != nil {
		t.Fatalf("new mcp client: %v", err)
	}

	result, err := client.Execute(t.Context(), mcpTask(map[string]any{textKey: echoInput}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if result.Output != echoOutput {
		t.Fatalf("output = %#v", result.Output)
	}

	if lookup.callCount() == 0 {
		t.Fatal("credential was never minted for the mcp request")
	}
}

// TestMCPClientStdioWithCredentialRejected pins the config rule: a stdio command
// child gets its environment from the process, not request headers, so pairing
// it with a credential is a config error rather than a silently ignored key.
func TestMCPClientStdioWithCredentialRejected(t *testing.T) {
	t.Parallel()

	lookup := &fakeTokenLookup{tokens: map[string]string{credName: credToken}}

	_, err := builtins.NewMCPClientWithCredentials(map[string]any{
		commandKey:    []any{stubCommand},
		toolKey:       echoTool,
		credentialKey: credName,
	}, lookup)
	if !errors.Is(err, builtins.ErrConfigValue) {
		t.Fatalf("error = %v, want ErrConfigValue", err)
	}
}
