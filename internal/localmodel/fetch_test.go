package localmodel_test

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/localmodel"
	"github.com/chadit/CairnOS/internal/nettest"
)

// artifactName labels the fixture artifact in progress output and errors.
const artifactName = "weights"

// unreachableOrigin is a loopback port nothing listens on, for cases that must
// never get an answer from the network.
const unreachableOrigin = "http://127.0.0.1:1"

// TestFetchWritesVerifiedFile proves a download lands only after its digest
// matches the pin.
func TestFetchWritesVerifiedFile(t *testing.T) {
	t.Parallel()

	payload := []byte("model weights")
	server := serveBytes(t, payload)
	dest := filepath.Join(t.TempDir(), "weights.gguf")

	art := localmodel.Artifact{Name: artifactName, URL: server.URL, SHA256: digestOf(payload), Bytes: int64(len(payload))}

	result, err := localmodel.Fetch(t.Context(), &art, dest, localmodel.FetchOptions{Client: server.Client()})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if result.Skipped {
		t.Error("first fetch reported a skip")
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}

	if !bytes.Equal(got, payload) {
		t.Errorf("content = %q, want %q", got, payload)
	}
}

// TestFetchSkipsVerifiedFileWithoutDownloading proves a re-run leaves a good
// file alone: the server records no second request.
func TestFetchSkipsVerifiedFileWithoutDownloading(t *testing.T) {
	t.Parallel()

	payload := []byte("model weights")
	dest := filepath.Join(t.TempDir(), "weights.gguf")

	if err := os.WriteFile(dest, payload, 0o600); err != nil {
		t.Fatalf("seed dest: %v", err)
	}

	var hits int

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++

		writeResponse(t, w, payload)
	}))

	art := localmodel.Artifact{Name: artifactName, URL: server.URL, SHA256: digestOf(payload)}

	result, err := localmodel.Fetch(t.Context(), &art, dest, localmodel.FetchOptions{Client: server.Client()})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if !result.Skipped {
		t.Error("second fetch did not report a skip")
	}

	if hits != 0 {
		t.Errorf("server saw %d requests, want 0", hits)
	}
}

// TestFetchRejectsWrongDigestAndLeavesNothing proves a corrupt download never
// reaches the destination, so a later run cannot mistake it for installed.
func TestFetchRejectsWrongDigestAndLeavesNothing(t *testing.T) {
	t.Parallel()

	server := serveBytes(t, []byte("tampered"))
	dest := filepath.Join(t.TempDir(), "weights.gguf")

	art := localmodel.Artifact{Name: artifactName, URL: server.URL, SHA256: digestOf([]byte("expected"))}

	_, err := localmodel.Fetch(t.Context(), &art, dest, localmodel.FetchOptions{Client: server.Client()})
	if !errors.Is(err, localmodel.ErrHashMismatch) {
		t.Fatalf("err = %v, want localmodel.ErrHashMismatch", err)
	}

	if _, statErr := os.Stat(dest); statErr == nil {
		t.Error("a failed download left a file behind")
	}
}

// TestFetchOfflineReportsMissingCache proves offline mode names the problem it
// cannot fix rather than reaching the network anyway.
func TestFetchOfflineReportsMissingCache(t *testing.T) {
	t.Parallel()

	art := localmodel.Artifact{Name: artifactName, URL: unreachableOrigin + "/never", SHA256: digestOf(nil)}

	_, err := localmodel.Fetch(t.Context(), &art, filepath.Join(t.TempDir(), "weights.gguf"), localmodel.FetchOptions{Offline: true})
	if !errors.Is(err, localmodel.ErrOffline) {
		t.Fatalf("err = %v, want localmodel.ErrOffline", err)
	}
}

// TestFetchOfflineReportsCorruptCache proves a present but wrong cached file
// is reported as a verification failure, not as a missing file.
func TestFetchOfflineReportsCorruptCache(t *testing.T) {
	t.Parallel()

	dest := filepath.Join(t.TempDir(), "weights.gguf")
	if err := os.WriteFile(dest, []byte("corrupt"), 0o600); err != nil {
		t.Fatalf("seed dest: %v", err)
	}

	art := localmodel.Artifact{Name: artifactName, URL: unreachableOrigin + "/never", SHA256: digestOf([]byte("expected"))}

	_, err := localmodel.Fetch(t.Context(), &art, dest, localmodel.FetchOptions{Offline: true})
	if !errors.Is(err, localmodel.ErrHashMismatch) {
		t.Fatalf("err = %v, want localmodel.ErrHashMismatch", err)
	}
}

// TestFetchReportsBadStatus proves an error page is refused rather than saved
// as if it were the artifact.
func TestFetchReportsBadStatus(t *testing.T) {
	t.Parallel()

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))

	art := localmodel.Artifact{Name: artifactName, URL: server.URL, SHA256: digestOf(nil)}

	_, err := localmodel.Fetch(t.Context(), &art, filepath.Join(t.TempDir(), "w.gguf"), localmodel.FetchOptions{Client: server.Client()})
	if !errors.Is(err, localmodel.ErrUnexpectedStatus) {
		t.Fatalf("err = %v, want localmodel.ErrUnexpectedStatus", err)
	}
}

// serveBytes answers every request with the same payload.
func serveBytes(t *testing.T, payload []byte) *nettest.Fake {
	t.Helper()

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeResponse(t, w, payload)
	}))

	return server
}

// TestProgressStopsAtCompletion proves the percentage line closes once and
// stays closed. A server that sends more than the pinned size keeps the
// percentage at or past 100, and an unlatched writer would print a newline for
// every chunk after that, turning the tail of a download into blank lines.
func TestProgressStopsAtCompletion(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	writer := localmodel.NewProgressWriter(&out, artifactName, 10)

	for range 8 {
		if _, err := writer.Write([]byte("xxxxx")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	if got := strings.Count(out.String(), "\n"); got != 1 {
		t.Errorf("progress printed %d newlines, want 1:\n%q", got, out.String())
	}
}

// TestProgressReportsIntermediateSteps proves the latch did not silence the
// progress it exists to throttle.
func TestProgressReportsIntermediateSteps(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	writer := localmodel.NewProgressWriter(&out, artifactName, 100)

	for range 10 {
		if _, err := writer.Write(make([]byte, 10)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	for _, want := range []string{"10%", "50%", "100%"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("progress is missing %s:\n%q", want, out.String())
		}
	}
}

// errProgressGone stands in for a progress stream that went away mid-download.
var errProgressGone = errors.New("progress stream closed")

// refusingWriter fails every write and counts the attempts.
type refusingWriter struct {
	attempts int
}

func (w *refusingWriter) Write([]byte) (int, error) {
	w.attempts++

	return 0, errProgressGone
}

// TestProgressStopsPrintingAfterAFailedLine proves a progress stream that
// refuses a line never fails the download and is not written to again.
func TestProgressStopsPrintingAfterAFailedLine(t *testing.T) {
	t.Parallel()

	const chunkBytes = 10

	refusing := &refusingWriter{}
	writer := localmodel.NewProgressWriter(refusing, artifactName, 100)

	for range 10 {
		count, err := writer.Write(make([]byte, chunkBytes))
		if err != nil || count != chunkBytes {
			t.Fatalf("write = %d, %v; want %d, nil", count, err, chunkBytes)
		}
	}

	if refusing.attempts != 1 {
		t.Errorf("progress attempted %d lines, want only the 1 that failed", refusing.attempts)
	}
}
