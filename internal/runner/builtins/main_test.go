package builtins_test

import (
	"testing"

	"go.uber.org/goleak"
)

// mcpServerRead is a real leak nobody is tracking: the MCP SDK parks a
// server-side reader per client the mcp_client cases open and never stops it.
// This line records it rather than calling it harmless, and keeps the sweep
// useful for everything else. Whoever fixes those cases deletes it.
const mcpServerRead = "github.com/modelcontextprotocol/go-sdk/mcp.(*streamableServerConn).Read"

// TestMain sweeps for goroutines the suite left behind. The source runner holds
// a per-spec mutex across a git call, so a caller that never releases it shows up
// here rather than as a suite that quietly slows down.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, goleak.IgnoreTopFunction(mcpServerRead))
}
