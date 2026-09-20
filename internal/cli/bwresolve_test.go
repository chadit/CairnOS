package cli_test

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
)

// toolsDirPerm is the mode the directory CairnOS keeps its own bw in has to
// carry. Anyone who can write it can swap the binary that opens the vault.
const toolsDirPerm = 0o700

// imposterPerm makes the staged mismatch runnable, so a case that claims it was
// never executed is claiming something that could have happened.
const imposterPerm = 0o700

// stageHostBW stages a bw on PATH reporting the version the case names, which
// is the value the resolver compares against the pin.
func stageHostBW(t *testing.T, fields map[string]string, version string) string {
	t.Helper()

	dir := t.TempDir()
	stageFakeBWAt(t, dir, fields, bwFakeSettings{Version: version})

	return dir
}

// pinnedCopyDir is where the resolver keeps the release this build pins: inside
// the CairnOS home, under a directory named for the version.
func pinnedCopyDir(home string) string {
	return filepath.Join(home, "tools", "bw", cli.PinnedBitwardenVersion())
}

// stagePinnedBW puts a working copy where the resolver expects the pinned one,
// so a case about the fallback does not have to download anything. runCLI pins
// the release to this test binary's digest, which is what the staged symlink
// hashes to.
func stagePinnedBW(t *testing.T, home string, fields map[string]string) string {
	t.Helper()

	dir := pinnedCopyDir(home)
	stageFakeBWAt(t, dir, fields, bwFakeSettings{Version: cli.PinnedBitwardenVersion()})

	return filepath.Join(dir, bwFakeName)
}

// stageImposterBW puts a runnable file that is not the pinned release where the
// pinned copy goes, and has it touch a marker so a case can tell whether it ran.
func stageImposterBW(t *testing.T, home, marker string) {
	t.Helper()

	dir := pinnedCopyDir(home)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("stage %s: %v", dir, err)
	}

	body := "#!/bin/sh\ntouch " + marker + "\n"
	if err := os.WriteFile(filepath.Join(dir, bwFakeName), []byte(body), imposterPerm); err != nil {
		t.Fatalf("stage the imposter bw: %v", err)
	}
}

// emptyPath makes PATH a directory holding nothing, so a case about a host with
// no bw is not answered by one this machine happens to have.
func emptyPath(t *testing.T) {
	t.Helper()

	t.Setenv("PATH", t.TempDir())
}

// dirListing returns the names in a directory, which is how a case shows that
// nothing was added beside the operator's own binary.
func dirListing(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	slices.Sort(names)

	return names
}

// linkState describes a symlink without reading the binary behind it: what it
// points at, its mode, and when it was last written. A CairnOS that replaced
// the operator's bw would have to change one of the three.
func linkState(t *testing.T, path string) string {
	t.Helper()

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	target, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("read the link at %s: %v", path, err)
	}

	return target + " " + info.Mode().String() + " " + info.ModTime().String()
}

// TestSecretsPullRunsTheHostCLIWhenItIsNotBehindThePin is the case a host with
// its own package-managed bw is in. Refusing that binary or replacing it with an
// older pinned build is what this resolution order exists to stop.
func TestSecretsPullRunsTheHostCLIWhenItIsNotBehindThePin(t *testing.T) {
	binDir := stageHostBW(t, bothFields(), hostNewerVersion)
	home := pullConfig(t)

	t.Setenv("PATH", pathWith(binDir))

	code, stdout, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, home)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stderr, filepath.Join(binDir, bwFakeName)) {
		t.Fatalf("the log does not name the host CLI as the one used:\n%s", stderr)
	}

	if _, err := os.Stat(pinnedCopyDir(home)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a host CLI that was used still made CairnOS install its own (stat error %v)", err)
	}

	if !strings.Contains(stdout, pullVarOne) {
		t.Fatalf("the pull did not render the vault fields:\n%s", stdout)
	}
}

