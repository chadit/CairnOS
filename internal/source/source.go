package source

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/chadit/CairnOS/pkg/runner"
)

// Stable machine codes every failure here carries. Clone and fetch are the two
// that a later attempt can clear on its own, so they come back transient; the
// rest describe a tree or a ref that stays wrong until someone changes it.
const (
	CodeClone       = "source.clone"
	CodeFetch       = "source.fetch"
	CodeMismatch    = "source.mismatch"
	CodeAmbiguous   = "source.ambiguous"
	CodeNoRef       = "source.no_ref"
	CodeNotWorktree = "source.not_worktree"

	// CodeGit is the fallback for a git invocation that failed for a reason none
	// of the codes above describes, such as a HEAD that will not resolve.
	CodeGit = "source.git"
)

// logReadyTimeout bounds the one call the startup log makes after Ensure has
// already returned and canceled its own context. Without it a HEAD read on a
// hung network mount blocks serve start with nothing left to unwind it.
const logReadyTimeout = 10 * time.Second

// Wall-clock bounds on the two entry points. Ensure covers a first clone of a
// large repository at serve start; Sync covers a fetch and a fast-forward and
// runs inside a scheduled task whose own deadline can cut it shorter.
const (
	ensureTimeout = 10 * time.Minute
	syncTimeout   = 2 * time.Minute
)

// globChars are the characters that make a ref a tag pattern rather than a name.
const globChars = "*?["

// Exit statuses the ancestry probe reads. merge-base --is-ancestor documents 1
// as "not an ancestor" and reserves every other non-zero status for a real
// failure, so the two must never be confused. unknownExit stands for a failure
// that never became a process exit at all.
const (
	exitNotAncestor = 1
	unknownExit     = -1
)

// parentPerm is the mode for a parent directory created on the way to dest:
// group-readable, owner-writable, within gosec's G301 bound. Git sets the mode
// of the tree itself.
const parentPerm = 0o750

// Spec is one configured source, flattened from config for the sync path.
type Spec struct {
	Name string
	Repo string
	Dest string
	Ref  string

	// DirtyReset lets Sync force a tree that cannot fast-forward back onto Ref.
	// False makes the same tree a skip with a reason instead of a write.
	DirtyReset bool
}

// Result reports where HEAD was and where it ended up, as short hashes. Equal
// fields mean the sync moved nothing.
type Result struct {
	Old string
	New string

	// Skipped is non-empty when DirtyReset is off and something in the tree
	// blocked the move. It is a one-line reason for an operator, and the sync
	// still counts as a success: nothing failed, the tree was left alone.
	Skipped string

	// Reset reports that the tree was forced onto the ref and tracked local
	// changes went with it.
	Reset bool
}

// The one-line reasons a blocked tree carries into Result.Skipped and the warn
// logs. They are phrased for an operator reading a chat notification, so they
// name the state rather than the git command that found it.
const (
	reasonDirty       = "uncommitted changes"
	reasonDetached    = "HEAD is detached"
	reasonOtherBranch = "HEAD is on branch %s, ref is %s"
	reasonDiverged    = "local commits the remote cannot fast-forward past"
)

// target is a resolved ref: either a branch to follow or the tag to pin to.
type target struct {
	name   string
	branch bool
}

// Ensure makes dest usable without fetching: it clones when dest is missing and
// otherwise proves the existing tree is the right one and leaves it on the
// configured ref. Skipping the fetch is what lets an offline host with a tree
// already on disk finish serve startup. logger may be nil.
func Ensure(ctx context.Context, git Git, spec Spec, logger *slog.Logger) error {
	runCtx, cancel := context.WithTimeout(ctx, ensureTimeout)
	defer cancel()

	present, err := destPresent(spec.Dest)
	if err != nil {
		return err
	}

	if present {
		return verifyExisting(runCtx, git, spec, logger)
	}

	return cloneInto(runCtx, git, spec)
}

// EnsureAll runs Ensure for every spec in name order and logs the tree each one
// left behind, so the startup log says which commit a host is about to run.
func EnsureAll(ctx context.Context, git Git, specs []Spec, logger *slog.Logger) error {
	ordered := slices.Clone(specs)
	slices.SortFunc(ordered, func(left, right Spec) int {
		return strings.Compare(left.Name, right.Name)
	})

	for _, spec := range ordered {
		if err := Ensure(ctx, git, spec, logger); err != nil {
			return fmt.Errorf("source %s: %w", spec.Name, err)
		}

		logReady(ctx, git, spec, logger)
	}

	return nil
}

