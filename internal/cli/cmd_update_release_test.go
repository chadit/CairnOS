package cli_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
)

const (
	updateStagedTag  = "v9.9.9"
	updateOldContent = "the build this host was running\n"
)

// The step names recordingSteps writes down, which is what the order
// assertions compare against.
const (
	stepBuild   = "build"
	stepInstall = "install"
	stepPull    = "pull"
	stepRestart = "restart"
	stepLink    = "link"
)

// stagedRelease is what the fake API publishes: the tag and the commit it
// points at, the asset bytes, and the checksum manifest a case may deliberately
// get wrong.
type stagedRelease struct {
	tag        string
	revision   string
	assetName  string
	sums       string
	asset      []byte
	latestCode int
	annotated  bool

	// noChecksums drops the checksums entry from the assets list, which is the
	// one release shape the update refuses rather than falls back from.
	noChecksums bool
}

// TestUpdateInstallsARelease covers the whole release path: the asset is
// verified and written over the running binary, and the pull still runs before
// the restart so the service comes back on both the new build and new values.
func TestUpdateInstallsARelease(t *testing.T) {
	t.Parallel()
	requireGoBuild(t)

	asset, revision := buildStampedBinary(t)
	server := startReleaseServer(t, defaultStagedRelease(asset, revision))

	target := filepath.Join(t.TempDir(), updateLinkName)
	writeFile(t, target, updateOldContent)

	var order []string

	steps := recordingSteps(t, &order, target)

	code, err := cli.New(&bytes.Buffer{}, &bytes.Buffer{}).DriveUpdate(t.Context(),
		cli.UpdateRun{APIBase: server.URL, Executable: target, Home: t.TempDir()}, steps)
	if err != nil {
		t.Fatalf("DriveUpdate() error = %v", err)
	}

	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want %d", code, cli.ExitOK)
	}

	want := []string{stepInstall, stepPull, stepRestart, stepLink}
	if !slices.Equal(order, want) {
		t.Fatalf("step order = %v, want %v", order, want)
	}

	installed, err := os.ReadFile(target) // #nosec G304 -- a path this test built
	if err != nil {
		t.Fatalf("read back %s: %v", target, err)
	}

	if !bytes.Equal(installed, asset) {
		t.Errorf("the installed binary is %d bytes, the asset is %d", len(installed), len(asset))
	}
}

// TestUpdateRefusesAMismatchedAsset pins the one guarantee the download rests
// on: a body that does not hash to the published digest never reaches the
// binary, and nothing downstream of the install runs.
func TestUpdateRefusesAMismatchedAsset(t *testing.T) {
	t.Parallel()
	requireGoBuild(t)

	asset, revision := buildStampedBinary(t)

	release := defaultStagedRelease(asset, revision)
	release.sums = fmt.Sprintf("%s  %s\n", strings.Repeat("0", 64), release.assetName)

	server := startReleaseServer(t, release)

	target := filepath.Join(t.TempDir(), updateLinkName)
	writeFile(t, target, updateOldContent)

	var order []string

	code, err := cli.New(&bytes.Buffer{}, &bytes.Buffer{}).DriveUpdate(t.Context(),
		cli.UpdateRun{APIBase: server.URL, Executable: target, Home: t.TempDir()},
		recordingSteps(t, &order, target))
	if err == nil {
		t.Fatalf("DriveUpdate() accepted an asset with the wrong digest, exit = %d", code)
	}

	if len(order) != 0 {
		t.Errorf("steps ran after the digest failed: %v", order)
	}

	content, err := os.ReadFile(target) // #nosec G304 -- a path this test built
	if err != nil {
		t.Fatalf("read back %s: %v", target, err)
	}

	if string(content) != updateOldContent {
		t.Errorf("the running binary was replaced: %q", content)
	}
}

// TestUpdateRefusesAReleaseWithoutChecksums covers the release that publishes a
// binary for this platform and nothing to verify it against. Falling back to
// the clone would hide a half-uploaded release, so the update stops instead.
func TestUpdateRefusesAReleaseWithoutChecksums(t *testing.T) {
	t.Parallel()
	requireGit(t)

	release := defaultStagedRelease([]byte("an asset nothing vouches for\n"), strings.Repeat("f", 40))
	release.noChecksums = true

	server := startReleaseServer(t, release)
	work, _ := stageClonePair(t)

	target := filepath.Join(work, "bin", updateLinkName)
	writeFile(t, target, updateOldContent)

	var order []string

	code, err := cli.New(&bytes.Buffer{}, &bytes.Buffer{}).DriveUpdate(t.Context(),
		cli.UpdateRun{APIBase: server.URL, Executable: target, Home: t.TempDir()},
		recordingSteps(t, &order, ""))
	if err == nil {
		t.Fatalf("DriveUpdate() accepted a release with no checksums, exit = %d", code)
	}

	if len(order) != 0 {
		t.Errorf("steps ran for a release with no checksums: %v", order)
	}

	content, err := os.ReadFile(target) // #nosec G304 -- a path this test built
	if err != nil {
		t.Fatalf("read back %s: %v", target, err)
	}

	if string(content) != updateOldContent {
		t.Errorf("the running binary was replaced: %q", content)
	}
}

