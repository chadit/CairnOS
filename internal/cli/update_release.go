package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/environ"
)

// The GitHub REST endpoints the release lookup reads and the headers that pick
// the documented response shape.
const (
	releaseAPIBase    = "https://api.github.com"
	releaseLatestPath = "/repos/%s/%s/releases/latest"
	releaseTagRefPath = "/repos/%s/%s/git/ref/tags/%s"
	releaseTagObjPath = "/repos/%s/%s/git/tags/%s"
	releaseAccept     = "application/vnd.github+json"
	releaseAPIVersion = "2022-11-28"
)

// releaseTimeout bounds every API call and every asset download, so a slow or
// unreachable api.github.com fails fast instead of hanging a deploy.
const releaseTimeout = 15 * time.Second

// Read ceilings for the two body shapes. A release binary is tens of megabytes
// while the JSON and the checksum list are kilobytes, and both are read through
// io.LimitReader so a wrong or hostile URL cannot fill memory or the disk.
const (
	releaseJSONLimit  = 4 << 20
	releaseAssetLimit = 512 << 20
)

// checksumsAssetName is the sha256sum-format manifest every release carries,
// and tagObjectType is what the ref API calls an annotated tag.
const (
	checksumsAssetName = "checksums.txt"
	tagObjectType      = "tag"
)

// The suffixes the in-place install uses beside the binary it replaces.
const (
	installNewSuffix = ".new"
	installOldSuffix = ".old"
)

// stagedAssetPerm is the mode the downloaded asset gets while it sits in the
// staging directory; the installed copy takes the mode of the file it replaces.
const stagedAssetPerm = 0o700

// The module host and the path depth a release lookup needs: "github.com",
// owner, repository.
const (
	githubModuleHost = "github.com"
	moduleSlugParts  = 3
)

// goosWindows is the one platform that cannot overwrite a file open for
// execution, which is why the install renames the running image aside.
const goosWindows = "windows"

// releaseBuild is the release this run would install: the tag, the commit that
// tag points at, and where the asset and its checksum list live.
type releaseBuild struct {
	tag       string
	revision  string
	assetName string
	assetURL  string
	sumsURL   string
}

// releaseAsset names one uploaded file and where to fetch it.
type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// releaseResponse is the slice of the latest-release payload this command reads.
type releaseResponse struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

// gitObjectResponse covers both the ref and the annotated-tag payloads, which
// carry the same object field.
type gitObjectResponse struct {
	Object struct {
		Type string `json:"type"`
		SHA  string `json:"sha"`
	} `json:"object"`
}

// releaseClient reads the release API and the asset downloads. base is a field
// so a test points it at a local server rather than the public API.
type releaseClient struct {
	http   *http.Client
	logger *slog.Logger
	base   string
	token  string
}

// releaseClient builds one invocation's lookup and download client, aimed at
// the public API unless opts names another base. A nil transport is the default
// one, so production opens real sockets; a test passes one that answers in
// process and never touches the network stack.
func (a *App) releaseClient(opts *updateOptions) *releaseClient {
	base := opts.apiBase
	if base == "" {
		base = releaseAPIBase
	}

	return &releaseClient{
		base:   base,
		token:  releaseToken(a.envLookup()),
		http:   &http.Client{Timeout: releaseTimeout, Transport: opts.transport},
		logger: a.lookupLogger(opts),
	}
}

// lookupLogger is where the release lookup reports what it found: the App's
// own logger unless the invocation names a quieter one.
func (a *App) lookupLogger(opts *updateOptions) *slog.Logger {
	if opts.lookupLogger != nil {
		return opts.lookupLogger
	}

	return a.logger
}

