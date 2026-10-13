package builtins_test

// The launch templates must pick each tool's headless one-shot mode: a bare
// positional prompt opens codex's or claude's interactive UI and hangs a
// workflow action, so the defaults are part of the runner contract.

import (
	"slices"
	"testing"

	"github.com/chadit/CairnOS/internal/runner/builtins"
)

// printArg is the shared print-and-exit flag the assertions pin.
const printArg = "-p"

// TestCLIDefaultsUseHeadlessTemplates pins each built-in wrapper's binary and
// argv template: codex routes through its exec subcommand, claude, qwen, and
// omp print-and-exit with -p, and antigravity keeps the bare positional
// fallback (nil template).
func TestCLIDefaultsUseHeadlessTemplates(t *testing.T) {
	t.Parallel()

	want := map[string]builtins.CLITool{
		builtins.ClaudeName:      {Binary: "claude", Args: []string{printArg, promptArg}},
		builtins.CodexName:       {Binary: codexBinary, Args: []string{"exec", promptArg}},
		builtins.QwenName:        {Binary: "qwen", Args: []string{printArg, promptArg}},
		builtins.OMPName:         {Binary: "omp", Args: []string{printArg, promptArg}},
		builtins.AntigravityName: {Binary: "antigravity"},
	}

	got := builtins.CLIDefaults()

	if len(got) != len(want) {
		t.Fatalf("CLIDefaults holds %d tools, want %d", len(got), len(want))
	}

	for name, wantTool := range want {
		tool, exists := got[name]
		if !exists {
			t.Errorf("CLIDefaults missing %q", name)

			continue
		}

		if tool.Binary != wantTool.Binary || !slices.Equal(tool.Args, wantTool.Args) {
			t.Errorf("%s launches %q %v, want %q %v", name, tool.Binary, tool.Args, wantTool.Binary, wantTool.Args)
		}
	}
}