// TestUpdateFallsBackToTheCheckout covers the two ordinary misses. Neither is a
// failure: a private repository read without a token answers 404, and a release
// cut before this platform was added carries no asset for it.
func TestUpdateFallsBackToTheCheckout(t *testing.T) {
	t.Parallel()
	requireGit(t)

	cases := []struct {
		name    string
		release stagedRelease
	}{
		{
			name:    "the API reports no release",
			release: stagedRelease{tag: updateStagedTag, latestCode: http.StatusNotFound},
		},
		{
			name: "the release carries no asset for this platform",
			release: stagedRelease{
				tag:       updateStagedTag,
				revision:  strings.Repeat("a", 40),
				assetName: "cairnos_plan9_mips",
				asset:     []byte("not this host\n"),
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			server := startReleaseServer(t, withDefaultSums(&testCase.release))
			work, _ := stageClonePair(t)

			var order []string

			code, err := cli.New(&bytes.Buffer{}, &bytes.Buffer{}).DriveUpdate(t.Context(),
				cli.UpdateRun{
					APIBase:    server.URL,
					Executable: filepath.Join(work, "bin", updateLinkName),
					Home:       t.TempDir(),
				},
				recordingSteps(t, &order, ""))
			if err != nil {
				t.Fatalf("DriveUpdate() error = %v", err)
			}

			if code != cli.ExitOK {
				t.Fatalf("exit = %d, want %d", code, cli.ExitOK)
			}

			want := []string{stepBuild, stepPull, stepRestart, stepLink}
			if !slices.Equal(order, want) {
				t.Fatalf("step order = %v, want the source path %v", order, want)
			}
		})
	}
}

// TestUpdateSkipsTheLookup pins the precedence deploy.sh relies on. Each of
// these flags means the checkout in front of you, so a developer's dirty tree
// is never replaced by whatever the last tag published.
func TestUpdateSkipsTheLookup(t *testing.T) {
	t.Parallel()
	requireGit(t)

	asset, revision := buildStampedBinaryOrSkip(t)

	cases := []struct {
		with func(run *cli.UpdateRun, checkout string)
		name string
	}{
		{name: "no-fetch", with: func(run *cli.UpdateRun, _ string) { run.NoFetch = true }},
		{name: "no-release", with: func(run *cli.UpdateRun, _ string) { run.NoRelease = true }},
		{name: "source", with: func(run *cli.UpdateRun, checkout string) { run.Source = checkout }},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			server, calls := startCountingReleaseServer(t, defaultStagedRelease(asset, revision))
			work, _ := stageClonePair(t)

			run := cli.UpdateRun{
				APIBase:    server.URL,
				Executable: filepath.Join(work, "bin", updateLinkName),
				Home:       t.TempDir(),
			}
			testCase.with(&run, work)

			var order []string

			if _, err := cli.New(&bytes.Buffer{}, &bytes.Buffer{}).
				DriveUpdate(t.Context(), run, recordingSteps(t, &order, "")); err != nil {
				t.Fatalf("DriveUpdate() error = %v", err)
			}

			if got := calls.Load(); got != 0 {
				t.Errorf("the release API was called %d time(s)", got)
			}

			if len(order) == 0 || order[0] != stepBuild {
				t.Errorf("step order = %v, want the source path to build first", order)
			}
		})
	}
}

// TestUpdateCheckAgainstARelease reports the tag's commit as the wanted
// revision and exits 3, because no service on the test host runs it. The tag
// here is annotated, so the report also covers dereferencing the tag object.
func TestUpdateCheckAgainstARelease(t *testing.T) {
	t.Parallel()

	revision := strings.Repeat("b", 40)

	release := withDefaultSums(&stagedRelease{
		tag:       updateStagedTag,
		revision:  revision,
		assetName: releaseAssetFor(runtime.GOOS, runtime.GOARCH),
		asset:     []byte("an asset the check never downloads\n"),
		annotated: true,
	})

	server := startReleaseServer(t, release)

	var stdout, stderr bytes.Buffer

	code, err := cli.New(&stdout, &stderr).DriveUpdate(t.Context(),
		cli.UpdateRun{
			APIBase:    server.URL,
			Executable: filepath.Join(t.TempDir(), updateLinkName),
			Home:       t.TempDir(),
			Check:      true,
		}, failingSteps(t))
	if err != nil {
		t.Fatalf("DriveUpdate() error = %v\nstderr: %s", err, stderr.String())
	}

	if code != cli.ExitBehind {
		t.Fatalf("exit = %d, want %d", code, cli.ExitBehind)
	}

	if !strings.Contains(stdout.String(), revision) {
		t.Errorf("stdout does not name the tag's commit:\n%s", stdout.String())
	}
}

