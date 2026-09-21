package bwpin_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chadit/CairnOS/internal/bwpin"
	"github.com/chadit/CairnOS/internal/nettest"
)

// The version already pinned in the fixture file, the one the fake feed is
// newest at, and the body the fixture archive carries.
const (
	pinnedVersion = "2026.8.0"
	newerVersion  = "2026.10.0"
	binaryBody    = "#!/bin/sh\necho 2026.10.0\n"
)

// entryName is the one file a release archive holds, spelled out here rather
// than read from the package, so a case fails if that name ever changes.
const entryName = "bw"

// downloadPrefix is the path the fake release host serves archives under, and
// emptyPage the body the API answers a page past the end with.
const (
	downloadPrefix = "/download/"
	emptyPage      = "[]"
)

// plaintextAsset is where the downgrade case redirects an asset request. The
// guard refuses the hop on its scheme, so nothing ever has to serve this.
const plaintextAsset = "http://bwpin.test/download/plaintext.zip"

// TestRunRewritesThePinForANewerRelease is the whole point of the tool: the
// feed has moved past the file, so both archives are hashed and the four
// digests land in the generated file.
func TestRunRewritesThePinForANewerRelease(t *testing.T) {
	t.Parallel()

	archive, archiveSum := buildZip(t, entryName)
	out := stagePinFile(t)

	cfg := serveRelease(t, feed(
		feedEntry{TagName: "web-v2027.1.0"},
		feedEntry{TagName: "cli-v" + newerVersion},
		feedEntry{TagName: "cli-v" + pinnedVersion},
	), archive)
	cfg.Out = out

	if err := bwpin.Run(t.Context(), cfg); err != nil {
		t.Fatalf("run: %v", err)
	}

	rendered := readFile(t, out)

	for _, want := range []string{newerVersion, archiveSum, digestOf(binaryBody)} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("the rewritten pin does not carry %q:\n%s", want, rendered)
		}
	}

	if version := versionIn(t, out); version != newerVersion {
		t.Fatalf("rewritten version = %q, want %q", version, newerVersion)
	}
}

// TestRunPrefersTheHighestVersionNotTheNewestEntry covers the calendar-shaped
// numbering. A string compare puts 2026.10.0 below 2026.9.0, and the feed is
// not guaranteed to list the CLI releases in any particular order.
func TestRunPrefersTheHighestVersionNotTheNewestEntry(t *testing.T) {
	t.Parallel()

	archive, _ := buildZip(t, entryName)
	out := stagePinFile(t)

	cfg := serveRelease(t, feed(
		feedEntry{TagName: "cli-v2026.9.0"},
		feedEntry{TagName: "cli-v" + newerVersion},
		feedEntry{TagName: "cli-v2027.1.0", Draft: true},
		feedEntry{TagName: "cli-v2027.2.0", Prerelease: true},
	), archive)
	cfg.Out = out

	if err := bwpin.Run(t.Context(), cfg); err != nil {
		t.Fatalf("run: %v", err)
	}

	if version := versionIn(t, out); version != newerVersion {
		t.Fatalf("picked %q, want the highest finished release %q", version, newerVersion)
	}
}

// TestRunLeavesACurrentPinAlone is what an operator sees most runs. Nothing is
// downloaded and the file keeps its bytes, so a bump that had nothing to do
// leaves no diff to review.
func TestRunLeavesACurrentPinAlone(t *testing.T) {
	t.Parallel()

	archive, _ := buildZip(t, entryName)
	out := stagePinFile(t)
	before := readFile(t, out)

	var downloads atomic.Int64

	cfg := serveRelease(t, feed(feedEntry{TagName: "cli-v" + pinnedVersion}), archive)
	cfg.Out = out
	cfg.Client = countingClient(cfg.Client, &downloads)

	if err := bwpin.Run(t.Context(), cfg); err != nil {
		t.Fatalf("run: %v", err)
	}

	if after := readFile(t, out); after != before {
		t.Fatalf("a current pin was rewritten:\n%s", after)
	}

	if hits := downloads.Load(); hits != 0 {
		t.Fatalf("a current pin fetched %d assets, want none", hits)
	}
}

