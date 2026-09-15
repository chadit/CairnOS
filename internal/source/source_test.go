package source_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/source"
	"github.com/chadit/CairnOS/pkg/runner"
)

// branchMain is the branch every fixture repository is built on.
const branchMain = "main"

func TestEnsureClonesAndChecksOutBranch(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	spec := specFor(t, repo, branchMain)

	mustEnsure(t, spec)

	if got := readFixtureFile(t, filepath.Join(spec.Dest, trackedFile)); got != "one\n" {
		t.Fatalf("cloned README = %q, want %q", got, "one\n")
	}

	if branch := git(t, spec.Dest, "symbolic-ref", "--short", "HEAD"); branch != branchMain {
		t.Fatalf("branch after ensure = %q, want %s", branch, branchMain)
	}
}

func TestEnsureChecksOutExactTag(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	repo.tag(t, "v1.0.0")

	spec := specFor(t, repo, "v1.0.0")

	mustEnsure(t, spec)

	want := git(t, spec.Dest, "rev-parse", "refs/tags/v1.0.0^{commit}")
	if head := git(t, spec.Dest, "rev-parse", "HEAD"); head != want {
		t.Fatalf("HEAD = %s, want the v1.0.0 commit %s", head, want)
	}
}

// TestEnsureChecksOutNewestGlobMatch pins the version-sort contract: under
// -v:refname v1.10.0 is newer than v1.2.0, which plain lexical order gets
// backwards. The pattern is the pre-release-skipping form the README shows.
func TestEnsureChecksOutNewestGlobMatch(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	repo.tag(t, "v1.2.0")
	repo.commit(t, "two\n", "second")
	repo.tag(t, "v1.10.0")

	spec := specFor(t, repo, "v[0-9]*.[0-9]*.[0-9]*")

	mustEnsure(t, spec)

	want := git(t, spec.Dest, "rev-parse", "refs/tags/v1.10.0^{commit}")
	if head := git(t, spec.Dest, "rev-parse", "HEAD"); head != want {
		t.Fatalf("HEAD = %s, want the v1.10.0 commit %s", head, want)
	}
}

func TestEnsureRejectsForeignOrigin(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	spec := specFor(t, repo, branchMain)

	mustEnsure(t, spec)

	spec.Repo = filepath.Join(t.TempDir(), "somewhere-else.git")

	err := source.Ensure(t.Context(), source.GitCommand, spec, nil)
	wantCode(t, err, source.CodeMismatch)
	wantClass(t, err, runner.ClassPermanent)
}

func TestEnsureRejectsNonWorkTree(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	spec := specFor(t, repo, branchMain)

	if err := os.MkdirAll(spec.Dest, fixtureDirPerm); err != nil {
		t.Fatalf("create dest: %v", err)
	}

	err := source.Ensure(t.Context(), source.GitCommand, spec, nil)
	wantCode(t, err, source.CodeNotWorktree)
}

// TestEnsureReportsUnreachableRepoAsRetryable covers FR10's transient half: a
// clone that failed gets another attempt rather than a dead run.
func TestEnsureReportsUnreachableRepoAsRetryable(t *testing.T) {
	t.Parallel()

	spec := brokenSpec(t)

	err := source.Ensure(t.Context(), source.GitCommand, spec, nil)
	wantCode(t, err, source.CodeClone)
	wantClass(t, err, runner.ClassTransient)
}

func TestSyncFastForwardsBranch(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	spec := specFor(t, repo, branchMain)

	mustEnsure(t, spec)

	repo.commit(t, "two\n", "second")
	repo.push(t)

	result, err := source.Sync(t.Context(), source.GitCommand, spec)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	if result.Old == result.New {
		t.Fatalf("sync reported no movement (%s) after a pushed commit", result.Old)
	}

	if got := readFixtureFile(t, filepath.Join(spec.Dest, trackedFile)); got != "two\n" {
		t.Fatalf("README after sync = %q, want %q", got, "two\n")
	}
}

func TestSyncReportsNoMovementWhenCurrent(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	spec := specFor(t, repo, branchMain)

	mustEnsure(t, spec)

	result, err := source.Sync(t.Context(), source.GitCommand, spec)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	if result.Old != result.New {
		t.Fatalf("sync moved HEAD %s..%s with nothing pushed", result.Old, result.New)
	}
}

// TestSyncRefusesDirtyTree covers FR8 on both halves: the failure is classified
// dirty, and the operator's edit is still there afterwards.
func TestSyncRefusesDirtyTree(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	spec := specFor(t, repo, branchMain)

	mustEnsure(t, spec)

	local := filepath.Join(spec.Dest, trackedFile)
	writeFixtureFile(t, local, "edited by hand\n")

	repo.commit(t, "two\n", "second")
	repo.push(t)

	_, err := source.Sync(t.Context(), source.GitCommand, spec)
	wantCode(t, err, source.CodeDirty)

	if got := readFixtureFile(t, local); got != "edited by hand\n" {
		t.Fatalf("README after a refused sync = %q, want the local edit intact", got)
	}
}