// Sync brings dest to the current tip of its ref. The caller serializes calls
// for one spec. The status read comes after the fetch, not before it, because a
// tree that has to be forced onto the ref needs the fetched tip to aim at.
func Sync(ctx context.Context, git Git, spec Spec) (Result, error) {
	runCtx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()

	if _, err := git(runCtx, spec.Dest, "fetch", "--prune", "--tags", "origin"); err != nil {
		return Result{}, transientf(err, CodeFetch, "fetch origin in %s: %v", spec.Dest, err)
	}

	want, err := resolveRef(runCtx, git, spec)
	if err != nil {
		return Result{}, err
	}

	old, err := shortHead(runCtx, git, spec.Dest)
	if err != nil {
		return Result{}, err
	}

	blocking, err := tryAdvance(runCtx, git, spec, want)
	if err != nil {
		return Result{}, err
	}

	if blocking != "" {
		return resolveBlocked(runCtx, git, spec, want, old, blocking)
	}

	moved, err := shortHead(runCtx, git, spec.Dest)
	if err != nil {
		return Result{}, err
	}

	return Result{Old: old, New: moved}, nil
}

// tryAdvance moves HEAD onto want when nothing is in the way, and otherwise
// names what is. Tracked changes are read first because a fast-forward merge
// over them fails with git's own wording rather than a reason an operator can
// act on.
func tryAdvance(ctx context.Context, git Git, spec Spec, want target) (string, error) {
	dirty, err := localChanges(ctx, git, spec)
	if err != nil {
		return "", err
	}

	if dirty {
		return reasonDirty, nil
	}

	return advance(ctx, git, spec, want)
}

// resolveBlocked settles a tree that cannot move on its own: force it back onto
// the ref, or report the skip and write nothing. A skip is a success, which is
// the whole point of dirty_reset = false: a run that fails every few minutes
// notifies every few minutes.
func resolveBlocked(
	ctx context.Context, git Git, spec Spec, want target, old, blocking string,
) (Result, error) {
	if !spec.DirtyReset {
		return Result{Old: old, New: old, Skipped: blocking}, nil
	}

	if err := forceOnto(ctx, git, spec, want); err != nil {
		return Result{}, err
	}

	moved, err := shortHead(ctx, git, spec.Dest)
	if err != nil {
		return Result{}, err
	}

	return Result{Old: old, New: moved, Reset: true}, nil
}

// destPresent reports whether dest already exists. A stat that fails for any
// reason other than absence is permanent: serve cannot clone over a path it
// cannot even read.
func destPresent(dest string) (bool, error) {
	_, err := os.Stat(dest)
	if err == nil {
		return true, nil
	}

	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}

	return false, permanentf(err, CodeGit, "read %s: %v", dest, err)
}

// cloneInto clones the repository and leaves the configured ref checked out.
// The clone itself runs from the parent directory, which dest does not exist
// inside yet.
func cloneInto(ctx context.Context, git Git, spec Spec) error {
	parent := filepath.Dir(spec.Dest)

	// A directory that cannot be created is not a clone failure, so it does not
	// take CodeClone: FR10 makes every source.clone retryable and a permissions
	// problem on the parent path is not.
	if err := os.MkdirAll(parent, parentPerm); err != nil {
		return permanentf(err, CodeGit, "create %s: %v", parent, err)
	}

	// The separator keeps a repo value from being read as an option. Config load
	// already refuses one that starts with a dash, and git clone is the only verb
	// here that documents an end-of-options marker.
	if _, err := git(ctx, parent, "clone", "--", spec.Repo, spec.Dest); err != nil {
		return transientf(err, CodeClone, "clone %s into %s: %v", spec.Repo, spec.Dest, err)
	}

	return checkoutRef(ctx, git, spec)
}

// verifyExisting proves an existing dest is the tree this spec describes, and
// that it is sitting where the block says. It reads only refs already on disk,
// so an offline host with a tree still finishes serve startup (FR5).
func verifyExisting(ctx context.Context, git Git, spec Spec, logger *slog.Logger) error {
	if err := verifyOrigin(ctx, git, spec); err != nil {
		return err
	}

	return positionExisting(ctx, git, spec, logger)
}

