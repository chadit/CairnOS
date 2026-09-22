package builtins

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/internal/execretry"
	"github.com/chadit/CairnOS/internal/shellquote"
)

// usageTailKeep is how much of a launcher's stderr survives into an error
// message. Neither protocol carries a token, but a short tail keeps whatever a
// wrapper prints out of the run store anyway.
const usageTailKeep = 512

// remoteScriptHint seeds the remote command builder. A wrapper plus a launcher
// and its protocol argv land near this, so the builder rarely grows.
const remoteScriptHint = 256

// usageWaitDelay bounds the wait after the child is signaled. A launcher that
// handed its stdout to a grandchild can delay EOF past its own exit, and Wait
// would otherwise hold the probe open with nothing left to read.
const usageWaitDelay = 2 * time.Second

// ssh flags every remote probe uses. BatchMode refuses a password prompt rather
// than blocking on a terminal the serve process does not have, -T keeps the
// stream clean for the protocol, and the connect bound stops a box that is off
// from eating the whole per-launcher budget.
const (
	sshNoTTY          = "-T"
	sshOption         = "-o"
	sshBatchMode      = "BatchMode=yes"
	sshConnectTimeout = "ConnectTimeout=10"
)

// converse runs one launcher and reads its answer. cancel ends the child once
// the answer is in hand: the Codex app-server keeps running after it replies,
// and the signal reaches whatever the launcher wrapped.
func (u *Usage) converse(
	ctx context.Context, cancel context.CancelFunc, host, launcher string, dialog usageDialog,
) (usageDetail, error) {
	path, argv, err := u.command(host, launcher, dialog)
	if err != nil {
		return usageDetail{}, err
	}

	child, err := startUsageChild(ctx, path, argv)
	if err != nil {
		return usageDetail{}, fmt.Errorf("%s: %w", launcher, err)
	}

	detail, exchangeErr := child.exchange(dialog)

	// The context is read before close, which cancels that same context, and
	// the stderr tail after it, because a copier goroutine os/exec owns writes
	// that tail until the child is reaped.
	cause := ctx.Err()

	child.close(cancel)

	if exchangeErr != nil {
		return usageDetail{}, child.explain(cause, exchangeErr)
	}

	return detail, nil
}

// command picks the program and argv for one probe: the launcher itself when
// the task named no host, or ssh carrying the launcher as a remote command.
func (u *Usage) command(host, launcher string, dialog usageDialog) (string, []string, error) {
	if host == "" {
		path, err := u.resolveLocal(launcher)
		if err != nil {
			return "", nil, err
		}

		return path, dialog.args, nil
	}

	return u.remoteCommand(host, launcher, dialog)
}

// resolveLocal finds the launcher on the PATH this runner was handed. A "~/"
// entry expands against the same environment's HOME, which is the one home a
// local probe can mean.
func (u *Usage) resolveLocal(launcher string) (string, error) {
	if rest, found := strings.CutPrefix(launcher, usageHomePrefix); found {
		home := u.lookup.Get("HOME")
		if home == "" {
			return "", fmt.Errorf("%w: %s needs HOME to expand %q", errUsageNoHome, UsageName, launcher)
		}

		launcher = filepath.Join(home, rest)
	}

	path, err := environ.LookPath(u.lookup, launcher)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", launcher, err)
	}

	return path, nil
}

// remoteCommand builds the ssh invocation. The launcher's own stdio is the
// protocol stream, so ssh runs it with exec and nothing else writes to the pipe.
func (u *Usage) remoteCommand(host, launcher string, dialog usageDialog) (string, []string, error) {
	entry := u.hosts[host]

	path, err := environ.LookPath(u.lookup, u.ssh)
	if err != nil {
		return "", nil, fmt.Errorf("resolve %s: %w", u.ssh, err)
	}

	argv := []string{
		sshNoTTY,
		sshOption, sshBatchMode,
		sshOption, sshConnectTimeout,
		entry.SSH,
		argTerminator,
		remoteScript(entry.Wrapper, launcher, dialog.args),
	}

	return path, argv, nil
}

// remoteScript renders the single command the remote login shell runs. A
// wrapper takes the launcher as its own argv and execs it, so exec here leaves
// no extra process between ssh and the protocol either way.
func remoteScript(wrapper, launcher string, args []string) string {
	var script strings.Builder

	script.Grow(remoteScriptHint)
	script.WriteString("exec ")

	if wrapper != "" {
		script.WriteString(remoteWord(wrapper))
		script.WriteByte(' ')
	}

	script.WriteString(remoteWord(launcher))

	for _, arg := range args {
		script.WriteByte(' ')
		script.WriteString(shellquote.Word(arg))
	}

	return script.String()
}

