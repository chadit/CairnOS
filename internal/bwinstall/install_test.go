package bwinstall_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/bwinstall"
	"github.com/chadit/CairnOS/internal/cairnerr"
)

// The version the fake binary reports, and the shell that reports it. A script
// stands in for the real CLI because the install path only ever runs it with
// --version.
const (
	fakeVersion = "2026.8.0"
	fakeScript  = "#!/bin/sh\necho " + fakeVersion + "\n"
)

// installPerm is the mode an installed binary carries, entryName the one file
// the release archive holds, and sidecarSuffix the record the installer leaves
// beside the binary saying what it wrote. sidecarPerm is that record's mode.
const (
	installPerm   = 0o755
	entryName     = "bw"
	sidecarSuffix = ".cairnos-sha256"
	sidecarPerm   = 0o600
)

// These cases do not run in parallel. Each one writes an executable and then
// runs it, and a fork from another goroutine between the write and the exec is
// what produces ETXTBSY. One install per process is the real shape anyway.

// wrongDigest is a well-formed sha256 that matches nothing.
const wrongDigest = "0000000000000000000000000000000000000000000000000000000000000000"

// unreachableSource is a URL nothing serves. A case that expects the install to
// skip or refuse points here, so a run that downloads anyway fails on the
// connection rather than passing quietly and hiding what the case was about.
const unreachableSource = "https://127.0.0.1:1/bw.zip"

// digestOf hashes one string the way the installer hashes bytes.
func digestOf(content string) string {
	sum := sha256.Sum256([]byte(content))

	return hex.EncodeToString(sum[:])
}

// buildZip renders an archive holding the given entries, and returns the bytes
// with their digest, so a case can serve a body whose checksum it knows.
func buildZip(t *testing.T, entries map[string]string) ([]byte, string) {
	t.Helper()

	var body bytes.Buffer

	archive := zip.NewWriter(&body)

	for name, content := range entries {
		entry, err := archive.Create(name)
		if err != nil {
			t.Fatalf("add %s to the archive: %v", name, err)
		}

		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatalf("write %s into the archive: %v", name, err)
		}
	}

	if err := archive.Close(); err != nil {
		t.Fatalf("close the archive: %v", err)
	}

	digest := sha256.Sum256(body.Bytes())

	return body.Bytes(), hex.EncodeToString(digest[:])
}

// serveBody stands one response up on a loopback TLS-free server and returns
// its URL. Install refuses anything but https, so the URL is rewritten to the
// https form and the test's own client is pointed back at the plain listener,
// which it dials whatever the URL says, so the scheme check sees https.
func serveBody(t *testing.T, handler http.Handler) (string, *http.Client) {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client := &http.Client{
		Transport: &rewriteTransport{target: strings.TrimPrefix(server.URL, "http://")},
		Timeout:   30 * time.Second,
	}

	return "https://bwinstall.test/bw-linux-" + fakeVersion + ".zip", client
}

// rewriteTransport sends every request to the test server, keeping the https
// URL the installer checked.
type rewriteTransport struct {
	target string
}

func (t *rewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	routed := request.Clone(request.Context())
	routed.URL.Scheme = "http"
	routed.URL.Host = t.target

	//nolint:wrapcheck // a test transport: the error goes straight back to the client
	return http.DefaultTransport.RoundTrip(routed)
}

// archiveServer serves a fixed body.
func archiveServer(body []byte) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(body)
	})
}

// options builds one install against a served archive. An empty sum takes the
// archive's real digest, and the binary digest always matches the entry, so a
// case states only what it is about.
func options(t *testing.T, entries map[string]string, sum string) *bwinstall.Options {
	t.Helper()

	body, actual := buildZip(t, entries)
	if sum == "" {
		sum = actual
	}

	url, client := serveBody(t, archiveServer(body))

	return &bwinstall.Options{
		URL:           url,
		Client:        client,
		ArchiveSHA256: sum,
		BinarySHA256:  digestOf(entries[entryName]),
		Version:       fakeVersion,
		Dest:          filepath.Join(t.TempDir(), entryName),
	}
}

