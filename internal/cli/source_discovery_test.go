package cli_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
)

// discoveredWorkflowRel is where each fixture origin commits its workflow, a
// path the discovery glob below matches.
const discoveredWorkflowRel = "engines/probe/cairnos/tick.toml"

// stageWorkflowOrigin builds a bare origin whose one commit carries a scheduled
// workflow named name, and returns the origin's path.
func stageWorkflowOrigin(t *testing.T, name string) string {
	t.Helper()

	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")

	gitOut(t, root, "init", "--bare", "--initial-branch=main", origin)
	gitOut(t, root, "clone", "--quiet", origin, seed)

	writeFile(t, filepath.Join(seed, discoveredWorkflowRel),
		"[workflow]\nname = \""+name+"\"\nschedule = \"@daily\"\n\n"+
			"[action.tick]\nrunner = \"mock\"\nparams = { output = \"tick\" }\n")
	gitOut(t, seed, "add", ".")
	gitOut(t, seed, "commit", "--quiet", "-m", "add "+name)
	gitOut(t, seed, "push", "--quiet", "origin", "HEAD:refs/heads/main")

	return origin
}

// TestServeSchedulesWorkflowsEachSourceCarries proves discovery runs at serve
// start for every source: each committed workflow is in the schedule store
// once serve has booted, with no `schedule add`.
func TestServeSchedulesWorkflowsEachSourceCarries(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	var blocks strings.Builder

	for _, name := range []string{"alpha", "beta"} {
		fmt.Fprintf(&blocks, "[source.%s]\nrepo = %q\ndest = %q\nworkflows = [\"engines/*/cairnos/*.toml\"]\n\n",
			name, stageWorkflowOrigin(t, name+"_tick"), filepath.Join(t.TempDir(), name))
	}

	cfgTOML := "[api]\ngrpc = \"127.0.0.1:0\"\nhttp = \"127.0.0.1:0\"\n\n" +
		"[telemetry]\nenabled = false\n\n" + blocks.String()

	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfgTOML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, stderr, exit, cancel := serveUntilReady(t, dir, withSourcePath(map[string]string{}))

	cancel()

	if code := <-exit; code != cli.ExitOK {
		t.Fatalf("serve exit = %d, want %d\nstderr: %s", code, cli.ExitOK, stderr.String())
	}

	code, stdout, listErr := runCLI(t, scheduleCmd, "list", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("schedule list exited %d: %s", code, listErr)
	}

	for _, name := range []string{"alpha_tick", "beta_tick"} {
		if strings.Count(stdout, name) != 1 {
			t.Fatalf("schedule list does not show %s exactly once:\n%s", name, stdout)
		}
	}
}
