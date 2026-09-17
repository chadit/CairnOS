package source_test

import (
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

	if got := readFixtureFile(t, filepath.Join(spec.Dest, trackedFile)); got != firstContent {
		t.Fatalf("cloned README = %q, want %q", got, firstContent)
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
	repo.commit(t, secondContent, "second")
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

	repo.commit(t, secondContent, "second")
	repo.push(t)

	result, err := source.Sync(t.Context(), source.GitCommand, spec)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	if result.Old == result.New {
		t.Fatalf("sync reported no movement (%s) after a pushed commit", result.Old)
	}

	if got := readFixtureFile(t, filepath.Join(spec.Dest, trackedFile)); got != secondContent {
		t.Fatalf("README after sync = %q, want %q", got, secondContent)
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

// untrackedFile is the file every blocked-tree case drops in the checkout. The
// fixture repository tracks nothing at this path, which is the case FR3 protects:
// it has to survive the reset and the skip alike. A forced checkout does replace
// an untracked file whose path the incoming ref tracks, so the name matters.
const untrackedFile = "scratch.local"

// dirtyReason is the line FR5 promises for a tree carrying tracked edits. It is
// asserted against both a branch ref and a tag ref, which is the only reason
// they can be blocked at all.
const dirtyReason = "uncommitted changes"

// blockedCase is one tree state a plain fast-forward cannot move. The reset
// cases and the skip cases walk the same list, so the two halves of FR4 and FR5
// cannot drift apart. keeps is the tracked content a skip has to preserve.
type blockedCase struct {
	setUp func(t *testing.T, spec source.Spec)
	name  string
	want  string
	keeps string
}

// blockedCases enumerates the four states FR5 names a reason for. Every case
// runs against an origin that has already moved ahead, since a branch the remote
// is not ahead of fast-forwards without complaint.
func blockedCases() []blockedCase {
	return []blockedCase{
		{
			name: "tracked edit",
			setUp: func(t *testing.T, spec source.Spec) {
				t.Helper()

				writeFixtureFile(t, filepath.Join(spec.Dest, trackedFile), handEdit)
			},
			want:  dirtyReason,
			keeps: handEdit,
		},
		{
			name: "detached head",
			setUp: func(t *testing.T, spec source.Spec) {
				t.Helper()

				detach(t, spec.Dest, "HEAD")
			},
			want:  "HEAD is detached",
			keeps: firstContent,
		},
		{
			name: "another branch",
			setUp: func(t *testing.T, spec source.Spec) {
				t.Helper()

				git(t, spec.Dest, "switch", "-c", "side")
			},
			want:  "HEAD is on branch side, ref is main",
			keeps: firstContent,
		},
		{
			name: "local commit the remote cannot reach",
			setUp: func(t *testing.T, spec source.Spec) {
				t.Helper()

				setIdentity(t, spec.Dest)
				writeFixtureFile(t, filepath.Join(spec.Dest, "local.txt"), "local only\n")
				git(t, spec.Dest, "add", "local.txt")
				git(t, spec.Dest, "commit", "-m", "local work")
			},
			want:  "local commits the remote cannot fast-forward past",
			keeps: firstContent,
		},
	}
}

// blockTree builds a checkout in one blocked state against an origin that has
// moved on, and returns the path of the untracked file both paths must spare.
func blockTree(t *testing.T, repo origin, spec source.Spec, setUp func(*testing.T, source.Spec)) string {
	t.Helper()

	mustEnsure(t, spec)
	setUp(t, spec)

	scratch := filepath.Join(spec.Dest, untrackedFile)
	writeFixtureFile(t, scratch, untrackedBody)

	repo.commit(t, secondContent, "second")
	repo.push(t)

	return scratch
}

// TestSyncResetsBlockedTreeOntoBranch covers FR4 for every state a plain
// fast-forward cannot move: the tree lands on the fetched tip through one forced
// checkout, and the untracked file is still there afterwards.
func TestSyncResetsBlockedTreeOntoBranch(t *testing.T) {
	t.Parallel()

	for _, testCase := range blockedCases() {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			repo := newOrigin(t)
			spec := specFor(t, repo, branchMain)
			scratch := blockTree(t, repo, spec, testCase.setUp)

			result, err := source.Sync(t.Context(), source.GitCommand, spec)
			if err != nil {
				t.Fatalf("sync: %v", err)
			}

			if !result.Reset || result.Skipped != "" {
				t.Fatalf("result reset=%v skipped=%q, want a reset and no skip", result.Reset, result.Skipped)
			}

			if got := readFixtureFile(t, filepath.Join(spec.Dest, trackedFile)); got != secondContent {
				t.Fatalf("README after a reset = %q, want the upstream content", got)
			}

			if got := readFixtureFile(t, scratch); got != untrackedBody {
				t.Fatalf("untracked file after a reset = %q, want it untouched", got)
			}

			if head := git(t, spec.Dest, "symbolic-ref", "--short", "HEAD"); head != branchMain {
				t.Fatalf("branch after a reset = %q, want %s", head, branchMain)
			}
		})
	}
}

// TestSyncSkipsBlockedTreeWhenResetIsOff covers FR5: the sync succeeds, writes
// nothing, and hands back the reason, so the action reports no failure and the
// five-minute run_failed line stops. Divergence is found by an ancestry probe
// that writes nothing, so HEAD and the two files are what prove the tree was
// left exactly as the setup left it.
func TestSyncSkipsBlockedTreeWhenResetIsOff(t *testing.T) {
	t.Parallel()

	for _, testCase := range blockedCases() {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			repo := newOrigin(t)
			spec := specFor(t, repo, branchMain)
			spec.DirtyReset = false

			scratch := blockTree(t, repo, spec, testCase.setUp)
			before := git(t, spec.Dest, "rev-parse", "HEAD")

			result, err := source.Sync(t.Context(), source.GitCommand, spec)
			if err != nil {
				t.Fatalf("sync with dirty_reset off = %v, want nil", err)
			}

			if result.Skipped != testCase.want {
				t.Fatalf("result.Skipped = %q, want %q", result.Skipped, testCase.want)
			}

			if result.Reset || result.Old != result.New {
				t.Fatalf("result reset=%v %s..%s, want no movement", result.Reset, result.Old, result.New)
			}

			if after := git(t, spec.Dest, "rev-parse", "HEAD"); after != before {
				t.Fatalf("HEAD moved from %s to %s on a skipped sync", before, after)
			}

			if got := readFixtureFile(t, filepath.Join(spec.Dest, trackedFile)); got != testCase.keeps {
				t.Fatalf("README after a skipped sync = %q, want %q", got, testCase.keeps)
			}

			if got := readFixtureFile(t, scratch); got != untrackedBody {
				t.Fatalf("untracked file after a skipped sync = %q, want it untouched", got)
			}
		})
	}
}

