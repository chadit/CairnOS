package builtins

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/chadit/CairnOS/pkg/runner"
)

// ExecName is the registry name of the host exec runner.
const ExecName = "exec"

// cfgKeyEnv names the optional table added to the inherited environment. Values
// are read straight from the config file and never resolved from the secret
// source: an operator who needs a secret here exports it into the serve process
// instead, which keeps this runner out of the credential path entirely.
const cfgKeyEnv = "env"

// paramArgs names the argv list a task supplies. The CLI runners spell the same
// word as a config key (cfgKeyArgs) because their argv is operator-owned; here
// the workflow chooses it, so the two are separate contracts.
const paramArgs = "args"

// paramStdin names the text handed to the command's standard input. A webhook
// receiver reads its payload from stdin, so the same script keeps one contract
// whether a scheduler or an HTTP hop feeds it, and the payload stays out of
// argv where a process listing would expose it.
const paramStdin = "stdin"

// Classified error codes this runner returns. The param codes (missing_command,
// bad_command, bad_workdir, bad_stdin) come from requireStringParam and
// optionalStringParam with the ExecName prefix.
const (
	codeExecDenied   = ExecName + ".denied"
	codeExecBadArgs  = ExecName + ".bad_" + paramArgs
	codeExecTimeout  = ExecName + ".timeout"
	codeExecExit     = ExecName + ".exit"
	codeExecStart    = ExecName + ".start"
	codeExecNotFound = ExecName + ".not_found"
	codeExecOutput   = ExecName + ".output"
)

// Exec runs one host command per task with the inherited environment and no
// isolation. That is the design: the operator's model CLIs and gh already hold
// their logins here, and copying credentials into a container is off the table.
//
// The action gate is the only control. An allowed action runs with the serve
// user's full authority.
type Exec struct {
	allowActions map[string]struct{}
	env          []string // "KEY=VALUE" entries appended to the inherited environment

	// inherited is the environment children start from. Nil leaves cmd.Env
	// unset, so the child inherits this process's environment the way it always
	// has; wiring passes os.Environ() and a test passes a fixed slice.
	inherited []string
	timeout   time.Duration
}

// NewExec decodes the runner config block. An absent or empty block builds a
// runner that denies every action, which is what keeps BuildAll green on a host
// that never configured exec. A block the operator actually wrote must carry a
// positive timeout, so a configured runner can never launch without a bound.
func NewExec(cfg map[string]any) (*Exec, error) {
	allowList, err := configStrings(cfg, cfgKeyAllowActions)
	if err != nil {
		return nil, wrapConfigErr(ExecName, err)
	}

	timeout, err := decodeExecTimeout(cfg)
	if err != nil {
		return nil, wrapConfigErr(ExecName, err)
	}

	env, err := decodeExecEnv(cfg)
	if err != nil {
		return nil, wrapConfigErr(ExecName, err)
	}

	return &Exec{allowActions: toSet(allowList), timeout: timeout, env: env}, nil
}

// ExecFactory adapts the runner to the registry contract with no host table,
// which is the shape the runner's own tests want. Production wiring must not use
// it: every entry point goes through newRegistry, and registering this one there
// would drop the host table for that path without any signal. Use
// ExecFactoryWithHostEnv, passing nil for the no-hosts case.
func ExecFactory(cfg map[string]any) (runner.Runner, error) {
	return NewExec(cfg)
}

// ExecFactoryWithHostEnv adapts the runner to the registry contract and hands
// every child the rendered host table. The entries land after the config env
// table, though nothing can collide there: decodeExecEnv refuses a key with the
// reserved prefix, so the rendered table is the only writer of those names.
func ExecFactoryWithHostEnv(hostEnv, inherited []string) runner.Factory {
	return func(cfg map[string]any) (runner.Runner, error) {
		built, err := NewExec(cfg)
		if err != nil {
			return nil, err
		}

		built.env = append(built.env, hostEnv...)
		built.inherited = inherited

		return built, nil
	}
}

// Name implements runner.Runner.
func (*Exec) Name() string { return ExecName }

// Validate runs in both engine passes. The gate fires regardless of
// Task.Resolved, since ActionName is never a ${{ }} reference: a denied action
// fails at workflow load, before anything runs.
func (e *Exec) Validate(task *runner.Task) error {
	if err := e.gate(task); err != nil {
		return err
	}

	if err := requireStringParam(task, paramCommand, ExecName); err != nil {
		return err
	}

	if err := validateExecArgs(task); err != nil {
		return err
	}

	if err := optionalStringParam(task, keyWorkdir, ExecName); err != nil {
		return err
	}

	return optionalStringParam(task, paramStdin, ExecName)
}

