package localmodel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Upstream endpoints. The download host is separate from the API host because
// GitHub serves release assets from the web host, not from the API.
const (
	githubAPIBase      = "https://api.github.com"
	githubDownloadBase = "https://github.com"
	huggingFaceBase    = "https://huggingface.co"

	// llamaRepo is the upstream project, used to build both API and download
	// paths.
	llamaRepo = "ggml-org/llama.cpp"
)

// buildTagAsset is the file a stable llama.cpp release carries instead of
// binaries: one line naming the nightly build tag whose assets hold them.
const buildTagAsset = "nightly-tag.txt"

// lookupTimeout bounds one upstream metadata call. These are small JSON
// documents, so a slow answer means something is wrong rather than large.
const lookupTimeout = 30 * time.Second

// maxMetadataBytes caps a metadata response. The Hugging Face model listing is
// the largest of them at a few tens of kilobytes, so 8 MiB bounds a
// misbehaving endpoint without ever truncating a real answer. The shift form
// is a power-of-two byte count: 8 << 20 is 8 multiplied by 2^20.
const maxMetadataBytes = 8 << 20

// Upstream resolves what the pins would be if they were made today. It is the
// only part of this package that talks to anything other than a download URL,
// and every host it uses is injectable so tests serve fixtures.
type Upstream struct {
	client      *http.Client
	githubAPI   string
	github      string
	huggingFace string
}

// UpstreamOptions overrides the client and hosts an Upstream talks to. Every
// field is optional; the zero value targets the real services.
type UpstreamOptions struct {
	Client         *http.Client
	GitHubAPI      string
	GitHubDownload string
	HuggingFace    string
}

// NewUpstream builds a client over the given overrides.
func NewUpstream(opts UpstreamOptions) *Upstream {
	client := &Upstream{
		client:      opts.Client,
		githubAPI:   orDefault(opts.GitHubAPI, githubAPIBase),
		github:      orDefault(opts.GitHubDownload, githubDownloadBase),
		huggingFace: orDefault(opts.HuggingFace, huggingFaceBase),
	}

	if client.client == nil {
		client.client = &http.Client{Timeout: lookupTimeout}
	}

	return client
}

// orDefault picks the override when it is set and the default otherwise.
func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}

	return value
}

// LatestRuntime is the newest stable llama.cpp release and the nightly build
// under it that carries the binaries.
type LatestRuntime struct {
	Version string
	Build   string
	URL     string
}

// LatestModel is the newest state of one file in a Hugging Face repository:
// the repository's head commit and that file's LFS digest at it.
type LatestModel struct {
	Revision string
	SHA256   string
	Bytes    int64
}

// Runtime resolves the newest stable llama.cpp release. It takes two calls:
// the releases endpoint gives the stable tag, and that release's
// nightly-tag.txt asset names the build whose assets carry the binaries.
func (u *Upstream) Runtime(ctx context.Context) (LatestRuntime, error) {
	var release struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}

	endpoint := fmt.Sprintf("%s/repos/%s/releases/latest", u.githubAPI, llamaRepo)
	if err := u.getJSON(ctx, endpoint, &release); err != nil {
		return LatestRuntime{}, err
	}

	if release.TagName == "" {
		return LatestRuntime{}, ErrNoRelease
	}

	build, err := u.buildTag(ctx, release.TagName)
	if err != nil {
		return LatestRuntime{}, err
	}

	url := release.HTMLURL
	if url == "" {
		url = fmt.Sprintf("%s/%s/releases/tag/%s", u.github, llamaRepo, release.TagName)
	}

	return LatestRuntime{Version: release.TagName, Build: build, URL: url}, nil
}

// Model resolves the newest revision of a repository and the LFS digest of one
// file at it, which is what tells a README-only commit from new weights.
func (u *Upstream) Model(ctx context.Context, repo, file string) (LatestModel, error) {
	var listing struct {
		SHA      string `json:"sha"`
		Siblings []struct {
			Filename string `json:"rfilename"`
			Size     int64  `json:"size"`
			LFS      *struct {
				SHA256 string `json:"sha256"`
			} `json:"lfs"`
		} `json:"siblings"`
	}

	endpoint := fmt.Sprintf("%s/api/models/%s?blobs=true", u.huggingFace, repo)
	if err := u.getJSON(ctx, endpoint, &listing); err != nil {
		return LatestModel{}, err
	}

	for _, sibling := range listing.Siblings {
		if sibling.Filename != file || sibling.LFS == nil {
			continue
		}

		return LatestModel{Revision: listing.SHA, SHA256: sibling.LFS.SHA256, Bytes: sibling.Size}, nil
	}

	return LatestModel{}, fmt.Errorf("%w: %s in %s", ErrFileNotInRepo, file, repo)
}

// buildTag fetches one stable release's nightly-tag.txt, whose entire content
// is the build tag.
func (u *Upstream) buildTag(ctx context.Context, version string) (string, error) {
	endpoint := fmt.Sprintf("%s/%s/releases/download/%s/%s", u.github, llamaRepo, version, buildTagAsset)

	raw, err := u.get(ctx, endpoint)
	if err != nil {
		return "", err
	}

	tag := strings.TrimSpace(string(raw))
	if tag == "" {
		return "", fmt.Errorf("%w: %s", ErrNoBuildTag, version)
	}

	return tag, nil
}

// getJSON fetches and decodes one metadata document.
func (u *Upstream) getJSON(ctx context.Context, endpoint string, into any) error {
	raw, err := u.get(ctx, endpoint)
	if err != nil {
		return err
	}

	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("decode %s: %w", endpoint, err)
	}

	return nil
}

// get performs one bounded GET, following the redirect GitHub's download host
// answers with, which the default client already does.
func (u *Upstream) get(ctx context.Context, endpoint string) ([]byte, error) {
	// #nosec G107 -- the endpoint is built from compiled-in hosts and the
	// pinned repository name, not from workflow or user input
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", endpoint, err)
	}

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: %w %d", endpoint, ErrUnexpectedStatus, resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBytes))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", endpoint, err)
	}

	return raw, nil
}
