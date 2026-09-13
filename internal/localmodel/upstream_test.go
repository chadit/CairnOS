package localmodel

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fixtureTag is the stable release the fake GitHub reports.
const fixtureTag = "v0.9.1"

// upstreamFixture is the upstream state one test wants the fake services to
// report.
type upstreamFixture struct {
	tag       string
	buildTag  string
	repoSHA   string
	fileSHA   string
	fileName  string
	fileBytes int64
}

// TestUpstreamRuntimeResolvesStableTagAndBuild proves the two-step lookup: the
// releases endpoint names the stable tag, and that release's nightly-tag asset
// names the build whose assets carry the binaries.
func TestUpstreamRuntimeResolvesStableTagAndBuild(t *testing.T) {
	t.Parallel()

	upstream := fakeUpstream(t, &upstreamFixture{tag: fixtureTag, buildTag: "b12345"})

	latest, err := upstream.Runtime(t.Context())
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}

	if latest.Version != fixtureTag || latest.Build != "b12345" {
		t.Errorf("latest = %+v, want v0.9.1 / b12345", latest)
	}

	if !strings.Contains(latest.URL, fixtureTag) {
		t.Errorf("url = %q, want the release tag in it", latest.URL)
	}
}

// TestUpstreamRuntimeRejectsReleaseWithoutBuildTag proves a stable release
// carrying no nightly-tag asset fails rather than reporting an empty build,
// which a comparison would read as a moved pin.
func TestUpstreamRuntimeRejectsReleaseWithoutBuildTag(t *testing.T) {
	t.Parallel()

	upstream := fakeUpstream(t, &upstreamFixture{tag: fixtureTag, buildTag: ""})

	_, err := upstream.Runtime(t.Context())
	if !errors.Is(err, ErrNoBuildTag) {
		t.Fatalf("err = %v, want ErrNoBuildTag", err)
	}
}

// TestUpstreamModelReadsLFSDigestForPinnedFile proves the lookup returns the
// digest of the requested file, not of whichever sibling comes first.
func TestUpstreamModelReadsLFSDigestForPinnedFile(t *testing.T) {
	t.Parallel()

	upstream := fakeUpstream(t, &upstreamFixture{
		repoSHA:   "c0ffee00000000000000000000000000000000ff",
		fileName:  "wanted.gguf",
		fileSHA:   "abc123",
		fileBytes: 42,
	})

	latest, err := upstream.Model(t.Context(), "org/repo", "wanted.gguf")
	if err != nil {
		t.Fatalf("model: %v", err)
	}

	if latest.SHA256 != "abc123" || latest.Bytes != 42 {
		t.Errorf("latest = %+v, want abc123 / 42", latest)
	}

	if latest.Revision != "c0ffee00000000000000000000000000000000ff" {
		t.Errorf("revision = %q", latest.Revision)
	}
}

// TestUpstreamModelReportsMissingFile proves a repository that no longer
// carries the pinned file is an error, not a silent empty digest that would
// read as "the weights changed".
func TestUpstreamModelReportsMissingFile(t *testing.T) {
	t.Parallel()

	upstream := fakeUpstream(t, &upstreamFixture{repoSHA: "deadbeef", fileName: "other.gguf", fileSHA: "zzz"})

	_, err := upstream.Model(t.Context(), "org/repo", "wanted.gguf")
	if !errors.Is(err, ErrFileNotInRepo) {
		t.Fatalf("err = %v, want ErrFileNotInRepo", err)
	}
}

// fakeUpstream stands up GitHub and Hugging Face stand-ins answering with the
// fixture's state, and points an Upstream at them.
func fakeUpstream(t *testing.T, fixture *upstreamFixture) *Upstream {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			_, _ = w.Write([]byte(`{"tag_name":"` + fixture.tag + `","html_url":"https://example.test/` + fixture.tag + `"}`))
		case strings.HasSuffix(r.URL.Path, buildTagAsset):
			_, _ = w.Write([]byte(fixture.buildTag + "\n"))
		case strings.HasPrefix(r.URL.Path, "/api/models/"):
			_, _ = w.Write([]byte(modelListingJSON(fixture)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	return NewUpstream(UpstreamOptions{
		Client:         server.Client(),
		GitHubAPI:      server.URL,
		GitHubDownload: server.URL,
		HuggingFace:    server.URL,
	})
}

// modelListingJSON renders the blobs listing the Hugging Face API returns.
func modelListingJSON(fixture *upstreamFixture) string {
	return `{"sha":"` + fixture.repoSHA + `","siblings":[` +
		`{"rfilename":"README.md"},` +
		`{"rfilename":"` + fixture.fileName + `","size":` + itoa(fixture.fileBytes) +
		`,"lfs":{"sha256":"` + fixture.fileSHA + `"}}]}`
}

// itoa keeps the fixture JSON readable without pulling strconv into the
// template above.
func itoa(value int64) string {
	if value == 0 {
		return "0"
	}

	var digits []byte

	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}

	return string(digits)
}
