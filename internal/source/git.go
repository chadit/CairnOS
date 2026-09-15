// Package source keeps a git working tree current from its remote so a host
// gets its workflows and scripts from a repository instead of a deploy step. It
// only ever clones, fetches, fast-forwards, and checks out a tag detached: reset,
// stash, clean, and a non-fast-forward merge are never run, so a tree an operator
// has edited fails the sync untouched.
package source

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
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
// recorder, and so the argv stays the unit both sides agree on.
type Git func(ctx context.Context, dir string, argv ...string) (string, error)

// GitCommand is the production Git over os/exec. It returns trimmed stdout on
// success and, on failure, an error carrying git's own stderr, which is where
// git explains a rejected merge or an unreachable remote.
func GitCommand(ctx context.Context, dir string, argv ...string) (string, error) {
	full := append([]string{"-C", dir}, argv...)

	// #nosec G204 -- argv comes from this package's own fixed command sequences
	// and from a repo and ref the operator wrote in config.toml, both of which
	// config load has already refused if they could pass as an option. Neither
	// workflow nor network input reaches here.
	cmd := exec.CommandContext(ctx, gitBinary, full...)

	cmd.Env = gitEnv(os.Environ())
	cmd.WaitDelay = waitDelay

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	stdout, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s",
			strings.Join(argv, " "), err, strings.TrimSpace(stderr.String()))
	}

	return strings.TrimSpace(string(stdout)), nil
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