// TestSyncReportsAProbeFailureRatherThanForcing is the reason the ancestry
// check is its own command. A merge that fails says the branch diverged or says
// git could not run; a probe that fails says only the second. A sync that cannot
// tell must report the failure, because forcing is destructive and a stale
// index.lock is not evidence that forcing is safe.
func TestSyncReportsAProbeFailureRatherThanForcing(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	spec := specFor(t, repo, branchMain)

	mustEnsure(t, spec)

	before := git(t, spec.Dest, "rev-parse", "HEAD")

	repo.commit(t, secondContent, "second")
	repo.push(t)

	probe := failingGit{verb: "merge-base", code: 128}

	_, err := source.Sync(t.Context(), probe.git, spec)
	wantCode(t, err, source.CodeGit)

	if after := git(t, spec.Dest, "rev-parse", "HEAD"); after != before {
		t.Fatalf("HEAD moved from %s to %s after a failed ancestry probe", before, after)
	}

	if got := readFixtureFile(t, filepath.Join(spec.Dest, trackedFile)); got != firstContent {
		t.Fatalf("README after a failed probe = %q, want the tree untouched", got)
	}
}

// TestSyncResetsDirtyTreeOntoTag is the tag half of FR4. A pinned source has no
// branch to recreate, so the forced checkout is detached and leaves the tree in
// the shape an ordinary tag sync does.
func TestSyncResetsDirtyTreeOntoTag(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	repo.tag(t, "v1.0.0")

	spec := specFor(t, repo, "v2.0.0")

	repo.commit(t, secondContent, "second")
	repo.tag(t, "v2.0.0")

	mustEnsure(t, spec)
	detach(t, spec.Dest, "refs/tags/v1.0.0")

	local := filepath.Join(spec.Dest, trackedFile)
	writeFixtureFile(t, local, handEdit)

	scratch := filepath.Join(spec.Dest, untrackedFile)
	writeFixtureFile(t, scratch, untrackedBody)

	result, err := source.Sync(t.Context(), source.GitCommand, spec)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	if !result.Reset {
		t.Fatal("result.Reset = false on a forced tag checkout")
	}

	if head := git(t, spec.Dest, "rev-parse", "--symbolic-full-name", "HEAD"); head != "HEAD" {
		t.Fatalf("HEAD is attached to %q after a forced tag checkout, want it detached", head)
	}

	if got := readFixtureFile(t, local); got != secondContent {
		t.Fatalf("README after a reset = %q, want the v2.0.0 content", got)
	}

	if got := readFixtureFile(t, scratch); got != untrackedBody {
		t.Fatalf("untracked file after a reset = %q, want it untouched", got)
	}

	want := git(t, spec.Dest, "rev-parse", "refs/tags/v2.0.0^{commit}")
	if head := git(t, spec.Dest, "rev-parse", "HEAD"); head != want {
		t.Fatalf("HEAD = %s, want the v2.0.0 commit %s", head, want)
	}
}

