package cli_test

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/goleak"
)

// TestMain has two jobs. Launched under the `bw` name, this process is a child
// the client started through the symlink, so it runs the fake and exits before
// the suite would start again. The name is the signal because the client scrubs
// its child's environment down to an allowlist.
//
// Otherwise it sweeps for leaked goroutines.
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == bwFakeName {
		os.Exit(runFakeBW(os.Args[1:]))
	}

	goleak.VerifyTestMain(m, knownLeaks()...)
}

// knownLeaks lists what the sweep found already present, so the check reports
// what this work introduces rather than what it inherited.
func knownLeaks() []goleak.Option {
	return []goleak.Option{
		// One name lookup, three stacks. The serve tests point the bridge at a
		// host that does not resolve, and the resolver's parallel queries have
		// no cancel path. Each frame is named because goleak matches a
		// goroutine's own frames, never its creator's.
		goleak.IgnoreAnyFunction("net.(*Resolver).lookupIPAddr.func2"),
		goleak.IgnoreAnyFunction("net.(*Resolver).goLookupIPCNAMEOrder"),
		goleak.IgnoreAnyFunction("net.(*Resolver).tryOneName"),

		// The MCP SDK's streamable connection keeps its reader until the peer
		// closes, which a test's in-process peer never does.
		goleak.IgnoreAnyFunction("github.com/modelcontextprotocol/go-sdk/mcp.(*streamableServerConn).Read"),
	}
}
