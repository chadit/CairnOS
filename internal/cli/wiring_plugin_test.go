package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/store"
)

// pluginWorkflow references the runner name only the [runner.echotest]
// config block defines, so a pass proves the wiring registered it.
const pluginWorkflow = `
[workflow]
name = "plugin_wired"

[action.bounce]
runner = "echotest"
params = { note = "wired" }
`

// buildEchoBinary compiles the reference plugin into the test's temp dir;
// the Go build cache keeps repeat compiles cheap.
func buildEchoBinary(t *testing.T) string {
	t.Helper()

	binary := filepath.Join(t.TempDir(), "echo")

	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary,
		"github.com/chadit/CairnOS/examples/plugins/echo")

	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build echo plugin: %v\n%s", err, output)
	}

	return binary
}

// appendPluginBlock adds a [runner.echotest] plugin block to the scaffolded
// config, the shape an operator writes to wire an out-of-process runner.
func appendPluginBlock(t *testing.T, dir, command string) {
	t.Helper()

	cfgPath := filepath.Join(dir, "config.toml")

	existing, err := os.ReadFile(cfgPath) // #nosec G304 -- test-owned scaffold path
	if err != nil {
		t.Fatalf("read scaffolded config: %v", err)
	}

	block := "\n[runner.echotest]\ntype = \"plugin\"\ncommand = [" + strconv.Quote(command) + "]\n"

	if err := os.WriteFile(cfgPath, append(existing, []byte(block)...), 0o600); err != nil {
		t.Fatalf("write config with plugin block: %v", err)
	}
}

func TestRunExecutesPluginBackedRunnerFromConfig(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	appendPluginBlock(t, dir, buildEchoBinary(t))
	path := writeWorkflow(t, dir, pluginWorkflow)

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

	if len(runs) != 1 || runs[0].Status != store.RunSucceeded {
		t.Fatalf("persisted runs = %+v, want one succeeded row", runs)
	}

	results, err := db.ListActionResults(t.Context(), runs[0].ID)
	if err != nil {
		t.Fatalf("list action results: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("results = %+v, want the one bounce action", results)
	}

	output, isMap := results[0].Output.(map[string]any)
	if !isMap || output["note"] != "wired" {
		t.Fatalf("stored output = %#v, want the echoed params", results[0].Output)
	}
}

func TestRunPluginBlockWithMissingBinaryFailsAtWiring(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	appendPluginBlock(t, dir, filepath.Join(dir, "missing-plugin"))
	path := writeWorkflow(t, dir, pluginWorkflow)

	code, _, stderr := runCLI(t, runCmd, path, flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("run exited %d, want %d (stderr: %s)", code, cli.ExitFailure, stderr)
	}

	if !strings.Contains(stderr, `start plugin runner "echotest"`) {
		t.Fatalf("stderr does not name the failed plugin wiring:\n%s", stderr)
	}
}
