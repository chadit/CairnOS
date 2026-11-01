package cli_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/environ"
)

const (
	updateCmd      = "update"
	flagCheckArg   = "--check"
	flagSourceArg  = "--source"
	updateLinkName = "cairnos"
	goosWindows    = "windows"
)

// errPullBroken stands in for any failure the secrets pull can end in: a vault
// that will not unlock, a bw that cannot run, a file the pull cannot write.
var errPullBroken = errors.New("secrets pull broke")

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

		// The sentinel is the assertion: a bare non-nil would also pass on a
		// stat failure, which is a different answer for the operator.
		if _, err := cli.UpdateSourceDir(bare, executable); !errors.Is(err, cli.ErrNoSourceCheckout) {
			t.Fatalf("UpdateSourceDir() error = %v, want ErrNoSourceCheckout", err)
		}
	})
}

// syncCheckoutCase is one shape a host's clone can be found in before the
// checkout step runs.
type syncCheckoutCase struct {
	// stage puts the clone in the case's shape and returns what the reset
	// warning must name, which can depend on the commits it makes.
	stage func(t *testing.T, work string) []string

	name string

	// unlogged is what the warning must not name, because the reset kept it.
	unlogged []string

	wantReset bool
}

// TestUpdateSyncCheckout covers the checkout step on each shape a host's clone
// can be found in. A clean clone behind its upstream is fast-forwarded with no
// warning, while anything else is reset onto the upstream branch with a warning
// naming what went. Every case ends on main at the upstream commit with a clean
// tree, and the ignored bin/ survives.
func TestUpdateSyncCheckout(t *testing.T) {
	t.Parallel()
	requireGit(t)

	cases := []syncCheckoutCase{
		{
			name:  "a clean checkout behind its upstream is fast-forwarded",
			stage: func(*testing.T, string) []string { return nil },
		},
		{
			name:      "uncommitted changes are discarded and named",
			wantReset: true,
			stage: func(t *testing.T, work string) []string {
				t.Helper()

				writeFile(t, filepath.Join(work, "main.go"), stagedModuleMain+"// wip\n")
				writeFile(t, filepath.Join(work, "scratch.txt"), "wip\n")
				// The upstream commit adds this path, so it stands in the reset's way.
				writeFile(t, filepath.Join(work, "upstream.txt"), "a local copy\n")

				return []string{" M main.go", "?? scratch.txt", "?? upstream.txt"}
			},
		},
		{
			name:      "a local commit is dropped and named",
			wantReset: true,
			stage: func(t *testing.T, work string) []string {
				t.Helper()

				commit := commitLocal(t, work, "local.txt")

				return []string{gitOut(t, work, "rev-parse", "--short", commit) + " add local.txt"}
			},
		},
		{
			name:      "a detached HEAD is put back on the upstream branch",
			wantReset: true,
			stage: func(t *testing.T, work string) []string {
				t.Helper()

				gitOut(t, work, "checkout", "--quiet", "--detach")
				commit := commitLocal(t, work, "detached.txt")

				return []string{"HEAD is detached", gitOut(t, work, "rev-parse", "--short", commit) + " add detached.txt"}
			},
		},
		{
			name:      "another branch is left for the upstream branch",
			wantReset: true,
			unlogged:  []string{"add feature.txt"},
			stage: func(t *testing.T, work string) []string {
				t.Helper()

				gitOut(t, work, "checkout", "--quiet", "-b", "feature")
				commitLocal(t, work, "feature.txt")

				return []string{"HEAD is on branch feature, not main"}
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			checkSyncCheckout(t, &testCase)
		})
	}
}

