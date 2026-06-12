package builtins

import (
	"context"

	"github.com/chadit/CairnOS/pkg/runner"
)

// Registry names of the wrapped command-line tools.
const (
	CodexName       = "codex"
	QwenName        = "qwen"
	OMPName         = "omp"
	OMPCodexName    = "omp_codex"
	OMPQwenName     = "omp_qwen"
	AntigravityName = "antigravity"
)

// CLIDefaults maps each built-in CLI runner name to the binary it launches
// when its config block does not override binary. The omp_* pair differs
// because those binaries carry a dash while runner names use underscores.
func CLIDefaults() map[string]string {
	return map[string]string{
		CodexName:       "codex",
		QwenName:        "qwen",
		OMPName:         "omp",
		OMPCodexName:    "omp-codex",
		OMPQwenName:     "omp-qwen",
		AntigravityName: "antigravity",
	}
}

// CLI wraps one prompt-driven command-line tool as a runner.
//
// Config keys (all optional): binary overrides the launched executable, args
// is the argv template where {prompt} substitutes the prompt param, and
// stdin = true sends the prompt on standard input instead. Params: prompt.
type CLI struct {
	exec cliExec
}

// NewCLI builds a named CLI runner over the shared subprocess helper.
func NewCLI(name, defaultBinary string, cfg map[string]any) (*CLI, error) {
	helper, err := newCLIExec(name, defaultBinary, cfg)
	if err != nil {
		return nil, wrapConfigErr(name, err)
	}

	return &CLI{exec: helper}, nil
}

// CLIFactory adapts one named tool to the registry contract.
func CLIFactory(name, defaultBinary string) runner.Factory {
	return func(cfg map[string]any) (runner.Runner, error) {
		return NewCLI(name, defaultBinary, cfg)
	}
}

// Name implements runner.Runner.
func (c *CLI) Name() string {
	return c.exec.name
}

// Validate implements runner.Runner: params must carry a string prompt.
func (c *CLI) Validate(task *runner.Task) error {
	return c.exec.validate(task)
}

// Execute implements runner.Runner.
func (c *CLI) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	return c.exec.run(ctx, task, nil)
}

// ExecuteStream implements runner.Streamer, forwarding stdout lines as
// chunks while the transcript still accumulates into Result.Output.
func (c *CLI) ExecuteStream(ctx context.Context, task *runner.Task, out chan<- runner.Chunk) (runner.Result, error) {
	return c.exec.run(ctx, task, out)
}
