// Package bwinstall places a pinned build of the Bitwarden CLI on a host. It
// exists because the alternative is a README snippet that downloads a zip,
// checks a hash, and unpacks it, which is four commands an operator can get
// three-quarters right.
//
// Two digests are pinned per platform: the archive as published, and the binary
// inside it. The second is what makes a re-run meaningful, since the file on
// disk can be compared to it without running anything.
package bwinstall

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/cairnerr"
)

// Canonical codes, one per step, so a failure says which part of the install
// broke without the caller reading the message.
const (
	CodeSource   = "bwinstall.source"
	CodeDownload = "bwinstall.download"
	CodeChecksum = "bwinstall.checksum"
	CodeArchive  = "bwinstall.archive"
	CodeInstall  = "bwinstall.install"
	CodeVersion  = "bwinstall.version"
	CodeExisting = "bwinstall.existing"
	CodeBinDir   = "bwinstall.bindir"
)

// entryName is the one file taken out of the archive. Both platform zips hold
// exactly this entry and nothing else.
const entryName = "bw"

// Default size limits. The archive is around 45 MB and the binary inside it
// around 142 MB, so these leave room for the release to grow without letting a
// redirected or hostile response write until the disk fills.
const (
	defaultMaxArchiveBytes = 128 << 20
	defaultMaxBinaryBytes  = 320 << 20
)

// Timeouts. The download budget covers a slow link on a home connection; the
// version probe is a local process that answers immediately or is broken.
const (
	defaultDownloadTimeout = 10 * time.Minute
	defaultProbeTimeout    = 30 * time.Second
)

// maxRedirects keeps the standard library's own hop limit, which a custom
// CheckRedirect otherwise replaces with no limit at all.
const maxRedirects = 10

// waitDelay bounds how long the probe waits for a child that kept stdout open
// after its context fired. Without it a bw that forks blocks the read forever
// and the install hangs with nothing on screen.
const waitDelay = 5 * time.Second

// binaryPerm is the installed mode: readable and runnable by everyone, writable
// only by its owner, like anything else in a personal bin directory. dirPerm
// creates the directory when the host has no personal bin yet.
const (
	binaryPerm = 0o755
	dirPerm    = 0o750
)

// writableByOthers are the mode bits that would let another account replace the
// installed binary between one run and the next, and writableByWorld the half of
// them no ownership check can ever excuse.
const (
	writableByOthers = 0o022
	writableByWorld  = 0o002
)

// Options is one install.
type Options struct {
	Client *http.Client
	Logger *slog.Logger

	// URL is the release asset to fetch, and ArchiveSHA256 its expected digest
	// in hex. They travel together because verifying a different artifact than
	// the one downloaded is the mistake this package exists to prevent.
	URL           string
	ArchiveSHA256 string

	// BinarySHA256 is the expected digest of the binary inside the archive. It
	// is checked on the way out of the archive, and it is what a later run
	// compares the installed file against.
	BinarySHA256 string

	// Version is what the installed binary reports. It is a label for output,
	// never the thing that decides whether an install is needed.
	Version string

	// Dest is the full path of the installed binary, not its directory.
	Dest string

	// Timeout bounds the download and ProbeTimeout the version probe. Zero
	// takes the package defaults.
	Timeout      time.Duration
	ProbeTimeout time.Duration

	// MaxArchiveBytes and MaxBinaryBytes bound what a response and its
	// decompressed entry may write. Zero takes the package defaults.
	MaxArchiveBytes int64
	MaxBinaryBytes  int64

	// Force reinstalls over a binary that is already the pinned one.
	Force bool
}

// Result says what happened, so a caller can report it without guessing.
type Result struct {
	Path    string
	Version string

	// Installed is false when the pinned binary was already in place.
	Installed bool
}

// Install downloads, verifies, and places the binary. Nothing is written
// outside Dest's directory, and a failure at any step leaves whatever was
// already installed untouched.
func Install(ctx context.Context, opts *Options) (Result, error) {
	if err := checkSource(opts.URL); err != nil {
		return Result{}, err
	}

	if !opts.Force {
		found, err := alreadyInstalled(opts)
		if found || err != nil {
			return Result{Path: opts.Dest, Version: opts.Version}, err
		}
	}

	dir := filepath.Dir(opts.Dest)
	if err := prepareDir(dir); err != nil {
		return Result{}, err
	}

	archive, err := download(ctx, opts, dir)
	if err != nil {
		return Result{}, err
	}

	defer func() { _ = os.Remove(archive) }()

	if extractErr := extract(archive, opts); extractErr != nil {
		return Result{}, extractErr
	}

	version, err := readVersion(ctx, opts)
	if err != nil {
		return Result{}, err
	}

	opts.log().Debug("installed the Bitwarden CLI", "path", opts.Dest, "version", version)

	return Result{Path: opts.Dest, Version: version, Installed: true}, nil
}

