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

// CLIDefaults maps each built-in CLI runner name to its launch defaults. Each
// key is a runner name and a [runner.<name>] type value, so removing one breaks
// the configs and workflows that name it. Every argv template picks headless
// one-shot mode: a bare `claude <prompt>` opens a TUI no workflow can answer.
// Wrapper-script blocks start from these defaults too.
func CLIDefaults() map[string]CLITool {
	return map[string]CLITool{
		ClaudeName:      {Binary: "claude", Args: []string{printFlag, promptToken}},
		CodexName:       {Binary: "codex", Args: []string{"exec", promptToken}},
		QwenName:        {Binary: "qwen", Args: []string{printFlag, promptToken}},
		OMPName:         {Binary: "omp", Args: []string{printFlag, promptToken}},
		AntigravityName: {Binary: "antigravity"},
	}
}

// CLI wraps one prompt-driven command-line tool as a runner. Every named CLI
// built-in is one CLI over its own launch template
// (.agents/spec/03-runner-interface.md, "Built-in runners").
//
// Config keys (all optional): binary overrides the launched executable, args
// is the argv template where {prompt} substitutes the prompt param, and
// stdin = true sends the prompt on standard input instead. Params: prompt.
type CLI struct {
	name   string
	binary string

	// reauthProvider is the built-in whose launch template this runner started
	// from. The re-auth code carries it instead of name, because the scheduler
	// holds a job on the credential named before the code's last dot, and a
	// [runner.summarize] block wrapping claude needs the claude credential.
	reauthProvider string

	args []string

	// env carries the rendered host table. Nil is the ordinary case and keeps
	// cmd.Env unset, so the child inherits serve's environment exactly as it did
	// before hosts existed; a non-empty slice is appended to that same
	// environment rather than replacing it.
	env []string

	// inherited is the environment the child starts from. Nil leaves cmd.Env
	// unset, so the child inherits this process's environment exactly as it did
	// before hosts existed; wiring passes os.Environ().
	inherited []string

	stdin bool

	// allowDashedPrompt opts out of the dashed-prompt guard, for operators who
	// have vetted the wrapped binary and want a prompt that may begin with a
	// dash to ride a bare positional anyway (spec M3).
	allowDashedPrompt bool
}

// NewCLI builds a named CLI runner from its launch template and config block.
func NewCLI(name string, tool CLITool, cfg map[string]any) (*CLI, error) {
	built, err := decodeCLI(name, tool, cfg)
	if err != nil {
		return nil, wrapConfigErr(name, err)
	}

	return built, nil
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

		built.env = hostEnv
		built.inherited = inherited

		return built, nil
	}
}

// Name implements runner.Runner.
func (c *CLI) Name() string {
	return c.name
}

// Execute implements runner.Runner.
func (c *CLI) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	return c.ExecuteStream(ctx, task, nil)
}