// TestInstallPlacesTheVerifiedBinary is the happy path: both digests match, the
// one entry comes out, and the directory holds only the binary and its digest
// record. The download is a temp file there, so a stopped cleanup would leave
// the release zip beside it.
//
//nolint:paralleltest // writes an executable then runs it; a concurrent fork trips ETXTBSY (Go issue 22315)
func TestInstallPlacesTheVerifiedBinary(t *testing.T) {
	opts := options(t, map[string]string{entryName: fakeScript}, "")

	result, err := bwinstall.Install(t.Context(), opts)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	if !result.Installed || result.Path != opts.Dest || result.Version != fakeVersion {
		t.Fatalf("result = %+v, want an install of version %s at %s", result, fakeVersion, opts.Dest)
	}

	info, err := os.Stat(opts.Dest)
	if err != nil {
		t.Fatalf("stat the installed binary: %v", err)
	}

	if info.Mode().Perm() != installPerm {
		t.Fatalf("installed mode = %04o, want %04o", info.Mode().Perm(), installPerm)
	}

	body, err := os.ReadFile(opts.Dest) // #nosec G304 -- the path is built by the test
	if err != nil {
		t.Fatalf("read the installed binary: %v", err)
	}

	if string(body) != fakeScript {
		t.Fatalf("installed contents = %q, want the archive entry", string(body))
	}

	assertHolds(t, filepath.Dir(opts.Dest), entryName, entryName+sidecarSuffix)

	assertRecorded(t, opts.Dest, opts.BinarySHA256)
}

// TestInstallRecordsTheDigestAtTheModeItPromises covers the record itself. It
// sits in a personal bin directory next to the binary it vouches for, so it is
// owner-only like the credentials file, not world-readable like the binary.
//
//nolint:paralleltest // writes an executable then runs it; a concurrent fork trips ETXTBSY (Go issue 22315)
func TestInstallRecordsTheDigestAtTheModeItPromises(t *testing.T) {
	opts := options(t, map[string]string{entryName: fakeScript}, "")

	if _, err := bwinstall.Install(t.Context(), opts); err != nil {
		t.Fatalf("install: %v", err)
	}

	info, err := os.Stat(opts.Dest + sidecarSuffix)
	if err != nil {
		t.Fatalf("stat the digest record: %v", err)
	}

	if info.Mode().Perm() != sidecarPerm {
		t.Fatalf("record mode = %04o, want %04o", info.Mode().Perm(), sidecarPerm)
	}
}

// TestInstallReplacesTheBinaryItWroteBefore is the upgrade a host takes on its
// own. The file at the destination is not the pin any more, but it is the one an
// earlier install recorded, so nothing unknown is being overwritten and --force
// would be asking the operator to approve this tool replacing its own work.
//
//nolint:paralleltest // writes an executable then runs it; a concurrent fork trips ETXTBSY (Go issue 22315)
func TestInstallReplacesTheBinaryItWroteBefore(t *testing.T) {
	const olderPin = "#!/bin/sh\necho 2026.7.0\n"

	opts := options(t, map[string]string{entryName: fakeScript}, "")
	stageBinary(t, opts.Dest, olderPin)
	stageRecord(t, opts.Dest, digestOf(olderPin))

	result, err := bwinstall.Install(t.Context(), opts)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	if !result.Installed || result.Version != fakeVersion {
		t.Fatalf("result = %+v, want the pinned version installed", result)
	}

	if !strings.EqualFold(result.Previous, digestOf(olderPin)) {
		t.Fatalf("result.Previous = %q, want the digest of the binary it replaced", result.Previous)
	}

	assertRecorded(t, opts.Dest, opts.BinarySHA256)
}