// TestUpdateCheckExitCodes pins the contract a scheduled check elsewhere keys
// on, including the match that DriveUpdate cannot reach: nothing on a test host
// runs the revision a staged release names.
func TestUpdateCheckExitCodes(t *testing.T) {
	t.Parallel()

	revision := strings.Repeat("c", 40)

	cases := []struct {
		name    string
		running string
		wanted  string
		code    int
	}{
		{"the service runs the wanted revision", revision, revision, cli.ExitOK},
		{"the service runs something older", strings.Repeat("d", 40), revision, cli.ExitBehind},
		{"nothing is running", "", revision, cli.ExitBehind},
		{"both sides are dirty", revision + "+dirty", revision + "+dirty", cli.ExitBehind},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer

			code := cli.New(&stdout, &bytes.Buffer{}).
				UpdateRevisionReport(testCase.running, testCase.wanted)
			if code != testCase.code {
				t.Fatalf("exit = %d, want %d", code, testCase.code)
			}

			if !strings.Contains(stdout.String(), testCase.wanted) {
				t.Errorf("stdout does not name the wanted revision:\n%s", stdout.String())
			}
		})
	}
}

// releaseAssetFor spells the asset name the release workflow uploads. It is
// written out here rather than read from the command, so a change to either
// side of that contract fails this test.
func releaseAssetFor(goos, goarch string) string {
	name := fmt.Sprintf("cairnos_%s_%s", goos, goarch)
	if goos == "windows" {
		return name + ".exe"
	}

	return name
}

// defaultStagedRelease publishes one asset for this platform with the checksum
// that matches it.
func defaultStagedRelease(asset []byte, revision string) *stagedRelease {
	return withDefaultSums(&stagedRelease{
		tag:       updateStagedTag,
		revision:  revision,
		assetName: releaseAssetFor(runtime.GOOS, runtime.GOARCH),
		asset:     asset,
	})
}

// withDefaultSums fills in the sha256sum manifest for a release that has not
// set one deliberately.
func withDefaultSums(release *stagedRelease) *stagedRelease {
	if release.sums == "" && release.assetName != "" {
		digest := sha256.Sum256(release.asset)
		release.sums = fmt.Sprintf("%s  %s\n", hex.EncodeToString(digest[:]), release.assetName)
	}

	if release.latestCode == 0 {
		release.latestCode = http.StatusOK
	}

	return release
}

// startReleaseServer stands up the fake release API and asset host.
func startReleaseServer(t *testing.T, release *stagedRelease) *httptest.Server {
	t.Helper()

	server, _ := startCountingReleaseServer(t, release)

	return server
}

// startCountingReleaseServer also reports how many requests reached it, which
// is how a case proves the lookup was skipped rather than merely unhelpful.
func startCountingReleaseServer(t *testing.T, release *stagedRelease) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	release = withDefaultSums(release)

	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		serveRelease(writer, request, release)
	}))

	t.Cleanup(server.Close)

	return server, &calls
}

// serveRelease answers the three endpoints the lookup reads plus the download
// the install streams, with the asset URLs pointing back at this server.
func serveRelease(writer http.ResponseWriter, request *http.Request, release *stagedRelease) {
	base := "http://" + request.Host

	switch {
	case strings.HasSuffix(request.URL.Path, "/releases/latest"):
		writeLatest(writer, release, base)
	case strings.Contains(request.URL.Path, "/git/ref/tags/"):
		writeTagRef(writer, release)
	case strings.Contains(request.URL.Path, "/git/tags/"):
		writeJSON(writer, fmt.Sprintf(`{"object":{"type":"commit","sha":%q}}`, release.revision))
	case request.URL.Path == "/dl/"+release.assetName:
		_, _ = writer.Write(release.asset)
	case request.URL.Path == "/dl/checksums.txt":
		_, _ = writer.Write([]byte(release.sums))
	default:
		writer.WriteHeader(http.StatusNotFound)
	}
}

