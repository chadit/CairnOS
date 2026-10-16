package source_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/chadit/CairnOS/internal/source"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Fixture constants: the mode for files and directories the fixtures create,
// and the source name they all carry.
const (
	fixtureFilePerm = 0o600
	fixtureDirPerm  = 0o750
	specName        = "automation"
	trackedFile     = "README.md"
)

// The tracked file contents the fixtures commit, plus the two strings a blocked
// tree carries. Named so a case reads as the first commit or the second rather
// than as two similar literals.
const (
	firstContent  = "one\n"
	secondContent = "two\n"
	handEdit      = "edited by hand\n"
	untrackedBody = "not tracked\n"
)

// TestMain points git at an empty global and system config for the whole
// package, so a developer's own gpg signing, commit template, or default branch
// cannot change what these fixtures build. The null device reads as an empty
// file, so no temp config is left behind when os.Exit skips deferred calls.
func TestMain(m *testing.M) {
	for _, key := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM"} {
		if err := os.Setenv(key, os.DevNull); err != nil {
			panic("set " + key + ": " + err.Error())
		}
	}

	os.Exit(m.Run())
}

// origin is a bare repository plus the work tree the fixtures commit through.
type origin struct {
	bare string
	work string
}

// newOrigin builds a bare repository with one commit on main, reachable over a
// local path so no test touches the network.
func newOrigin(t *testing.T) origin {
	t.Helper()

	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	work := filepath.Join(root, "work")

	git(t, root, "init", "--bare", "--initial-branch=main", bare)
	git(t, root, "init", "--initial-branch=main", work)
	setIdentity(t, work)
	git(t, work, "remote", "add", "origin", bare)

	repo := origin{bare: bare, work: work}
	repo.commit(t, firstContent, "first")
	repo.push(t)

	return repo
}

// commit rewrites the tracked README in the work tree and commits it. One
// tracked file is enough: these fixtures care about where HEAD lands, not about
// the shape of the tree.
func (o origin) commit(t *testing.T, content, message string) {
	t.Helper()

	writeFixtureFile(t, filepath.Join(o.work, trackedFile), content)
	git(t, o.work, "add", trackedFile)
	git(t, o.work, "commit", "-m", message)
}

// push sends main and every tag to the bare repository. The branch goes by its
// full ref because one case deliberately creates a tag named main, and a short
// name would then match two refs and fail the push.
func (o origin) push(t *testing.T) {
	t.Helper()

	git(t, o.work, "push", "--tags", "origin", "refs/heads/main:refs/heads/main")
}

// tag names the work tree's current commit and pushes the tag.
func (o origin) tag(t *testing.T, name string) {
	t.Helper()

	git(t, o.work, "tag", name)
	o.push(t)
}

// mustEnsure runs Ensure for spec and stops on failure, so a Sync case says
// nothing about the clone that set it up.
func mustEnsure(t *testing.T, spec source.Spec) {
	t.Helper()

	if err := source.Ensure(t.Context(), source.GitCommand, spec, nil); err != nil {
		t.Fatalf("ensure %s: %v", spec.Name, err)
	}
}

// specFor builds a spec pointing at a fresh destination under a temp directory.
// DirtyReset matches what config seeds for a block that never names the key, so
// a fixture and a real host agree on what a blocked tree gets.
func specFor(t *testing.T, repo origin, ref string) source.Spec {
	t.Helper()

	return source.Spec{
		Name:       specName,
		Repo:       repo.bare,
		Dest:       filepath.Join(t.TempDir(), "checkout"),
		Ref:        ref,
		DirtyReset: true,
	}
}

// brokenSpec names a repository path that was never created, which is the
// cheapest offline stand-in for a remote a host cannot reach.
func brokenSpec(t *testing.T) source.Spec {
	t.Helper()

	return source.Spec{
		Name: specName,
		Repo: filepath.Join(t.TempDir(), "no-such-repo.git"),
		Dest: filepath.Join(t.TempDir(), "checkout"),
		Ref:  "main",
	}
}

// detach parks a work tree's HEAD on one commit-ish without a branch, the state
// a pinned tag leaves behind.
func detach(t *testing.T, repo, commitish string) {
	t.Helper()

	git(t, repo, "checkout", "--detach", commitish)
}

// failingGit wraps the production git so one verb fails with a chosen exit
// status while every other command still does real work against the fixture. It
// is how a case reaches a git failure a real fixture cannot be talked into.
type failingGit struct {
	verb string
	code int
}

// git fails the chosen verb with the canned status and passes everything else
// straight through, so the production error reaches the caller exactly as it
// would without the wrapper.
func (f failingGit) git(ctx context.Context, dir string, argv ...string) (string, error) {
	if len(argv) > 0 && argv[0] == f.verb {
		return "", &source.ExitError{
			Code:  f.code,
			Cause: errors.New("fatal: the fixture forced " + f.verb + " to fail"),
		}
	}

	return source.GitCommand(ctx, dir, argv...)
}

// setInsteadOf records a url rewrite in one checkout's own config. git merges
// every config level before get-url applies a rewrite, so this reaches the path
// a host gitconfig would without touching the process environment, which every
// other test in this package shares.
func setInsteadOf(t *testing.T, repo, from, to string) {
	t.Helper()

	git(t, repo, "config", "url."+to+".insteadOf", from)
}

// setIdentity gives one repository a committer, since the package runs with an
// empty global config and git refuses to commit without one.
func setIdentity(t *testing.T, repo string) {
	t.Helper()

	git(t, repo, "config", "user.name", "CairnOS Fixture")
	git(t, repo, "config", "user.email", "fixture@example.invalid")
	git(t, repo, "config", "commit.gpgsign", "false")
}

// git runs one fixture command through the production entry point, stopping the
// case on error.
func git(t *testing.T, dir string, argv ...string) string {
	t.Helper()

	out, err := source.GitCommand(t.Context(), dir, argv...)
	if err != nil {
		t.Fatalf("git %v in %s: %v", argv, dir, err)
	}

	return out
}

// writeFixtureFile writes one file, creating its directory when needed.
func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), fixtureDirPerm); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, []byte(content), fixtureFilePerm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// readFixtureFile reads one file and fails the test when it is gone.
func readFixtureFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) // #nosec G304 -- the path is built by the test itself
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(data)
}

// wantCode asserts the failure is classified and carries one code.
func wantCode(t *testing.T, err error, code string) {
	t.Helper()

	classified, ok := errors.AsType[*runner.Error](err)
	if !ok {
		t.Fatalf("error %v is not a classified runner error", err)
	}

	if classified.Code != code {
		t.Fatalf("error code = %q, want %q (%v)", classified.Code, code, err)
	}
}

// wantClass asserts the retry class the engine will act on.
func wantClass(t *testing.T, err error, class runner.ErrorClass) {
	t.Helper()

	classified, ok := errors.AsType[*runner.Error](err)
	if !ok {
		t.Fatalf("error %v is not a classified runner error", err)
	}

	if classified.Class != class {
		t.Fatalf("error class = %v, want %v (%v)", classified.Class, class, err)
	}
}
