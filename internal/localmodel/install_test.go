package localmodel_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/localmodel"
	"github.com/chadit/CairnOS/internal/nettest"
)

// fixtureRuntimeLib is the shared library the fixture archive ships beside its
// server, standing in for the ggml backends a real release carries.
const fixtureRuntimeLib = "libggml-base.so"

// installHarness is one install pass wired to fixture servers: a synthetic
// release archive and a synthetic GGUF, both digest-pinned the way the real
// registry pins the real ones.
type installHarness struct {
	opts    localmodel.InstallOptions
	hits    map[string]int
	layout  localmodel.Layout
	archive []byte

	// mu guards archive, which a test swaps between passes to stand in for a
	// moved pin while the fixture server is still able to serve it.
	mu sync.Mutex
}

// serveRuntime swaps in a new release archive and repoints the pin at it, the
// fixture equivalent of upstream cutting a build the pins now name.
func (h *installHarness) serveRuntime(t *testing.T, body string) {
	t.Helper()

	archive := tarGzFixture(t, []fixtureEntry{
		{Name: localmodel.HostServerBinary(), Content: body},
		{Name: fixtureRuntimeLib, Content: "library"},
	})

	h.mu.Lock()
	h.archive = archive
	h.mu.Unlock()

	build := *h.opts.Build
	build.SHA256 = digestOf(archive)
	h.opts.Build = &build
}

// serverBody is the content of the installed llama-server, which the fixtures
// vary so a test can say which build is on disk.
func (h *installHarness) serverBody(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(h.layout.ServerPath())
	if err != nil {
		t.Fatalf("read installed server: %v", err)
	}

	return string(raw)
}

// TestInstallLaysOutRuntimeAndWeights proves a first pass installs the server,
// the libraries it needs, the weights, and a manifest describing all of it.
func TestInstallLaysOutRuntimeAndWeights(t *testing.T) {
	t.Parallel()

	harness := newInstallHarness(t)

	result, err := localmodel.Install(t.Context(), &harness.opts)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	if !result.RuntimeInstalled || !result.ModelInstalled {
		t.Errorf("result = %+v, want both components installed", result)
	}

	if _, statErr := os.Stat(harness.layout.ServerPath()); statErr != nil {
		t.Errorf("server missing: %v", statErr)
	}

	if _, statErr := os.Stat(filepath.Join(harness.layout.RuntimeDir(), fixtureRuntimeLib)); statErr != nil {
		t.Errorf("library missing: %v", statErr)
	}

	if _, statErr := os.Stat(result.ModelPath); statErr != nil {
		t.Errorf("weights missing: %v", statErr)
	}

	manifest, found, err := localmodel.LoadManifest(harness.layout.ManifestPath())
	if err != nil || !found {
		t.Fatalf("manifest: found=%t err=%v", found, err)
	}

	if manifest.Model.Name != harness.opts.Model.Name {
		t.Errorf("manifest model = %q, want %q", manifest.Model.Name, harness.opts.Model.Name)
	}

	archive := filepath.Join(harness.layout.Root(), harness.opts.Build.Asset)
	if _, err := os.Stat(archive); err == nil {
		t.Errorf("%s survived extraction as a second copy of the runtime", archive)
	}
}

// TestInstallSecondPassDownloadsNothing proves a re-run against unchanged pins
// leaves both components alone and reaches the network for neither. The model
// short-circuit is the point: without it every re-run would re-read gigabytes
// to recompute a digest that cannot have changed.
func TestInstallSecondPassDownloadsNothing(t *testing.T) {
	t.Parallel()

	harness := newInstallHarness(t)

	if _, err := localmodel.Install(t.Context(), &harness.opts); err != nil {
		t.Fatalf("first install: %v", err)
	}

	if len(harness.hits) == 0 {
		t.Fatal("first install fetched nothing")
	}

	harness.mu.Lock()
	clear(harness.hits)
	harness.mu.Unlock()

	result, err := localmodel.Install(t.Context(), &harness.opts)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}

	if result.RuntimeInstalled || result.ModelInstalled {
		t.Errorf("result = %+v, want nothing re-installed", result)
	}

	if len(harness.hits) != 0 {
		t.Errorf("second install made %d requests, want 0", len(harness.hits))
	}
}

