package localmodel

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Filesystem permissions for the managed tree: group-readable directories,
// owner-only files, and an executable bit on what the supervisor runs.
const (
	dirPerm  = 0o750
	filePerm = 0o600
	execPerm = 0o750
)

// subdirModel is the directory the whole local-model footprint lives under,
// so one path covers the runtime, the weights, and the manifest.
const subdirModel = "model"

// Directory names inside the model tree. The staging and previous names are
// siblings of the live runtime rather than children, so an upgrade swaps
// directories with a rename instead of copying gigabytes.
const (
	dirRuntime  = "runtime"
	dirModels   = "models"
	dirStaging  = "runtime.incoming"
	dirPrevious = "runtime.previous"

	manifestFileName = "manifest.json"
)

// Layout resolves every path the local model uses, rooted at one directory so
// setup, status, and uninstall agree on where things are. NewLayout takes the
// CairnOS home and puts the whole footprint in one subdirectory of it.
type Layout struct {
	root string
}

// NewLayout roots a Layout at home/model.
func NewLayout(home string) Layout {
	return Layout{root: filepath.Join(home, subdirModel)}
}

// Root is the base directory holding the runtime, the weights, and the
// manifest.
func (l Layout) Root() string { return l.root }

// RuntimeDir holds the extracted llama-server and its shared libraries.
func (l Layout) RuntimeDir() string { return filepath.Join(l.root, dirRuntime) }

// StagingDir is where a new runtime is extracted before it takes over, so a
// failed download never disturbs a working install.
func (l Layout) StagingDir() string { return filepath.Join(l.root, dirStaging) }

// PreviousDir holds the runtime an upgrade displaced, kept until the new one
// answers a health probe so a bad build can be rolled back.
func (l Layout) PreviousDir() string { return filepath.Join(l.root, dirPrevious) }

// ServerPath is the llama-server executable inside the live runtime.
func (l Layout) ServerPath() string {
	return filepath.Join(l.RuntimeDir(), hostServerBinary())
}

// ModelsDir holds the downloaded GGUF files.
func (l Layout) ModelsDir() string { return filepath.Join(l.root, dirModels) }

// ModelPath is where one catalog model's file lives once downloaded.
func (l Layout) ModelPath(m *Model) string { return filepath.Join(l.ModelsDir(), m.File) }

// ManifestPath is the record of what is actually installed.
func (l Layout) ManifestPath() string { return filepath.Join(l.root, manifestFileName) }

// EnsureDirs creates the runtime and models directories so later writes find
// their parents present.
func (l Layout) EnsureDirs() error {
	for _, dir := range []string{l.RuntimeDir(), l.ModelsDir()} {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	return nil
}

// hostServerBinary is the server executable's name on the running platform.
func hostServerBinary() string {
	if runtime.GOOS == osWindows {
		return serverBinaryWin
	}

	return serverBinary
}