// checkSyncCheckout runs one case: the upstream moves one commit ahead, the
// case stages the clone, and a source update has to land it on that commit
// before the substituted build runs.
func checkSyncCheckout(t *testing.T, testCase *syncCheckoutCase) {
	t.Helper()

	work, upstream := stageClonePair(t)
	wanted := commitAndPush(t, upstream, "upstream.txt")
	binary := filepath.Join(work, "bin", updateLinkName)

	writeFile(t, binary, "#!/bin/sh\n")

	logged := testCase.stage(t, work)

	var (
		stderr bytes.Buffer
		order  []string
	)

	app := cli.New(&bytes.Buffer{}, &stderr)

	if _, err := app.DriveUpdate(t.Context(), sourceRun(t, work, ""), recordingSteps(t, &order, "")); err != nil {
		t.Fatalf("DriveUpdate() error = %v\nstderr: %s", err, stderr.String())
	}

	assertOnUpstream(t, work, wanted)

	if _, err := os.Stat(binary); err != nil {
		t.Errorf("the ignored binary did not survive: %v", err)
	}

	log := stderr.String()

	if reset := strings.Contains(log, "level=WARN"); reset != testCase.wantReset {
		t.Errorf("reset warning = %t, want %t:\n%s", reset, testCase.wantReset, log)
	}

	for _, want := range logged {
		if !strings.Contains(log, want) {
			t.Errorf("the reset warning does not name %q:\n%s", want, log)
		}
	}

	for _, kept := range testCase.unlogged {
		if strings.Contains(log, kept) {
			t.Errorf("the reset warning names %q, which the reset kept:\n%s", kept, log)
		}
	}
}

// assertOnUpstream checks that work sits on main at wanted, tracking
// origin/main, with a clean tree: where every update leaves a host's clone, and
// the shape the next update fast-forwards from.
func assertOnUpstream(t *testing.T, work, wanted string) {
	t.Helper()

	if head := gitOut(t, work, "rev-parse", "HEAD"); head != wanted {
		t.Errorf("HEAD = %s, want the upstream commit %s", head, wanted)
	}

	if branch := gitOut(t, work, "symbolic-ref", "--short", "HEAD"); branch != "main" {
		t.Errorf("HEAD is on %s, want main", branch)
	}

	if tracked := gitOut(t, work, "rev-parse", "--abbrev-ref", "main@{upstream}"); tracked != "origin/main" {
		t.Errorf("main tracks %q, want origin/main", tracked)
	}

	if status := gitOut(t, work, "status", "--porcelain"); status != "" {
		t.Errorf("the tree still carries changes:\n%s", status)
	}
}

// TestUpdateCheckReportsACheckoutAnUpdateWouldReset covers --check on a clone
// the update would reset. The service already runs the upstream commit, so only
// the reset can make the check report behind, and the reset line has to say
// why. The check itself leaves HEAD and the tree as they were.
func TestUpdateCheckReportsACheckoutAnUpdateWouldReset(t *testing.T) {
	t.Parallel()
	requireGit(t)

	cases := []struct {
		stage  func(t *testing.T, work string)
		name   string
		reason string
	}{
		{
			name:   "uncommitted changes",
			reason: "1 changed or untracked path(s)",
			stage: func(t *testing.T, work string) {
				t.Helper()
				writeFile(t, filepath.Join(work, "main.go"), stagedModuleMain+"// wip\n")
			},
		},
		{
			name:   "a local commit the upstream lacks",
			reason: "1 commit(s) not on origin/main",
			stage:  func(t *testing.T, work string) { t.Helper(); commitLocal(t, work, "local.txt") },
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			work, _ := stageClonePair(t)
			running := gitOut(t, work, "rev-parse", "origin/main")

			testCase.stage(t, work)

			head := gitOut(t, work, "rev-parse", "HEAD")
			status := gitOut(t, work, "status", "--porcelain")

			var stdout, stderr bytes.Buffer

			code, err := cli.New(&stdout, &stderr).DriveUpdate(t.Context(), &cli.UpdateRun{
				Executable: filepath.Join(work, "bin", updateLinkName),
				Home:       t.TempDir(),
				Running:    running,
				Check:      true,
				NoRelease:  true,
			}, failingSteps(t))
			if err != nil {
				t.Fatalf("DriveUpdate() error = %v\nstderr: %s", err, stderr.String())
			}

			if code != cli.ExitBehind {
				t.Fatalf("exit = %d, want %d\nstdout: %s", code, cli.ExitBehind, stdout.String())
			}

			for _, want := range []string{"wanted:  " + running, "reset:   " + testCase.reason} {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout is missing %q:\n%s", want, stdout.String())
				}
			}

			if after := gitOut(t, work, "rev-parse", "HEAD"); after != head {
				t.Errorf("the check moved HEAD to %s, want it left at %s", after, head)
			}

			if after := gitOut(t, work, "status", "--porcelain"); after != status {
				t.Errorf("the check changed the tree:\nbefore:\n%s\nafter:\n%s", status, after)
			}
		})
	}
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

