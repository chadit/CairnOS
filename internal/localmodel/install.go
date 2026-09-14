package localmodel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// InstallOptions drives one install pass. Layout and Model are required; the
// host overrides exist so tests point the whole flow at httptest servers.
type InstallOptions struct {
	// Now supplies the manifest timestamp; nil uses the wall clock.
	Now func() time.Time

	// Build overrides the pinned release asset. Nil uses the pin for the
	// running platform, which is what every command does; tests substitute a
	// fixture archive whose digest they control.
	Build *Build

	Fetch  FetchOptions
	Layout Layout

	// DownloadBase overrides the GitHub release host.
	DownloadBase string

	// ResolveBase overrides the Hugging Face host.
	ResolveBase string

	Model Model
}

// InstallResult says what an install pass actually did, so the command can
// report a re-run that changed nothing differently from a fresh download.
type InstallResult struct {
	ServerPath string
	ModelPath  string

	// previousManifest is the install record as it stood before this pass.
	// Rollback restores it alongside the displaced binary: putting the binary
	// back without the record would leave status reporting the version that
	// failed, and leave the next pass short-circuiting on a manifest entry
	// that no longer matches what is on disk.
	previousManifest Manifest

	// RuntimeInstalled is true when the runtime was downloaded and extracted
	// rather than left in place.
	RuntimeInstalled bool

	// ModelInstalled is true when the weights were downloaded rather than
	// found already verified.
	ModelInstalled bool

	// RolledOver is true when a previous runtime was displaced and now waits
	// on CommitRuntime or Rollback.
	RolledOver bool

	// hadPreviousManifest says whether this pass found an install record at
	// all, which is what tells Rollback to restore one rather than delete it.
	hadPreviousManifest bool
}

// Install brings the runtime and the model up to the compiled-in pins.
// Setup and upgrade share it, so anything already matching its pin is left
// alone.
//
// A replaced runtime is kept at Layout.PreviousDir; the caller calls
// CommitRuntime once the new server answers a health probe, Rollback if not.
func Install(ctx context.Context, opts *InstallOptions) (InstallResult, error) {
	if err := opts.Layout.EnsureDirs(); err != nil {
		return InstallResult{}, err
	}

	build, err := installBuild(opts)
	if err != nil {
		return InstallResult{}, err
	}

	manifest, found, err := LoadManifest(opts.Layout.ManifestPath())
	if err != nil {
		return InstallResult{}, err
	}

	result := InstallResult{
		ServerPath:          opts.Layout.ServerPath(),
		ModelPath:           opts.Layout.ModelPath(&opts.Model),
		previousManifest:    manifest,
		hadPreviousManifest: found,
	}

	if err := installRuntime(ctx, opts, &build, &manifest, &result); err != nil {
		return InstallResult{}, err
	}

	if err := installModel(ctx, opts, &manifest, &result); err != nil {
		return InstallResult{}, err
	}

	manifest.Runtime = RuntimeRecord(&build)
	manifest.Model = ModelRecord(&opts.Model)

	if err := SaveManifest(opts.Layout.ManifestPath(), &manifest, timestamp(opts.Now)); err != nil {
		return InstallResult{}, err
	}

	return result, nil
}

// installBuild picks the release asset this pass installs.
func installBuild(opts *InstallOptions) (Build, error) {
	if opts.Build != nil {
		return *opts.Build, nil
	}

	return HostBuild()
}

// installRuntime downloads and extracts the pinned llama.cpp build unless the
// manifest and the filesystem already agree it is there.
func installRuntime(
	ctx context.Context, opts *InstallOptions, build *Build, manifest *Manifest, result *InstallResult,
) error {
	if !opts.Fetch.Force && manifest.RuntimeMatches(build) && fileExists(opts.Layout.ServerPath()) {
		return nil
	}

	archive := filepath.Join(opts.Layout.Root(), build.Asset)
	art := Artifact{
		Name:   RuntimeName + " " + PinnedVersion,
		URL:    buildURL(build, downloadHost(opts)),
		SHA256: build.SHA256,
		Bytes:  build.Bytes,
	}

	if _, err := Fetch(ctx, &art, archive, opts.Fetch); err != nil {
		return err
	}

	staging := opts.Layout.StagingDir()
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("clear %s: %w", staging, err)
	}

	if err := ExtractServer(archive, staging, build, MaxMemberBytes); err != nil {
		return err
	}

	// The archive has given up everything the install needs, so it goes rather
	// than sitting in the model tree as a second copy of what was extracted.
	if err := os.Remove(archive); err != nil {
		return fmt.Errorf("remove %s: %w", archive, err)
	}

	rolled, err := opts.Layout.swapRuntime()
	if err != nil {
		return err
	}

	result.RuntimeInstalled = true
	result.RolledOver = rolled

	return nil
}