// TestInstallRefusesABinaryTheRecordDoesNotVouchFor is the other half. A record
// naming some other digest says nothing about the file that is actually there,
// so the refusal has to stand: this is the shape a wrapper dropped over a real
// install would take.
//
//nolint:paralleltest // writes an executable then runs it; a concurrent fork trips ETXTBSY (Go issue 22315)
func TestInstallRefusesABinaryTheRecordDoesNotVouchFor(t *testing.T) {
	opts := options(t, map[string]string{entryName: fakeScript}, "")
	stageBinary(t, opts.Dest, "#!/bin/sh\necho "+fakeVersion+"\n# and then something else\n")
	stageRecord(t, opts.Dest, wrongDigest)

	opts.URL = unreachableSource

	_, err := bwinstall.Install(t.Context(), opts)
	if err == nil {
		t.Fatal("install replaced a binary its own record did not vouch for")
	}

	if !messageMentions(t, err, "--force") {
		t.Fatalf("the error does not point at the way past it: %v", err)
	}
}

// TestInstallRefusesAChecksumMismatch is the case the whole package exists for:
// a body that is not the pinned artifact must not reach the disk, whatever it
// claims to be.
//
//nolint:paralleltest // writes an executable then runs it; a concurrent fork trips ETXTBSY (Go issue 22315)
func TestInstallRefusesAChecksumMismatch(t *testing.T) {
	opts := options(t, map[string]string{entryName: fakeScript}, wrongDigest)

	if _, err := bwinstall.Install(t.Context(), opts); err == nil {
		t.Fatal("install accepted an archive whose digest did not match")
	}

	assertNothingLeftBehind(t, opts.Dest)
}

// TestInstallRefusesABinaryThatIsNotThePinnedOne covers the second digest: the
// archive is the published one, and what came out of it is not what this build
// pinned.
//
//nolint:paralleltest // writes an executable then runs it; a concurrent fork trips ETXTBSY (Go issue 22315)
func TestInstallRefusesABinaryThatIsNotThePinnedOne(t *testing.T) {
	opts := options(t, map[string]string{entryName: fakeScript}, "")
	opts.BinarySHA256 = wrongDigest

	if _, err := bwinstall.Install(t.Context(), opts); err == nil {
		t.Fatal("install accepted a binary whose digest did not match")
	}

	assertNothingLeftBehind(t, opts.Dest)
}

// TestInstallRefusesAnArchiveWithATraversalEntry covers the shape that would
// write outside the install directory. The entry is refused for the same reason
// any other unexpected name is: the pinned release holds one file called bw.
//
//nolint:paralleltest // writes an executable then runs it; a concurrent fork trips ETXTBSY (Go issue 22315)
func TestInstallRefusesAnArchiveWithATraversalEntry(t *testing.T) {
	opts := options(t, map[string]string{"../../bw": fakeScript}, "")

	_, err := bwinstall.Install(t.Context(), opts)
	if err == nil {
		t.Fatal("install unpacked an archive whose entry climbs out of the directory")
	}

	if !messageMentions(t, err, "../../bw") {
		t.Fatalf("the error does not name the entry it refused: %v", err)
	}

	assertNothingLeftBehind(t, opts.Dest)

	escaped := filepath.Join(filepath.Dir(opts.Dest), "..", "..", entryName)
	if _, statErr := os.Stat(escaped); statErr == nil {
		t.Fatalf("the traversal entry was written to %s", escaped)
	}
}

// TestInstallRefusesAnOversizedDownload pins the cap that stops a redirected or
// hostile response from writing until the disk fills. The body is plain bytes,
// since the size check fires while the response is still being read.
//
//nolint:paralleltest // writes an executable then runs it; a concurrent fork trips ETXTBSY (Go issue 22315)
func TestInstallRefusesAnOversizedDownload(t *testing.T) {
	url, client := serveBody(t, archiveServer(bytes.Repeat([]byte("a"), 4096)))

	opts := &bwinstall.Options{
		URL:             url,
		Client:          client,
		ArchiveSHA256:   wrongDigest,
		BinarySHA256:    wrongDigest,
		Version:         fakeVersion,
		Dest:            filepath.Join(t.TempDir(), entryName),
		MaxArchiveBytes: 512,
	}

	_, err := bwinstall.Install(t.Context(), opts)
	if err == nil {
		t.Fatal("install accepted a response past the size limit")
	}

	if !messageMentions(t, err, "larger than") {
		t.Fatalf("the error does not report a size limit: %v", err)
	}

	assertNothingLeftBehind(t, opts.Dest)
}