// findRelease answers with the release to install when one fits this platform
// and no checkout sits around the running binary. A binary built from a
// checkout follows that checkout's upstream instead, so a host on a newer main
// commit is never moved onto an older tag. --no-fetch, --source and
// --no-release each skip the lookup too, so a developer deploying the tree in
// front of them is never clobbered.
func (a *App) findRelease(ctx context.Context, opts *updateOptions) (releaseBuild, bool, error) {
	if !opts.fetch || opts.source != "" || !opts.release {
		return releaseBuild{}, false, nil
	}

	if _, err := discoverSource("", opts.executable); err == nil {
		return releaseBuild{}, false, nil
	}

	owner, repo, err := moduleSlug()
	if err != nil {
		a.lookupLogger(opts).Info("update: no GitHub module to look for releases in", "error", err)

		return releaseBuild{}, false, nil
	}

	return a.releaseClient(opts).latestRelease(ctx, owner, repo)
}

// moduleSlug reads owner and repository out of the module path the running
// binary was built from, so a fork looks at its own releases.
func moduleSlug() (string, string, error) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", "", errNoBuildInfo
	}

	parts := strings.Split(info.Main.Path, "/")
	if len(parts) < moduleSlugParts || parts[0] != githubModuleHost {
		return "", "", fmt.Errorf("%w: %s", errNotAGitHubModule, info.Main.Path)
	}

	return parts[1], parts[2], nil
}

// releaseAssetName spells the asset for one platform, matching what the release
// workflow uploads.
func releaseAssetName(goos, goarch string) string {
	name := fmt.Sprintf("cairnos_%s_%s", goos, goarch)
	if goos == goosWindows {
		return name + ".exe"
	}

	return name
}

// releaseToken reads the token the API calls authenticate with. The repository
// is private, so without one every lookup answers 404 and the update builds the
// checkout instead.
func releaseToken(lookup environ.Lookup) string {
	for _, key := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if value := strings.TrimSpace(lookup.Get(key)); value != "" {
			return value
		}
	}

	return ""
}

// latestRelease resolves the newest release into something installable, and
// answers false for the two ordinary misses: a private repository read without
// a token answers 404, and a release cut before this platform was added carries
// no asset for it.
func (c *releaseClient) latestRelease(ctx context.Context, owner, repo string) (releaseBuild, bool, error) {
	var payload releaseResponse

	found, err := c.getJSON(ctx, fmt.Sprintf(releaseLatestPath, owner, repo), &payload)
	if err != nil {
		return releaseBuild{}, false, err
	}

	if !found {
		c.logger.Info("update: no release is published for this repository, building the checkout")

		return releaseBuild{}, false, nil
	}

	wanted := releaseAssetName(runtime.GOOS, runtime.GOARCH)

	asset, sums, found := pickAssets(payload.Assets, wanted)
	if !found {
		c.logger.Info("update: the latest release carries no asset for this platform, building the checkout",
			"tag", payload.TagName, "asset", wanted)

		return releaseBuild{}, false, nil
	}

	if sums.URL == "" {
		return releaseBuild{}, false, fmt.Errorf("%w: %s publishes %s but no %s",
			errNoChecksumsAsset, payload.TagName, wanted, checksumsAssetName)
	}

	revision, err := c.tagRevision(ctx, owner, repo, payload.TagName)
	if err != nil {
		return releaseBuild{}, false, err
	}

	return releaseBuild{
		tag:      payload.TagName,
		revision: revision,
		// The staged file is named from this platform rather than from the
		// response, so nothing the API returns reaches a path this command joins.
		assetName: wanted,
		assetURL:  asset.URL,
		sumsURL:   sums.URL,
	}, true, nil
}

// pickAssets finds the platform's binary and the checksum manifest. The bool
// covers only the binary, since a release with no build for this host is a
// miss while one with a build and no checksums is a broken release.
func pickAssets(assets []releaseAsset, wanted string) (releaseAsset, releaseAsset, bool) {
	var binary, sums releaseAsset

	for _, asset := range assets {
		switch asset.Name {
		case wanted:
			binary = asset
		case checksumsAssetName:
			sums = asset
		}
	}

	return binary, sums, binary.URL != ""
}

