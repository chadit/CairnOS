package cli_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chadit/CairnOS/internal/cli"
)

// mcpEchoWorkflow references the runner name only the [runner.remote_echo]
// config block defines (the exact shape `cairnos mcp import` emits), so a
// pass proves the wiring registered it.
const mcpEchoWorkflow = `
[workflow]
name = "mcp_wired"

[action.bounce]
runner = "remote_echo"
params = { text = "wired" }
`

// mcpBuiltinWorkflow references the built-in name, proving a block literally
// named mcp_client keeps configuring the built-in instance instead of
// tripping duplicate registration.
const mcpBuiltinWorkflow = `
[workflow]
name = "mcp_builtin"

[action.bounce]
runner = "mcp_client"
params = { text = "wired" }
`

// mcpEchoTool is the one tool the in-process server exposes and the tool the
// imported runner block calls.
const mcpEchoTool = "echo"

// mcpEchoArgs is the echo tool's typed input; the SDK derives the input
// schema from it.
type mcpEchoArgs struct {
	Text string `json:"text" jsonschema:"the text to echo back"`
}

// newMCPEchoServerURL hosts an in-process MCP server with one echo tool
// behind an httptest streamable HTTP endpoint and returns its URL. It
// mirrors the helper in builtins/mcp_client_test.go, duplicated because
// Go's per-package test binaries can't share helpers.
func newMCPEchoServerURL(t *testing.T) string {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "cairnos-test", Version: "0.0.1"}, nil)

	mcp.AddTool(server, &mcp.Tool{Name: mcpEchoTool, Description: "echoes text back"},
		func(_ context.Context, _ *mcp.CallToolRequest, args mcpEchoArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + args.Text}},
			}, nil, nil
		})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)

	// Real socket on purpose: the workflow run builds its runner clients from
	// config, so the server_url has to be one something can dial.
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	return httpServer.URL
}

// appendMCPBlock adds a [runner.<name>] mcp_client block to the scaffolded
// config, the exact shape `cairnos mcp import` emits. It always sets
// allow_private_targets = true: inert until the egress guard lands (unknown
// keys pass through the config maps) and load-bearing after it, since the
// echo server sits on a loopback address, so these tests never churn.
func appendMCPBlock(t *testing.T, dir, name, serverURL string) {
	t.Helper()

	block := "\n[runner." + name + "]\n" +
		"type                  = \"mcp_client\"\n" +
		"server_url            = " + strconv.Quote(serverURL) + "\n" +
		"tool                  = " + strconv.Quote(mcpEchoTool) + "\n" +
		"allow_private_targets = true\n"

	appendConfig(t, dir, block)
}

func TestRunExecutesImportedMCPRunnerFromConfig(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	appendMCPBlock(t, dir, "remote_echo", newMCPEchoServerURL(t))
	path := writeWorkflow(t, dir, mcpEchoWorkflow)

	code, stdout, stderr := runCLI(t, runCmd, path, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("run exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, "succeeded") {
		t.Fatalf("summary missing success:\n%s", stdout)
	}

	if got := onlyActionOutput(t, dir); got != "echo: wired" {
		t.Fatalf("stored output = %#v, want the echoed text", got)
	}
}

func TestRunMCPBlockCollidingWithBuiltinFailsAtWiring(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	appendMCPBlock(t, dir, "github", newMCPEchoServerURL(t))
	path := writeWorkflow(t, dir, mcpEchoWorkflow)

	code, _, stderr := runCLI(t, runCmd, path, flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("run exited %d, want %d (stderr: %s)", code, cli.ExitFailure, stderr)
	}

	if !strings.Contains(stderr, `register mcp_client runner "github"`) {
		t.Fatalf("stderr does not name the colliding block:\n%s", stderr)
	}

	if !strings.Contains(stderr, "already registered") {
		t.Fatalf("stderr does not carry the duplicate-runner cause:\n%s", stderr)
	}
}

func TestRunMCPClientNamedBlockConfiguresBuiltin(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	appendMCPBlock(t, dir, "mcp_client", newMCPEchoServerURL(t))
	path := writeWorkflow(t, dir, mcpBuiltinWorkflow)

	code, stdout, stderr := runCLI(t, runCmd, path, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("run exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, "succeeded") {
		t.Fatalf("summary missing success:\n%s", stdout)
	}
}
