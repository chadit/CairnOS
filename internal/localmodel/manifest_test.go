package localmodel

import (
	"path/filepath"
	"testing"
	"time"
)

// TestManifestRoundTripsAndComparesToPins proves the install record survives a
// write and read, and that comparing it against the pins is what tells an
// up-to-date install from a stale one.
func TestManifestRoundTripsAndComparesToPins(t *testing.T) {
	t.Parallel()

	build, err := HostBuild()
	if err != nil {
		t.Skipf("no pinned build for this platform: %v", err)
	}

	model, err := Lookup(DefaultModel)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}

	path := filepath.Join(t.TempDir(), manifestFileName)
	written := Manifest{Runtime: runtimeRecord(&build), Model: modelRecord(&model)}

	if err := SaveManifest(path, &written, time.Unix(1, 0).UTC()); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, found, err := LoadManifest(path)
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

	manifest, found, err := LoadManifest(filepath.Join(t.TempDir(), manifestFileName))
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

	model := Model{Name: "m", SHA256: "aaa", Revision: "r1"}
	manifest := Manifest{Model: InstalledModel{Name: "m", SHA256: "aaa", Revision: "r0"}}

	if !manifest.ModelMatches(&model) {
		t.Error("same digest under a different revision should match")
	}

	changed := Model{Name: "m", SHA256: "bbb", Revision: "r0"}
	if manifest.ModelMatches(&changed) {
		t.Error("a different digest should not match")
	}
}
