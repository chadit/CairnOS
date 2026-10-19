package cli_test

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
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

	// Windows runs only a file whose name ends in .exe, and go build -o writes
	// the exact name it is given, so exec would report the plugin not found.
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}

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

	block := "\n[runner.echotest]\ntype = \"plugin\"\ncommand = [" + strconv.Quote(command) + "]\n"

	appendConfig(t, dir, block)
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

	got := onlyActionOutput(t, dir)

	output, isMap := got.(map[string]any)
	if !isMap || output["note"] != "wired" {
		t.Fatalf("stored output = %#v, want the echoed params", got)
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
