package cli_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
)

const (
	updateCmd      = "update"
	flagCheckArg   = "--check"
	flagSourceArg  = "--source"
	updateLinkName = "cairnos"
)

func TestUpdateSourceDiscovery(t *testing.T) {
	t.Parallel()

	checkout := stageCheckout(t)
	executable := filepath.Join(checkout, "bin", updateLinkName)

	t.Run("finds the checkout around the running binary", func(t *testing.T) {
		t.Parallel()

		dir, err := cli.UpdateSourceDir("", executable)
		if err != nil {
			t.Fatalf("UpdateSourceDir() error = %v", err)
		}

		if dir != checkout {
			t.Errorf("dir = %s, want %s", dir, checkout)
		}
	})

	t.Run("source overrides the binary's own checkout", func(t *testing.T) {
		t.Parallel()

		other := stageCheckout(t)

		dir, err := cli.UpdateSourceDir(other, executable)
		if err != nil {
			t.Fatalf("UpdateSourceDir() error = %v", err)
		}

		if dir != other {
			t.Errorf("dir = %s, want %s", dir, other)
		}
	})

	t.Run("a directory that is not a checkout is refused", func(t *testing.T) {
		t.Parallel()

		bare := t.TempDir()

		if _, err := cli.UpdateSourceDir(bare, executable); err == nil {
			t.Fatal("UpdateSourceDir() accepted a directory with no go.mod and .git")
		}
	})
}

func TestUpdateFastForward(t *testing.T) {
	t.Parallel()
	requireGit(t)

	t.Run("a checkout that is behind is fast-forwarded", func(t *testing.T) {
		t.Parallel()

		work, upstream := stageClonePair(t)
		wanted := commitAndPush(t, upstream, "upstream.txt")

		if err := cli.New(&bytes.Buffer{}, &bytes.Buffer{}).
			UpdateFastForward(t.Context(), work); err != nil {
			t.Fatalf("UpdateFastForward() error = %v", err)
		}

		if head := gitOut(t, work, "rev-parse", "HEAD"); head != wanted {
			t.Errorf("HEAD = %s, want the upstream commit %s", head, wanted)
		}
	})

	t.Run("a checkout with local commits is left alone", func(t *testing.T) {
		t.Parallel()

		work, _ := stageClonePair(t)
		wanted := commitLocal(t, work, "local.txt")

		if err := cli.New(&bytes.Buffer{}, &bytes.Buffer{}).
			UpdateFastForward(t.Context(), work); err != nil {
			t.Fatalf("UpdateFastForward() error = %v", err)
		}

		if head := gitOut(t, work, "rev-parse", "HEAD"); head != wanted {
			t.Errorf("HEAD = %s, want the local commit %s", head, wanted)
		}
	})

	t.Run("a diverged checkout is left alone", func(t *testing.T) {
		t.Parallel()

		work, upstream := stageClonePair(t)
		wanted := commitLocal(t, work, "local.txt")
		commitAndPush(t, upstream, "upstream.txt")

		if err := cli.New(&bytes.Buffer{}, &bytes.Buffer{}).
			UpdateFastForward(t.Context(), work); err != nil {
			t.Fatalf("UpdateFastForward() error = %v", err)
		}

		if head := gitOut(t, work, "rev-parse", "HEAD"); head != wanted {
			t.Errorf("HEAD = %s, want the local commit %s", head, wanted)
		}
	})
}

// TestUpdateCheckExitsBehind pins the exit code a scheduled check elsewhere
// keys on. Nothing on the test host runs the temporary checkout's commit, so
// the two revisions always differ.
func TestUpdateCheckExitsBehind(t *testing.T) {
	t.Parallel()
	requireGit(t)

	work, _ := stageClonePair(t)
	commitLocal(t, work, "local.txt")

	var stdout, stderr bytes.Buffer

	code := cli.New(&stdout, &stderr).Run(t.Context(),
		[]string{updateCmd, flagCheckArg, flagSourceArg, work, flagDirArg, t.TempDir()})
	if code != cli.ExitBehind {
		t.Fatalf("exit = %d, want %d\nstderr: %s", code, cli.ExitBehind, stderr.String())
	}

	for _, want := range []string{"running:", "wanted:"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout is missing %q:\n%s", want, stdout.String())
		}
	}
}

// TestUpdateRunsSecretsPullBeforeRestart covers FR11. The service reads both
// environment files once, at startup, so a pull after the restart would leave
// the new process on the previous values.
func TestUpdateRunsSecretsPullBeforeRestart(t *testing.T) {
	t.Parallel()
	requireGit(t)

	work, _ := stageClonePair(t)

	var order []string

	steps := cli.UpdateSteps{
		Build: func(context.Context, string) error {
			order = append(order, "build")

			return nil
		},
		Pull: func(context.Context, string, string) (bool, error) {
			order = append(order, "pull")

			return true, nil
		},
		Restart: func(_ context.Context, _, secretsMoved bool, _ string) error {
			order = append(order, "restart")

			if !secretsMoved {
				t.Error("the restart did not see the pull's answer")
			}

			return nil
		},
		Link: func(string) error {
			order = append(order, "link")

			return nil
		},
	}

	if err := cli.New(&bytes.Buffer{}, &bytes.Buffer{}).
		RunUpdateSteps(t.Context(), work, t.TempDir(), steps); err != nil {
		t.Fatalf("RunUpdateSteps() error = %v", err)
	}

	want := []string{"build", "pull", "restart", "link"}
	if !slices.Equal(order, want) {
		t.Fatalf("step order = %v, want %v", order, want)
	}
}