func TestSyncRefusesDivergedBranch(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	spec := specFor(t, repo, branchMain)

	mustEnsure(t, spec)
	setIdentity(t, spec.Dest)

	writeFixtureFile(t, filepath.Join(spec.Dest, "local.txt"), "local only\n")
	git(t, spec.Dest, "add", "local.txt")
	git(t, spec.Dest, "commit", "-m", "local work")

	before := git(t, spec.Dest, "rev-parse", "HEAD")

	repo.commit(t, "two\n", "second")
	repo.push(t)

	_, err := source.Sync(t.Context(), source.GitCommand, spec)
	wantCode(t, err, source.CodeDiverged)

	if after := git(t, spec.Dest, "rev-parse", "HEAD"); after != before {
		t.Fatalf("HEAD moved from %s to %s on a refused sync", before, after)
	}
}

func TestSyncRefusesDetachedHeadOnBranchRef(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	spec := specFor(t, repo, branchMain)

	mustEnsure(t, spec)
	detach(t, spec.Dest, "HEAD")

	_, err := source.Sync(t.Context(), source.GitCommand, spec)
	wantCode(t, err, source.CodeDiverged)
}

func TestSyncMovesToExactTagWhenHeadDiffers(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	repo.tag(t, "v1.0.0")

	spec := specFor(t, repo, "v2.0.0")

	repo.commit(t, "two\n", "second")
	repo.tag(t, "v2.0.0")

	mustEnsure(t, spec)

	// Park HEAD on the older tag so the sync has somewhere to move from.
	detach(t, spec.Dest, "refs/tags/v1.0.0")

	result, err := source.Sync(t.Context(), source.GitCommand, spec)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	if result.Old == result.New {
		t.Fatalf("sync stayed at %s instead of moving to v2.0.0", result.Old)
	}

	want := git(t, spec.Dest, "rev-parse", "refs/tags/v2.0.0^{commit}")
	if head := git(t, spec.Dest, "rev-parse", "HEAD"); head != want {
		t.Fatalf("HEAD = %s, want the v2.0.0 commit %s", head, want)
	}
}

func TestSyncFollowsNewestGlobMatch(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	repo.tag(t, "v1.2.0")

	spec := specFor(t, repo, "v*")

	mustEnsure(t, spec)

	repo.commit(t, "two\n", "second")
	repo.tag(t, "v1.10.0")

	result, err := source.Sync(t.Context(), source.GitCommand, spec)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	if result.Old == result.New {
		t.Fatalf("sync stayed at %s instead of following the new tag", result.Old)
	}

	want := git(t, spec.Dest, "rev-parse", "refs/tags/v1.10.0^{commit}")
	if head := git(t, spec.Dest, "rev-parse", "HEAD"); head != want {
		t.Fatalf("HEAD = %s, want the v1.10.0 commit %s", head, want)
	}
}

func TestSyncRefusesAmbiguousRef(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	spec := specFor(t, repo, branchMain)

	mustEnsure(t, spec)

	repo.tag(t, branchMain)

	_, err := source.Sync(t.Context(), source.GitCommand, spec)
	wantCode(t, err, source.CodeAmbiguous)
}

func TestSyncRefusesUnknownRef(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	spec := specFor(t, repo, branchMain)

	mustEnsure(t, spec)

	spec.Ref = "release-that-was-never-cut"

	_, err := source.Sync(t.Context(), source.GitCommand, spec)
	wantCode(t, err, source.CodeNoRef)
}

// TestEnsureAllStopsAtTheFirstFailure proves serve learns which source broke:
// the error names it, and the loop stops there. The working spec sorts first, so
// it is already cloned when the broken one ends the pass.
func TestEnsureAllStopsAtTheFirstFailure(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	good := specFor(t, repo, branchMain)

	broken := brokenSpec(t)
	broken.Name = "zz_broken"

	err := source.EnsureAll(t.Context(), source.GitCommand, []source.Spec{broken, good}, nil)
	if err == nil {
		t.Fatal("EnsureAll returned nil for a source that cannot clone")
	}

	message := err.Error()
	if !strings.Contains(message, broken.Name) {
		t.Fatalf("EnsureAll error %q does not name the failing source", message)
	}

	wantCode(t, err, source.CodeClone)

	if _, statErr := os.Stat(filepath.Join(good.Dest, trackedFile)); statErr != nil {
		t.Fatalf("the source that sorts first was not cloned: %v", statErr)
	}
}

// TestEnsureRepositionsTreeOnAnotherBranch closes the gap a failed first checkout
// leaves. The tree is a valid clone of the right repository, so the origin check
// passes, and only the ref check notices it is on someone else's branch. Serve
// puts it back rather than refusing, since nothing on the boot path could clear
// a refusal.
func TestEnsureRepositionsTreeOnAnotherBranch(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	spec := specFor(t, repo, branchMain)

	mustEnsure(t, spec)
	git(t, spec.Dest, "switch", "-c", "side")

	mustEnsure(t, spec)

	if branch := git(t, spec.Dest, "symbolic-ref", "--short", "HEAD"); branch != branchMain {
		t.Fatalf("branch after ensure = %q, want %s", branch, branchMain)
	}

	if status := git(t, spec.Dest, "status", "--porcelain"); status != "" {
		t.Fatalf("ensure left the tree dirty:\n%s", status)
	}
}