// verifyOrigin proves dest is the root of a work tree whose origin is this
// spec's repository. A path that is not a work tree, one that is a subdirectory
// of some other checkout, or one whose origin points somewhere else is an
// operator mistake that syncing would turn into a surprise write.
func verifyOrigin(ctx context.Context, git Git, spec Spec) error {
	toplevel, err := git(ctx, spec.Dest, "rev-parse", "--show-toplevel")
	if err != nil {
		return permanentf(err, CodeNotWorktree, "%s is not a git work tree: %v", spec.Dest, err)
	}

	if rootErr := sameDir(toplevel, spec.Dest); rootErr != nil {
		return rootErr
	}

	origin, err := git(ctx, spec.Dest, "remote", "get-url", "origin")
	if err != nil {
		return permanentf(err, CodeMismatch, "%s has no origin remote: %v", spec.Dest, err)
	}

	if origin != spec.Repo {
		return permanentf(nil, CodeMismatch, "%s has origin %q, but source %s is %q",
			spec.Dest, origin, spec.Name, spec.Repo)
	}

	return nil
}

// sameDir fails unless dest is the work tree root git reported. Symlinks are
// resolved on both sides, since a dest reached through one names the same
// directory git answers with its real path.
func sameDir(toplevel, dest string) error {
	root, rootErr := filepath.EvalSymlinks(toplevel)
	if rootErr != nil {
		root = filepath.Clean(toplevel)
	}

	want, wantErr := filepath.EvalSymlinks(dest)
	if wantErr != nil {
		want = filepath.Clean(dest)
	}

	if root == want {
		return nil
	}

	return permanentf(nil, CodeNotWorktree,
		"%s is inside the work tree rooted at %s, not the root of one", dest, root)
}

// positionExisting leaves an existing tree on the configured ref. Moving a pin
// is routine and an interrupted sync leaves the same shape, so a tree sitting
// elsewhere is repaired rather than refused: refusing stops serve with nothing
// able to clear it. Nothing here fetches, and a tree carrying local work is left
// for the first sync.
func positionExisting(ctx context.Context, git Git, spec Spec, logger *slog.Logger) error {
	want, err := resolveRef(ctx, git, spec)
	if err != nil {
		return tolerateMissingRef(spec, logger, err)
	}

	onRef, old, err := headOnRef(ctx, git, spec, want)
	if err != nil {
		return err
	}

	if onRef {
		return nil
	}

	dirty, err := localChanges(ctx, git, spec)
	if err != nil {
		return err
	}

	if dirty {
		logDirtyAtBoot(spec, logger)

		return nil
	}

	if checkoutErr := checkoutTarget(ctx, git, spec, want); checkoutErr != nil {
		return checkoutErr
	}

	logRepositioned(ctx, git, spec, logger, old)

	return nil
}

// logDirtyAtBoot records a tree the boot leaves where it is. Serve used to stop
// here, which left an unattended host down until someone logged in, so the
// repair or the report is the first sync's job now.
func logDirtyAtBoot(spec Spec, logger *slog.Logger) {
	if logger == nil {
		return
	}

	logger.Warn("source tree has local changes; the first sync will reset or skip it",
		"source", spec.Name, "dest", spec.Dest, "ref", spec.Ref, "dirty_reset", spec.DirtyReset)
}

// tolerateMissingRef lets a ref that no local ref answers through with a warning.
// Ensure never fetches, so a ref the operator has just pinned may simply not be
// on disk yet and the first sync is what brings it down. An ambiguous ref or a
// failed git call still stops serve, because neither improves with a fetch.
func tolerateMissingRef(spec Spec, logger *slog.Logger, cause error) error {
	classified, ok := errors.AsType[*runner.Error](cause)
	if !ok || classified.Code != CodeNoRef {
		return cause
	}

	if logger != nil {
		logger.Warn("source ref is not on disk yet; the first sync will fetch and position it",
			"source", spec.Name, "dest", spec.Dest, "ref", spec.Ref)
	}

	return nil
}

