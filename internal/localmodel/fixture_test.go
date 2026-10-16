package localmodel_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"testing"

	"github.com/chadit/CairnOS/internal/localmodel"
)

// fixtureEntry is one member of a synthetic release archive. Link, when set,
// makes the entry a symlink to that sibling name instead of a regular file.
// Raw names an entry whose path is written verbatim, skipping the wrapper
// directory the tarball otherwise puts every member under.
type fixtureEntry struct {
	Name    string
	Content string
	Link    string
	Raw     bool
}

// tarGzFixture builds a gzipped tarball shaped like a llama.cpp POSIX release:
// every member under the same wrapper directory the real assets use, with the
// versioned-soname symlinks they carry.
func tarGzFixture(t *testing.T, entries []fixtureEntry) []byte {
	t.Helper()

	var raw bytes.Buffer

	zipped := gzip.NewWriter(&raw)
	writer := tar.NewWriter(zipped)

	for _, entry := range entries {
		name := localmodel.ArchivePrefix + entry.Name
		if entry.Raw {
			name = entry.Name
		}

		writeTarFixtureEntry(t, writer, name, entry)
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	if err := zipped.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	return raw.Bytes()
}

// writeTarFixtureEntry writes one fixture member, as a symlink or a file.
func writeTarFixtureEntry(t *testing.T, writer *tar.Writer, name string, entry fixtureEntry) {
	t.Helper()

	if err := writer.WriteHeader(tarFixtureHeader(name, entry)); err != nil {
		t.Fatalf("write header %s: %v", name, err)
	}

	if entry.Link != "" {
		return
	}

	if _, err := writer.Write([]byte(entry.Content)); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// tarFixtureHeader shapes one member header: a symlink carries its target in
// Linkname and no body, while a regular file has to declare its size up front.
func tarFixtureHeader(name string, entry fixtureEntry) *tar.Header {
	header := &tar.Header{Name: name, Mode: 0o755}

	if entry.Link != "" {
		header.Typeflag = tar.TypeSymlink
		header.Linkname = entry.Link

		return header
	}

	header.Typeflag = tar.TypeReg
	header.Size = int64(len(entry.Content))

	return header
}

// zipFixture builds a flat zip shaped like the Windows release asset. An entry
// with Link set becomes a symlink-mode member whose body is the link text,
// which is how a zip records a symlink.
func zipFixture(t *testing.T, entries []fixtureEntry) []byte {
	t.Helper()

	var raw bytes.Buffer

	writer := zip.NewWriter(&raw)

	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.Name, Method: zip.Deflate}
		body := entry.Content

		if entry.Link != "" {
			header.SetMode(os.ModeSymlink | 0o777)

			body = entry.Link
		}

		member, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatalf("create %s: %v", entry.Name, err)
		}

		if _, err := member.Write([]byte(body)); err != nil {
			t.Fatalf("write %s: %v", entry.Name, err)
		}
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	return raw.Bytes()
}

// digestOf is the lowercase hex SHA-256 a fixture pins itself to.
func digestOf(payload []byte) string {
	sum := sha256.Sum256(payload)

	return hex.EncodeToString(sum[:])
}

// writeResponse sends body from a fixture handler and fails the test when the
// write does not go through. It reports with Errorf, since a handler can run
// off the test goroutine where Fatalf must not be called.
func writeResponse(t *testing.T, w http.ResponseWriter, body []byte) {
	t.Helper()

	if _, err := w.Write(body); err != nil {
		t.Errorf("write fixture response: %v", err)
	}
}