// installModel downloads the pinned GGUF unless it is already installed.
//
// The manifest short-circuit matters more here than for the runtime: without
// it, every setup or re-run would read 4.6 GB off disk to recompute a digest
// that has not changed. A matching manifest entry next to a file of the
// recorded size stands in for that read. Force still hashes.
func installModel(ctx context.Context, opts *InstallOptions, manifest *Manifest, result *InstallResult) error {
	dest := opts.Layout.ModelPath(&opts.Model)

	if !opts.Fetch.Force && manifest.ModelMatches(&opts.Model) && fileHasSize(dest, opts.Model.Bytes) {
		return nil
	}

	art := Artifact{
		Name:   opts.Model.Name,
		URL:    modelURL(&opts.Model, resolveHost(opts)),
		SHA256: opts.Model.SHA256,
		Bytes:  opts.Model.Bytes,
	}

	fetched, err := Fetch(ctx, &art, dest, opts.Fetch)
	if err != nil {
		return err
	}

	result.ModelInstalled = !fetched.Skipped

	return nil
}

// swapRuntime moves a freshly extracted staging directory into place, setting
// the old runtime aside rather than deleting it. It reports whether anything
// was set aside, since a first install has nothing to roll back to.
func (l Layout) swapRuntime() (bool, error) {
	previous := l.PreviousDir()
	if err := os.RemoveAll(previous); err != nil {
		return false, fmt.Errorf("clear %s: %w", previous, err)
	}

	var rolled bool

	if dirHasEntries(l.RuntimeDir()) {
		if err := os.Rename(l.RuntimeDir(), previous); err != nil {
			return false, fmt.Errorf("set aside %s: %w", l.RuntimeDir(), err)
		}

		rolled = true
	}

	// An empty runtime directory is left by EnsureDirs on a first install, and
	// rename refuses to overwrite it.
	if err := os.RemoveAll(l.RuntimeDir()); err != nil {
		return false, fmt.Errorf("clear %s: %w", l.RuntimeDir(), err)
	}

	if err := os.Rename(l.StagingDir(), l.RuntimeDir()); err != nil {
		return false, fmt.Errorf("install %s: %w", l.RuntimeDir(), err)
	}

	return rolled, nil
}

// CommitRuntime discards the runtime an upgrade displaced, once the new one
// has proven it answers.
func (l Layout) CommitRuntime() error {
	if err := os.RemoveAll(l.PreviousDir()); err != nil {
		return fmt.Errorf("remove %s: %w", l.PreviousDir(), err)
	}

	return nil
}

// Rollback undoes an install pass whose server never answered: it puts the
// displaced runtime back and rewrites the install record as it stood before
// the pass, so the two halves agree again. A pass that installed onto an empty
// tree leaves no record behind, since there is nothing it could honestly say.
func Rollback(layout Layout, result *InstallResult) error {
	if err := layout.rollbackRuntime(); err != nil {
		return err
	}

	if !result.hadPreviousManifest {
		if err := os.Remove(layout.ManifestPath()); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", layout.ManifestPath(), err)
		}

		return nil
	}

	// The recorded time is preserved rather than restamped: the record is the
	// one written by the last pass that worked, and that is when it was written.
	return SaveManifest(layout.ManifestPath(), &result.previousManifest, result.previousManifest.UpdatedAt)
}

// rollbackRuntime puts the displaced runtime directory back.
func (l Layout) rollbackRuntime() error {
	if !dirHasEntries(l.PreviousDir()) {
		return nil
	}

	if err := os.RemoveAll(l.RuntimeDir()); err != nil {
		return fmt.Errorf("clear %s: %w", l.RuntimeDir(), err)
	}

	if err := os.Rename(l.PreviousDir(), l.RuntimeDir()); err != nil {
		return fmt.Errorf("restore %s: %w", l.RuntimeDir(), err)
	}

	return nil
}

// Purge removes the whole managed tree: the runtime, the weights, the cached
// archive, and the manifest. It is what `model uninstall --purge` runs.
func (l Layout) Purge() error {
	if err := os.RemoveAll(l.Root()); err != nil {
		return fmt.Errorf("remove %s: %w", l.Root(), err)
	}

	return nil
}

// downloadHost is the GitHub release host this pass fetches from.
func downloadHost(opts *InstallOptions) string {
	return orDefault(opts.DownloadBase, githubDownloadBase)
}

// resolveHost is the Hugging Face host this pass fetches weights from.
func resolveHost(opts *InstallOptions) string {
	return orDefault(opts.ResolveBase, HuggingFaceBase)
}

// timestamp reads the injected clock, falling back to the wall clock.
func timestamp(now func() time.Time) time.Time {
	if now == nil {
		return time.Now()
	}

	return now()
}

// fileExists reports whether path is a regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)

	return err == nil && !info.IsDir()
}

// fileHasSize reports whether path is a regular file of exactly want bytes.
func fileHasSize(path string, want int64) bool {
	info, err := os.Stat(path)

	return err == nil && !info.IsDir() && info.Size() == want
}

// dirHasEntries reports whether path is a directory holding anything.
func dirHasEntries(path string) bool {
	entries, err := os.ReadDir(path)

	return err == nil && len(entries) > 0
}