// TestUpdateRefusesWhileAnotherUpdateHoldsTheLock covers `cairnos update` run
// while serve's loop is mid-install. It stops at once, says why, and leaves the
// checkout and every step alone.
func TestUpdateRefusesWhileAnotherUpdateHoldsTheLock(t *testing.T) {
	t.Parallel()
	requireGit(t)

	work, upstream := stageClonePair(t)
	commitAndPush(t, upstream, "upstream.txt")
	before := gitOut(t, work, "rev-parse", "HEAD")
	home := t.TempDir()

	release, err := cli.HoldUpdateLock(home)
	if err != nil {
		t.Fatalf("HoldUpdateLock() error = %v", err)
	}

	t.Cleanup(func() {
		if releaseErr := release(); releaseErr != nil {
			t.Errorf("release the update lock: %v", releaseErr)
		}
	})

	var order []string

	_, err = cli.New(&bytes.Buffer{}, &bytes.Buffer{}).DriveUpdate(t.Context(), &cli.UpdateRun{
		Executable: filepath.Join(work, "bin", updateLinkName),
		Home:       home,
		NoRelease:  true,
	}, recordingSteps(t, &order, ""))
	// The sentinel's own text is the message the operator reads.
	if !errors.Is(err, cli.ErrUpdateBusy) {
		t.Fatalf("DriveUpdate() error = %v, want ErrUpdateBusy", err)
	}

	if len(order) != 0 {
		t.Errorf("steps ran while another update held the lock: %v", order)
	}

	if head := gitOut(t, work, "rev-parse", "HEAD"); head != before {
		t.Errorf("HEAD moved to %s while another update held the lock", head)
	}
}

// TestUpdateCheckFetchesTheTrackedRemote covers a checkout whose branch tracks
// a remote not called origin. The fetch follows the tracking config, so the
// commit waiting there is still what the check reports.
func TestUpdateCheckFetchesTheTrackedRemote(t *testing.T) {
	t.Parallel()
	requireGit(t)

	work, upstream := stageClonePair(t)
	gitOut(t, work, "remote", "rename", "origin", "mirror")
	remote := commitAndPush(t, upstream, "upstream.txt")

	var stdout, stderr bytes.Buffer

	code, err := cli.New(&stdout, &stderr).DriveUpdate(t.Context(), &cli.UpdateRun{
		Executable: filepath.Join(work, "bin", updateLinkName),
		Home:       t.TempDir(),
		Check:      true,
		NoRelease:  true,
	}, failingSteps(t))
	if err != nil {
		t.Fatalf("DriveUpdate() error = %v\nstderr: %s", err, stderr.String())
	}

	if code != cli.ExitBehind {
		t.Fatalf("exit = %d, want %d", code, cli.ExitBehind)
	}

	if !strings.Contains(stdout.String(), "wanted:  "+remote) {
		t.Errorf("stdout does not report the commit on the tracked remote %s:\n%s", remote, stdout.String())
	}
}