// TestSyncSkipsDirtyTagWhenResetIsOff is the tag half of FR5: the pin does not
// outrank an edit an operator left behind when they asked it not to.
func TestSyncSkipsDirtyTagWhenResetIsOff(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	repo.tag(t, "v1.0.0")

	spec := specFor(t, repo, "v2.0.0")
	spec.DirtyReset = false

	repo.commit(t, secondContent, "second")
	repo.tag(t, "v2.0.0")

	mustEnsure(t, spec)
	detach(t, spec.Dest, "refs/tags/v1.0.0")

	local := filepath.Join(spec.Dest, trackedFile)
	writeFixtureFile(t, local, handEdit)

	result, err := source.Sync(t.Context(), source.GitCommand, spec)
	if err != nil {
		t.Fatalf("sync with dirty_reset off = %v, want nil", err)
	}

	if result.Skipped != dirtyReason {
		t.Fatalf("result.Skipped = %q, want the uncommitted-changes reason", result.Skipped)
	}

	if got := readFixtureFile(t, local); got != handEdit {
		t.Fatalf("README after a skipped sync = %q, want the local edit intact", got)
	}
}

// TestSyncMovesToExactTagWhenHeadDiffers parks HEAD on the older tag first, so
// the sync has somewhere to move from.
func TestSyncMovesToExactTagWhenHeadDiffers(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	repo.tag(t, "v1.0.0")

	spec := specFor(t, repo, "v2.0.0")

	repo.commit(t, secondContent, "second")
	repo.tag(t, "v2.0.0")

	mustEnsure(t, spec)

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

	repo.commit(t, secondContent, "second")
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
	repo.commit(t, secondContent, "second")
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

// TestEnsureLeavesADirtyTreeAlone covers FR7. Stopping serve here left an
// unattended host down until someone logged in, so boot now finishes and leaves
// the repair or the skip to the first sync, with the operator's edit still there.
func TestEnsureLeavesADirtyTreeAlone(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	spec := specFor(t, repo, branchMain)

	mustEnsure(t, spec)
	git(t, spec.Dest, "switch", "-c", "side")

	local := filepath.Join(spec.Dest, trackedFile)
	writeFixtureFile(t, local, handEdit)

	if err := source.Ensure(t.Context(), source.GitCommand, spec, nil); err != nil {
		t.Fatalf("ensure over a dirty tree = %v, want nil", err)
	}

	if got := readFixtureFile(t, local); got != handEdit {
		t.Fatalf("README after ensure = %q, want the local edit intact", got)
	}

	if branch := git(t, spec.Dest, "symbolic-ref", "--short", "HEAD"); branch != "side" {
		t.Fatalf("branch after ensure = %q, want side", branch)
	}
}

// TestEnsureAllowsRefNotYetOnDisk is the other half of the pin move: the new tag
// has not been fetched. Ensure never fetches, so refusing here would stop serve
// for a state only the first sync can resolve. The tree stays where the previous
// pin left it until that sync moves it.
func TestEnsureAllowsRefNotYetOnDisk(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	spec := specFor(t, repo, branchMain)

	mustEnsure(t, spec)

	before := git(t, spec.Dest, "rev-parse", "HEAD")

	spec.Ref = "v9.9.9"

	if err := source.Ensure(t.Context(), source.GitCommand, spec, nil); err != nil {
		t.Fatalf("ensure with a ref not yet on disk = %v, want nil", err)
	}

	if after := git(t, spec.Dest, "rev-parse", "HEAD"); after != before {
		t.Fatalf("HEAD moved from %s to %s for a ref no local ref resolves", before, after)
	}

	if branch := git(t, spec.Dest, "symbolic-ref", "--short", "HEAD"); branch != branchMain {
		t.Fatalf("branch after ensure = %q, want %s", branch, branchMain)
	}
}

// TestEnsureStartsWithAnUnreachableRemote covers FR5. Deleting the origin is the
// offline host: a tree already on disk has to finish serve startup, which it can
// only do by reading local refs and never reaching for the remote.
func TestEnsureStartsWithAnUnreachableRemote(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	spec := specFor(t, repo, branchMain)

	mustEnsure(t, spec)

	if err := os.RemoveAll(repo.bare); err != nil {
		t.Fatalf("remove the origin: %v", err)
	}

	if err := source.Ensure(t.Context(), source.GitCommand, spec, nil); err != nil {
		t.Fatalf("ensure over an existing tree with no reachable origin: %v", err)
	}

	if branch := git(t, spec.Dest, "symbolic-ref", "--short", "HEAD"); branch != branchMain {
		t.Fatalf("branch after an offline ensure = %q, want %s", branch, branchMain)
	}

	if got := readFixtureFile(t, filepath.Join(spec.Dest, trackedFile)); got != firstContent {
		t.Fatalf("README after an offline ensure = %q, want %q", got, firstContent)
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