// writeLatest publishes the release payload, or the status a case staged in
// place of it.
func writeLatest(writer http.ResponseWriter, release *stagedRelease, base string) {
	if release.latestCode != http.StatusOK {
		writer.WriteHeader(release.latestCode)

		return
	}

	assets := fmt.Sprintf(`{"name":%q,"browser_download_url":"%s/dl/%s"}`,
		release.assetName, base, release.assetName)

	if !release.noChecksums {
		assets += fmt.Sprintf(`,{"name":"checksums.txt","browser_download_url":"%s/dl/checksums.txt"}`, base)
	}

	writeJSON(writer, fmt.Sprintf(`{"tag_name":%q,"assets":[%s]}`, release.tag, assets))
}

// writeTagRef answers the ref endpoint, pointing at a tag object when the case
// staged an annotated tag and at the commit itself otherwise.
func writeTagRef(writer http.ResponseWriter, release *stagedRelease) {
	kind := "commit"
	if release.annotated {
		kind = "tag"
	}

	writeJSON(writer, fmt.Sprintf(`{"object":{"type":%q,"sha":%q}}`, kind, release.revision))
}

// writeJSON sends one API body with the content type the client expects.
func writeJSON(writer http.ResponseWriter, body string) {
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write([]byte(body))
}

// recordingSteps names every step as it runs. install performs the real
// replacement when target is set, so the order and the installed bytes come
// from the same run.
func recordingSteps(t *testing.T, order *[]string, target string) cli.UpdateSteps {
	t.Helper()

	return cli.UpdateSteps{
		Build: func(context.Context, string) error {
			*order = append(*order, stepBuild)

			return nil
		},
		Install: func(_, staged string) error {
			*order = append(*order, stepInstall)

			if target == "" {
				return nil
			}

			return cli.UpdateInstallBinary(target, staged)
		},
		Pull: func(context.Context, string, string) (bool, error) {
			*order = append(*order, stepPull)

			return false, nil
		},
		Restart: func(context.Context, bool, bool, string) error {
			*order = append(*order, stepRestart)

			return nil
		},
		Link: func(string) error {
			*order = append(*order, stepLink)

			return nil
		},
	}
}

// failingSteps fails the case if any step runs, which is what --check promises.
func failingSteps(t *testing.T) cli.UpdateSteps {
	t.Helper()

	fail := func(name string) {
		t.Helper()
		t.Errorf("--check ran the %s step", name)
	}

	return cli.UpdateSteps{
		Build: func(context.Context, string) error {
			fail(stepBuild)

			return nil
		},
		Install: func(_, _ string) error {
			fail(stepInstall)

			return nil
		},
		Pull: func(context.Context, string, string) (bool, error) {
			fail(stepPull)

			return false, nil
		},
		Restart: func(context.Context, bool, bool, string) error {
			fail(stepRestart)

			return nil
		},
		Link: func(string) error {
			fail(stepLink)

			return nil
		},
	}
}

// requireGoBuild skips a case that needs a real stamped binary, which only the
// go toolchain and git together can produce.
func requireGoBuild(t *testing.T) {
	t.Helper()
	requireGit(t)

	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the go toolchain is not on PATH")
	}
}

// buildStampedBinaryOrSkip is buildStampedBinary for a case that needs an asset
// but never installs it, so a host without the toolchain still runs the case
// with a stand-in.
func buildStampedBinaryOrSkip(t *testing.T) ([]byte, string) {
	t.Helper()

	if _, err := exec.LookPath("go"); err != nil {
		return []byte("no toolchain here\n"), strings.Repeat("e", 40)
	}

	return buildStampedBinary(t)
}

// buildStampedBinary compiles a throwaway main package inside its own clean
// git repository and answers with the binary and the commit cmd/go stamped
// into it. The install refuses an asset whose stamp is missing or dirty, so a
// fixture has to be the real thing.
func buildStampedBinary(t *testing.T) ([]byte, string) {
	t.Helper()

	module := t.TempDir()

	writeFile(t, filepath.Join(module, "go.mod"), "module example.invalid/staged\n\ngo 1.27\n")
	writeFile(t, filepath.Join(module, "main.go"), "package main\n\nfunc main() {}\n")

	gitOut(t, module, "init", "--quiet", "--initial-branch=main", ".")
	gitOut(t, module, "add", ".")
	gitOut(t, module, "commit", "--quiet", "-m", "staged release")

	out := filepath.Join(t.TempDir(), "staged")

	buildEnv := append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull)

	build := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-o", out, ".")
	build.Dir = module
	build.Env = buildEnv

	if output, err := build.CombinedOutput(); err != nil {
		t.Skipf("go build is unavailable here: %v\n%s", err, output)
	}

	asset, err := os.ReadFile(out) // #nosec G304 -- a path this test built
	if err != nil {
		t.Fatalf("read the staged binary: %v", err)
	}

	return asset, gitOut(t, module, "rev-parse", "HEAD")
}
