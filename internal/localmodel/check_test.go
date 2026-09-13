package localmodel

import (
	"strings"
	"testing"
)

// TestCheckReportsPinsAsCurrentWhenUpstreamMatches proves a check against an
// upstream that has not moved says so, and says it for both components.
func TestCheckReportsPinsAsCurrentWhenUpstreamMatches(t *testing.T) {
	t.Parallel()

	current := catalog()[0]
	upstream := fakeUpstream(t, &upstreamFixture{
		tag:      PinnedVersion,
		buildTag: PinnedBuild,
		repoSHA:  current.Revision,
		fileName: current.File,
		fileSHA:  current.SHA256,
	})

	report, err := Check(t.Context(), upstream)
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	if report.Outdated() {
		t.Errorf("report says outdated against an unchanged upstream: %+v", report)
	}

	if report.Runtime.Latest != PinnedVersion || report.Runtime.LatestBuild != PinnedBuild {
		t.Errorf("runtime = %+v", report.Runtime)
	}
}

// TestCheckFlagsMovedBuildUnderSameStableTag proves the build half of the pin
// is compared too: upstream re-cuts a stable tag against a newer nightly, and
// a check that only looked at the version would miss it.
func TestCheckFlagsMovedBuildUnderSameStableTag(t *testing.T) {
	t.Parallel()

	current := catalog()[0]
	upstream := fakeUpstream(t, &upstreamFixture{
		tag:      PinnedVersion,
		buildTag: "b99999",
		repoSHA:  current.Revision,
		fileName: current.File,
		fileSHA:  current.SHA256,
	})

	report, err := Check(t.Context(), upstream)
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	if !report.Runtime.Outdated {
		t.Error("a moved build tag was not reported as outdated")
	}
}

// TestCheckIgnoresRevisionMoveWithUnchangedWeights proves a README-only commit
// does not read as new weights: the revision moves, the digest does not, and
// the model is still current.
func TestCheckIgnoresRevisionMoveWithUnchangedWeights(t *testing.T) {
	t.Parallel()

	current := catalog()[0]
	upstream := fakeUpstream(t, &upstreamFixture{
		tag:      PinnedVersion,
		buildTag: PinnedBuild,
		repoSHA:  "1111111111111111111111111111111111111111",
		fileName: current.File,
		fileSHA:  current.SHA256,
	})

	report, err := Check(t.Context(), upstream)
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	if report.Models[0].Outdated {
		t.Error("a README-only commit was reported as new weights")
	}

	if report.Models[0].LatestRevision == report.Models[0].PinnedRevision {
		t.Error("the report should still show the moved revision")
	}
}

// TestCheckFlagsNewWeights proves a changed LFS digest is what marks a model
// outdated.
func TestCheckFlagsNewWeights(t *testing.T) {
	t.Parallel()

	current := catalog()[0]
	upstream := fakeUpstream(t, &upstreamFixture{
		tag:      PinnedVersion,
		buildTag: PinnedBuild,
		repoSHA:  "2222222222222222222222222222222222222222",
		fileName: current.File,
		fileSHA:  "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
	})

	report, err := Check(t.Context(), upstream)
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	if !report.Models[0].Outdated {
		t.Error("changed weights were not reported as outdated")
	}

	if !report.Outdated() {
		t.Error("the report as a whole should be outdated")
	}
}

// TestCheckFailsWhenUpstreamIsUnreachable proves an unreachable upstream is an
// error, since a check that learned nothing must not read as a clean report.
func TestCheckFailsWhenUpstreamIsUnreachable(t *testing.T) {
	t.Parallel()

	upstream := NewUpstream(UpstreamOptions{GitHubAPI: "http://127.0.0.1:1", GitHubDownload: "http://127.0.0.1:1"})

	if _, err := Check(t.Context(), upstream); err == nil {
		t.Fatal("check against an unreachable upstream returned no error")
	}
}

// TestReportJSONCarriesTheContractFields proves the published field names a CI
// job parses are still there and spelled the documented way.
func TestReportJSONCarriesTheContractFields(t *testing.T) {
	t.Parallel()

	current := catalog()[0]
	upstream := fakeUpstream(t, &upstreamFixture{
		tag:      PinnedVersion,
		buildTag: PinnedBuild,
		repoSHA:  current.Revision,
		fileName: current.File,
		fileSHA:  current.SHA256,
	})

	report, err := Check(t.Context(), upstream)
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	encoded, err := report.JSON()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	for _, field := range []string{
		`"pinned_build"`, `"latest_build"`, `"outdated"`, `"pinned_revision"`,
		`"latest_revision"`, `"pinned_sha256"`, `"latest_sha256"`, `"repo"`, `"file"`,
	} {
		if !strings.Contains(encoded, field) {
			t.Errorf("report JSON is missing %s:\n%s", field, encoded)
		}
	}

	if !strings.Contains(encoded, "\n  \"runtime\"") {
		t.Errorf("report JSON is not indented two spaces:\n%s", encoded)
	}
}