// TestUpdateCheckSeesTheRemote covers a host whose checkout nobody fetches.
// The check fetches the remote-tracking refs and so reports the commit waiting
// upstream; --no-fetch compares against the local HEAD alone. Neither moves the
// checkout.
func TestUpdateCheckSeesTheRemote(t *testing.T) {
	t.Parallel()
	requireGit(t)

	cases := []struct {
		name         string
		noFetch      bool
		wantUpstream bool
	}{
		{name: "the check fetches and sees the upstream commit", wantUpstream: true},
		{name: "no-fetch sees only the local HEAD", noFetch: true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			work, upstream := stageClonePair(t)
			local := gitOut(t, work, "rev-parse", "HEAD")
			remote := commitAndPush(t, upstream, "upstream.txt")

			var stdout, stderr bytes.Buffer

			code, err := cli.New(&stdout, &stderr).DriveUpdate(t.Context(), &cli.UpdateRun{
				Executable: filepath.Join(work, "bin", updateLinkName),
				Home:       t.TempDir(),
				Check:      true,
				NoFetch:    testCase.noFetch,
				NoRelease:  true,
			}, failingSteps(t))
			if err != nil {
				t.Fatalf("DriveUpdate() error = %v\nstderr: %s", err, stderr.String())
			}

			if code != cli.ExitBehind {
				t.Fatalf("exit = %d, want %d", code, cli.ExitBehind)
			}

			wanted, unseen := local, remote
			if testCase.wantUpstream {
				wanted, unseen = remote, local
			}

			if !strings.Contains(stdout.String(), "wanted:  "+wanted) {
				t.Errorf("stdout does not report %s as wanted:\n%s", wanted, stdout.String())
			}

			if strings.Contains(stdout.String(), unseen) {
				t.Errorf("stdout names %s, which this check should not report:\n%s", unseen, stdout.String())
			}

			if head := gitOut(t, work, "rev-parse", "HEAD"); head != local {
				t.Errorf("the check moved HEAD to %s, want it left at %s", head, local)
			}
		})
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
			order = append(order, stepBuild)

			return nil
		},
		Pull: func(context.Context, string, string) (bool, error) {
			order = append(order, stepPull)

			return true, nil
		},
		Restart: func(_ context.Context, _, secretsMoved bool, _ string) error {
			order = append(order, stepRestart)

			if !secretsMoved {
				t.Error("the restart did not see the pull's answer")
			}

			return nil
		},
		Link: func(string) error {
			order = append(order, stepLink)

			return nil
		},
	}

	if err := cli.New(&bytes.Buffer{}, &bytes.Buffer{}).
		RunUpdateSteps(t.Context(), work, t.TempDir(), steps); err != nil {
		t.Fatalf("RunUpdateSteps() error = %v", err)
	}

	want := []string{stepBuild, stepPull, stepRestart, stepLink}
	if !slices.Equal(order, want) {
		t.Fatalf("step order = %v, want %v", order, want)
	}
}

// TestUpdateStopsWhenTheSecretsPullFails is the other half of FR11. A pull that
// failed left the generated file at whatever it was, so restarting would put
// the service on values nobody checked and the link would advertise the build
// that runs them. The failure has to reach the operator instead.
func TestUpdateStopsWhenTheSecretsPullFails(t *testing.T) {
	t.Parallel()
	requireGit(t)

	work, _ := stageClonePair(t)

	var order []string

	steps := cli.UpdateSteps{
		Build: func(context.Context, string) error {
			order = append(order, stepBuild)

			return nil
		},
		Pull: func(context.Context, string, string) (bool, error) {
			order = append(order, stepPull)

			return false, errPullBroken
		},
		Restart: func(context.Context, bool, bool, string) error {
			order = append(order, stepRestart)

			return nil
		},
		Link: func(string) error {
			order = append(order, stepLink)

			return nil
		},
	}

	err := cli.New(&bytes.Buffer{}, &bytes.Buffer{}).
		RunUpdateSteps(t.Context(), work, t.TempDir(), steps)
	if !errors.Is(err, errPullBroken) {
		t.Fatalf("RunUpdateSteps() error = %v, want the pull's failure", err)
	}

	want := []string{stepBuild, stepPull}
	if !slices.Equal(order, want) {
		t.Fatalf("step order = %v, want %v with nothing after the failed pull", order, want)
	}
}

