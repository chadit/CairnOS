package localmodel_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/localmodel"
)

// Fixture member names and bodies. Naming them keeps an assertion and the
// archive it checks from drifting apart on a rename.
const (
	fixtureOtherTool    = "llama-cli"
	fixtureLicense      = "LICENSE"
	fixtureVersionedLib = "libllama.so.0.4.0"
	fixtureSoname       = "libllama.so.0"
	fixtureServerBody   = "server"
	fixtureOtherBody    = "cli"
)

// Fixture bodies and the Windows library name, named where a literal now
// repeats across cases.
const (
	fixtureLibBody    = "lib"
	fixtureRogueBody  = "payload"
	fixtureWindowsLib = "ggml-base.dll"

	// Library names a POSIX release carries: a CPU backend variant, an
	// unversioned soname, and one the size tests make too large.
	fixtureBackendLib   = "libggml-cpu-haswell.so"
	fixtureUnversioned  = "libllama.so"
	fixtureOversizedLib = "libbig.so"
)

// fixtureCeiling is the per-member byte cap the size tests run against, small
// enough that a fixture can sit one byte over it.
const fixtureCeiling int64 = 8

// posixBuild describes the tarball shape the POSIX fixtures use.
func posixBuild() *localmodel.Build {
	return &localmodel.Build{Format: localmodel.FormatTarGz, Prefix: localmodel.ArchivePrefix, Binary: localmodel.ServerBinary}
}

// TestExtractServerKeepsServerAndLibrariesOnly proves the install carries what
// llama-server needs to start and nothing else: the other release executables
// stay in the archive, and the soname symlinks the loader follows survive as
// symlinks rather than being flattened into copies.
func TestExtractServerKeepsServerAndLibrariesOnly(t *testing.T) {
	t.Parallel()

	payload := tarGzFixture(t, []fixtureEntry{
		{Name: localmodel.ServerBinary, Content: fixtureServerBody},
		{Name: fixtureVersionedLib, Content: fixtureLibBody},
		{Name: fixtureSoname, Link: fixtureVersionedLib},
		{Name: fixtureBackendLib, Content: "backend"},
		{Name: fixtureOtherTool, Content: fixtureOtherBody},
		{Name: fixtureLicense, Content: "text"},
	})

	dest := extractFixture(t, payload, posixBuild())

	for _, name := range []string{localmodel.ServerBinary, fixtureVersionedLib, fixtureSoname, fixtureBackendLib} {
		if _, err := os.Stat(filepath.Join(dest, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}

	for _, name := range []string{fixtureOtherTool, fixtureLicense} {
		if _, err := os.Stat(filepath.Join(dest, name)); err == nil {
			t.Errorf("%s was installed but nothing runs it", name)
		}
	}

	info, err := os.Lstat(filepath.Join(dest, fixtureSoname))
	if err != nil {
		t.Fatalf("lstat soname link: %v", err)
	}

	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("soname link was copied instead of linked; the loader follows the link name")
	}
}

// TestExtractServerMakesServerExecutable proves the installed binary carries
// the bit the supervisor needs to start it, and that no other account can read
// or run it.
func TestExtractServerMakesServerExecutable(t *testing.T) {
	t.Parallel()

	payload := tarGzFixture(t, []fixtureEntry{{Name: localmodel.ServerBinary, Content: fixtureServerBody}})
	dest := extractFixture(t, payload, posixBuild())

	info, err := os.Stat(filepath.Join(dest, localmodel.ServerBinary))
	if err != nil {
		t.Fatalf("stat server: %v", err)
	}

	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("server mode = %v, want the owner execute bit set", info.Mode().Perm())
	}

	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("server mode = %v, want no group or other access", info.Mode().Perm())
	}
}