// TestEnsureRepositionsTreeAtAnotherTag is the operator moving a pin: edit ref
// from one tag to the next and restart. The tag is already on disk from an
// earlier sync, so no fetch is needed to land on it.
func TestEnsureRepositionsTreeAtAnotherTag(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	repo.tag(t, "v1.0.0")
	repo.commit(t, "two\n", "second")
	repo.tag(t, "v2.0.0")

	spec := specFor(t, repo, "v2.0.0")

	mustEnsure(t, spec)
	detach(t, spec.Dest, "refs/tags/v1.0.0")

	mustEnsure(t, spec)

	want := git(t, spec.Dest, "rev-parse", "refs/tags/v2.0.0^{commit}")
	if head := git(t, spec.Dest, "rev-parse", "HEAD"); head != want {
		t.Fatalf("HEAD = %s, want the v2.0.0 commit %s", head, want)
	}

	if status := git(t, spec.Dest, "status", "--porcelain"); status != "" {
		t.Fatalf("ensure left the tree dirty:\n%s", status)
	}
}

// TestEnsureRefusesToRepositionDirtyTree proves the repair still honors FR8: an
// operator's uncommitted work outranks the pin, so serve stops and says why
// rather than checking out over it.
func TestEnsureRefusesToRepositionDirtyTree(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	spec := specFor(t, repo, branchMain)

	mustEnsure(t, spec)
	git(t, spec.Dest, "switch", "-c", "side")

	local := filepath.Join(spec.Dest, trackedFile)
	writeFixtureFile(t, local, "edited by hand\n")

	err := source.Ensure(t.Context(), source.GitCommand, spec, nil)
	wantCode(t, err, source.CodeDirty)
	wantClass(t, err, runner.ClassPermanent)

	if got := readFixtureFile(t, local); got != "edited by hand\n" {
		t.Fatalf("README after a refused ensure = %q, want the local edit intact", got)
	}

	if branch := git(t, spec.Dest, "symbolic-ref", "--short", "HEAD"); branch != "side" {
		t.Fatalf("branch after a refused ensure = %q, want side", branch)
	}
}

// TestEnsureAllowsRefNotYetOnDisk is the other half of the pin move: the new tag
// has not been fetched. Ensure never fetches, so refusing here would stop serve
// for a state only the first sync can resolve.
func TestEnsureAllowsRefNotYetOnDisk(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	spec := specFor(t, repo, branchMain)

	mustEnsure(t, spec)

	spec.Ref = "v9.9.9"

	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))

	if err := source.Ensure(t.Context(), source.GitCommand, spec, logger); err != nil {
		t.Fatalf("ensure with a ref not yet on disk = %v, want nil", err)
	}

	line := logs.String()
	if !strings.Contains(line, "v9.9.9") || !strings.Contains(line, specName) {
		t.Fatalf("log %q does not name the source and the ref the first sync has to fetch", line)
	}
}

// TestEnsureDoesNotFetchExistingTree covers FR5. An offline host with a tree on
// disk has to finish serve startup, so nothing on this path may reach the
// network.
func TestEnsureDoesNotFetchExistingTree(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	spec := specFor(t, repo, branchMain)

	mustEnsure(t, spec)

	rec := &recorder{}
	if err := source.Ensure(t.Context(), rec.git, spec, nil); err != nil {
		t.Fatalf("ensure over an existing tree: %v", err)
	}

	for _, verb := range []string{"fetch", "clone", "pull", "remote-update"} {
		if rec.ran(verb) {
			t.Fatalf("ensure over an existing tree ran %q; it must read only what is on disk", verb)
		}
	}
}

// TestEnsureRejectsSubdirectoryOfAnotherTree proves dest has to be the root of
// its work tree. A dest one level inside another checkout answers every origin
// question correctly and would let a sync fast-forward the enclosing repository.
func TestEnsureRejectsSubdirectoryOfAnotherTree(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	outer := specFor(t, repo, branchMain)

	mustEnsure(t, outer)

	nested := outer
	nested.Dest = filepath.Join(outer.Dest, "nested")

	if err := os.MkdirAll(nested.Dest, fixtureDirPerm); err != nil {
		t.Fatalf("create nested dir: %v", err)
	}

	err := source.Ensure(t.Context(), source.GitCommand, nested, nil)
	wantCode(t, err, source.CodeNotWorktree)
}

// TestSyncRefusesGlobWithNoMatchingTag is the ordinary first-boot state for a new
// tag pin: the pattern is right and the repository has not been tagged yet.
func TestSyncRefusesGlobWithNoMatchingTag(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	spec := specFor(t, repo, branchMain)

	mustEnsure(t, spec)

	spec.Ref = "v*"

	_, err := source.Sync(t.Context(), source.GitCommand, spec)
	wantCode(t, err, source.CodeNoRef)
}
