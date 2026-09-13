package localmodel

import (
	"fmt"
	"runtime"
	"sort"
)

// Runtime identity. llama.cpp publishes two tags per release: a stable vX.Y.Z
// that carries only a nightly-tag.txt pointer, and the bNNNNN nightly that
// carries the platform binaries. Both are pinned because the stable tag is
// what a human reads and the build tag is what the download URLs use.
const (
	// RuntimeName is what reports call the server, matching upstream.
	RuntimeName = "llama.cpp"
	// PinnedVersion is the stable release this build of CairnOS targets.
	PinnedVersion = "v0.4.0"
	// PinnedBuild is the nightly tag under that release whose assets carry
	// the binaries.
	PinnedBuild = "b10809"
)

// Archive formats the release assets come in.
const (
	formatTarGz = "tar.gz"
	formatZip   = "zip"
)

// serverBinary is the executable CairnOS installs out of a release archive.
// Every other tool in the archive stays behind.
const (
	serverBinary    = "llama-server"
	serverBinaryWin = "llama-server.exe"
)

// archivePrefix is the single directory the POSIX tarballs wrap their members
// in. The Windows zip is flat and carries no prefix.
const archivePrefix = "llama-" + PinnedBuild + "/"

// Platform GOOS and GOARCH values the pins cover.
const (
	osDarwin  = "darwin"
	osLinux   = "linux"
	osWindows = "windows"
	archAMD64 = "amd64"
	archARM64 = "arm64"
)

// Build is one pinned llama.cpp release asset: the archive to download for a
// platform, its SHA-256, and what to pull out of it. SHA256 is the digest of
// the archive as published, computed once when the pin was made, so a fetched
// archive is checked bit for bit before anything is extracted. Bytes is the
// published size, which is what lets the download print a percentage instead
// of going quiet for a ten-megabyte transfer.
type Build struct {
	GOOS   string
	GOARCH string
	Asset  string
	SHA256 string
	Bytes  int64
	Format string
	// Prefix is the leading directory to strip from member names, empty for a
	// flat archive.
	Prefix string
	// Binary is the server executable's name inside the archive.
	Binary string
}

// URL is the public download URL for this asset, under the build tag that
// carries the binaries rather than the stable tag that points at it.
func (b *Build) URL() string {
	return buildURL(b, githubDownloadBase)
}

// buildURL joins a download host with the pinned release path. Fetch overrides
// the host so tests serve fixture archives from an httptest server.
func buildURL(b *Build, base string) string {
	return fmt.Sprintf("%s/%s/releases/download/%s/%s", base, llamaRepo, PinnedBuild, b.Asset)
}

// builds is the pinned asset table, one entry per platform upstream ships a
// CPU build for. It is a function rather than a package variable because the
// linter forbids mutable globals; the table is five entries, so rebuilding it
// costs nothing.
//
// The Windows entry covers the CPU build only. CUDA and Vulkan variants exist
// upstream but need a matching driver stack, which unattended setup cannot
// check, so a CPU build that always runs beats a faster one that sometimes
// does not.
func builds() []Build {
	return []Build{
		{
			GOOS: osLinux, GOARCH: archAMD64,
			Asset:  "llama-" + PinnedBuild + "-bin-ubuntu-x64.tar.gz",
			SHA256: "5e34434ddc6d03cd1584f403201aff0d4bd1a5793a72ff7e286532dfd1e4b941",
			Bytes:  16734586,
			Format: formatTarGz, Prefix: archivePrefix, Binary: serverBinary,
		},
		{
			GOOS: osLinux, GOARCH: archARM64,
			Asset:  "llama-" + PinnedBuild + "-bin-ubuntu-arm64.tar.gz",
			SHA256: "f2b7333971e1b7b42e9268bfdbfa30f5f56e2897156084d2251385df94aec358",
			Bytes:  13380118,
			Format: formatTarGz, Prefix: archivePrefix, Binary: serverBinary,
		},
		{
			GOOS: osDarwin, GOARCH: archARM64,
			Asset:  "llama-" + PinnedBuild + "-bin-macos-arm64.tar.gz",
			SHA256: "7d692df9e1e386e62f1c12b843903218041e6cd74c9415aa39a7ed3176f9eaa2",
			Bytes:  11123196,
			Format: formatTarGz, Prefix: archivePrefix, Binary: serverBinary,
		},
		{
			GOOS: osDarwin, GOARCH: archAMD64,
			Asset:  "llama-" + PinnedBuild + "-bin-macos-x64.tar.gz",
			SHA256: "13b34aa8a5d87341a21065a83f54a8167e1aaa6fe0d66065de01632a1ed64be6",
			Bytes:  11175330,
			Format: formatTarGz, Prefix: archivePrefix, Binary: serverBinary,
		},
		{
			GOOS: osWindows, GOARCH: archAMD64,
			Asset:  "llama-" + PinnedBuild + "-bin-win-cpu-x64.zip",
			SHA256: "9df3158ed228a641a4b127942d7f459f24c9e13f04682659d05c00c80099b6b5",
			Bytes:  18407457,
			Format: formatZip, Prefix: "", Binary: serverBinaryWin,
		},
	}
}

