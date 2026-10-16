package localmodel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// downloadTimeout bounds one file download. The shipped model is 4.6 GB, which
// a 10 Mbit link takes about an hour to pull, so the ceiling is generous
// rather than tight: a download that dies halfway leaves nothing installed and
// the user has to start over.
const downloadTimeout = 2 * time.Hour

// Progress reporting throttle: one line every two percent keeps a multi-gigabyte
// download legible without flooding stderr.
const (
	percentScale        = 100
	progressStepPercent = 2
)

// Artifact is one verified download: where it comes from, what it should hash
// to, and how big it is. Both the release archive and the GGUF go through it,
// so there is one place that knows how to stream, hash, and commit a file.
type Artifact struct {
	// Name labels the artifact in progress output and error messages.
	Name string
	// URL is the source, already built from the pinned registry.
	URL string
	// SHA256 is the pinned lowercase hex digest the download must match.
	SHA256 string
	// Bytes is the expected size, used for the progress percentage. Zero
	// disables the percentage and prints nothing.
	Bytes int64
}

// FetchOptions controls how Fetch acquires an artifact.
type FetchOptions struct {
	// Progress, when non-nil, receives human-readable percent lines.
	Progress io.Writer

	// Client overrides the HTTP client; nil uses one bounded by
	// downloadTimeout.
	Client *http.Client

	// Force re-downloads even when a verified file is already present.
	Force bool

	// Offline forbids the network: a missing or corrupt cached file is an
	// error rather than a download.
	Offline bool
}

// FetchResult reports one artifact's outcome.
type FetchResult struct {
	Name    string
	Path    string
	Bytes   int64
	Skipped bool
}

// Fetch ensures the artifact exists at dest with a SHA-256 matching its pinned
// digest. A present, verified file is left alone and reported Skipped, unless
// Force is set. In Offline mode Fetch never reaches the network: a missing
// cached file yields ErrOffline and a corrupt one ErrHashMismatch.
func Fetch(ctx context.Context, art *Artifact, dest string, opts FetchOptions) (FetchResult, error) {
	result := FetchResult{Name: art.Name, Path: dest, Bytes: art.Bytes}

	if !opts.Force {
		ok, err := verifyExisting(dest, art.SHA256)
		if err != nil {
			return FetchResult{}, err
		}

		if ok {
			result.Skipped = true

			return result, nil
		}
	}

	if opts.Offline {
		return FetchResult{}, offlineError(dest)
	}

	if err := os.MkdirAll(filepath.Dir(dest), dirPerm); err != nil {
		return FetchResult{}, fmt.Errorf("create download directory: %w", err)
	}

	if err := download(ctx, art, dest, opts); err != nil {
		return FetchResult{}, err
	}

	return result, nil
}

// offlineError distinguishes a missing cached file from a corrupt one so the
// message says which problem dropping --offline would fix.
func offlineError(dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("%w: cached %s failed verification", ErrHashMismatch, dest)
	}

	return fmt.Errorf("%w: %s", ErrOffline, dest)
}

// verifyExisting reports whether the file at path is present and matches want.
// An absent file is not an error: it returns false so the caller downloads it.
func verifyExisting(path, want string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}

		return false, fmt.Errorf("stat %s: %w", path, err)
	}

	if info.IsDir() {
		return false, fmt.Errorf("%w: %s", ErrPathIsDir, path)
	}

	got, err := hashFile(path)
	if err != nil {
		return false, err
	}

	return strings.EqualFold(got, want), nil
}

// hashFile returns the lowercase hex SHA-256 of the file at path.
func hashFile(path string) (string, error) {
	// #nosec G304 -- path is a managed layout location, not user input
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// download streams the artifact to a temp file beside dest, verifies its hash,
// and renames it into place only on success, so an interrupted transfer never
// leaves a half-written file where the server would load it.
func download(ctx context.Context, art *Artifact, dest string, opts FetchOptions) error {
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: downloadTimeout}
	}

	// #nosec G107 -- the URL comes from the pinned registry, not user input
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, art.URL, http.NoBody)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", art.Name, err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", art.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %w %d", art.Name, ErrUnexpectedStatus, resp.StatusCode)
	}

	return writeVerified(resp.Body, art, dest, opts.Progress)
}

// writeVerified copies body into a temp file while hashing it, checks the
// digest, and atomically renames it to dest.
func writeVerified(body io.Reader, art *Artifact, dest string, progress io.Writer) error {
	tmp, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".part-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	tmpName := tmp.Name()

	var committed bool

	defer func() {
		_ = tmp.Close()

		if committed {
			return
		}

		// The download already failed with its own error, so a leftover part
		// file is worth a warning, not a second error.
		if removeErr := os.Remove(tmpName); removeErr != nil {
			slog.Warn("remove partial download", "path", tmpName, "error", removeErr)
		}
	}()

	sum, err := streamHashed(tmp, body, art, progress)
	if err != nil {
		return err
	}

	if !strings.EqualFold(sum, art.SHA256) {
		return fmt.Errorf("%w: %s want %s got %s", ErrHashMismatch, art.Name, art.SHA256, sum)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Chmod(tmpName, filePerm); err != nil {
		return fmt.Errorf("set permissions on %s: %w", tmpName, err)
	}

	if err := os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("install %s: %w", dest, err)
	}

	committed = true

	return nil
}

// streamHashed copies src into dst while computing the SHA-256 and, when
// progress is non-nil, emitting percent lines. It returns the lowercase hex sum.
func streamHashed(dst io.Writer, src io.Reader, art *Artifact, progress io.Writer) (string, error) {
	hasher := sha256.New()
	writers := []io.Writer{dst, hasher}

	if progress != nil && art.Bytes > 0 {
		writers = append(writers, NewProgressWriter(progress, art.Name, art.Bytes))
	}

	if _, err := io.Copy(io.MultiWriter(writers...), src); err != nil {
		return "", fmt.Errorf("stream %s: %w", art.Name, err)
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// ProgressWriter prints download percent to out, throttled to one line every
// progressStepPercent percent. It sits in the download's MultiWriter, so it
// sees every byte without a second pass over the data.
type ProgressWriter struct {
	out     io.Writer
	name    string
	total   int64
	written int64
	lastPct int64
	// done latches once the closing newline has been printed or a print
	// failed. A server that sends more than Artifact.Bytes keeps the percentage
	// at or past 100, and without the latch every further chunk would print
	// another newline.
	done bool
}

// NewProgressWriter builds a ProgressWriter that reports name's progress
// against total bytes to out.
func NewProgressWriter(out io.Writer, name string, total int64) *ProgressWriter {
	return &ProgressWriter{out: out, name: name, total: total}
}

// Write counts bytes and prints percent at the throttle boundary. It never
// fails the download, since the bytes still landed: a progress line that will
// not print stops further printing instead.
func (p *ProgressWriter) Write(chunk []byte) (int, error) {
	count := len(chunk)
	p.written += int64(count)

	if p.done {
		return count, nil
	}

	pct := p.written * percentScale / p.total
	if pct < p.lastPct+progressStepPercent && pct < percentScale {
		return count, nil
	}

	p.lastPct = pct
	finished := pct >= percentScale

	line := fmt.Sprintf("\r%s: %d%%", p.name, pct)
	if finished {
		line += "\n"
	}

	if _, err := io.WriteString(p.out, line); err != nil || finished {
		p.done = true
	}

	return count, nil
}