// tagRevision resolves a tag to the commit it names, following an annotated
// tag's own object, because that commit is what the running binary's stamp is
// compared against.
func (c *releaseClient) tagRevision(ctx context.Context, owner, repo, tag string) (string, error) {
	var ref gitObjectResponse

	found, err := c.getJSON(ctx, fmt.Sprintf(releaseTagRefPath, owner, repo, tag), &ref)
	if err != nil {
		return "", err
	}

	if !found {
		return "", fmt.Errorf("%w: %s", errNoTagRef, tag)
	}

	if ref.Object.Type != tagObjectType {
		return ref.Object.SHA, nil
	}

	return c.annotatedTagCommit(ctx, owner, repo, ref.Object.SHA)
}

// annotatedTagCommit dereferences an annotated tag object to the commit it
// wraps.
func (c *releaseClient) annotatedTagCommit(ctx context.Context, owner, repo, sha string) (string, error) {
	var object gitObjectResponse

	found, err := c.getJSON(ctx, fmt.Sprintf(releaseTagObjPath, owner, repo, sha), &object)
	if err != nil {
		return "", err
	}

	if !found {
		return "", fmt.Errorf("%w: %s", errNoTagRef, sha)
	}

	return object.Object.SHA, nil
}

// getJSON reads one API endpoint into out. A 404 answers false rather than
// failing, since a missing release and an unauthenticated read of a private
// repository look the same from here.
func (c *releaseClient) getJSON(ctx context.Context, path string, out any) (bool, error) {
	body, status, err := c.get(ctx, c.base+path, releaseJSONLimit)
	if err != nil {
		return false, err
	}

	if status == http.StatusNotFound {
		return false, nil
	}

	if status != http.StatusOK {
		return false, fmt.Errorf("%w: %s answered %d", errReleaseAPI, path, status)
	}

	if err := json.Unmarshal(body, out); err != nil {
		return false, fmt.Errorf("read the response from %s: %w", path, err)
	}

	return true, nil
}

// get performs one request and reads at most limit bytes of the body.
func (c *releaseClient) get(ctx context.Context, url string, limit int64) ([]byte, int, error) {
	response, err := c.do(ctx, url)
	if err != nil {
		return nil, 0, err
	}

	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, limit))
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", url, err)
	}

	return body, response.StatusCode, nil
}

// do sends one GET carrying the headers the REST API documents, with a bearer
// token when the environment holds one.
func (c *releaseClient) do(ctx context.Context, url string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build a request for %s: %w", url, err)
	}

	request.Header.Set("Accept", releaseAccept)
	request.Header.Set("X-Github-Api-Version", releaseAPIVersion)

	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}

	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", url, err)
	}

	return response, nil
}

// stageAsset downloads the release binary into dir and answers with its path
// once the sha256 matches the one the release publishes. Nothing else touches
// the file before that comparison.
func (c *releaseClient) stageAsset(ctx context.Context, build *releaseBuild, dir string) (string, error) {
	manifest, status, err := c.get(ctx, build.sumsURL, releaseJSONLimit)
	if err != nil {
		return "", err
	}

	if status != http.StatusOK {
		return "", fmt.Errorf("%w: %s answered %d", errReleaseAPI, checksumsAssetName, status)
	}

	want, err := checksumFor(string(manifest), build.assetName)
	if err != nil {
		return "", err
	}

	path := filepath.Join(dir, build.assetName)

	got, err := c.downloadTo(ctx, build.assetURL, path)
	if err != nil {
		return "", err
	}

	if got != want {
		return "", fmt.Errorf("%w: %s hashes to %s, the release publishes %s",
			errAssetDigest, build.assetName, got, want)
	}

	return path, nil
}

// checksumFor reads the digest a sha256sum manifest publishes for one file.
func checksumFor(manifest, name string) (string, error) {
	for line := range strings.SplitSeq(manifest, "\n") {
		if sum, ok := checksumLine(line, name); ok {
			return sum, nil
		}
	}

	return "", fmt.Errorf("%w: %s", errNoChecksumLine, name)
}

