package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/goleak"

	"github.com/chadit/CairnOS/internal/cli"
)

// cairnosName is the name a case links this test binary under to run it as
// the real command, in a process of its own with a terminal it controls.
const cairnosName = "cairnos"

// TestMain dispatches on the name this binary was launched under, because the
// bw client scrubs its child's environment to an allowlist. As `bw` it runs the
// fake; as `cairnos`, the command the way cmd/cairnos does. Either exits
// before the suite would start again.
//
// Otherwise it isolates git from the developer's config and sweeps for leaks.
func TestMain(m *testing.M) {
	switch filepath.Base(os.Args[0]) {
	case bwFakeName:
		os.Exit(runFakeBW(os.Args[1:]))
	case cairnosName:
		os.Exit(runAsCairnos(os.Args[1:]))
	}

	isolateGitConfig()
	goleak.VerifyTestMain(m)
}

// runAsCairnos is cmd/cairnos's main: the same signal trap around the same
// App, so a case driving it sees what an operator would.
func runAsCairnos(args []string) int {
	ctx, stop := cli.StopContext(context.Background()) //nolint:contextbackground // the re-executed child has no *testing.T to take t.Context() from
	defer stop()

	return cli.New(os.Stdout, os.Stderr).Run(ctx, args)
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
