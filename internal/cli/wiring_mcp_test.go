package cli_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/nettest"
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

// newMCPEchoServer hosts an in-process MCP server with one echo tool behind a
// streamable HTTP endpoint on an in-memory listener. It mirrors the helper in
// builtins/mcp_client_test.go, duplicated because Go's per-package test
// binaries can't share helpers. The listener is in memory because a loopback
// one can sit on a port with stale connection tracking.
func newMCPEchoServer(t *testing.T) *nettest.Server {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "cairnos-test", Version: "0.0.1"}, nil)

	mcp.AddTool(server, &mcp.Tool{Name: mcpEchoTool, Description: "echoes text back"},
		func(_ context.Context, _ *mcp.CallToolRequest, args mcpEchoArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + args.Text}},
			}, nil, nil
		})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)

	return nettest.NewServer(t, handler)
}

// runOverMCPServer drives one invocation whose mcp_client runners reach server.
func runOverMCPServer(t *testing.T, server *nettest.Server, args ...string) (int, string, string) {
	t.Helper()

	return runConfiguredCLI(t, func(app *cli.App) *cli.App {
		return app.WithRunnerTransport(server.Transport())
	}, args...)
}

// appendMCPBlock adds a [runner.<name>] mcp_client block to the scaffolded
// config, the exact shape `cairnos mcp import` emits, pointed at the fixture
// origin the in-memory server answers.
func appendMCPBlock(t *testing.T, dir, name string) {
	t.Helper()

	block := "\n[runner." + name + "]\n" +
		"type       = \"mcp_client\"\n" +
		"server_url = " + strconv.Quote(nettest.BaseURL) + "\n" +
		"tool       = " + strconv.Quote(mcpEchoTool) + "\n"

	appendConfig(t, dir, block)
}

func TestRunExecutesImportedMCPRunnerFromConfig(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	appendMCPBlock(t, dir, "remote_echo")
	path := writeWorkflow(t, dir, mcpEchoWorkflow)

	code, stdout, stderr := runOverMCPServer(t, newMCPEchoServer(t), runCmd, path, flagDirArg, dir)
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
	appendMCPBlock(t, dir, "github")
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
	appendMCPBlock(t, dir, "mcp_client")
	path := writeWorkflow(t, dir, mcpBuiltinWorkflow)

	code, stdout, stderr := runOverMCPServer(t, newMCPEchoServer(t), runCmd, path, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("run exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, "succeeded") {
		t.Fatalf("summary missing success:\n%s", stdout)
	}
}
