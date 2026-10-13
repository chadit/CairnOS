package builtins

import (
	"context"

	"github.com/chadit/CairnOS/pkg/runner"
)

// printFlag is the print-and-exit flag claude, qwen, and omp share for
// one-shot runs.
const printFlag = "-p"

// Registry names of the wrapped command-line tools.
const (
	ClaudeName      = "claude"
	CodexName       = "codex"
	QwenName        = "qwen"
	OMPName         = "omp"
	AntigravityName = "antigravity"
)

// CLITool is one built-in wrapper's launch defaults: the binary it runs and
// the argv template that makes the tool print one response and exit.
type CLITool struct {
	Binary string
	Args   []string
}

// CLIDefaults maps each built-in CLI runner name to its launch defaults. Every
// key is both a runner name workflows use and a type value [runner.<name>]
// blocks set, so removing one breaks the configs and workflows that name it.
// Every argv template picks the tool's headless one-shot mode, because a
// workflow can never answer an interactive UI: a bare `claude <prompt>` opens
// the tool's TUI and hangs the run. configuredRunnerFactory in
// internal/cli/wiring.go starts a wrapper script's block from the defaults its
// type names.
func CLIDefaults() map[string]CLITool {
	return map[string]CLITool{
		ClaudeName:      {Binary: "claude", Args: []string{printFlag, promptToken}},
		CodexName:       {Binary: "codex", Args: []string{"exec", promptToken}},
		QwenName:        {Binary: "qwen", Args: []string{printFlag, promptToken}},
		OMPName:         {Binary: "omp", Args: []string{printFlag, promptToken}},
		AntigravityName: {Binary: "antigravity"},
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
func NewCLI(name string, tool CLITool, cfg map[string]any) (*CLI, error) {
	helper, err := newCLIExec(name, tool, cfg)
	if err != nil {
		return nil, wrapConfigErr(name, err)
	}

	return &CLI{exec: helper}, nil
}

// CLIFactory adapts one named tool to the registry contract, handing its
// children the rendered host table on top of inherited, the environment they
// start from. Both nil is the no-hosts case and leaves the child inheriting
// serve's environment untouched.
func CLIFactory(name string, tool CLITool, hostEnv, inherited []string) runner.Factory {
	return func(cfg map[string]any) (runner.Runner, error) {
		built, err := NewCLI(name, tool, cfg)
		if err != nil {
			return nil, err
		}

		built.exec.env = hostEnv
		built.exec.inherited = inherited

		return built, nil
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