func TestUpdateLink(t *testing.T) {
	t.Parallel()

	binary := filepath.Join(t.TempDir(), updateLinkName)
	writeFile(t, binary, "#!/bin/sh\n")

	t.Run("creates the link and the directory holding it", func(t *testing.T) {
		t.Parallel()

		linkDir := filepath.Join(t.TempDir(), ".local", "bin")

		if err := cli.UpdateLink(binary, linkDir); err != nil {
			t.Fatalf("UpdateLink() error = %v", err)
		}

		assertLinkTarget(t, filepath.Join(linkDir, updateLinkName), binary)
	})

	t.Run("replaces a symlink pointing somewhere else", func(t *testing.T) {
		t.Parallel()

		linkDir := t.TempDir()
		link := filepath.Join(linkDir, updateLinkName)

		if err := os.Symlink(filepath.Join(linkDir, "stale"), link); err != nil {
			t.Fatalf("stage the stale link: %v", err)
		}

		if err := cli.UpdateLink(binary, linkDir); err != nil {
			t.Fatalf("UpdateLink() error = %v", err)
		}

		assertLinkTarget(t, link, binary)
	})

	t.Run("refuses to replace a regular file", func(t *testing.T) {
		t.Parallel()

		linkDir := t.TempDir()
		link := filepath.Join(linkDir, updateLinkName)

		writeFile(t, link, "someone else's install\n")

		if err := cli.UpdateLink(binary, linkDir); err == nil {
			t.Fatal("UpdateLink() replaced a regular file")
		}

		content, err := os.ReadFile(link) // #nosec G304 -- a path this test built
		if err != nil {
			t.Fatalf("read back the file: %v", err)
		}

		if string(content) != "someone else's install\n" {
			t.Errorf("the file was rewritten: %q", content)
		}
	})
}

// requireGit skips a case that needs the real binary, since the fast-forward
// decision is only worth testing against git's own answers.
func requireGit(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
}

// stageCheckout builds the layout source discovery looks for: a directory
// carrying go.mod and .git with the binary under bin/.
func stageCheckout(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "go.mod"), "module example.invalid/staged\n")
	writeFile(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(dir, "bin", updateLinkName), "#!/bin/sh\n")

	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve %s: %v", dir, err)
	}

	return resolved
}

// stageClonePair builds a bare origin with one commit and two clones of it, so
// a case can move one side and leave the other where it was.
func stageClonePair(t *testing.T) (string, string) {
	t.Helper()

	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")

	gitOut(t, root, "init", "--bare", "--initial-branch=main", origin)

	seed := filepath.Join(root, "seed")

	gitOut(t, root, "clone", "--quiet", origin, seed)

	// go.mod is in the seed commit because source discovery refuses a directory
	// without it, so every clone here is a checkout the update would accept.
	commitAndPush(t, seed, "go.mod")

	work := filepath.Join(root, "work")
	upstream := filepath.Join(root, "upstream")

	gitOut(t, root, "clone", "--quiet", origin, work)
	gitOut(t, root, "clone", "--quiet", origin, upstream)

	return work, upstream
}

// commitLocal adds one commit to a clone without pushing it and returns its
// hash.
func commitLocal(t *testing.T, dir, name string) string {
	t.Helper()

	writeFile(t, filepath.Join(dir, name), name+"\n")
	gitOut(t, dir, "add", name)
	gitOut(t, dir, "commit", "--quiet", "-m", "add "+name)

	return gitOut(t, dir, "rev-parse", "HEAD")
}

// commitAndPush adds one commit and publishes it, so the other clone falls
// behind by exactly that commit.
func commitAndPush(t *testing.T, dir, name string) string {
	t.Helper()

	head := commitLocal(t, dir, name)

	gitOut(t, dir, "push", "--quiet", "origin", "HEAD:refs/heads/main")

	return head
}

// gitOut runs one git command with an identity and no user configuration, so
// the suite behaves the same on a machine with opinionated git defaults.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()

	command := exec.CommandContext(t.Context(), "git", args...) // #nosec G204 -- fixed argv from this test
	command.Dir = dir

	command.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_AUTHOR_NAME=cairnos test",
		"GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=cairnos test",
		"GIT_COMMITTER_EMAIL=test@example.invalid",
	)

	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}

	return strings.TrimSpace(string(output))
}

// writeFile writes one staged file, creating the directories above it.
func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// assertLinkTarget checks that link is a symlink aimed at the binary.
func assertLinkTarget(t *testing.T, link, binary string) {
	t.Helper()

	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("read %s: %v", link, err)
	}

	if target != binary {
		t.Errorf("target = %s, want %s", target, binary)
	}
}
