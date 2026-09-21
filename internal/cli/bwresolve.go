package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/chadit/CairnOS/internal/bitwarden"
	"github.com/chadit/CairnOS/internal/bwinstall"
	"github.com/chadit/CairnOS/internal/environ"
)

// bwToolsDir is the directory under the CairnOS home that holds the pinned
// copy, and bwToolsDirPerm keeps it to its owner. That binary runs with the
// vault credentials, so another account being able to swap it would undo the
// digest check that put it there.
const (
	bwToolsDir     = "tools"
	bwToolsDirPerm = 0o700
)

// The reasons a resolution reports, so one log line says why this bw and not
// the other one.
const (
	bwReasonHostCurrent = "the host CLI is at or past the pinned version"
	bwReasonNoHost      = "no bw on the host"
	bwReasonHostOlder   = "the host CLI is older than the pinned version"
	bwReasonHostUnread  = "the host CLI did not report a readable version"
	bwReasonHostFailed  = "the host CLI could not reach the vault"
	bwReasonUnpinned    = "no release is pinned for " + runtime.GOOS
)

// unknownVersion stands in when a binary is used without its version being
// read, which is only the unpinned-platform path.
const unknownVersion = "unknown"

// bwChoice is the Bitwarden CLI one command runs: where it is, what version it
// reports, and why it was picked over the alternative.
type bwChoice struct {
	path    string
	version string
	reason  string

	// pinned marks the copy CairnOS installed and verified itself. It is the
	// end of the line: a failure through it has no second binary to try.
	pinned bool
}

// PinnedBitwardenVersion is the Bitwarden CLI release this build carries the
// digests for. A host CLI at or past it is used as it is, and anything behind
// it falls back to this release.
func PinnedBitwardenVersion() string {
	return bwVersion
}

// bwPinnedPath is where the pinned copy lives: inside the CairnOS home, keyed
// by version so a pin bump installs beside the old one, and named bw because
// the binary reads its own argv0.
func bwPinnedPath(home string) string {
	return filepath.Join(home, bwToolsDir, bwBinary, bwVersion, bwBinary)
}

// readVaultItem opens the vault and returns the item, through whichever bw the
// host's own version and the pin decide on. A host CLI that turns out to be
// unusable is retried once through the pinned copy, so a broken or unreachable
// CLI costs a moment rather than a failed deploy.
func (a *App) readVaultItem(
	ctx context.Context, home string, creds bitwarden.Credentials, itemName string,
) (bitwarden.Item, error) {
	choice, err := a.resolveBitwarden(ctx, home)
	if err != nil {
		return bitwarden.Item{}, err
	}

	item, err := a.readThrough(ctx, choice, creds, itemName)
	if err == nil {
		return item, nil
	}

	if choice.pinned || !bitwarden.Unusable(err) {
		return bitwarden.Item{}, err
	}

	return a.retryPinned(ctx, home, creds, itemName, err)
}

// retryPinned runs the read again through the pinned copy, after the host CLI
// failed in a way that says the binary is the problem. A pinned copy that
// cannot be had leaves the host's failure as the one reported, since that is
// the one the operator has to act on.
func (a *App) retryPinned(
	ctx context.Context, home string, creds bitwarden.Credentials, itemName string, cause error,
) (bitwarden.Item, error) {
	a.logger.Warn("the host Bitwarden CLI could not reach the vault; trying the pinned copy",
		"error", cause)

	choice, err := a.pinnedBitwarden(ctx, home, bwReasonHostFailed)
	if err != nil {
		a.logger.Warn("the pinned Bitwarden CLI is not available", "error", err)

		return bitwarden.Item{}, cause
	}

	return a.readThrough(ctx, choice, creds, itemName)
}

// readThrough records which binary is about to run and runs it. Every vault
// call goes through here, so a deploy log always says which bw answered and
// why that one.
func (a *App) readThrough(
	ctx context.Context, choice bwChoice, creds bitwarden.Credentials, itemName string,
) (bitwarden.Item, error) {
	a.logger.Info("using the Bitwarden CLI",
		"path", choice.path, "version", choice.version, "reason", choice.reason)

	client := &bitwarden.Client{
		Binary: choice.path,
		Logger: a.logger,
		Env:    a.envLookup(),
		Clock:  a.bwClock,
	}

	item, err := client.Read(ctx, creds, itemName)
	if err != nil {
		return bitwarden.Item{}, fmt.Errorf("read the vault: %w", err)
	}

	return item, nil
}

// resolveBitwarden picks the Bitwarden CLI a vault command runs. A bw the host
// already has wins when it is at least the pinned version: it is the one the
// operator maintains, and CairnOS has no business replacing or downgrading it.
// Anything else falls back to the copy CairnOS installs and verifies itself.
func (a *App) resolveBitwarden(ctx context.Context, home string) (bwChoice, error) {
	if _, err := a.bwPin(runtime.GOOS); err != nil {
		return unpinnedBitwarden(ctx, a.envLookup(), err)
	}

	choice, usable := a.hostBitwarden(ctx)
	if usable {
		return choice, nil
	}

	return a.pinnedBitwarden(ctx, home, choice.reason)
}