// checkSource refuses anything but https. Both callers pass a compiled-in
// constant today, and this keeps the package's promise true for the next one:
// a plain http source has nothing authenticating where the bytes came from.
func checkSource(source string) error {
	parsed, err := url.Parse(source)
	if err != nil {
		return cairnerr.Wrapf(err, CodeSource, "parse the download URL: %v", err)
	}

	if parsed.Scheme != "https" {
		return cairnerr.Newf(CodeSource, "the download URL must be https, not %q", parsed.Scheme)
	}

	return nil
}

// alreadyInstalled reports whether the pinned binary is in place, by hashing
// the file rather than asking it what it is. A binary that prints the right
// version string is not evidence of anything: whatever can write the
// destination can write a wrapper that prints it and forwards the vault
// credentials somewhere else.
func alreadyInstalled(opts *Options) (bool, error) {
	if !exists(opts.Dest) {
		return false, nil
	}

	sum, err := fileDigest(opts.Dest)
	if err != nil {
		return false, err
	}

	if strings.EqualFold(sum, opts.BinarySHA256) {
		return true, nil
	}

	return false, cairnerr.Newf(CodeExisting,
		"%s is not the pinned version %s (sha256 %s); re-run with --force to replace it",
		opts.Dest, opts.Version, sum)
}

// exists reports whether anything is at the path. Why it cannot be read does
// not matter here: an install is about to write over it either way.
func exists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

// fileDigest hashes a file on disk.
func fileDigest(path string) (string, error) {
	file, err := os.Open(path) // #nosec G304 -- the path is the install destination
	if err != nil {
		return "", cairnerr.Wrapf(err, CodeChecksum, "open %s: %v", path, err)
	}

	defer func() { _ = file.Close() }()

	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", cairnerr.Wrapf(err, CodeChecksum, "read %s: %v", path, err)
	}

	return hex.EncodeToString(digest.Sum(nil)), nil
}

// prepareDir creates the install directory and refuses one that another account
// can write, since swapping the binary between one run and the next is exactly
// what verifying it at install time cannot catch.
//
// A directory whose group is the owner's own private group is the exception.
// Debian and Fedora hosts ship ~/.local/bin as 0775 under a group named after
// the account and holding only that account, so the group bit there is the owner
// under another name and adds no second writer. World-writable stays refused
// whatever the ownership says.
func prepareDir(dir string) error {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return cairnerr.Wrapf(err, CodeInstall, "create %s: %v", dir, err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		return cairnerr.Wrapf(err, CodeBinDir, "inspect %s: %v", dir, err)
	}

	mode := info.Mode().Perm()
	if mode&writableByOthers == 0 {
		return nil
	}

	if mode&writableByWorld == 0 && ownedByPrivateGroup(info) {
		return nil
	}

	return cairnerr.Newf(CodeBinDir,
		"%s is mode %04o, which lets another account replace the binary; "+
			"tighten it to 0755 or install elsewhere with --bin-dir", dir, mode)
}

// readVersion asks the installed binary what it is, for display only. The child
// gets a minimal environment: a host may hold the vault API key and the master
// password in this process's environment, and a version probe has no business
// seeing either.
func readVersion(ctx context.Context, opts *Options) (string, error) {
	probe, cancel := context.WithTimeout(ctx, probeTimeout(opts))
	defer cancel()

	command := exec.CommandContext(probe, opts.Dest, "--version") // #nosec G204 -- the path is the install destination
	command.Env = probeEnv()
	command.Stdin = nil
	command.WaitDelay = waitDelay

	out, err := command.Output()
	if err != nil {
		return "", cairnerr.Wrapf(err, CodeVersion, "run %s --version: %v", opts.Dest, err)
	}

	return strings.TrimSpace(string(out)), nil
}