// TestInstallRefusesAnOversizedEntry pins the other cap. The archive is small
// and its digest matches, so only the decompressed size can stop this, which is
// the zip-bomb shape. The empty directory also proves the binary is assembled
// elsewhere and renamed in.
//
//nolint:paralleltest // writes an executable then runs it; a concurrent fork trips ETXTBSY (Go issue 22315)
func TestInstallRefusesAnOversizedEntry(t *testing.T) {
	opts := options(t, map[string]string{entryName: strings.Repeat("a", 8192)}, "")
	opts.MaxBinaryBytes = 512

	_, err := bwinstall.Install(t.Context(), opts)
	if err == nil {
		t.Fatal("install unpacked an entry past the size limit")
	}

	if !messageMentions(t, err, "larger than") {
		t.Fatalf("the error does not report a size limit: %v", err)
	}

	assertNothingLeftBehind(t, opts.Dest)
}

// TestInstallRefusesANonOKResponse covers a release host answering with an
// error page. Without the status check the body would flow into the digest
// comparison and fail there, which reports the wrong problem.
//
//nolint:paralleltest // writes an executable then runs it; a concurrent fork trips ETXTBSY (Go issue 22315)
func TestInstallRefusesANonOKResponse(t *testing.T) {
	url, client := serveBody(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "gone", http.StatusInternalServerError)
	}))

	opts := &bwinstall.Options{
		URL:           url,
		Client:        client,
		ArchiveSHA256: wrongDigest,
		BinarySHA256:  wrongDigest,
		Version:       fakeVersion,
		Dest:          filepath.Join(t.TempDir(), entryName),
	}

	_, err := bwinstall.Install(t.Context(), opts)
	if err == nil {
		t.Fatal("install accepted an error response")
	}

	if !messageMentions(t, err, "HTTP 500") {
		t.Fatalf("the error does not report the status: %v", err)
	}

	assertNothingLeftBehind(t, opts.Dest)
}

// TestInstallRefusesAPlainHTTPSource keeps the package's promise true for the
// next call site: nothing authenticates where bytes from an http URL came from.
//
//nolint:paralleltest // writes an executable then runs it; a concurrent fork trips ETXTBSY (Go issue 22315)
func TestInstallRefusesAPlainHTTPSource(t *testing.T) {
	opts := options(t, map[string]string{entryName: fakeScript}, "")
	opts.URL = "http://bwinstall.test/bw.zip"

	if _, err := bwinstall.Install(t.Context(), opts); err == nil {
		t.Fatal("install accepted a plain http source")
	}
}

// TestInstallStopsAProbeThatHangs pins the probe's timeout. A bw that never
// answers --version would otherwise hold the install open with nothing on
// screen.
//
//nolint:paralleltest // writes an executable then runs it; a concurrent fork trips ETXTBSY (Go issue 22315)
func TestInstallStopsAProbeThatHangs(t *testing.T) {
	const sleeper = "#!/bin/sh\nsleep 120\n"

	opts := options(t, map[string]string{entryName: sleeper}, "")
	opts.ProbeTimeout = 200 * time.Millisecond

	started := time.Now()

	_, err := bwinstall.Install(t.Context(), opts)
	if err == nil {
		t.Fatal("install waited out a binary that never answered")
	}

	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Fatalf("the probe ran for %s, so its timeout is not bounding it", elapsed)
	}
}

// TestInstallLeavesTheProbeWithoutVaultCredentials covers the child's
// environment. `secrets auth` reads the vault API key and master password from
// this process when a host injects them, and a version probe has no business
// seeing either. The staged fake reports the marker, so its output says what the
// probe was handed.
//
// No paralleltest directive: t.Setenv already rules the case out of parallel.
func TestInstallLeavesTheProbeWithoutVaultCredentials(t *testing.T) {
	const marker = "BW_PROBE_MARKER"

	t.Setenv(marker, "should-not-reach-the-child")

	reporter := "#!/bin/sh\necho \"${" + marker + ":-" + fakeVersion + "}\"\n"

	opts := options(t, map[string]string{entryName: reporter}, "")

	result, err := bwinstall.Install(t.Context(), opts)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	if result.Version != fakeVersion {
		t.Fatalf("the probe passed %s through to the child: %q", marker, result.Version)
	}
}