// TestExtractServerRejectsArchiveWithoutServer proves a release asset that
// carries no server fails loudly instead of leaving an empty runtime that
// status would later call installed.
func TestExtractServerRejectsArchiveWithoutServer(t *testing.T) {
	t.Parallel()

	payload := tarGzFixture(t, []fixtureEntry{{Name: fixtureOtherTool, Content: fixtureOtherBody}})

	build := posixBuild()
	build.Asset = "llama-fixture.tar.gz"

	err := localmodel.ExtractServer(writeFixture(t, payload), t.TempDir(), build, localmodel.MaxMemberBytes)
	if !errors.Is(err, localmodel.ErrNoServerInArchive) {
		t.Fatalf("err = %v, want localmodel.ErrNoServerInArchive", err)
	}
}

// TestExtractServerRefusesMembersOutsideThePrefix proves the wrapper directory
// is enforced rather than assumed: a member reached through a parent traversal,
// and one nested a level deeper, are both skipped instead of being flattened
// into the runtime directory.
func TestExtractServerRefusesMembersOutsideThePrefix(t *testing.T) {
	t.Parallel()

	payload := tarGzFixture(t, []fixtureEntry{
		{Name: localmodel.ServerBinary, Content: fixtureServerBody},
		{Name: "../../evil.so", Content: fixtureRogueBody, Raw: true},
		{Name: "nested/deeper.so", Content: fixtureRogueBody},
		{Name: "elsewhere/rogue.so", Content: fixtureRogueBody, Raw: true},
	})

	dest := extractFixture(t, payload, posixBuild())

	if _, err := os.Stat(filepath.Join(dest, "..", "..", "evil.so")); err == nil {
		t.Error("archive member escaped the runtime directory")
	}

	for _, name := range []string{"evil.so", "deeper.so", "rogue.so"} {
		if _, err := os.Stat(filepath.Join(dest, name)); err == nil {
			t.Errorf("%s was installed from outside the archive prefix", name)
		}
	}

	if _, err := os.Stat(filepath.Join(dest, localmodel.ServerBinary)); err != nil {
		t.Errorf("the legitimate member was dropped too: %v", err)
	}
}

// TestExtractServerRefusesAnOversizedMember proves a member past the ceiling
// fails the install rather than landing truncated. A cut-off shared library
// with the execute bit on it would surface much later as the loader refusing a
// corrupt object, with nothing pointing back at the download.
func TestExtractServerRefusesAnOversizedMember(t *testing.T) {
	t.Parallel()

	payload := tarGzFixture(t, []fixtureEntry{
		{Name: localmodel.ServerBinary, Content: fixtureServerBody},
		{Name: fixtureOversizedLib, Content: strings.Repeat("x", int(fixtureCeiling)+1)},
	})

	dest := filepath.Join(t.TempDir(), "runtime")

	err := localmodel.ExtractServer(writeFixture(t, payload), dest, posixBuild(), fixtureCeiling)
	if !errors.Is(err, localmodel.ErrArchiveMember) {
		t.Fatalf("err = %v, want localmodel.ErrArchiveMember", err)
	}

	tooLarge, isTooLarge := errors.AsType[*localmodel.MemberTooLargeError](err)
	if !isTooLarge {
		t.Fatalf("err = %v, want a *localmodel.MemberTooLargeError naming the member", err)
	}

	if filepath.Base(tooLarge.Path) != fixtureOversizedLib {
		t.Errorf("path = %q, want the oversized member named", tooLarge.Path)
	}
}

// TestExtractServerAcceptsAMemberExactlyAtTheCeiling proves the bound is
// inclusive, so the refusal above is about oversize and not an off-by-one.
func TestExtractServerAcceptsAMemberExactlyAtTheCeiling(t *testing.T) {
	t.Parallel()

	payload := tarGzFixture(t, []fixtureEntry{
		{Name: localmodel.ServerBinary, Content: strings.Repeat("x", int(fixtureCeiling))},
	})

	dest := filepath.Join(t.TempDir(), "runtime")

	if err := localmodel.ExtractServer(writeFixture(t, payload), dest, posixBuild(), fixtureCeiling); err != nil {
		t.Fatalf("extract: %v", err)
	}

	info, err := os.Stat(filepath.Join(dest, localmodel.ServerBinary))
	if err != nil {
		t.Fatalf("stat server: %v", err)
	}

	if info.Size() != fixtureCeiling {
		t.Errorf("installed size = %d, want %d", info.Size(), fixtureCeiling)
	}
}