// probeEnv is the child's whole environment. It mirrors the allowlist the
// Bitwarden client builds for its own children, minus the vault variables,
// which a version probe never needs.
func probeEnv() []string {
	var env []string

	for _, name := range []string{"PATH", "HOME"} {
		if value, found := os.LookupEnv(name); found {
			env = append(env, name+"="+value)
		}
	}

	return env
}

// download fetches the archive into a temp file beside the destination and
// checks its digest before anything reads it as an archive. The file is in the
// destination directory so the later rename stays on one filesystem.
func download(ctx context.Context, opts *Options, dir string) (string, error) {
	fetch, cancel := context.WithTimeout(ctx, timeout(opts))
	defer cancel()

	opts.log().Debug("fetching the Bitwarden CLI", "url", opts.URL)

	body, err := open(fetch, opts)
	if err != nil {
		return "", err
	}

	defer func() { _ = body.Close() }()

	temp, err := os.CreateTemp(dir, "bw-download-*")
	if err != nil {
		return "", cairnerr.Wrapf(err, CodeDownload, "create a temp file in %s: %v", dir, err)
	}

	sum, err := copyChecked(temp, body, maxArchive(opts))
	if err != nil {
		_ = os.Remove(temp.Name())

		return "", err
	}

	if !strings.EqualFold(sum, opts.ArchiveSHA256) {
		_ = os.Remove(temp.Name())

		return "", cairnerr.Newf(CodeChecksum,
			"%s has sha256 %s, want the pinned %s", opts.URL, sum, opts.ArchiveSHA256)
	}

	return temp.Name(), nil
}

// open starts the request and hands back the body, leaving the caller to close
// it.
func open(ctx context.Context, opts *Options) (io.ReadCloser, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.URL, http.NoBody)
	if err != nil {
		return nil, cairnerr.Wrapf(err, CodeDownload, "build a request for %s: %v", opts.URL, err)
	}

	response, err := client(opts).Do(request)
	if err != nil {
		return nil, cairnerr.Wrapf(err, CodeDownload, "fetch %s: %v", opts.URL, err)
	}

	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()

		return nil, cairnerr.Newf(CodeDownload, "%s answered HTTP %d", opts.URL, response.StatusCode)
	}

	return response.Body, nil
}

// copyChecked writes at most limit bytes and returns the digest of what it
// wrote. A response longer than the limit fails rather than filling the disk,
// and the file is closed here so the error can be reported.
func copyChecked(file *os.File, source io.Reader, limit int64) (string, error) {
	digest := sha256.New()

	written, err := io.Copy(io.MultiWriter(file, digest), io.LimitReader(source, limit+1))

	if closeErr := file.Close(); closeErr != nil && err == nil {
		err = closeErr
	}

	if err != nil {
		return "", cairnerr.Wrapf(err, CodeDownload, "write %s: %v", file.Name(), err)
	}

	if written > limit {
		return "", cairnerr.Newf(CodeDownload, "the download is larger than the %d byte limit", limit)
	}

	return hex.EncodeToString(digest.Sum(nil)), nil
}

// extract takes the single binary out of the verified archive and puts it in
// place. Only the one expected entry is read, so an archive carrying extra
// files, absolute paths, or parent-directory names writes nothing.
func extract(archive string, opts *Options) error {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return cairnerr.Wrapf(err, CodeArchive, "read %s as a zip: %v", archive, err)
	}

	defer func() { _ = reader.Close() }()

	entry, err := findEntry(reader)
	if err != nil {
		return err
	}

	source, err := entry.Open()
	if err != nil {
		return cairnerr.Wrapf(err, CodeArchive, "open %s inside the archive: %v", entry.Name, err)
	}

	defer func() { _ = source.Close() }()

	return place(source, opts)
}

// findEntry picks the binary out of the archive by exact name and mode. A name
// carrying a directory, an absolute path, or a parent reference never matches,
// and a symlink or device entry is refused rather than written out as a regular
// file holding whatever it pointed at.
func findEntry(reader *zip.ReadCloser) (*zip.File, error) {
	for _, file := range reader.File {
		if file.Name != entryName {
			continue
		}

		if !file.Mode().IsRegular() {
			return nil, cairnerr.Newf(CodeArchive,
				"the %q entry is mode %s, not a regular file", entryName, file.Mode())
		}

		return file, nil
	}

	return nil, cairnerr.Newf(CodeArchive,
		"the archive holds no entry named %q; it holds %s", entryName, entryNames(reader))
}