// TestRunRefusesAnArchiveThatIsNotOneBinary covers a release layout change. A
// digest taken from the wrong entry would describe a file the installer never
// unpacks, and every host would then refuse the download it just verified.
func TestRunRefusesAnArchiveThatIsNotOneBinary(t *testing.T) {
	t.Parallel()

	archive, _ := buildZip(t, "bw-oss")
	out := stagePinFile(t)

	cfg := serveRelease(t, feed(feedEntry{TagName: "cli-v" + newerVersion}), archive)
	cfg.Out = out

	err := bwpin.Run(t.Context(), cfg)
	if !errors.Is(err, bwpin.ErrArchiveLayout) {
		t.Fatalf("run accepted an archive holding something other than bw: %v", err)
	}

	if version := versionIn(t, out); version != pinnedVersion {
		t.Fatalf("a failed bump left the file at %q, want the previous pin %q", version, pinnedVersion)
	}
}

// TestRunRefusesARedirectOffHTTPS is what the scheme check on the built URL
// cannot cover. These bytes become the digests every host installs without
// asking, so a hop into plaintext has to stop the bump rather than be hashed.
func TestRunRefusesARedirectOffHTTPS(t *testing.T) {
	t.Parallel()

	out := stagePinFile(t)

	cfg := serveDowngrade(t, feed(feedEntry{TagName: "cli-v" + newerVersion}))
	cfg.Out = out

	if err := bwpin.Run(t.Context(), cfg); !errors.Is(err, bwpin.ErrNotHTTPS) {
		t.Fatalf("run followed a redirect out of https: %v", err)
	}

	if version := versionIn(t, out); version != pinnedVersion {
		t.Fatalf("a refused bump left the file at %q, want the previous pin %q", version, pinnedVersion)
	}
}

// TestRunReportsAFeedWithNoCLIRelease covers the tag pattern moving. Hashing
// whatever is newest in that case would pin a desktop or web build.
func TestRunReportsAFeedWithNoCLIRelease(t *testing.T) {
	t.Parallel()

	archive, _ := buildZip(t, entryName)
	out := stagePinFile(t)

	cfg := serveRelease(t, feed(feedEntry{TagName: "web-v2027.1.0"}), archive)
	cfg.Out = out

	if err := bwpin.Run(t.Context(), cfg); !errors.Is(err, bwpin.ErrNoCLIRelease) {
		t.Fatalf("run accepted a feed carrying no CLI release: %v", err)
	}
}

// feedEntry is one release as the GitHub API renders it. It is spelled out here
// rather than taken from the package, so a case serves the wire shape the tool
// has to read rather than whatever struct it happens to parse into.
type feedEntry struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// feed renders one page of the release list, and an empty second page, the way
// the API answers when there is nothing more.
func feed(releases ...feedEntry) map[string]string {
	return map[string]string{"1": encode(releases), "2": emptyPage}
}

// encode renders a fixture through encoding/json rather than by concatenation.
func encode(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return emptyPage
	}

	return string(raw)
}

// serveRelease stands the feed and both platform assets up on one loopback
// server and returns a bwpin.Config pointed at it. The tool refuses anything but
// https, so the URLs keep that scheme and the client dials the listener.
func serveRelease(t *testing.T, pages map[string]string, archive []byte) bwpin.Config {
	t.Helper()

	return pointAt(releaseHandler(pages, archive))
}

// serveDowngrade answers the feed normally and sends every asset request to a
// plaintext URL, which is the hop the redirect guard has to stop.
func serveDowngrade(t *testing.T, pages map[string]string) bwpin.Config {
	t.Helper()

	return pointAt(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, downloadPrefix) {
			http.Redirect(writer, request, plaintextAsset, http.StatusFound)

			return
		}

		feedHandler(pages).ServeHTTP(writer, request)
	}))
}

// pointAt returns a bwpin.Config whose client answers from handler in this
// process. The tool refuses anything but https, and the fixture transport
// serves the handler whatever the URL says, so the scheme check still sees
// https.
func pointAt(handler http.Handler) bwpin.Config {
	return bwpin.Config{
		Client:   nettest.Client(handler),
		Logger:   quietLogger(),
		API:      "https://bwpin.test",
		Download: "https://bwpin.test/download",
	}
}