// TestInstallForceRefetchesBothComponents proves --force does what it says
// even when the pins and the disk already agree.
func TestInstallForceRefetchesBothComponents(t *testing.T) {
	t.Parallel()

	harness := newInstallHarness(t)

	if _, err := localmodel.Install(t.Context(), &harness.opts); err != nil {
		t.Fatalf("first install: %v", err)
	}

	harness.mu.Lock()
	clear(harness.hits)
	harness.mu.Unlock()

	harness.opts.Fetch.Force = true

	result, err := localmodel.Install(t.Context(), &harness.opts)
	if err != nil {
		t.Fatalf("forced install: %v", err)
	}

	if !result.RuntimeInstalled || !result.ModelInstalled {
		t.Errorf("result = %+v, want both re-installed", result)
	}

	if len(harness.hits) == 0 {
		t.Error("forced install made no requests")
	}
}

// TestInstallKeepsPreviousRuntimeForRollback proves a replacement sets the old
// runtime aside instead of deleting it, which is what lets an upgrade whose
// server never answers put the working build back.
func TestInstallKeepsPreviousRuntimeForRollback(t *testing.T) {
	t.Parallel()

	harness := newInstallHarness(t)

	if _, err := localmodel.Install(t.Context(), &harness.opts); err != nil {
		t.Fatalf("first install: %v", err)
	}

	marker := filepath.Join(harness.layout.RuntimeDir(), "marker.so")
	if err := os.WriteFile(marker, []byte("old"), 0o600); err != nil {
		t.Fatalf("mark the old runtime: %v", err)
	}

	harness.opts.Fetch.Force = true

	result, err := localmodel.Install(t.Context(), &harness.opts)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}

	if !result.RolledOver {
		t.Fatal("replacing a runtime did not report a rollover")
	}

	if _, err := os.Stat(filepath.Join(harness.layout.PreviousDir(), "marker.so")); err != nil {
		t.Errorf("the displaced runtime was not kept: %v", err)
	}

	if err := localmodel.Rollback(harness.layout, &result); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if _, err := os.Stat(marker); err != nil {
		t.Errorf("rollback did not restore the previous runtime: %v", err)
	}
}