// BuildFor resolves the pinned asset for one platform. The ok result is false
// when no build is pinned, which the caller turns into ErrUnsupportedPlatform.
func BuildFor(goos, goarch string) (Build, bool) {
	entries := builds()
	for idx := range entries {
		if entries[idx].GOOS == goos && entries[idx].GOARCH == goarch {
			return entries[idx], true
		}
	}

	return Build{}, false
}

// HostBuild resolves the pinned asset for the running platform.
func HostBuild() (Build, error) {
	found, ok := BuildFor(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return Build{}, fmt.Errorf("%w (%s/%s)", ErrUnsupportedPlatform, runtime.GOOS, runtime.GOARCH)
	}

	return found, nil
}

// DefaultModel is the model setup installs when --model is not given.
const DefaultModel = "gemma-4-e4b"

// Model is one downloadable GGUF. SHA256 is the file's Hugging Face LFS
// digest, the value the hub itself verifies downloads against, so a fetched
// file is checked against the same number the publisher signed off on.
// Revision is the repository commit the digest was read at, which is what a
// staleness check compares against rather than the commit alone: a
// README-only commit moves the revision and leaves the weights untouched.
type Model struct {
	Name     string
	Repo     string
	File     string
	SHA256   string
	Revision string
	Bytes    int64
	Quant    string
	Params   string
	License  string
}

// URL is the public resolve URL for the model file.
func (m *Model) URL() string {
	return modelURL(m, huggingFaceBase)
}

// modelURL joins a Hugging Face host with the repo and file, which Fetch
// overrides so tests serve fixtures from an httptest server.
func modelURL(m *Model, base string) string {
	return fmt.Sprintf("%s/%s/resolve/main/%s", base, m.Repo, m.File)
}

// CommitsURL points a human at the repository history, so a "newer weights
// exist" report comes with somewhere to go and read what changed.
func (m *Model) CommitsURL() string {
	return fmt.Sprintf("%s/%s/commits/main", huggingFaceBase, m.Repo)
}

// catalog builds the set of models setup can install. Every entry is ungated
// and permissively licensed: a gated repo needs an interactive token grant,
// which breaks the unattended install this command exists to provide.
//
// Q4_0 is the shipped quantization. Chat quality holds up at four bits and the
// file is 4.6 GB against 8 GB for Q8_0, which decides whether the model fits
// alongside everything else on an ordinary laptop.
func catalog() []Model {
	return []Model{
		{
			Name:     DefaultModel,
			Repo:     "ggml-org/gemma-4-E4B-it-GGUF",
			File:     "gemma-4-E4B-it-Q4_0.gguf",
			SHA256:   "a555b900214b477d8880e7832e0b8925e139b0159640036b09fe472b6f2097f2",
			Revision: "b8093469224f83f5c38f691eb906c380e9e63114",
			Bytes:    4590807392,
			Quant:    "Q4_0",
			Params:   "E4B",
			License:  "apache-2.0",
		},
	}
}

// Lookup resolves a catalog name to its Model.
func Lookup(name string) (Model, error) {
	entries := catalog()
	for idx := range entries {
		if entries[idx].Name == name {
			return entries[idx], nil
		}
	}

	return Model{}, fmt.Errorf("%w: %s", ErrUnknownModel, name)
}

// Models returns every catalog entry, ordered by name for stable listing.
func Models() []Model {
	entries := catalog()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

	return entries
}