// TestInstallLeavesAMatchingBinaryAlone keeps a re-run cheap, and does it by
// hashing the file rather than asking it what it is. The record still gets
// written: every host installed its bw before the record existed, and this skip
// is the path an ordinary re-run takes, so it is the one chance to bootstrap
// one before the pin next moves.
//
//nolint:paralleltest // writes an executable then runs it; a concurrent fork trips ETXTBSY (Go issue 22315)
func TestInstallLeavesAMatchingBinaryAlone(t *testing.T) {
	opts := options(t, map[string]string{entryName: fakeScript}, "")
	stageBinary(t, opts.Dest, fakeScript)

	opts.URL = unreachableSource

	result, err := bwinstall.Install(t.Context(), opts)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	if result.Installed {
		t.Fatal("install replaced a binary that was already the pinned one")
	}

	assertRecorded(t, opts.Dest, opts.BinarySHA256)
}

// TestInstallRefusesAnImpostorThatPrintsTheRightVersion is the reason the skip
// hashes the file. A wrapper that answers --version with the pinned string and
// forwards everything else would otherwise be left in place forever, holding
// the vault API key and the master password every time the CLI runs.
//
//nolint:paralleltest // writes an executable then runs it; a concurrent fork trips ETXTBSY (Go issue 22315)
func TestInstallRefusesAnImpostorThatPrintsTheRightVersion(t *testing.T) {
	opts := options(t, map[string]string{entryName: fakeScript}, "")
	stageBinary(t, opts.Dest, "#!/bin/sh\necho "+fakeVersion+"\n# and then something else\n")

	opts.URL = unreachableSource

	_, err := bwinstall.Install(t.Context(), opts)
	if err == nil {
		t.Fatal("install accepted a binary that only prints the right version")
	}

	for _, want := range []string{opts.Dest, "--force"} {
		if !messageMentions(t, err, want) {
			t.Fatalf("the error does not mention %q: %v", want, err)
		}
	}
}

// TestInstallReplacesOnForce is what --force is for: the operator said to take
// the pinned release over whatever is there.
//
//nolint:paralleltest // writes an executable then runs it; a concurrent fork trips ETXTBSY (Go issue 22315)
func TestInstallReplacesOnForce(t *testing.T) {
	opts := options(t, map[string]string{entryName: fakeScript}, "")
	opts.Force = true

	stageBinary(t, opts.Dest, "#!/bin/sh\necho 2020.1.0\n")

	result, err := bwinstall.Install(t.Context(), opts)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	if !result.Installed || result.Version != fakeVersion {
		t.Fatalf("result = %+v, want the pinned version installed", result)
	}
}

// TestInstallRefusesAWorldWritableBinDir covers the directory the binary lands
// in. Verifying the artifact at install time proves nothing if anyone else can
// swap it afterwards.
//
//nolint:paralleltest // writes an executable then runs it; a concurrent fork trips ETXTBSY (Go issue 22315)
func TestInstallRefusesAWorldWritableBinDir(t *testing.T) {
	opts := options(t, map[string]string{entryName: fakeScript}, "")

	dir := filepath.Dir(opts.Dest)
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("loosen the install directory: %v", err)
	}

	_, err := bwinstall.Install(t.Context(), opts)
	if err == nil {
		t.Fatal("install wrote into a directory anyone can replace files in")
	}

	if !messageMentions(t, err, dir) {
		t.Fatalf("the error does not name the directory: %v", err)
	}
}