// TestUpdateBuildPutsGoOnMakesPath pins the build a systemd session or a bare
// ssh command runs: the Makefile calls go by name, so make has to start with
// the toolchain the update found on its PATH, not whatever PATH this process
// inherited. The make stub fails unless go resolves to the staged toolchain.
func TestUpdateBuildPutsGoOnMakesPath(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == goosWindows {
		t.Skip("the make stub is a shell script")
	}

	goDir := t.TempDir()
	goBin := filepath.Join(goDir, "go")
	writeExecutable(t, goBin, "#!/bin/sh\nexit 0\n")

	makeDir := t.TempDir()
	writeExecutable(t, filepath.Join(makeDir, "make"),
		"#!/bin/sh\nfound=$(command -v go)\n[ \"$found\" = \""+goBin+"\" ] || { echo \"go resolved to '$found'\" >&2; exit 1; }\n")

	app := cli.New(&bytes.Buffer{}, &bytes.Buffer{}).
		WithEnv(environ.Map(map[string]string{pathEnvKey: makeDir + string(os.PathListSeparator) + goDir}))

	if err := app.UpdateBuild(t.Context(), t.TempDir()); err != nil {
		t.Fatalf("UpdateBuild() error = %v, want make to find the staged go", err)
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

	t.Run("replaces a link whose target is gone", func(t *testing.T) {
		t.Parallel()

		linkDir := t.TempDir()
		link := filepath.Join(linkDir, updateLinkName)

		// A dangling link is what a previous install leaves behind when its
		// checkout is deleted. Lstat sees it, os.Stat would not.
		if err := os.Symlink(filepath.Join(linkDir, "deleted-checkout", "bin", updateLinkName), link); err != nil {
			t.Fatalf("stage the dangling link: %v", err)
		}

		if err := cli.UpdateLink(binary, linkDir); err != nil {
			t.Fatalf("UpdateLink() error = %v", err)
		}

		assertLinkTarget(t, link, binary)
	})

	t.Run("a link directory that cannot be created is reported", func(t *testing.T) {
		t.Parallel()
		requireNotRoot(t)

		parent := t.TempDir()
		if err := os.Chmod(parent, 0o500); err != nil {
			t.Fatalf("seal the parent directory: %v", err)
		}

		linkDir := filepath.Join(parent, ".local", "bin")

		if err := cli.UpdateLink(binary, linkDir); err == nil {
			t.Fatal("UpdateLink() reported success with nowhere to put the link")
		}

		if _, err := os.Lstat(filepath.Join(linkDir, updateLinkName)); err == nil {
			t.Fatal("a link exists under a directory that could not be created")
		}
	})
}

// requireNotRoot skips a case whose whole subject is a permission refusal, since
// root is not refused and the case would assert nothing.
func requireNotRoot(t *testing.T) {
	t.Helper()

	if os.Geteuid() == 0 {
		t.Skip("running as root, so an unwritable directory is still writable")
	}
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
// a case can move one side and leave the other where it was. The seed ignores
// bin/ the way the real repository does, so a build or the running binary there
// never reads as a change and a reset leaves it in place.
func stageClonePair(t *testing.T) (string, string) {
	t.Helper()

	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")

	gitOut(t, root, "init", "--bare", "--initial-branch=main", origin)

	seed := filepath.Join(root, "seed")

	gitOut(t, root, "clone", "--quiet", origin, seed)

	// The seed commit is a module that builds: source discovery refuses a
	// directory without go.mod, and a case that runs serve's loop through to
	// the drain builds the clone for real.
	writeFile(t, filepath.Join(seed, ".gitignore"), "bin/\n")
	writeFile(t, filepath.Join(seed, "go.mod"), stagedModuleGoMod)
	writeFile(t, filepath.Join(seed, "main.go"), stagedModuleMain)
	gitOut(t, seed, "add", ".")
	gitOut(t, seed, "commit", "--quiet", "-m", "seed a module that builds")
	gitOut(t, seed, "push", "--quiet", "origin", "HEAD:refs/heads/main")

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

// writeExecutable writes one staged stub the test runs as a command.
func writeExecutable(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o700); err != nil { // #nosec G306 -- an executable fixture
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