// entryNames lists what the archive did hold, so a refusal says what arrived.
// Names are reported as stored, which is what makes a traversal attempt visible
// in the message.
func entryNames(reader *zip.ReadCloser) string {
	names := make([]string, 0, len(reader.File))
	for _, file := range reader.File {
		names = append(names, fmt.Sprintf("%q", file.Name))
	}

	if len(names) == 0 {
		return "nothing"
	}

	return strings.Join(names, ", ")
}

// place writes the binary through a temp file and renames, so a partial write
// never becomes the installed binary and an interrupted install leaves the
// previous one running.
func place(source io.Reader, opts *Options) error {
	temp, err := os.CreateTemp(filepath.Dir(opts.Dest), "bw-install-*")
	if err != nil {
		return cairnerr.Wrapf(err, CodeInstall, "create a temp file beside %s: %v", opts.Dest, err)
	}

	if err := fill(temp, source, opts); err != nil {
		_ = os.Remove(temp.Name())

		return err
	}

	if err := os.Rename(temp.Name(), opts.Dest); err != nil {
		_ = os.Remove(temp.Name())

		return cairnerr.Wrapf(err, CodeInstall, "move the binary into %s: %v", opts.Dest, err)
	}

	return nil
}

// fill writes the binary, checks it against the pinned digest, and sets its
// mode, closing the file either way. The digest is taken on the way through, so
// a mismatch is caught before the rename rather than by the next run.
func fill(temp *os.File, source io.Reader, opts *Options) (err error) {
	defer func() {
		if closeErr := temp.Close(); closeErr != nil && err == nil {
			err = cairnerr.Wrapf(closeErr, CodeInstall, "close %s: %v", temp.Name(), closeErr)
		}
	}()

	digest := sha256.New()
	limit := maxBinary(opts)

	written, err := io.Copy(io.MultiWriter(temp, digest), io.LimitReader(source, limit+1))
	if err != nil {
		return cairnerr.Wrapf(err, CodeInstall, "write %s: %v", temp.Name(), err)
	}

	if written > limit {
		return cairnerr.Newf(CodeInstall, "the binary is larger than the %d byte limit", limit)
	}

	if sum := hex.EncodeToString(digest.Sum(nil)); !strings.EqualFold(sum, opts.BinarySHA256) {
		return cairnerr.Newf(CodeChecksum,
			"the binary inside the archive has sha256 %s, want the pinned %s", sum, opts.BinarySHA256)
	}

	if err := temp.Chmod(binaryPerm); err != nil {
		return cairnerr.Wrapf(err, CodeInstall, "set the mode of %s: %v", temp.Name(), err)
	}

	return nil
}

// client resolves the HTTP client. The default carries the same bound as the
// context so a stalled body cannot outlive the budget, and it refuses a
// redirect that drops out of https on the way to the asset host.
func client(opts *Options) *http.Client {
	if opts.Client != nil {
		return opts.Client
	}

	return &http.Client{Timeout: timeout(opts), CheckRedirect: refuseDowngrade}
}

// refuseDowngrade stops a redirect chain that leaves https, and keeps the
// standard library's own hop limit.
func refuseDowngrade(request *http.Request, via []*http.Request) error {
	if request.URL.Scheme != "https" {
		return cairnerr.Newf(CodeSource, "the download was redirected to %q, which is not https",
			request.URL.Scheme)
	}

	if len(via) >= maxRedirects {
		return cairnerr.Newf(CodeDownload, "the download was redirected %d times", len(via))
	}

	return nil
}

// log resolves the logger, so every call site can write a line without a nil
// check of its own.
func (o *Options) log() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}

	return slog.Default()
}

// timeout resolves the download budget.
func timeout(opts *Options) time.Duration {
	if opts.Timeout > 0 {
		return opts.Timeout
	}

	return defaultDownloadTimeout
}

// probeTimeout resolves the version probe's budget.
func probeTimeout(opts *Options) time.Duration {
	if opts.ProbeTimeout > 0 {
		return opts.ProbeTimeout
	}

	return defaultProbeTimeout
}

// maxArchive resolves the download cap.
func maxArchive(opts *Options) int64 {
	if opts.MaxArchiveBytes > 0 {
		return opts.MaxArchiveBytes
	}

	return defaultMaxArchiveBytes
}

// maxBinary resolves the decompressed cap.
func maxBinary(opts *Options) int64 {
	if opts.MaxBinaryBytes > 0 {
		return opts.MaxBinaryBytes
	}

	return defaultMaxBinaryBytes
}