// TestInstallAcceptsAUserPrivateGroupBinDir covers the stock Ubuntu layout.
// ~/.local/bin arrives as 0775 owned by a group named after the account and
// holding only that account, so refusing the group bit there would fail the
// install on every default host while protecting nothing.
//
//nolint:paralleltest // writes an executable then runs it; a concurrent fork trips ETXTBSY (Go issue 22315)
func TestInstallAcceptsAUserPrivateGroupBinDir(t *testing.T) {
	requirePrivateGroup(t)

	opts := options(t, map[string]string{entryName: fakeScript}, "")

	dir := filepath.Dir(opts.Dest)
	if err := os.Chmod(dir, 0o775); err != nil {
		t.Fatalf("set the install directory to the stock bin-dir mode: %v", err)
	}

	result, err := bwinstall.Install(t.Context(), opts)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	if !result.Installed {
		t.Fatalf("install wrote nothing into %s, want the pinned binary", dir)
	}
}

// requirePrivateGroup skips unless the account running the test sits in the
// user-private-group layout the acceptance case is about. CI accounts vary: a
// runner whose primary group is shared, such as a user in "staff" or "users",
// is a directory the installer is supposed to keep refusing.
func requirePrivateGroup(t *testing.T) {
	t.Helper()

	current, err := user.Current()
	if err != nil {
		t.Skipf("cannot read the current account: %v", err)
	}

	group, err := user.LookupGroupId(current.Gid)
	if err != nil {
		t.Skipf("cannot read group %s: %v", current.Gid, err)
	}

	if group.Name != current.Username {
		t.Skipf("account %q has primary group %q, so this host has no user-private group",
			current.Username, group.Name)
	}
}

// messageMentions reports whether the failure's own message carries the text.
// It reads the structured message rather than the rendered error, so the code
// prefix never satisfies a check about what the message says.
func messageMentions(t *testing.T, err error, want string) bool {
	t.Helper()

	structured, isStructured := errors.AsType[*cairnerr.Error](err)
	if !isStructured {
		t.Fatalf("error %v is not a cairnerr.Error", err)
	}

	return strings.Contains(structured.Message, want)
}

// stageRecord writes the digest record an earlier install would have left, so a
// case can say what this tool believes it put there.
func stageRecord(t *testing.T, dest, digest string) {
	t.Helper()

	if err := os.WriteFile(dest+sidecarSuffix, []byte(digest+"\n"), sidecarPerm); err != nil {
		t.Fatalf("stage a digest record beside %s: %v", dest, err)
	}
}

// assertRecorded proves the record beside the binary names the digest given.
func assertRecorded(t *testing.T, dest, want string) {
	t.Helper()

	body, err := os.ReadFile(dest + sidecarSuffix) // #nosec G304 -- the path is built by the test
	if err != nil {
		t.Fatalf("read the digest record: %v", err)
	}

	if recorded := strings.TrimSpace(string(body)); !strings.EqualFold(recorded, want) {
		t.Fatalf("recorded digest = %q, want %q", recorded, want)
	}
}

// stageBinary puts a runnable stand-in at the destination.
func stageBinary(t *testing.T, path, script string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(script), installPerm); err != nil {
		t.Fatalf("stage a binary at %s: %v", path, err)
	}
}

// assertHolds proves the directory holds exactly the named files and nothing
// else, which is what catches a temp file or a downloaded archive left behind.
func assertHolds(t *testing.T, dir string, want ...string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the install directory: %v", err)
	}

	found := entryNamesOf(entries)

	slices.Sort(found)
	slices.Sort(want)

	if !slices.Equal(found, want) {
		t.Fatalf("install directory holds %v, want %v", found, want)
	}
}

// assertNothingLeftBehind proves a refused install wrote neither the binary nor
// a temp file next to it.
func assertNothingLeftBehind(t *testing.T, dest string) {
	t.Helper()

	if _, err := os.Stat(dest); err == nil {
		t.Fatalf("a refused install wrote %s", dest)
	}

	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		t.Fatalf("read the install directory: %v", err)
	}

	if len(entries) != 0 {
		t.Fatalf("a refused install left %v behind", entryNamesOf(entries))
	}
}

// entryNamesOf renders a directory listing for a failure message.
func entryNamesOf(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	return names
}