// hostBitwarden reads the version of the bw the host already has and decides
// whether CairnOS can use it. The three false cases carry the reason the pinned
// copy is logged under: no binary, a version that does not parse, and a version
// behind the pin.
func (a *App) hostBitwarden(ctx context.Context) (bwChoice, bool) {
	binary, err := lookBitwardenBinary(a.envLookup())
	if err != nil {
		return bwChoice{reason: bwReasonNoHost}, false
	}

	version, err := bwinstall.ProbeVersion(ctx, binary, 0)
	if err != nil {
		a.logger.Debug("the bw on the host did not answer --version", "path", binary, "error", err)

		return bwChoice{reason: bwReasonHostUnread}, false
	}

	order, ordered := compareBWVersion(version, bwVersion)
	if !ordered {
		a.logger.Debug("the bw on the host reported a version that does not compare",
			"path", binary, "version", version)

		return bwChoice{reason: bwReasonHostUnread}, false
	}

	if order < 0 {
		return bwChoice{reason: bwReasonHostOlder}, false
	}

	return bwChoice{path: binary, version: version, reason: bwReasonHostCurrent}, true
}

// pinnedBitwarden returns the copy CairnOS keeps in its own directory,
// installing it when it is missing and fetching it again when the file there
// does not hash to the pin. Nothing here reads or writes anything on PATH.
func (a *App) pinnedBitwarden(ctx context.Context, home, reason string) (bwChoice, error) {
	pin, err := a.bwPin(runtime.GOOS)
	if err != nil {
		return bwChoice{}, err
	}

	dest := bwPinnedPath(home)

	dir := filepath.Dir(dest)
	if prepErr := prepareToolsDir(dir); prepErr != nil {
		return bwChoice{}, prepErr
	}

	if matchesPin(dest, pin.binarySum) {
		return bwChoice{path: dest, version: bwVersion, reason: reason, pinned: true}, nil
	}

	result, err := a.installBitwarden(ctx, dir, true)
	if err != nil {
		return bwChoice{}, err
	}

	return bwChoice{path: result.Path, version: result.Version, reason: reason, pinned: true}, nil
}

// matchesPin reports whether the file CairnOS installed is still the pinned
// release. It hashes the file rather than asking the binary, because one that
// prints the right version is no evidence of what it does with a master
// password.
func matchesPin(dest, want string) bool {
	digest, err := bwinstall.FileDigest(dest)
	if err != nil {
		return false
	}

	return strings.EqualFold(digest, want)
}

// unpinnedBitwarden accepts the operator's own bw on a platform this build pins
// nothing for. There is no digest and no version to compare against there, and
// refusing a working command over it would leave nothing to get past.
func unpinnedBitwarden(ctx context.Context, lookup environ.Lookup, cause error) (bwChoice, error) {
	if !errors.Is(cause, errNoPinnedBitwarden) {
		return bwChoice{}, cause
	}

	binary, err := lookBitwardenBinary(lookup)
	if err != nil {
		return bwChoice{}, err
	}

	version, err := bwinstall.ProbeVersion(ctx, binary, 0)
	if err != nil {
		version = unknownVersion
	}

	return bwChoice{path: binary, version: version, reason: bwReasonUnpinned}, nil
}

// prepareToolsDir creates the directory and keeps it to its owner. MkdirAll
// leaves an existing directory's mode alone, so the chmod is what makes the
// guarantee hold on a home created before this directory existed.
func prepareToolsDir(dir string) error {
	if err := os.MkdirAll(dir, bwToolsDirPerm); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	if err := os.Chmod(dir, bwToolsDirPerm); err != nil {
		return fmt.Errorf("set the mode of %s: %w", dir, err)
	}

	return nil
}

// compareBWVersion orders two dotted numeric versions and reports whether they
// could be ordered at all. bw publishes calendar versions like 2026.8.0, so
// each component compares as a number; anything with a suffix or a non-numeric
// component is unreadable, which is a case that falls back to the pin.
func compareBWVersion(left, right string) (int, bool) {
	first, numeric := splitVersion(left)
	if !numeric {
		return 0, false
	}

	second, numeric := splitVersion(right)
	if !numeric {
		return 0, false
	}

	for index := range max(len(first), len(second)) {
		if order := cmp.Compare(componentAt(first, index), componentAt(second, index)); order != 0 {
			return order, true
		}
	}

	return 0, true
}

// splitVersion parses the dot-separated components, refusing an empty or
// non-numeric one so a pre-release or build suffix never compares as a number.
func splitVersion(version string) ([]int, bool) {
	fields := strings.Split(strings.TrimSpace(version), ".")
	components := make([]int, 0, len(fields))

	for _, field := range fields {
		value, err := strconv.Atoi(field)
		if err != nil || value < 0 {
			return nil, false
		}

		components = append(components, value)
	}

	return components, true
}

// componentAt reads one component, treating a missing trailing one as zero so
// 2026.9 and 2026.9.0 compare equal.
func componentAt(components []int, index int) int {
	if index < len(components) {
		return components[index]
	}

	return 0
}
