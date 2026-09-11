package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/store"
)

// execWorkflow names the exec runner and an action the config block allows, so
// a pass proves the wiring registered the built-in and the gate let the action
// through. The command echoes a variable only the config env table sets.
const execWorkflow = `
[workflow]
name = "exec_wired"

[action.reconcile]
runner = "exec"
params = { command = "/bin/sh", args = ["-c", "printf '%s' \"$CAIRNOS_EXEC_MARKER\""] }
`

// deniedExecWorkflow names an action the config block does not allow.
const deniedExecWorkflow = `
[workflow]
name = "exec_denied"

[action.not_listed]
runner = "exec"
params = { command = "/bin/true" }
`

// appendExecBlock adds a [runner.exec] block to the scaffolded config.
func appendExecBlock(t *testing.T, dir string) {
	t.Helper()

	cfgPath := filepath.Join(dir, "config.toml")

	existing, err := os.ReadFile(cfgPath) // #nosec G304 -- test-owned scaffold path
	if err != nil {
		t.Fatalf("read scaffolded config: %v", err)
	}

	block := "\n[runner.exec]\n" +
		"allow_actions = [\"reconcile\"]\n" +
		"timeout       = 30\n" +
		"env           = { CAIRNOS_EXEC_MARKER = \"from-config\" }\n"

	if err := os.WriteFile(cfgPath, append(existing, []byte(block)...), 0o600); err != nil {
		t.Fatalf("write config with exec block: %v", err)
	}
}

func TestRunExecutesHostExecRunnerFromConfig(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	appendExecBlock(t, dir)
	path := writeWorkflow(t, dir, execWorkflow)

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

	if len(results) != 1 || results[0].Output != "from-config" {
		t.Fatalf("stored results = %+v, want the config env value", results)
	}
}

// TestRunRefusesAnExecActionOutsideAllowActions proves the gate reaches the
// workflow load pass, so a denied action never launches anything.
func TestRunRefusesAnExecActionOutsideAllowActions(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	appendExecBlock(t, dir)
	path := writeWorkflow(t, dir, deniedExecWorkflow)

	code, _, stderr := runCLI(t, runCmd, path, flagDirArg, dir)
	if code != cli.ExitUsage {
		t.Fatalf("run exited %d, want %d (stderr: %s)", code, cli.ExitUsage, stderr)
	}

	if !strings.Contains(stderr, "is not in exec allow_actions") {
		t.Fatalf("stderr does not name the gate:\n%s", stderr)
	}
}
