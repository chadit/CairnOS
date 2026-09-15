package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/store"
)

// hostWorkflow prints the rendered launch path for one tool on one host. A pass
// proves the whole chain: config parsed the two-level block, wiring rendered it,
// and the exec runner put it in the child's environment.
const hostWorkflow = `
[workflow]
name = "host_wired"

[action.reconcile]
runner = "exec"
params = { command = "/bin/sh", args = ["-c", "printf '%s' \"$CAIRNOS_HOST_BUILD_BOX_HARNESS_CLAUDE\""] }
`

// wantLaunchPath is what the block below renders: the default directory, kept
// with its "~" because only the host running the binary can expand it, plus the
// configured binary name.
const wantLaunchPath = "~/.local/bin/claude-agent"

// appendHostBlocks adds the exec runner block and one host to the scaffolded
// config. The tool block names only a binary, so the path falls back to the
// default and the test covers both halves of the rendered value.
func appendHostBlocks(t *testing.T, dir string) {
	t.Helper()

	cfgPath := filepath.Join(dir, "config.toml")

	existing, err := os.ReadFile(cfgPath) // #nosec G304 -- test-owned scaffold path
	if err != nil {
		t.Fatalf("read scaffolded config: %v", err)
	}

	block := "\n[runner.exec]\n" +
		"allow_actions = [\"reconcile\"]\n" +
		"timeout       = 30\n" +
		"\n[host.build_box]\n" +
		"type = \"worker\"\n" +
		"ssh  = \"build-box\"\n" +
		"\n[host.build_box.harness.claude]\n" +
		"name = \"claude-agent\"\n"

	if err := os.WriteFile(cfgPath, append(existing, []byte(block)...), 0o600); err != nil {
		t.Fatalf("write config with host blocks: %v", err)
	}
}

// TestRunPassesTheHostTableToAnExecAction is the end-to-end case for FR5 and
// FR7: `cairnos run` renders the same table serve does, so a job started by hand
// sees the tool paths a scheduled one would.
func TestRunPassesTheHostTableToAnExecAction(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	appendHostBlocks(t, dir)
	path := writeWorkflow(t, dir, hostWorkflow)

	code, stdout, stderr := runCLI(t, runCmd, path, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("run exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, "succeeded") {
		t.Fatalf("summary missing success:\n%s", stdout)
	}

	db := openRunStore(t, dir)

	runs, err := db.ListRuns(t.Context(), store.ListFilter{})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}

	if len(runs) != 1 {
		t.Fatalf("persisted runs = %+v, want one row", runs)
	}

	results, err := db.ListActionResults(t.Context(), runs[0].ID)
	if err != nil {
		t.Fatalf("list action results: %v", err)
	}

	if len(results) != 1 || results[0].Output != wantLaunchPath {
		t.Fatalf("stored results = %+v, want the rendered launch path %q", results, wantLaunchPath)
	}
}
