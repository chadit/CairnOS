package cli

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"github.com/chadit/CairnOS/internal/source"
)

// defaultRemote is the remote a checkout whose HEAD tracks nothing, a detached
// HEAD included, is put back on. It is the name git clone gives the remote a
// host's checkout came from.
const defaultRemote = "origin"

// headRef names the commit the working tree sits on.
const headRef = "HEAD"

// checkoutTarget is where an update puts a checkout: the remote it fetches, the
// remote-tracking ref it builds, and the local branch that carries that ref.
type checkoutTarget struct {
	remote string
	ref    string
	branch string
}

// checkoutState is how a checkout sits against its target, read after the
// fetch and before an update picks between a fast-forward and a reset.
type checkoutState struct {
	// branch is the branch HEAD is on, empty for a detached HEAD.
	branch string

	// changes are the `git status --porcelain` lines, untracked files
	// included: everything a reset takes out of the working tree.
	changes []string

	ahead  int
	behind int
}

// reasons names everything that keeps the checkout from a plain fast-forward
// onto target, in the words the --check report and the reset warning share. An
// empty list means a fast-forward, or nothing at all, gets it there.
func (s *checkoutState) reasons(target *checkoutTarget) []string {
	var reasons []string

	switch {
	case s.branch == "":
		reasons = append(reasons, "HEAD is detached")
	case s.branch != target.branch:
		reasons = append(reasons, fmt.Sprintf("HEAD is on branch %s, not %s", s.branch, target.branch))
	}

	if len(s.changes) > 0 {
		reasons = append(reasons, fmt.Sprintf("%d changed or untracked path(s)", len(s.changes)))
	}

	if s.ahead > 0 {
		reasons = append(reasons, fmt.Sprintf("%d commit(s) not on %s", s.ahead, target.ref))
	}

	return reasons
}

// resolveTarget names the branch a checkout should sit on: the default branch
// git clone recorded for the tracked remote, or origin, which brings a checkout
// on another branch back. Without a recorded default the branch's upstream
// stands in, and with neither the checkout is left alone. A probe's non-zero
// exit is its answer.
func resolveTarget(ctx context.Context, dir string) (checkoutTarget, bool) {
	branch := currentBranch(ctx, dir)
	remote := cmp.Or(trackedRemote(ctx, dir, branch), defaultRemote)

	defaultRef, err := source.GitCommand(ctx, dir,
		"symbolic-ref", "--quiet", "--short", "refs/remotes/"+remote+"/"+headRef)
	if err == nil {
		return checkoutTarget{remote: remote, ref: defaultRef, branch: strings.TrimPrefix(defaultRef, remote+"/")}, true
	}

	upstream := upstreamRef(ctx, dir)
	if upstream == "" {
		return checkoutTarget{}, false
	}

	return checkoutTarget{remote: remote, ref: upstream, branch: branch}, true
}

// currentBranch names the branch HEAD is on, empty for a detached HEAD, which
// symbolic-ref answers with a non-zero exit.
func currentBranch(ctx context.Context, dir string) string {
	branch, err := source.GitCommand(ctx, dir, "symbolic-ref", "--quiet", "--short", headRef)
	if err != nil {
		return ""
	}

	return branch
}

// trackedRemote names the remote branch tracks, which need not be origin, and
// answers empty for a detached HEAD or a branch tracking nothing.
func trackedRemote(ctx context.Context, dir, branch string) string {
	if branch == "" {
		return ""
	}

	remote, err := source.GitCommand(ctx, dir, "config", "--get", "branch."+branch+".remote")
	if err != nil {
		return ""
	}

	return remote
}

// fetchTarget updates the remote-tracking refs of the target's remote and
// nothing else, so learning what landed upstream never touches the working tree.
func fetchTarget(ctx context.Context, dir string, target *checkoutTarget) error {
	if _, err := source.GitCommand(ctx, dir, "fetch", "--quiet", "--", target.remote); err != nil {
		return fmt.Errorf("fetch %s in %s: %w", target.remote, dir, err)
	}

	return nil
}

