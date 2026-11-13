package source_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/source"
)

// stubGitScript stands in for git long enough to report what GitCommand handed
// it: the two prompt settings and the argv it was launched with.
const stubGitScript = `#!/bin/sh
echo "GIT_TERMINAL_PROMPT=${GIT_TERMINAL_PROMPT}"
echo "GIT_SSH_COMMAND=${GIT_SSH_COMMAND}"
echo "argv=$*"
`

// orphanGitScript stands in for a git that leaves a grandchild holding the
// pipes, the shape a credential helper or an ssh process takes when it survives
// the kill a context deadline triggers. The background sleeper inherits stdout,
// so nothing closes the write end on its own.
const orphanGitScript = `#!/bin/sh
sleep 60 &
sleep 60
`

// killedCallDeadline is how long the stand-in runs before the context kills it,
// short enough that the wait afterwards is the only thing the case measures.
const killedCallDeadline = 200 * time.Millisecond

// returnBound is the ceiling the killed call has to come back inside. With no
// WaitDelay on the command, Wait blocks on the orphan's pipe for the sleeper's
// full minute, so this case reports the hang instead of sitting through it.
const returnBound = 20 * time.Second

// oneShotChanSize buffers the channel carrying the killed call's one error, so
// the goroutine finishes even when the case fails before reading it.
const oneShotChanSize = 1

// stubPerm is the mode the stand-in script needs to be launched.
const stubPerm = 0o700

// stageStubGit writes one stand-in script into a fresh directory and returns a
// source.Git whose PATH is that directory plus searchPath. Handing the
// environment over is what replaces t.Setenv, so nothing a case stages reaches
// the rest of the process. Each case calls it before t.Parallel, because a fork
// racing the open write fd fails with ETXTBSY.
func stageStubGit(t *testing.T, script, searchPath string, extra ...string) source.Git {
	t.Helper()

	binDir := t.TempDir()

	stub := filepath.Join(binDir, "git")
	if err := os.WriteFile(stub, []byte(script), stubPerm); err != nil {
		t.Fatalf("write stub git: %v", err)
	}

	search := binDir
	if searchPath != "" {
		search += string(os.PathListSeparator) + searchPath
	}

	return source.GitCommandWith(append([]string{"PATH=" + search}, extra...))
}

// TestGitCommandRunsNonInteractively proves the production command cannot stop
// on a credential prompt: a real prompt would hang the clone at serve start or a
// sync inside a scheduled run.
func TestGitCommandRunsNonInteractively(t *testing.T) {
	runGit := stageStubGit(t, stubGitScript, "", "GIT_TERMINAL_PROMPT=1")

	t.Parallel()

	out, err := runGit(t.Context(), "/some/checkout", "fetch", "origin")
	if err != nil {
		t.Fatalf("GitCommand: %v", err)
	}

	if !strings.Contains(out, "GIT_TERMINAL_PROMPT=0") {
		t.Fatalf("git saw %q, want GIT_TERMINAL_PROMPT=0 even though the parent set 1", out)
	}

	if !strings.Contains(out, "argv=-C /some/checkout fetch origin") {
		t.Fatalf("git saw %q, want the invocation rooted at the checkout with -C", out)
	}

	if !strings.Contains(out, "GIT_SSH_COMMAND=ssh -o BatchMode=yes") {
		t.Fatalf("git saw %q, want ssh put in batch mode so a host-key question fails instead of asking", out)
	}
}

// TestGitCommandKeepsOperatorSSHCommand proves the batch-mode default yields to
// an operator who has already said how this host reaches its remotes.
func TestGitCommandKeepsOperatorSSHCommand(t *testing.T) {
	runGit := stageStubGit(t, stubGitScript, "", "GIT_SSH_COMMAND=ssh -i /etc/cairnos/deploy_key")

	t.Parallel()

	out, err := runGit(t.Context(), "/some/checkout", "fetch", "origin")
	if err != nil {
		t.Fatalf("GitCommand: %v", err)
	}

	if !strings.Contains(out, "GIT_SSH_COMMAND=ssh -i /etc/cairnos/deploy_key") {
		t.Fatalf("git saw %q, want the operator's own ssh command left alone", out)
	}
}

// TestGitCommandReturnsAfterKillWithOrphanHoldingPipes proves the git time bound
// survives a grandchild. Killing git does not close the stdout it handed down,
// so without a WaitDelay the call never returns and the runner's per-source lock
// stays held for the life of the process.
func TestGitCommandReturnsAfterKillWithOrphanHoldingPipes(t *testing.T) {
	// The real search path rides along because the script needs sleep to
	// resolve. Without it both sleeps fail instantly, no orphan holds the pipes,
	// and the case passes while proving nothing.
	runGit := stageStubGit(t, orphanGitScript, os.Getenv("PATH"))

	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), killedCallDeadline)
	defer cancel()

	done := make(chan error, oneShotChanSize)
	start := time.Now()

	go func() {
		_, err := runGit(ctx, t.TempDir(), "fetch", "origin")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("GitCommand returned nil for a call its context killed")
		}
	case <-time.After(returnBound):
		t.Fatalf("GitCommand did not return within %s of its context deadline", returnBound)
	}

	// A call that comes back before the deadline never reached the kill, which
	// means the stand-in died on its own and the orphan this case is about was
	// never there.
	if elapsed := time.Since(start); elapsed < killedCallDeadline {
		t.Fatalf("GitCommand returned after %s, before its %s deadline; the stand-in never held the pipes",
			elapsed, killedCallDeadline)
	}
}