// Execute implements runner.Runner. A non-zero exit comes back permanent with
// its code in the message, so the engine's retry policy decides what happens
// next rather than this runner.
func (e *Exec) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	if err := e.Validate(task); err != nil {
		return runner.Result{}, err
	}

	command, _ := task.Params[paramCommand].(string)

	runCtx, cancel := e.runContext(ctx, task)
	defer cancel()

	return launchExec(runCtx, func() *exec.Cmd {
		// #nosec G204 -- the command and its argv come from a workflow action whose
		// name the operator listed in allow_actions. This runner has no isolation by
		// design (see the type doc); the gate is the trust boundary.
		cmd := exec.CommandContext(runCtx, command, execArgv(task)...)
		cmd.Dir, _ = task.Params[keyWorkdir].(string)

		if e.inherited != nil || len(e.env) > 0 {
			cmd.Env = append(slices.Clone(e.inherited), e.env...)
		}

		// A nil Stdin hands the child /dev/null, so an absent param leaves a command
		// that reads stdin seeing EOF straight away instead of blocking on the
		// serve process's own terminal.
		if text, hasStdin := task.Params[paramStdin].(string); hasStdin {
			cmd.Stdin = strings.NewReader(text)
		}

		return cmd
	})
}

// gate is the deny-by-default check and the only control this runner has. An
// absent block, an absent key, and an empty list all leave allowActions empty,
// which denies everything.
func (e *Exec) gate(task *runner.Task) error {
	if _, allowed := e.allowActions[task.ActionName]; allowed {
		return nil
	}

	return runner.NewError(runner.ClassPermanent, codeExecDenied,
		"action "+task.ActionName+" is not in "+ExecName+" allow_actions")
}

// runContext derives the effective deadline: the config timeout as the
// backstop, Task.Deadline when the engine set an earlier one. The timeout is
// always positive whenever the gate can pass, since only a configured block
// carries a non-empty allow list and such a block must set one.
func (e *Exec) runContext(ctx context.Context, task *runner.Task) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(e.timeout)

	if !task.Deadline.IsZero() && task.Deadline.Before(deadline) {
		deadline = task.Deadline
	}

	return context.WithDeadline(ctx, deadline)
}

// launchExec runs the command, drains stdout under the shared 1 MiB cap, and
// classifies the outcome. build runs once per start attempt, since an exec.Cmd
// cannot be restarted after a busy executable.
func launchExec(runCtx context.Context, build func() *exec.Cmd) (runner.Result, error) {
	start := time.Now()

	piped, err := startPiped(runCtx, build)
	if err != nil {
		if errors.Is(err, errStdoutPipe) {
			return runner.Result{}, runner.NewError(runner.ClassPermanent, codeExecStart, err.Error())
		}

		return runner.Result{}, classifyExecStart(runCtx, piped.path, err)
	}

	cmd, stderr := piped.cmd, piped.stderr

	stdoutText, scanErr := collectStdout(runCtx, piped.stdout, nil)

	waitErr := cmd.Wait()
	latency := time.Since(start)

	if waitErr != nil {
		return runner.Result{}, classifyExecWait(runCtx, waitErr, cmd.ProcessState.ExitCode(), stderr.Text())
	}

	if scanErr != nil {
		return runner.Result{}, runner.NewError(runner.ClassPermanent, codeExecOutput,
			"read stdout: "+scanErr.Error())
	}

	return runner.Result{
		Output: stdoutText,
		Usage:  runner.Usage{Latency: latency},
		Meta: map[string]string{
			metaExitCode:   strconv.Itoa(cmd.ProcessState.ExitCode()),
			metaStderrTail: stderr.Text(),
		},
	}, nil
}

// classifyExecStart maps a launch failure. A missing command is permanent, since
// no retry installs it. A deadline already spent at launch is the exception: it
// is the same transient timeout classifyExecWait reports mid-run, and calling it
// permanent would deny the retry a fresh deadline could clear.
func classifyExecStart(runCtx context.Context, command string, err error) error {
	if runCtx.Err() != nil {
		return runner.NewError(runner.ClassTransient, codeExecTimeout,
			command+" canceled or timed out")
	}

	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return runner.NewError(runner.ClassPermanent, codeExecNotFound,
			fmt.Sprintf("command %q not found: %v", command, err))
	}

	return runner.NewError(runner.ClassPermanent, codeExecStart,
		"start "+command+": "+err.Error())
}

// classifyExecWait maps a finished-but-failed process. A dead run context wins
// over the exit status because CommandContext kills the child on cancellation,
// so the non-zero exit is an effect of the timeout, not the command's verdict.
func classifyExecWait(runCtx context.Context, waitErr error, exitCode int, stderrTail string) error {
	if runCtx.Err() != nil {
		return runner.NewError(runner.ClassTransient, codeExecTimeout,
			"command canceled or timed out")
	}

	if _, isExit := errors.AsType[*exec.ExitError](waitErr); isExit {
		message := fmt.Sprintf("command exited with code %d", exitCode)
		if stderrTail != "" {
			message += ": " + stderrTail
		}

		return runner.NewError(runner.ClassPermanent, codeExecExit, message)
	}

	return runner.NewError(runner.ClassPermanent, codeExecStart,
		"wait for command: "+waitErr.Error())
}