// releaseHandler answers the two endpoints the tool drives: the paged release
// list, and the asset for either platform.
func releaseHandler(pages map[string]string, archive []byte) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, downloadPrefix) {
			_, _ = writer.Write(archive)

			return
		}

		feedHandler(pages).ServeHTTP(writer, request)
	})
}

// feedHandler answers one page of the release list, and an empty body for a
// page the case did not stage.
func feedHandler(pages map[string]string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, served := pages[request.URL.Query().Get("page")]
		if !served {
			body = emptyPage
		}

		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(body))
	})
}

// countingClient wraps a client and counts asset fetches, which is how a case
// proves an unchanged pin downloaded nothing.
func countingClient(inner *http.Client, hits *atomic.Int64) *http.Client {
	return &http.Client{Transport: &countingTransport{inner: inner.Transport, hits: hits}}
}

// countingTransport counts requests for a release asset and passes everything
// through.
type countingTransport struct {
	inner http.RoundTripper
	hits  *atomic.Int64
}

func (t *countingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if strings.HasPrefix(request.URL.Path, downloadPrefix) {
		t.hits.Add(1)
	}

	//nolint:wrapcheck // a test transport: the error goes straight back to the client
	return t.inner.RoundTrip(request)
}

// buildZip renders an archive holding one named entry, and returns its bytes
// with their digest, so a case can serve a body whose checksum it knows.
func buildZip(t *testing.T, name string) ([]byte, string) {
	t.Helper()

	var body bytes.Buffer

	archive := zip.NewWriter(&body)

	entry, err := archive.Create(name)
	if err != nil {
		t.Fatalf("add %s to the archive: %v", name, err)
	}

	if _, err := entry.Write([]byte(binaryBody)); err != nil {
		t.Fatalf("write %s into the archive: %v", name, err)
	}

	if err := archive.Close(); err != nil {
		t.Fatalf("close the archive: %v", err)
	}

	return body.Bytes(), digestOf(body.String())
}

// digestOf hashes one string the way the tool hashes bytes.
func digestOf(content string) string {
	sum := sha256.Sum256([]byte(content))

	return hex.EncodeToString(sum[:])
}

// stagePinFile writes a generated file at the version every case starts from,
// so the run has a pin to read before it decides anything. It is a literal
// rather than a render, so a case that asserts on the rewritten file is not
// comparing the renderer against itself.
func stagePinFile(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "bwpin_gen.go")

	body := "// Code generated by tools/bwpin. DO NOT EDIT.\n\n" +
		"package cli\n\nconst (\n" +
		"\tbwVersion = \"" + pinnedVersion + "\"\n\n" +
		"\tbwLinuxArchiveSHA256 = \"" + digestOf("linux-archive") + "\"\n" +
		"\tbwLinuxBinarySHA256  = \"" + digestOf("linux-binary") + "\"\n\n" +
		"\tbwMacOSArchiveSHA256 = \"" + digestOf("macos-archive") + "\"\n" +
		"\tbwMacOSBinarySHA256  = \"" + digestOf("macos-binary") + "\"\n)\n"

	if err := os.WriteFile(path, []byte(body), pinFilePerm); err != nil {
		t.Fatalf("stage a pin file: %v", err)
	}

	return path
}

// pinFilePerm is the mode the staged fixture carries; the package rewrites it
// whatever it arrives as.
const pinFilePerm = 0o600

// versionIn reads the version the file on disk pins, through the same reader
// the tool uses to decide whether there is anything to do.
func versionIn(t *testing.T, path string) string {
	t.Helper()

	version, err := bwpin.CurrentVersion(path)
	if err != nil {
		t.Fatalf("read the pinned version: %v", err)
	}

	return version
}

// readFile reads a fixture the test wrote.
func readFile(t *testing.T, path string) string {
	t.Helper()

	body, err := os.ReadFile(path) // #nosec G304 -- the path is built by the test
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(body)
}

// quietLogger keeps the tool's progress lines out of the test output.
func quietLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}