// TestCommitRuntimeDiscardsThePreviousBuild proves a successful upgrade frees
// the disk the old runtime held.
func TestCommitRuntimeDiscardsThePreviousBuild(t *testing.T) {
	t.Parallel()

	harness := newInstallHarness(t)

	if _, err := localmodel.Install(t.Context(), &harness.opts); err != nil {
		t.Fatalf("first install: %v", err)
	}

	harness.opts.Fetch.Force = true

	if _, err := localmodel.Install(t.Context(), &harness.opts); err != nil {
		t.Fatalf("second install: %v", err)
	}

	if err := harness.layout.CommitRuntime(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if _, err := os.Stat(harness.layout.PreviousDir()); err == nil {
		t.Error("commit left the previous runtime on disk")
	}
}

// TestPurgeRemovesTheWholeManagedTree proves uninstall --purge reclaims the
// download rather than only the service.
func TestPurgeRemovesTheWholeManagedTree(t *testing.T) {
	t.Parallel()

	harness := newInstallHarness(t)

	if _, err := localmodel.Install(t.Context(), &harness.opts); err != nil {
		t.Fatalf("install: %v", err)
	}

	if err := harness.layout.Purge(); err != nil {
		t.Fatalf("purge: %v", err)
	}

	if _, err := os.Stat(harness.layout.Root()); err == nil {
		t.Error("purge left the model tree behind")
	}
}

// TestFailedUpgradeRestoresBothTheBinaryAndTheRecord walks the upgrade path the
// way the command does: install, re-install against a moved pin, fail the
// health probe, roll back. Restoring the binary alone would leave status
// reporting a build that never started, and the next pass short-circuiting on
// a manifest entry that no longer describes what is on disk.
func TestFailedUpgradeRestoresBothTheBinaryAndTheRecord(t *testing.T) {
	t.Parallel()

	harness := newInstallHarness(t)
	harness.serveRuntime(t, "good build")

	if _, err := localmodel.Install(t.Context(), &harness.opts); err != nil {
		t.Fatalf("first install: %v", err)
	}

	installed, _, err := localmodel.LoadManifest(harness.layout.ManifestPath())
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	harness.serveRuntime(t, "broken build")

	result, err := localmodel.Install(t.Context(), &harness.opts)
	if err != nil {
		t.Fatalf("upgrade install: %v", err)
	}

	if !result.RolledOver {
		t.Fatal("the upgrade did not displace the previous runtime")
	}

	if harness.serverBody(t) != "broken build" {
		t.Fatal("the upgrade did not install the new build")
	}

	// The probe the command runs, against a port nothing is listening on.
	if probeErr := localmodel.WaitHealthy(t.Context(), nil, "http://127.0.0.1:1/v1", 50*time.Millisecond); probeErr == nil {
		t.Fatal("the health probe passed against a dead address")
	}

	if rollbackErr := localmodel.Rollback(harness.layout, &result); rollbackErr != nil {
		t.Fatalf("rollback: %v", rollbackErr)
	}

	if got := harness.serverBody(t); got != "good build" {
		t.Errorf("installed server = %q, want the previous build back", got)
	}

	restored, found, err := localmodel.LoadManifest(harness.layout.ManifestPath())
	if err != nil || !found {
		t.Fatalf("read manifest after rollback: found=%t err=%v", found, err)
	}

	if restored.Runtime.SHA256 != installed.Runtime.SHA256 {
		t.Errorf("manifest records %q, want the restored build %q",
			restored.Runtime.SHA256, installed.Runtime.SHA256)
	}
}

// TestPassAfterAFailedUpgradeStillReinstalls proves the rollback left the
// record and the disk agreeing: with the pin still pointing at the build that
// failed, the next pass downloads it again instead of short-circuiting on a
// manifest entry that says it is already there.
func TestPassAfterAFailedUpgradeStillReinstalls(t *testing.T) {
	t.Parallel()

	harness := newInstallHarness(t)
	harness.serveRuntime(t, "good build")

	if _, err := localmodel.Install(t.Context(), &harness.opts); err != nil {
		t.Fatalf("first install: %v", err)
	}

	harness.serveRuntime(t, "broken build")

	result, err := localmodel.Install(t.Context(), &harness.opts)
	if err != nil {
		t.Fatalf("upgrade install: %v", err)
	}

	if rollbackErr := localmodel.Rollback(harness.layout, &result); rollbackErr != nil {
		t.Fatalf("rollback: %v", rollbackErr)
	}

	retry, err := localmodel.Install(t.Context(), &harness.opts)
	if err != nil {
		t.Fatalf("retry install: %v", err)
	}

	if !retry.RuntimeInstalled {
		t.Error("the retry short-circuited on a manifest entry the disk does not match")
	}

	if got := harness.serverBody(t); got != "broken build" {
		t.Errorf("installed server = %q, want the pinned build re-fetched", got)
	}
}

// TestRollbackOfAFirstInstallLeavesNoRecord proves a failed first setup does
// not leave a manifest claiming an install that is not there.
func TestRollbackOfAFirstInstallLeavesNoRecord(t *testing.T) {
	t.Parallel()

	harness := newInstallHarness(t)

	result, err := localmodel.Install(t.Context(), &harness.opts)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	if rollbackErr := localmodel.Rollback(harness.layout, &result); rollbackErr != nil {
		t.Fatalf("rollback: %v", rollbackErr)
	}

	_, found, err := localmodel.LoadManifest(harness.layout.ManifestPath())
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	if found {
		t.Error("a rolled-back first install left an install record behind")
	}
}

// newInstallHarness stands up fixture download servers and an localmodel.InstallOptions
// pointed at them, rooted in a temp home.
func newInstallHarness(t *testing.T) *installHarness {
	t.Helper()

	archive := tarGzFixture(t, []fixtureEntry{
		{Name: localmodel.HostServerBinary(), Content: "server binary"},
		{Name: fixtureRuntimeLib, Content: "library"},
		{Name: fixtureOtherTool, Content: "another tool"},
	})
	weights := []byte("gguf weights fixture")

	harness := &installHarness{hits: map[string]int{}, archive: archive}

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		harness.mu.Lock()
		defer harness.mu.Unlock()

		harness.hits[r.URL.Path]++

		if strings.HasSuffix(r.URL.Path, ".tar.gz") {
			_, _ = w.Write(harness.archive)

			return
		}

		_, _ = w.Write(weights)
	}))

	build := localmodel.Build{
		GOOS: localmodel.OSLinux, GOARCH: localmodel.ArchAMD64,
		Asset:  "llama-fixture-bin.tar.gz",
		SHA256: digestOf(archive),
		Format: localmodel.FormatTarGz, Prefix: localmodel.ArchivePrefix, Binary: localmodel.HostServerBinary(),
	}

	harness.layout = localmodel.NewLayout(t.TempDir())
	harness.opts = localmodel.InstallOptions{
		Layout: harness.layout,
		Model: localmodel.Model{
			Name: "fixture-model", Repo: "org/repo", File: "fixture.gguf",
			SHA256: digestOf(weights), Revision: "abc", Bytes: int64(len(weights)),
		},
		Fetch:        localmodel.FetchOptions{Client: server.Client()},
		DownloadBase: server.URL,
		ResolveBase:  server.URL,
		Build:        &build,
		Now:          func() time.Time { return time.Unix(0, 0).UTC() },
	}

	return harness
}
