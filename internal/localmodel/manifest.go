package localmodel

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// jsonIndent is the two-space indent every JSON document this package writes
// uses, so a manifest and a check report read the same way.
const jsonIndent = "  "

// Manifest records what is installed, which is what makes "installed versus
// pinned" answerable without hashing 4.6 GB on every status call. An upgrade
// compares it against the compiled-in pins to decide what to re-download.
type Manifest struct {
	UpdatedAt time.Time        `json:"updated_at"`
	Runtime   InstalledRuntime `json:"runtime"`
	Model     InstalledModel   `json:"model"`
}

// InstalledRuntime is the llama.cpp build sitting in the runtime directory.
type InstalledRuntime struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Build   string `json:"build"`
	Asset   string `json:"asset"`
	SHA256  string `json:"sha256"`
}

// InstalledModel is the GGUF sitting in the models directory.
type InstalledModel struct {
	Name     string `json:"name"`
	Repo     string `json:"repo"`
	File     string `json:"file"`
	Revision string `json:"revision"`
	SHA256   string `json:"sha256"`
	Bytes    int64  `json:"bytes"`
}

// LoadManifest reads the install record. A missing file is not an error: it
// means nothing is installed yet, which the zero Manifest says, and the found
// result distinguishes that from an empty record someone wrote.
func LoadManifest(path string) (Manifest, bool, error) {
	// #nosec G304 -- path is a managed layout location
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Manifest{}, false, nil
		}

		return Manifest{}, false, fmt.Errorf("read %s: %w", path, err)
	}

	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return Manifest{}, false, fmt.Errorf("parse %s: %w", path, err)
	}

	return manifest, true, nil
}

// SaveManifest writes the install record, stamping the time so a user can see
// when setup last ran.
func SaveManifest(path string, manifest *Manifest, now time.Time) error {
	manifest.UpdatedAt = now

	raw, err := json.MarshalIndent(manifest, "", jsonIndent)
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}

	if err := os.WriteFile(path, append(raw, '\n'), filePerm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

// RuntimeMatches reports whether the installed runtime is the pinned one.
func (m *Manifest) RuntimeMatches(build *Build) bool {
	return m.Runtime.Version == PinnedVersion &&
		m.Runtime.Build == PinnedBuild &&
		m.Runtime.SHA256 == build.SHA256
}

// ModelMatches reports whether the installed model is the pinned one. The
// digest decides rather than the revision, because a revision moves on a
// README edit that leaves the weights untouched.
func (m *Manifest) ModelMatches(model *Model) bool {
	return m.Model.Name == model.Name && m.Model.SHA256 == model.SHA256
}

// RuntimeRecord builds the manifest entry for a freshly installed runtime.
func RuntimeRecord(build *Build) InstalledRuntime {
	return InstalledRuntime{
		Name:    RuntimeName,
		Version: PinnedVersion,
		Build:   PinnedBuild,
		Asset:   build.Asset,
		SHA256:  build.SHA256,
	}
}

// ModelRecord builds the manifest entry for a freshly installed model.
func ModelRecord(model *Model) InstalledModel {
	return InstalledModel{
		Name:     model.Name,
		Repo:     model.Repo,
		File:     model.File,
		Revision: model.Revision,
		SHA256:   model.SHA256,
		Bytes:    model.Bytes,
	}
}