// headOnRef reports whether HEAD already sits on the resolved target, with the
// short HEAD to log when it does not. A branch only has to be checked out, not at
// the remote tip, since catching up is the sync's job.
func headOnRef(ctx context.Context, git Git, spec Spec, want target) (bool, string, error) {
	old, err := shortHead(ctx, git, spec.Dest)
	if err != nil {
		return false, "", err
	}

	if want.branch {
		// A detached HEAD makes symbolic-ref exit non-zero, which is one of the
		// ways of not being on the branch rather than a failure.
		head, symErr := git(ctx, spec.Dest, "symbolic-ref", "--short", "-q", "HEAD")

		return symErr == nil && head == spec.Ref, old, nil
	}

	pinned, head, err := tagAndHead(ctx, git, spec, want.name)
	if err != nil {
		return false, "", err
	}

	return head == pinned, old, nil
}

// logRepositioned records a boot that moved the tree, so the startup log says a
// pin changed rather than leaving the new commit unexplained.
func logRepositioned(ctx context.Context, git Git, spec Spec, logger *slog.Logger, old string) {
	if logger == nil {
		return
	}

	moved, err := shortHead(ctx, git, spec.Dest)
	if err != nil {
		moved = "unknown"
	}

	logger.Info("source repositioned", "source", spec.Name, "dest", spec.Dest,
		"ref", spec.Ref, "old", old, "new", moved)
}

// checkoutRef resolves the configured ref and puts the tree on it.
func checkoutRef(ctx context.Context, git Git, spec Spec) error {
	want, err := resolveRef(ctx, git, spec)
	if err != nil {
		return err
	}

	return checkoutTarget(ctx, git, spec, want)
}

// checkoutTarget puts a tree on an already-resolved ref. A branch gets a local
// branch tracking the remote; a tag is checked out detached, because a pinned tag
// has no branch to move.
func checkoutTarget(ctx context.Context, git Git, spec Spec, want target) error {
	if !want.branch {
		return detachAtTag(ctx, git, spec, want.name)
	}

	// The trailing separator settles branch-versus-file ambiguity in favor of the
	// branch. It cannot turn the name into a path the way a leading one would,
	// since nothing follows it.
	if _, err := git(ctx, spec.Dest, "checkout", want.name, "--"); err != nil {
		return permanentf(err, CodeGit, "check out branch %s in %s: %v", want.name, spec.Dest, err)
	}

	return nil
}

// resolveRef asks the remote refs what the configured ref is, rather than making
// the operator declare it. A glob is always a tag pattern; anything else is a
// branch or a tag, and a name that is both is refused instead of guessed at.
func resolveRef(ctx context.Context, git Git, spec Spec) (target, error) {
	if strings.ContainsAny(spec.Ref, globChars) {
		return resolveGlob(ctx, git, spec)
	}

	isBranch := refExists(ctx, git, spec.Dest, "refs/remotes/origin/"+spec.Ref)
	isTag := refExists(ctx, git, spec.Dest, "refs/tags/"+spec.Ref)

	switch {
	case isBranch && isTag:
		return target{}, permanentf(nil, CodeAmbiguous,
			"ref %q of source %s names both a branch and a tag", spec.Ref, spec.Name)
	case isBranch:
		return target{name: spec.Ref, branch: true}, nil
	case isTag:
		return target{name: spec.Ref}, nil
	default:
		return target{}, permanentf(nil, CodeNoRef,
			"ref %q of source %s names no remote branch or tag", spec.Ref, spec.Name)
	}
}

// resolveGlob picks the newest tag matching the pattern. Version sort, not
// creation date, decides "newest", so a patch tagged late on an old line does
// not pull the tree backwards.
func resolveGlob(ctx context.Context, git Git, spec Spec) (target, error) {
	// git tag documents no end-of-options separator, so the pattern is guarded the
	// same way the branch checkout is: config load refuses a ref starting with a
	// dash, which is the only way this argument could read as an option.
	listed, err := git(ctx, spec.Dest, "tag", "--list", spec.Ref, "--sort=-v:refname")
	if err != nil {
		return target{}, permanentf(err, CodeGit, "list tags matching %q: %v", spec.Ref, err)
	}

	newest, _, _ := strings.Cut(listed, "\n")

	newest = strings.TrimSpace(newest)
	if newest == "" {
		return target{}, permanentf(nil, CodeNoRef,
			"tag pattern %q of source %s matches no tag", spec.Ref, spec.Name)
	}

	return target{name: newest}, nil
}

