package cli_test

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/goleak"
)

// TestMain has two jobs. Launched under the `bw` name, this process is the
// child the client started through the symlink, so it runs the fake and exits
// before the suite would start again. The name is the signal because the client
// scrubs its child's environment to an allowlist.
//
// Otherwise it isolates git from the developer's config and sweeps for leaks.
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == bwFakeName {
		os.Exit(runFakeBW(os.Args[1:]))
	}

	isolateGitConfig()
	goleak.VerifyTestMain(m, knownLeaks()...)
}

// isolateGitConfig keeps the developer's git settings out of the git commands
// the code under test runs in its temp clones; the fixtures already do this per
// call. With core.fsmonitor on, the update loop's `git status` starts a detached
// daemon in the clone, still writing under .git when t.TempDir removes the
// tree, which fails the test with "directory not empty".
func isolateGitConfig() {
	for _, key := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM"} {
		if err := os.Setenv(key, os.DevNull); err != nil {
			panic("set " + key + ": " + err.Error())
		}
	}
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
