// Package source keeps a git working tree current from its remote so a host
// gets its workflows and scripts from a repository instead of a deploy step. A
// tree it can fast-forward it fast-forwards; a tree it cannot it puts back on
// the ref with a forced checkout, discarding tracked edits, but only when the
// source leaves dirty_reset at its default. With dirty_reset off the sync
// reports what is in the way and writes nothing.
//
// Untracked files survive with one exception. clean, stash, and reset are never
// run, so nothing sweeps them, but a forced checkout replaces an untracked file
// whose path the incoming ref also tracks, because that is what -f means.
package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/internal/execretry"
)

// gitBinary is resolved through PATH on every call. Config load already refused
// a host that has no git, so a lookup failure here is a PATH that changed under
// a running serve.
const gitBinary = "git"

// noPromptEnv keeps git's own credential prompt shut. Without it a private repo
// with no usable credential opens a terminal prompt, which would hang a clone at
// serve start or a sync inside a scheduled run rather than failing it.
const noPromptEnv = "GIT_TERMINAL_PROMPT=0"

// sshEnvKey and batchModeSSH close the other prompt: an unknown host key or a
// passphrase-protected key asks on ssh's own terminal, which GIT_TERMINAL_PROMPT
// never reaches. BatchMode turns both into an immediate failure. An operator who
// has already set GIT_SSH_COMMAND keeps theirs, since they have said how this
// host reaches its remotes.
const (
	sshEnvKey    = "GIT_SSH_COMMAND"
	batchModeSSH = "ssh -o BatchMode=yes"
)

// waitDelay bounds the gap between killing git and giving up on the pipes it
// left behind. A credential helper or an ssh grandchild inherits the stdout and
// stderr write ends, so a plain Wait blocks forever after the context kill has
// already removed git itself. Five seconds lets a healthy child flush.
const waitDelay = 5 * time.Second

// Git runs one git invocation inside dir and returns its trimmed stdout. It is a
// function rather than an interface so a test can hand Ensure and Sync a
// recorder, and so the argv stays the unit both sides agree on. A failure that
// was a process exiting non-zero carries an *ExitError.
type Git func(ctx context.Context, dir string, argv ...string) (string, error)

// ExitError reports a git invocation that ran and exited non-zero, carrying the
// status alongside git's own message. merge-base --is-ancestor is why the status
// has to survive the Git seam: it answers through the exit code, where 1 is an
// answer and anything else is a real failure.
type ExitError struct {
	// Cause carries the message, including the stderr git printed.
	Cause error

	// Code is the process exit status.
	Code int
}

// Error implements error.
func (e *ExitError) Error() string {
	if e.Cause == nil {
		return fmt.Sprintf("git exited %d", e.Code)
	}

	return e.Cause.Error()
}

// Unwrap keeps errors.Is and errors.As reaching the underlying failure.
func (e *ExitError) Unwrap() error {
	return e.Cause
}

// GitCommand is the production Git over os/exec, under this process's own
// environment.
func GitCommand(ctx context.Context, dir string, argv ...string) (string, error) {
	return GitCommandWith(os.Environ())(ctx, dir, argv...)
}

// GitCommandWith is GitCommand over a supplied parent environment, which
// decides both what the child inherits and which binary PATH resolves to, so a
// caller can stage a stand-in without changing the real PATH.
//
// It returns trimmed stdout, or an error carrying the child's own stderr, where
// it explains a rejected merge or an unreachable remote.
func GitCommandWith(parent []string) Git {
	return func(ctx context.Context, dir string, argv ...string) (string, error) {
		return runGit(ctx, parent, dir, argv)
	}
}

// runGit launches one invocation rooted at dir.
func runGit(ctx context.Context, parent []string, dir string, argv []string) (string, error) {
	full := append([]string{"-C", dir}, argv...)

	binary, err := environ.LookPath(environ.Slice(parent), gitBinary)
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", gitBinary, strings.Join(argv, " "), err)
	}

	var stderr bytes.Buffer

	stdout, err := execretry.Output(ctx, func() (*exec.Cmd, error) {
		// #nosec G204 -- argv comes from this package's own fixed command sequences,
		// a repo and ref config load refused if they could pass as an option, and a
		// workflow file's path relative to dest, which always follows "--".
		cmd := exec.CommandContext(ctx, binary, full...)

		cmd.Env = gitEnv(parent)
		cmd.WaitDelay = waitDelay

		stderr.Reset()
		cmd.Stderr = &stderr

		return cmd, nil
	})
	if err != nil {
		return "", gitFailure(argv, stderr.String(), err)
	}

	return strings.TrimSpace(string(stdout)), nil
}

// gitFailure builds the error a failed invocation returns: git's own argv and
// stderr, wrapped in an ExitError when the process actually ran, so a caller
// probing through the exit status can read it. A failure to start the process
// carries no status and stays a plain error.
func gitFailure(argv []string, stderr string, cause error) error {
	failure := fmt.Errorf("git %s: %w: %s",
		strings.Join(argv, " "), cause, strings.TrimSpace(stderr))

	exit, ok := errors.AsType[*exec.ExitError](cause)
	if !ok {
		return failure
	}

	return &ExitError{Code: exit.ExitCode(), Cause: failure}
}

// gitEnv builds the child's environment from the parent's: prompts off always,
// and ssh put in batch mode only when the operator has not named their own ssh
// command.
func gitEnv(parent []string) []string {
	env := append(append([]string{}, parent...), noPromptEnv)

	if hasEnvValue(parent, sshEnvKey) {
		return env
	}

	return append(env, sshEnvKey+"="+batchModeSSH)
}

// hasEnvValue reports whether env carries a non-empty value for key. An empty
// assignment does not count, because git treats one as unset and falls back to
// plain ssh, which is the prompt this setting exists to close.
func hasEnvValue(env []string, key string) bool {
	prefix := key + "="

	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) && entry != prefix {
			return true
		}
	}

	return false
}
