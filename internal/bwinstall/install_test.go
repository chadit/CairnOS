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
	"path/filepath"
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

// installPerm is the mode an installed binary carries, and entryName the one
// file the release archive holds.
const (
	installPerm = 0o755
	entryName   = "bw"
)

// These cases do not run in parallel. Each one writes an executable and then
// runs it, and a fork from another goroutine between the write and the exec is
// what produces ETXTBSY. One install per process is the real shape anyway.

// wrongDigest is a well-formed sha256 that matches nothing.
const wrongDigest = "0000000000000000000000000000000000000000000000000000000000000000"

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
// https form and the test's own client is pointed back at the plain listener.
func serveBody(t *testing.T, handler http.Handler) (string, *http.Client) {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	// The client dials the real listener whatever the URL says, so the scheme
	// check sees https while the request still reaches the test server.
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
// one entry comes out, and the directory is left holding only the binary.
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

	// The downloaded archive is a temp file in this directory, so a cleanup
	// that stopped running would leave the release zip behind next to the
	// binary.
	assertOnlyEntry(t, filepath.Dir(opts.Dest), entryName)
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
// hostile response from writing until the disk fills.
//
//nolint:paralleltest // writes an executable then runs it; a concurrent fork trips ETXTBSY (Go issue 22315)
func TestInstallRefusesAnOversizedDownload(t *testing.T) {
	// A plain body rather than an archive: the size check fires while the
	// response is still being read, long before anything looks at it as a zip.
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
// the zip-bomb shape.
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

	// The write had already started when the limit hit, so this is also what
	// proves the binary is assembled somewhere else and renamed into place.
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
// environment. `secrets auth` reads the vault API key and the master password
// out of this process's environment when a host injects them, and a version
// probe has no business seeing either.
//
// This one needs no paralleltest directive: t.Setenv already rules the case out
// of running in parallel, so the linter never asks.
func TestInstallLeavesTheProbeWithoutVaultCredentials(t *testing.T) {
	const marker = "BW_PROBE_MARKER"

	t.Setenv(marker, "should-not-reach-the-child")

	// The fake reports whatever the marker holds, so the installed binary's own
	// output says what the probe handed it.
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
// hashing the file rather than asking it what it is.
//
//nolint:paralleltest // writes an executable then runs it; a concurrent fork trips ETXTBSY (Go issue 22315)
func TestInstallLeavesAMatchingBinaryAlone(t *testing.T) {
	opts := options(t, map[string]string{entryName: fakeScript}, "")
	stageBinary(t, opts.Dest, fakeScript)

	// A URL nothing serves, so a download would fail the case rather than pass
	// it quietly.
	opts.URL = "https://127.0.0.1:1/bw.zip"

	result, err := bwinstall.Install(t.Context(), opts)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	if result.Installed {
		t.Fatal("install replaced a binary that was already the pinned one")
	}
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

	// The download is unreachable, so a run that gets past the skip fails here
	// rather than quietly reinstalling and hiding the point of the case.
	opts.URL = "https://127.0.0.1:1/bw.zip"

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

// stageBinary puts a runnable stand-in at the destination.
func stageBinary(t *testing.T, path, script string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(script), installPerm); err != nil {
		t.Fatalf("stage a binary at %s: %v", path, err)
	}
}

// assertOnlyEntry proves the directory holds exactly the one file.
func assertOnlyEntry(t *testing.T, dir, name string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the install directory: %v", err)
	}

	if len(entries) != 1 || entries[0].Name() != name {
		t.Fatalf("install directory holds %v, want just %q", entryNamesOf(entries), name)
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