// remoteWord quotes one path for the remote shell. A leading "~/" becomes
// "$HOME"/ so the far side expands it against its own home; the rest stays
// single-quoted, since only that host knows what the tilde meant.
func remoteWord(path string) string {
	if rest, found := strings.CutPrefix(path, usageHomePrefix); found {
		return `"$HOME"/` + shellquote.Word(rest)
	}

	return shellquote.Word(path)
}

// usageChild is a started launcher with the three streams the exchange uses.
type usageChild struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *tailBuffer
}

// exchange writes the dialog's requests and reads back the answer.
func (c *usageChild) exchange(dialog usageDialog) (usageDetail, error) {
	if err := c.send(dialog.stdinLines, dialog.closeStdin); err != nil {
		return usageDetail{}, err
	}

	return readUsageAnswer(c.stdout, dialog.decode)
}

// startUsageChild launches one probe with its pipes, retrying a busy executable
// the way the other process runners here do.
func startUsageChild(ctx context.Context, path string, argv []string) (*usageChild, error) {
	child := &usageChild{}

	started, err := execretry.Start(ctx, func() (*exec.Cmd, error) {
		// #nosec G204 -- path and argv come from the operator's [runner.usage]
		// block and [host.<name>] table, both held to a grammar with no shell
		// metacharacters in it. No workflow param reaches either one.
		cmd := exec.CommandContext(ctx, path, argv...)
		cmd.WaitDelay = usageWaitDelay
		groupProcess(cmd)

		child.stderr = &tailBuffer{limit: usageTailKeep}
		cmd.Stderr = child.stderr

		return child.attachPipes(cmd)
	})

	child.cmd = started

	if err != nil {
		return child, fmt.Errorf("start process: %w", err)
	}

	return child, nil
}

// attachPipes wires stdin and stdout onto one attempt's command.
func (c *usageChild) attachPipes(cmd *exec.Cmd) (*exec.Cmd, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errUsagePipe, err)
	}

	c.stdin = stdin

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()

		return nil, fmt.Errorf("%w: %w", errStdoutPipe, err)
	}

	c.stdout = stdout

	return cmd, nil
}

// send writes the dialog's requests and closes stdin when the protocol wants
// EOF. Both payloads are a few hundred bytes, well inside a pipe buffer, so the
// write cannot block on a child that has not started reading.
func (c *usageChild) send(lines []string, closeAfter bool) error {
	for _, line := range lines {
		if _, err := io.WriteString(c.stdin, line+"\n"); err != nil {
			return fmt.Errorf("write request: %w", err)
		}
	}

	if !closeAfter {
		return nil
	}

	if err := c.stdin.Close(); err != nil {
		return fmt.Errorf("close stdin: %w", err)
	}

	return nil
}

// close ends the exchange. stdin goes first, because both CLIs shut down on
// EOF, and the context cancel then signals anything still running.
func (c *usageChild) close(cancel context.CancelFunc) {
	_ = c.stdin.Close()

	cancel()

	_ = c.cmd.Wait()
}

// explain turns a failed exchange into the message the report carries. cause is
// the run context's error, which separates a spent deadline from a run someone
// stopped: the second says nothing about the CLI and must not read as a
// timeout. Either way the launcher's own stderr tail rides along, because that
// is where a wrapper says what went wrong.
func (c *usageChild) explain(cause, err error) error {
	reported := err

	switch {
	case errors.Is(cause, context.DeadlineExceeded):
		reported = fmt.Errorf("%w waiting for a usage response", errUsageTimeout)
	case cause != nil:
		reported = fmt.Errorf("%w: %w", errUsageCanceled, cause)
	}

	tail := c.stderr.Text()
	if tail == "" {
		return reported
	}

	return fmt.Errorf("%w: %s", reported, usageTail(tail))
}

// readUsageAnswer scans stdout until decode matches a line. Both streams carry
// notifications and system events, so only a matched line ends the read.
func readUsageAnswer(pipe io.Reader, decode usageDecoder) (usageDetail, error) {
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 0, initialScanBuffer), maxCapturedOutput)

	for scanner.Scan() {
		detail, matched, err := decode(scanner.Bytes())
		if err != nil {
			return usageDetail{}, err
		}

		if matched {
			return detail, nil
		}
	}

	if err := scanner.Err(); err != nil {
		return usageDetail{}, fmt.Errorf("read usage response: %w", err)
	}

	return usageDetail{}, errUsageNoResponse
}

// usageTail trims a message to the same bound the stderr tail gets, so a
// chatty failure cannot balloon a stored run.
func usageTail(text string) string {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) <= usageTailKeep {
		return trimmed
	}

	return trimmed[:usageTailKeep] + "..."
}