// refExists reports whether one fully qualified ref resolves in dir.
func refExists(ctx context.Context, git Git, dir, ref string) bool {
	_, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", ref)

	return err == nil
}

// localChanges reports whether the tree carries tracked modifications.
// Untracked files are deliberately excluded: a repair does not remove them, so
// counting them would leave a tree that still reads dirty after every repair and
// never settles.
func localChanges(ctx context.Context, git Git, spec Spec) (bool, error) {
	status, err := git(ctx, spec.Dest, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return false, permanentf(err, CodeGit, "read the status of %s: %v", spec.Dest, err)
	}

	return status != "", nil
}

// advance moves HEAD onto the resolved target, or names what blocks it. A tag
// is checked out detached from wherever the tree sits, so only a branch ref has
// anything that can block.
func advance(ctx context.Context, git Git, spec Spec, want target) (string, error) {
	if want.branch {
		return fastForward(ctx, git, spec, want)
	}

	return "", detachAtTag(ctx, git, spec, want.name)
}

// fastForward moves the local branch to the remote tip, or reports why it
// cannot. A detached HEAD, a different branch, and a history the remote cannot
// reach each mean the tree carries work only a forced checkout would discard,
// so that choice belongs to the caller. The ref comes from want, leaving
// resolveRef to decide what it means.
func fastForward(ctx context.Context, git Git, spec Spec, want target) (string, error) {
	head, err := git(ctx, spec.Dest, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		// -q makes a detached HEAD exit non-zero with no output, so this is the
		// state itself rather than a command that went wrong.
		return reasonDetached, nil //nolint:nilerr // a non-zero exit here means detached HEAD
	}

	if head != want.name {
		return fmt.Sprintf(reasonOtherBranch, head, want.name), nil
	}

	remote := remoteRef(want.name)

	ahead, err := remoteIsAhead(ctx, git, spec.Dest, remote)
	if err != nil {
		return "", err
	}

	if !ahead {
		return reasonDiverged, nil
	}

	if _, mergeErr := git(ctx, spec.Dest, "merge", "--ff-only", remote); mergeErr != nil {
		return "", permanentf(mergeErr, CodeGit,
			"fast-forward %s to %s: %v", spec.Dest, remote, mergeErr)
	}

	return "", nil
}

// remoteIsAhead asks whether the remote tip has HEAD in its history, which is
// the one condition a fast-forward needs. Asking outright is what keeps a
// locked index or an unreadable object from being reported as a diverged branch
// and then forced over: merge-base writes nothing, answers "no" with exit 1, and
// signals a genuine failure with any other status.
func remoteIsAhead(ctx context.Context, git Git, dir, remote string) (bool, error) {
	_, err := git(ctx, dir, "merge-base", "--is-ancestor", "HEAD", remote)
	if err == nil {
		return true, nil
	}

	if exitCode(err) == exitNotAncestor {
		return false, nil
	}

	return false, permanentf(err, CodeGit, "compare %s with %s: %v", dir, remote, err)
}

// exitCode reports the status a failed git invocation exited with, or
// unknownExit when the failure was not a process that ran and exited.
func exitCode(err error) int {
	exit, ok := errors.AsType[*ExitError](err)
	if !ok {
		return unknownExit
	}

	return exit.Code
}

// remoteRef is the fully qualified remote-tracking ref for a branch name, the
// one spelling both the probe and the forced checkout aim at.
func remoteRef(branch string) string {
	return "refs/remotes/origin/" + branch
}

// forceOnto puts the tree on want whatever it was carrying. Tracked edits and
// local commits are discarded. An untracked file survives unless want tracks a
// file at the same path, where -f lets the ref's copy replace it; clean, stash,
// and reset are never run, so nothing else untracked is touched.
func forceOnto(ctx context.Context, git Git, spec Spec, want target) error {
	if want.branch {
		return forceOntoBranch(ctx, git, spec, want)
	}

	return forceOntoTag(ctx, git, spec, want.name)
}

