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
	goleak.VerifyTestMain(m)
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
