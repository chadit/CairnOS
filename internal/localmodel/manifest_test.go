package localmodel_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/localmodel"
)

// TestManifestRoundTripsAndComparesToPins proves the install record survives a
// write and read, and that comparing it against the pins is what tells an
// up-to-date install from a stale one.
func TestManifestRoundTripsAndComparesToPins(t *testing.T) {
	t.Parallel()

	build, err := localmodel.HostBuild()
	if err != nil {
		t.Skipf("no pinned build for this platform: %v", err)
	}

	model, err := localmodel.Lookup(localmodel.DefaultModel)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}

	path := filepath.Join(t.TempDir(), localmodel.ManifestFileName)
	written := localmodel.Manifest{Runtime: localmodel.RuntimeRecord(&build), Model: localmodel.ModelRecord(&model)}

	if saveManifestErr := localmodel.SaveManifest(path, &written, time.Unix(1, 0).UTC()); saveManifestErr != nil {
		t.Fatalf("save: %v", saveManifestErr)
	}

	loaded, found, err := localmodel.LoadManifest(path)
	if err != nil || !found {
		t.Fatalf("load: found=%t err=%v", found, err)
	}

	if !loaded.RuntimeMatches(&build) {
		t.Error("a manifest written from the pins does not match them")
	}

	if !loaded.ModelMatches(&model) {
		t.Error("a manifest written from the catalog does not match it")
	}
}

// TestMissingManifestReadsAsNothingInstalled proves a fresh home is reported
// as empty rather than as an error, so status works before setup runs.
func TestMissingManifestReadsAsNothingInstalled(t *testing.T) {
	t.Parallel()

	manifest, found, err := localmodel.LoadManifest(filepath.Join(t.TempDir(), localmodel.ManifestFileName))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if found {
		t.Error("an absent manifest reported as found")
	}

	if manifest.Runtime.Version != "" {
		t.Errorf("manifest = %+v, want the zero value", manifest)
	}
}

// TestModelMatchesIgnoresRevisionAndTracksDigest proves the digest decides:
// the same weights under a newer commit still match, and different weights
// under the pinned commit do not.
func TestModelMatchesIgnoresRevisionAndTracksDigest(t *testing.T) {
	t.Parallel()

	const (
		installedDigest   = "aaa"
		installedRevision = "r0"
	)

	model := localmodel.Model{Name: "m", SHA256: installedDigest, Revision: "r1"}
	manifest := localmodel.Manifest{Model: localmodel.InstalledModel{Name: "m", SHA256: installedDigest, Revision: installedRevision}}

	if !manifest.ModelMatches(&model) {
		t.Error("same digest under a different revision should match")
	}

	changed := localmodel.Model{Name: "m", SHA256: "bbb", Revision: installedRevision}
	if manifest.ModelMatches(&changed) {
		t.Error("a different digest should not match")
	}
}