// TestBitwardenVersionDecidesWhichCLIRuns is the comparison rule end to end.
// Components compare as numbers, a missing trailing one counts as zero, and
// anything that does not parse is treated as no answer at all.
func TestBitwardenVersionDecidesWhichCLIRuns(t *testing.T) {
	cases := []struct {
		name     string
		version  string
		wantHost bool
	}{
		{name: "past the pin", version: "9999.12.31", wantHost: true},
		{name: "the pinned version itself", version: cli.PinnedBitwardenVersion(), wantHost: true},
		{name: "fewer components but still past the pin", version: "9999.12", wantHost: true},
		{name: "far behind the pin", version: "1970.1.1", wantHost: false},
		{name: "a pre-release suffix", version: "9999.12.31-beta", wantHost: false},
		{name: "not a version at all", version: "unreleased", wantHost: false},
		{name: "no answer", version: "", wantHost: false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			binDir := stageHostBW(t, bothFields(), testCase.version)
			home := pullConfig(t)
			pinned := stagePinnedBW(t, home, bothFields())

			t.Setenv("PATH", pathWith(binDir))

			code, _, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, home)
			if code != cli.ExitOK {
				t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
			}

			ranHost := strings.Contains(stderr, filepath.Join(binDir, bwFakeName))
			if ranHost != testCase.wantHost {
				t.Fatalf("host CLI used = %t, want %t (pinned copy %s)\n%s",
					ranHost, testCase.wantHost, pinned, stderr)
			}
		})
	}
}

// TestSecretsPullLeavesAnOlderHostCLIAlone is the failure this resolution order
// was written for. The operator's bw stays exactly where and what it was, and
// the older version is worked around rather than overwritten.
func TestSecretsPullLeavesAnOlderHostCLIAlone(t *testing.T) {
	binDir := stageHostBW(t, bothFields(), "1970.1.1")
	home := pullConfig(t)
	pinned := stagePinnedBW(t, home, bothFields())

	hostBinary := filepath.Join(binDir, bwFakeName)
	before := linkState(t, hostBinary)
	staged := dirListing(t, binDir)

	t.Setenv("PATH", pathWith(binDir))

	code, stdout, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, home)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stderr, pinned) {
		t.Fatalf("the log does not name the pinned copy as the one used:\n%s", stderr)
	}

	if after := linkState(t, hostBinary); after != before {
		t.Fatalf("the host bw changed: %s, was %s", after, before)
	}

	if after := dirListing(t, binDir); !slices.Equal(after, staged) {
		t.Fatalf("the host bw directory holds %v, staged %v", after, staged)
	}

	if !strings.Contains(stdout, pullVarOne) {
		t.Fatalf("the pull did not render the vault fields:\n%s", stdout)
	}
}

// TestSecretsPullInstallsThePinnedCopyWhenTheHostHasNone covers a fresh box.
// The release this build pins is unreachable in a test, so the failure it
// arrives at is the proof that an install was started at all.
//
//nolint:paralleltest // emptyPath rewrites PATH for the whole process
func TestSecretsPullInstallsThePinnedCopyWhenTheHostHasNone(t *testing.T) {
	home := pullConfig(t)

	emptyPath(t)

	code, _, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, home)
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d, want the unreachable download to fail\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stderr, unreachableAsset) {
		t.Fatalf("the failure does not come from the install this should have started:\n%s", stderr)
	}

	info, err := os.Stat(pinnedCopyDir(home))
	if err != nil {
		t.Fatalf("the resolver made no directory of its own: %v", err)
	}

	if info.Mode().Perm() != toolsDirPerm {
		t.Fatalf("the directory is mode %04o, want %04o", info.Mode().Perm(), toolsDirPerm)
	}
}

