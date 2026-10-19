package engine_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// stdinFileTOML feeds prompt.md to cat through the exec runner, and the
// capture step records what cat printed.
const stdinFileTOML = `
[workflow]
name = "report"

[action.up]
runner = "mock"
params = { output = { field = "x" } }

[action.down]
runner     = "exec"
depends_on = ["up"]
params     = { command = "cat", stdin_file = "prompt.md" }

[action.record]
runner     = "local_fn"
depends_on = ["down"]
params     = { fn = "capture", text = "${{ down.output }}" }
`

// The file's text has to reach the command with its ${{ }} references
// resolved, exactly as an inline stdin string would.
func TestStdinFileExpandsReferencesAtRunTime(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	files := map[string]string{
		"report.toml": stdinFileTOML,
		"prompt.md":   "Summarize.\n\n## INPUT\n${{ up.output.field }}\n",
	}

	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	flow, err := workflow.Load(filepath.Join(dir, "report.toml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	sink := &paramSink{}
	eng, _ := newEngineWithRunners(t, baseConfig(), map[string]runner.Factory{
		builtins.MockName:    builtins.MockFactory(),
		builtins.LocalFnName: builtins.LocalFnFactory(map[string]builtins.Func{fnCapture: sink.capture}),
		builtins.ExecName: func(map[string]any) (runner.Runner, error) {
			return builtins.NewExec(map[string]any{"allow_actions": []any{"down"}, "timeout": int64(30)})
		},
	})

	wantRunStatus(t, mustRun(t, eng, flow, nil), store.RunSucceeded)

	// exec output drops the trailing newline, so the file's last one is gone.
	if got, want := sink.snapshot()["text"], "Summarize.\n\n## INPUT\nx"; got != want {
		t.Fatalf("cat printed %q, want the resolved prompt %q", got, want)
	}
}