// readCheckoutState reads how the checkout sits against target. The status
// carries the branch header because the git runner trims its output, and the
// header is what keeps the first change line's leading space, half of that
// line's status code, intact.
func readCheckoutState(ctx context.Context, dir string, target *checkoutTarget) (checkoutState, error) {
	status, err := source.GitCommand(ctx, dir, "status", "--porcelain", "--branch", "--untracked-files=normal")
	if err != nil {
		return checkoutState{}, fmt.Errorf("read the working tree state in %s: %w", dir, err)
	}

	counts, err := source.GitCommand(ctx, dir, "rev-list", "--left-right", "--count", headRef+"..."+target.ref)
	if err != nil {
		return checkoutState{}, fmt.Errorf("count commits against %s in %s: %w", target.ref, dir, err)
	}

	ahead, behind, err := parseGapCounts(counts)
	if err != nil {
		return checkoutState{}, err
	}

	return checkoutState{
		branch:  currentBranch(ctx, dir),
		changes: strings.Split(status, "\n")[1:],
		ahead:   ahead,
		behind:  behind,
	}, nil
}

// alignCheckout puts the checkout on its already-fetched target. A clean
// checkout on the target branch that is behind fast-forwards, which is the
// normal case; anything a fast-forward cannot carry is reset onto the target,
// since a host's checkout exists to build what upstream holds.
func (a *App) alignCheckout(ctx context.Context, dir string, target *checkoutTarget) error {
	state, err := readCheckoutState(ctx, dir, target)
	if err != nil {
		return err
	}

	if len(state.reasons(target)) > 0 {
		return a.resetCheckout(ctx, dir, target, &state)
	}

	if state.behind == 0 {
		a.logger.Info("update: the checkout already sits on its upstream", "upstream", target.ref)

		return nil
	}

	if _, err := source.GitCommand(ctx, dir, "merge", "--ff-only", target.ref); err != nil {
		return fmt.Errorf("fast-forward %s onto %s: %w", dir, target.ref, err)
	}

	a.logger.Info("update: fast-forwarded", "upstream", target.ref, "commits", state.behind)

	return nil
}

// resetCheckout puts the checkout on target and logs at warn what it threw
// away. checkout -f discards edits and untracked files in the way, and -B
// recreates the branch, covering a detached HEAD or another branch. clean takes
// other untracked files, which would make cmd/go stamp "+dirty" and serve's
// loop refuse the build. Without -x, ignored bin/ survives.
func (a *App) resetCheckout(ctx context.Context, dir string, target *checkoutTarget, state *checkoutState) error {
	dropped, err := droppedCommits(ctx, dir, state, target)
	if err != nil {
		return err
	}

	if _, err := source.GitCommand(ctx, dir, "checkout", "-f", "--track", "-B", target.branch, target.ref); err != nil {
		return fmt.Errorf("reset %s onto %s: %w", dir, target.ref, err)
	}

	if _, err := source.GitCommand(ctx, dir, "clean", "-f", "-d"); err != nil {
		return fmt.Errorf("remove the untracked files in %s: %w", dir, err)
	}

	a.logger.Warn("update: checkout reset onto its upstream, local state discarded",
		"dir", dir,
		"upstream", target.ref,
		"reason", strings.Join(state.reasons(target), "; "),
		"discarded", strings.Join(state.changes, "; "),
		"dropped_commits", strings.Join(dropped, "; "))

	return nil
}

// droppedCommits lists, by short sha and subject, the commits a reset onto
// target leaves on no branch: those on a detached HEAD and those on the local
// branch the reset moves. Another branch HEAD sat on keeps its own commits. The
// ref probe's non-zero exit means the local branch does not exist yet.
func droppedCommits(ctx context.Context, dir string, state *checkoutState, target *checkoutTarget) ([]string, error) {
	var tips []string

	if state.branch == "" {
		tips = append(tips, headRef)
	}

	local := "refs/heads/" + target.branch
	if _, err := source.GitCommand(ctx, dir, "rev-parse", "--verify", "--quiet", local); err == nil {
		tips = append(tips, local)
	}

	if len(tips) == 0 {
		return nil, nil
	}

	argv := append([]string{"log", "--format=%h %s"}, tips...)
	argv = append(argv, "--not", target.ref, "--")

	listing, err := source.GitCommand(ctx, dir, argv...)
	if err != nil {
		return nil, fmt.Errorf("list the commits a reset of %s drops: %w", dir, err)
	}

	if listing == "" {
		return nil, nil
	}

	return strings.Split(listing, "\n"), nil
}
