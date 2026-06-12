package builtins_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Shared fixture names for the MCP client tests.
const (
	serverURLKey = "server_url"
	toolKey      = "tool"
	echoTool     = "echo"
	textKey      = "text"
	actionMCP    = "mcp"
	echoInput    = "hi"
	exampleMCP   = "http://mcp.test"
)

// echoArgs is the echo tool's typed input; the SDK derives the input schema
// from it, which is what Describe must translate.
type echoArgs struct {
	Text string `json:"text" jsonschema:"the text to echo back"`
}

// newMCPServerURL hosts an in-process MCP server with one echo tool behind
// an httptest streamable HTTP endpoint and returns its URL.
func newMCPServerURL(t *testing.T) string {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "cairnos-test", Version: "0.0.1"}, nil)

	mcp.AddTool(server, &mcp.Tool{Name: echoTool, Description: "echoes text back"},
		func(_ context.Context, _ *mcp.CallToolRequest, args echoArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + args.Text}},
			}, nil, nil
		})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	return httpServer.URL
}

func newMCPClient(t *testing.T, url, tool string) *builtins.MCPClient {
	t.Helper()

	client, err := builtins.NewMCPClient(map[string]any{serverURLKey: url, toolKey: tool})
	if err != nil {
		t.Fatalf("new mcp client: %v", err)
	}

	return client
}

func mcpTask(params map[string]any) *runner.Task {
	return &runner.Task{RunID: testRunID, ActionName: actionMCP, Params: params, Resolved: true}
}

func TestMCPClientCallsRemoteTool(t *testing.T) {
	t.Parallel()

	client := newMCPClient(t, newMCPServerURL(t), echoTool)

	result, err := client.Execute(t.Context(), mcpTask(map[string]any{textKey: echoInput}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if result.Output != "echo: hi" {
		t.Fatalf("output = %#v", result.Output)
	}

	if result.Meta[toolKey] != echoTool {
		t.Fatalf("meta = %v", result.Meta)
	}
}

func TestMCPClientDescribeTranslatesSchema(t *testing.T) {
	t.Parallel()

	client := newMCPClient(t, newMCPServerURL(t), echoTool)

	spec := client.Describe()

	if spec.Name != echoTool || spec.Description != "echoes text back" {
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

	client := newMCPClient(t, newMCPServerURL(t), "ghost")

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

	client := newMCPClient(t, newMCPServerURL(t), echoTool)

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

	// Closing the server before the call makes the endpoint refuse
	// connections, the transport-failure case.
	deadServer := httptest.NewServer(http.NotFoundHandler())
	url := deadServer.URL

	deadServer.Close()

	client := newMCPClient(t, url, echoTool)

	_, err := client.Execute(t.Context(), mcpTask(map[string]any{textKey: echoInput}))

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
		name    string
		cfg     map[string]any
		wantErr bool
	}{
		{name: "empty config", cfg: nil, wantErr: true},
		{name: "tool without endpoint", cfg: map[string]any{toolKey: echoTool}, wantErr: true},
		{name: "endpoint without tool", cfg: map[string]any{serverURLKey: exampleMCP}, wantErr: true},
		{name: "server url and tool", cfg: map[string]any{serverURLKey: exampleMCP, toolKey: echoTool}, wantErr: false},
		{name: "command and tool", cfg: map[string]any{"command": []any{"mcp-server"}, toolKey: echoTool}, wantErr: false},
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
		"command":    []any{"mcp-server"},
		toolKey:      echoTool,
	})
	if err == nil {
		t.Fatal("config with both server_url and command built")
	}
}