// forceOntoBranch recreates the local branch at the fetched remote tip. The -B
// is what covers a detached HEAD and a tree sitting on some other branch: a
// plain forced checkout of the branch would leave a diverged local branch
// exactly where it was.
func forceOntoBranch(ctx context.Context, git Git, spec Spec, want target) error {
	remote := remoteRef(want.name)

	if _, err := git(ctx, spec.Dest, "checkout", "-f", "-B", want.name, remote); err != nil {
		return permanentf(err, CodeGit, "force %s onto %s: %v", spec.Dest, remote, err)
	}

	return nil
}

// forceOntoTag parks HEAD on the tag's commit with no branch attached, the same
// shape an ordinary pinned-tag sync leaves behind.
func forceOntoTag(ctx context.Context, git Git, spec Spec, tag string) error {
	if _, err := git(ctx, spec.Dest, "checkout", "-f", "--detach", "refs/tags/"+tag); err != nil {
		return permanentf(err, CodeGit, "force %s onto tag %s: %v", spec.Dest, tag, err)
	}

	return nil
}

// detachAtTag parks HEAD on the tag's commit, and does nothing when it is
// already there, so a sync that finds the pin unchanged makes no write at all.
func detachAtTag(ctx context.Context, git Git, spec Spec, tag string) error {
	pinned, head, err := tagAndHead(ctx, git, spec, tag)
	if err != nil {
		return err
	}

	if head == pinned {
		return nil
	}

	if _, checkoutErr := git(ctx, spec.Dest, "checkout", "--detach", "refs/tags/"+tag); checkoutErr != nil {
		return permanentf(checkoutErr, CodeGit, "check out tag %s in %s: %v", tag, spec.Dest, checkoutErr)
	}

	return nil
}

// tagAndHead resolves one tag's commit and the current HEAD, the pair both the
// boot-time check and the sync compare.
func tagAndHead(ctx context.Context, git Git, spec Spec, tag string) (string, string, error) {
	pinned, err := git(ctx, spec.Dest, "rev-parse", "refs/tags/"+tag+"^{commit}")
	if err != nil {
		return "", "", permanentf(err, CodeGit, "resolve tag %s in %s: %v", tag, spec.Dest, err)
	}

	head, err := git(ctx, spec.Dest, "rev-parse", "HEAD")
	if err != nil {
		return "", "", permanentf(err, CodeGit, "resolve HEAD in %s: %v", spec.Dest, err)
	}

	return pinned, head, nil
}

// shortHead reads the current commit in the short form the runner reports.
func shortHead(ctx context.Context, git Git, dir string) (string, error) {
	head, err := git(ctx, dir, "rev-parse", "--short", "HEAD")
	if err != nil {
		return "", permanentf(err, CodeGit, "resolve HEAD in %s: %v", dir, err)
	}

	return head, nil
}

// logReady records what a source is sitting on after Ensure. A HEAD that will
// not resolve is reported rather than raised: Ensure already passed, so the
// startup line is worth more than a second failure path.
func logReady(ctx context.Context, git Git, spec Spec, logger *slog.Logger) {
	if logger == nil {
		return
	}

	// Ensure has already canceled its own deadline by the time this runs, so the
	// bound FR11 asks for has to come from here.
	logCtx, cancel := context.WithTimeout(ctx, logReadyTimeout)
	defer cancel()

	head, err := shortHead(logCtx, git, spec.Dest)
	if err != nil {
		head = "unknown"
	}

	logger.Info("source ready", "source", spec.Name, "dest", spec.Dest, "ref", spec.Ref, "head", head)
}

// permanentf builds a classified failure retrying cannot clear. cause is the
// git error behind it, or nil when this package decided on its own. Setting
// Cause is what %w would do here: runner.Error unwraps to it, so a caller can
// still tell a DNS failure from an auth failure inside one code.
func permanentf(cause error, code, format string, args ...any) error {
	return &runner.Error{
		Class: runner.ClassPermanent, Code: code,
		Message: fmt.Sprintf(format, args...), Cause: cause,
	}
}

// transientf builds a classified failure a later attempt may clear on its own,
// carrying its cause the same way permanentf does.
func transientf(cause error, code, format string, args ...any) error {
	return &runner.Error{
		Class: runner.ClassTransient, Code: code,
		Message: fmt.Sprintf(format, args...), Cause: cause,
	}
}