// checksumLine reads one sha256sum row, answering with its digest when the row
// names this file. The "*" marking a binary-mode entry is not part of the name.
func checksumLine(line, name string) (string, bool) {
	fields := strings.Fields(line)
	if len(fields) != 2 { //nolint:mnd // a sha256sum row is a digest and a name
		return "", false
	}

	if strings.TrimPrefix(fields[1], "*") != name {
		return "", false
	}

	return strings.ToLower(fields[0]), true
}

// downloadTo streams one asset to path and answers with its sha256, hashed on
// the way through so the body is never read twice or held in memory.
func (c *releaseClient) downloadTo(ctx context.Context, url, path string) (string, error) {
	response, err := c.do(ctx, url)
	if err != nil {
		return "", err
	}

	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: %s answered %d", errReleaseAPI, url, response.StatusCode)
	}

	// #nosec G304 -- path joins a directory this command made with the asset
	// name it computed from GOOS and GOARCH, never a name the API returned.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, stagedAssetPerm)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", path, err)
	}

	defer func() { _ = file.Close() }()

	digest := sha256.New()

	if _, err := io.Copy(io.MultiWriter(file, digest),
		io.LimitReader(response.Body, releaseAssetLimit)); err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}

	return hex.EncodeToString(digest.Sum(nil)), nil
}

// verifyStamp refuses an asset whose own stamp is absent, dirty, or a different
// commit than the tag names. A release is built from a clean checkout, so a
// mismatch means the artifact and the tag are not the same code.
func verifyStamp(path, revision string) error {
	stamp, err := stampedRevision(path)
	if err != nil {
		return err
	}

	if stamp != revision {
		return fmt.Errorf("%w: the asset is stamped %q, the tag names %s", errAssetStamp, stamp, revision)
	}

	return nil
}

// installBinary replaces the binary at target with the staged one. The rename
// is what makes it atomic: a reader opens either the old image or the new one,
// never a half-written file.
func installBinary(target, staged string) error {
	info, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", target, err)
	}

	next := target + installNewSuffix

	if err := copyFile(staged, next, info.Mode().Perm()); err != nil {
		return err
	}

	if err := replaceRunning(target, next); err != nil {
		_ = os.Remove(next)

		return err
	}

	return nil
}

// replaceRunning moves next over target. Windows refuses to replace a file that
// is open for execution, so the running image is renamed aside first; a failed
// sweep of the leftover is harmless because the next install removes it.
func replaceRunning(target, next string) error {
	if runtime.GOOS != goosWindows {
		return renameOver(next, target)
	}

	old := target + installOldSuffix

	_ = os.Remove(old)

	if err := os.Rename(target, old); err != nil {
		return fmt.Errorf("move %s aside: %w", target, err)
	}

	if err := renameOver(next, target); err != nil {
		return err
	}

	_ = os.Remove(old)

	return nil
}

// renameOver puts source at dest, replacing whatever was there.
func renameOver(source, dest string) error {
	if err := os.Rename(source, dest); err != nil {
		return fmt.Errorf("move %s into place at %s: %w", source, dest, err)
	}

	return nil
}

// copyFile writes sourcePath to destPath with the given mode, refusing to
// reuse a file already sitting there so a leftover from a failed install is
// never shipped.
func copyFile(sourcePath, destPath string, mode fs.FileMode) error {
	source, err := os.Open(sourcePath) // #nosec G304 -- the file this command staged and verified
	if err != nil {
		return fmt.Errorf("open %s: %w", sourcePath, err)
	}

	defer func() { _ = source.Close() }()

	_ = os.Remove(destPath)

	// #nosec G304 -- destPath is the running binary's own path with a fixed suffix
	dest, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", destPath, err)
	}

	if _, err := io.Copy(dest, source); err != nil {
		_ = dest.Close()

		return fmt.Errorf("write %s: %w", destPath, err)
	}

	if err := dest.Close(); err != nil {
		return fmt.Errorf("close %s: %w", destPath, err)
	}

	return nil
}