// TestExtractServerSkipsZipSymlinkEntries proves a zip symlink is not
// installed as a regular file holding the link text with the execute bit set.
func TestExtractServerSkipsZipSymlinkEntries(t *testing.T) {
	t.Parallel()

	payload := zipFixture(t, []fixtureEntry{
		{Name: localmodel.ServerBinaryWin, Content: fixtureServerBody},
		{Name: fixtureWindowsLib, Content: fixtureLibBody},
		{Name: "ggml.dll", Link: fixtureWindowsLib},
	})

	dest := extractFixture(t, payload, &localmodel.Build{Format: localmodel.FormatZip, Binary: localmodel.ServerBinaryWin})

	if _, err := os.Lstat(filepath.Join(dest, "ggml.dll")); err == nil {
		t.Error("a zip symlink entry was installed as a file of link text")
	}

	if _, err := os.Stat(filepath.Join(dest, fixtureWindowsLib)); err != nil {
		t.Errorf("the real library was dropped: %v", err)
	}
}

// TestExtractServerRejectsSymlinkWithPathTarget proves a link target naming a
// path rather than a sibling is refused, since honoring one would point the
// loader outside the install.
func TestExtractServerRejectsSymlinkWithPathTarget(t *testing.T) {
	t.Parallel()

	payload := tarGzFixture(t, []fixtureEntry{
		{Name: localmodel.ServerBinary, Content: fixtureServerBody},
		{Name: fixtureUnversioned, Link: "/etc/passwd"},
	})

	err := localmodel.ExtractServer(writeFixture(t, payload), t.TempDir(), posixBuild(), localmodel.MaxMemberBytes)
	if !errors.Is(err, localmodel.ErrArchiveMember) {
		t.Fatalf("err = %v, want localmodel.ErrArchiveMember", err)
	}
}

// TestExtractServerReadsFlatZip proves the Windows asset shape works: no
// wrapper directory, .exe server, .dll libraries.
func TestExtractServerReadsFlatZip(t *testing.T) {
	t.Parallel()

	payload := zipFixture(t, []fixtureEntry{
		{Name: localmodel.ServerBinaryWin, Content: fixtureServerBody},
		{Name: fixtureWindowsLib, Content: fixtureLibBody},
		{Name: "llama-cli.exe", Content: fixtureOtherBody},
	})

	dest := extractFixture(t, payload, &localmodel.Build{Format: localmodel.FormatZip, Binary: localmodel.ServerBinaryWin})

	if _, err := os.Stat(filepath.Join(dest, "llama-server.exe")); err != nil {
		t.Errorf("missing server: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dest, fixtureWindowsLib)); err != nil {
		t.Errorf("missing library: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dest, "llama-cli.exe")); err == nil {
		t.Error("llama-cli.exe was installed but nothing runs it")
	}
}

// TestIsSharedLibCoversVersionedSonames proves both soname spellings count as
// libraries: Linux puts the version after the extension, macOS before it.
func TestIsSharedLibCoversVersionedSonames(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		want bool
	}{
		{fixtureUnversioned, true},
		{"libllama.so.0.4.0", true},
		{"libggml-base.0.23.0.dylib", true},
		{"ggml-cpu-zen4.dll", true},
		{"llama-cli", false},
		{"LICENSE", false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := localmodel.IsSharedLib(testCase.name); got != testCase.want {
				t.Errorf("localmodel.IsSharedLib(%q) = %t, want %t", testCase.name, got, testCase.want)
			}
		})
	}
}

// extractFixture writes an archive to disk and extracts it, returning the
// destination directory.
func extractFixture(t *testing.T, payload []byte, build *localmodel.Build) string {
	t.Helper()

	dest := filepath.Join(t.TempDir(), "runtime")

	if err := localmodel.ExtractServer(writeFixture(t, payload), dest, build, localmodel.MaxMemberBytes); err != nil {
		t.Fatalf("extract: %v", err)
	}

	return dest
}

// writeFixture drops an archive into a temp file and returns its path.
func writeFixture(t *testing.T, payload []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "release-archive")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	return path
}