// TestSecretsPullKeepsThePinnedCopyOffPATH is what makes the pinned copy
// CairnOS's own rather than a second bw the operator now has to know about.
// The directory is staged wide open, so the mode check is about what the
// command does and not about what the fixture made.
func TestSecretsPullKeepsThePinnedCopyOffPATH(t *testing.T) {
	binDir := stageHostBW(t, bothFields(), "1970.1.1")
	home := pullConfig(t)
	pinned := stagePinnedBW(t, home, bothFields())

	dir := pinnedCopyDir(home)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("loosen the staged directory: %v", err)
	}

	t.Setenv("PATH", pathWith(binDir))

	if code, _, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, home); code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat the pinned copy's directory: %v", err)
	}

	if info.Mode().Perm() != toolsDirPerm {
		t.Fatalf("the directory is mode %04o, want %04o", info.Mode().Perm(), toolsDirPerm)
	}

	if slices.Contains(filepath.SplitList(os.Getenv("PATH")), dir) {
		t.Fatalf("%s ended up on PATH", dir)
	}

	if found, lookErr := exec.LookPath(bwFakeName); lookErr == nil && found == pinned {
		t.Fatalf("the pinned copy answers to the bare name %q", bwFakeName)
	}
}

// TestSecretsPullRefetchesAPinnedCopyThatDoesNotMatch covers the file in
// CairnOS's own directory going wrong. Running it would mean handing the vault
// credentials to whatever is there, so it is replaced rather than trusted.
//
//nolint:paralleltest // emptyPath rewrites PATH for the whole process
func TestSecretsPullRefetchesAPinnedCopyThatDoesNotMatch(t *testing.T) {
	home := pullConfig(t)
	marker := filepath.Join(home, "the-imposter-ran")

	stageImposterBW(t, home, marker)
	emptyPath(t)

	code, _, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, home)
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d, want the unreachable download to fail\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stderr, unreachableAsset) {
		t.Fatalf("a copy that does not match the pin was not fetched again:\n%s", stderr)
	}

	if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a copy that does not match the pin was executed (stat error %v)", err)
	}
}

// TestSecretsPullRetriesThroughThePinnedCopyWhenTheHostCLICannotRun covers a
// host bw that is new enough and still cannot do the job. The first call proves
// it, and the pinned copy finishes the pull rather than failing the deploy.
func TestSecretsPullRetriesThroughThePinnedCopyWhenTheHostCLICannotRun(t *testing.T) {
	binDir := t.TempDir()
	stageFakeBWAt(t, binDir, bothFields(), bwFakeSettings{Version: hostNewerVersion, FailStatus: true})

	home := pullConfig(t)
	pinned := stagePinnedBW(t, home, bothFields())

	t.Setenv("PATH", pathWith(binDir))

	code, stdout, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, home)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want the retry to carry the pull\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stderr, filepath.Join(binDir, bwFakeName)) {
		t.Fatalf("the host CLI was never tried:\n%s", stderr)
	}

	if !strings.Contains(stderr, pinned) {
		t.Fatalf("the pinned copy did not pick the failed call up:\n%s", stderr)
	}

	if !strings.Contains(stdout, pullVarOne) {
		t.Fatalf("the pull did not render the vault fields:\n%s", stdout)
	}
}

// TestSecretsPullDoesNotRetryWhenTheVaultRefusesTheCredentials is the other half
// of that rule. A master password the vault rejects reads the same from any
// build, so a second attempt would cost a deploy another daemon for the same
// answer. The pinned copy here would succeed, so the failure is the assertion.
func TestSecretsPullDoesNotRetryWhenTheVaultRefusesTheCredentials(t *testing.T) {
	binDir := t.TempDir()
	stageFakeBWAt(t, binDir, bothFields(), bwFakeSettings{Version: hostNewerVersion, RefuseUnlock: true})

	home := pullConfig(t)
	pinned := stagePinnedBW(t, home, bothFields())

	t.Setenv("PATH", pathWith(binDir))

	code, _, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, home)
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d, want the refused unlock to fail\nstderr: %s", code, stderr)
	}

	if strings.Contains(stderr, pinned) {
		t.Fatalf("a refused unlock fell back to the pinned copy:\n%s", stderr)
	}
}