// validateExecArgs checks the optional argv list. A whole-value reference is
// still a string during the load-time pass, so that shape defers to the
// launch-time pass the way a string param does.
func validateExecArgs(task *runner.Task) error {
	raw, exists := task.Params[paramArgs]
	if !exists {
		return nil
	}

	if deferred, isText := raw.(string); isText && !task.Resolved && containsRefDelimiter(deferred) {
		return nil
	}

	items, isList := raw.([]any)
	if !isList {
		return runner.NewError(runner.ClassPermanent, codeExecBadArgs,
			paramArgs+" must be a list of strings")
	}

	return checkArgvStrings(items)
}

// checkArgvStrings rejects the first non-string element, naming its position so
// a long argv points at the entry to fix.
func checkArgvStrings(items []any) error {
	for index, item := range items {
		if _, isString := item.(string); !isString {
			return runner.NewError(runner.ClassPermanent, codeExecBadArgs,
				fmt.Sprintf("%s[%d] must be a string, got %T", paramArgs, index, item))
		}
	}

	return nil
}

// execArgv converts the validated argv param into the slice os/exec wants.
// Validate has already proved every element is a string.
func execArgv(task *runner.Task) []string {
	items, isList := task.Params[paramArgs].([]any)
	if !isList {
		return nil
	}

	argv := make([]string, 0, len(items))

	for _, item := range items {
		text, _ := item.(string)
		argv = append(argv, text)
	}

	return argv
}

// decodeExecTimeout reads the wall-clock bound in seconds. It is optional only
// for a block nobody wrote, which builds the deny-everything runner; once a
// block exists the timeout is required, because a host command with no bound
// outlives the schedule that started it.
func decodeExecTimeout(cfg map[string]any) (time.Duration, error) {
	seconds, present, err := configInt(cfg, cfgKeyTimeout)
	if err != nil {
		return 0, err
	}

	if !present {
		return unconfiguredExecTimeout(cfg)
	}

	if seconds <= 0 {
		return 0, fmt.Errorf("%w: %s must be a positive number of seconds", ErrConfigValue, cfgKeyTimeout)
	}

	return time.Duration(seconds) * time.Second, nil
}

// unconfiguredExecTimeout decides what an absent timeout means. No keys at all
// is the startup default on a host that never enabled exec; anything else is an
// operator who wrote a block and left the bound out.
func unconfiguredExecTimeout(cfg map[string]any) (time.Duration, error) {
	if len(cfg) == 0 {
		return 0, nil
	}

	return 0, fmt.Errorf("%w: %s is required and must be a positive number of seconds",
		ErrConfigValue, cfgKeyTimeout)
}

// decodeExecEnv renders the optional env table into the "KEY=VALUE" form os/exec
// wants, sorted so the child sees a stable environment tail whatever order the
// map iterates in. A non-string value names its own key, since a config file
// with ten entries should not send the operator hunting for the bad one.
func decodeExecEnv(cfg map[string]any) ([]string, error) {
	raw, exists := cfg[cfgKeyEnv]
	if !exists {
		return nil, nil
	}

	table, isTable := raw.(map[string]any)
	if !isTable {
		return nil, fmt.Errorf("%w: %s must be a table of strings", ErrConfigValue, cfgKeyEnv)
	}

	entries := make([]string, 0, len(table))

	for _, name := range slices.Sorted(maps.Keys(table)) {
		if err := checkExecEnvKey(name); err != nil {
			return nil, err
		}

		text, isString := table[name].(string)
		if !isString {
			return nil, fmt.Errorf("%w: %s.%s must be a string, got %T",
				ErrConfigValue, cfgKeyEnv, name, table[name])
		}

		entries = append(entries, name+"="+text)
	}

	return entries, nil
}

// hostEnvPrefix is reserved for the rendered host table. A config env key using
// it would shadow or be shadowed by a rendered entry depending on append order,
// which is a question nobody should have to answer; refusing the key keeps the
// table's one source obvious.
const hostEnvPrefix = "CAIRNOS_HOST_"

// checkExecEnvKey rejects a name os/exec cannot carry, or one that belongs to
// the host table. An empty key, an equals sign inside one, or a NUL byte each
// produce an entry the child ignores or reads as a different variable, so the
// config file would be describing an environment the command never sees.
func checkExecEnvKey(name string) error {
	if name == "" {
		return fmt.Errorf("%w: %s has an empty key", ErrConfigValue, cfgKeyEnv)
	}

	if strings.ContainsAny(name, "=\x00") {
		return fmt.Errorf("%w: %s key %q must not contain an equals sign or a NUL byte",
			ErrConfigValue, cfgKeyEnv, name)
	}

	if strings.HasPrefix(name, hostEnvPrefix) {
		return fmt.Errorf("%w: %s key %q is reserved; %s variables come from the [host.<name>] blocks",
			ErrConfigValue, cfgKeyEnv, name, hostEnvPrefix)
	}

	return nil
}
