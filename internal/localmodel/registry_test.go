package localmodel_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/localmodel"
)

// TestBuildForCoversEveryShippedPlatform proves the pin table answers for each
// platform CairnOS builds for, so setup never dead-ends on a supported host.
func TestBuildForCoversEveryShippedPlatform(t *testing.T) {
	t.Parallel()

	cases := []struct{ goos, goarch string }{
		{localmodel.OSLinux, localmodel.ArchAMD64},
		{localmodel.OSLinux, localmodel.ArchARM64},
		{localmodel.OSDarwin, localmodel.ArchARM64},
		{localmodel.OSDarwin, localmodel.ArchAMD64},
		{localmodel.OSWindows, localmodel.ArchAMD64},
	}

	for _, testCase := range cases {
		t.Run(testCase.goos+"/"+testCase.goarch, func(t *testing.T) {
			t.Parallel()

			build, ok := localmodel.BuildFor(testCase.goos, testCase.goarch)
			if !ok {
				t.Fatalf("no pinned build for %s/%s", testCase.goos, testCase.goarch)
			}

			if len(build.SHA256) != 64 {
				t.Errorf("sha256 = %q, want 64 hex characters", build.SHA256)
			}

			if build.Bytes <= 0 {
				t.Errorf("bytes = %d, want the published size so a download can show progress", build.Bytes)
			}

			if !strings.Contains(build.URL(), localmodel.PinnedBuild) {
				t.Errorf("url = %q, want the build tag that carries the binaries", build.URL())
			}
		})
	}
}

// TestBuildForRejectsUnsupportedPlatform proves an unpinned platform says so
// rather than handing back a zero localmodel.Build that would download nothing.
func TestBuildForRejectsUnsupportedPlatform(t *testing.T) {
	t.Parallel()

	if _, ok := localmodel.BuildFor("plan9", "riscv64"); ok {
		t.Error("plan9/riscv64 resolved to a pinned build")
	}
}

// TestWindowsBuildNamesTheExeServer proves the Windows pin looks for the
// executable name that archive actually carries.
func TestWindowsBuildNamesTheExeServer(t *testing.T) {
	t.Parallel()

	build, ok := localmodel.BuildFor(localmodel.OSWindows, localmodel.ArchAMD64)
	if !ok {
		t.Fatal("no windows build")
	}

	if build.Binary != localmodel.ServerBinaryWin || build.Format != localmodel.FormatZip || build.Prefix != "" {
		t.Errorf("windows build = %+v, want a flat zip carrying llama-server.exe", build)
	}
}

// TestLookupResolvesTheDefaultModel proves the default name setup uses is a
// real catalog entry with a pinned digest and revision.
func TestLookupResolvesTheDefaultModel(t *testing.T) {
	t.Parallel()

	model, err := localmodel.Lookup(localmodel.DefaultModel)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}

	if len(model.SHA256) != 64 || len(model.Revision) != 40 {
		t.Errorf("model = %+v, want a 64-hex digest and a 40-hex revision", model)
	}

	if !strings.HasPrefix(model.URL(), localmodel.HuggingFaceBase+"/"+model.Repo) {
		t.Errorf("url = %q", model.URL())
	}

	if !strings.Contains(model.CommitsURL(), model.Repo) {
		t.Errorf("commits url = %q", model.CommitsURL())
	}
}

// TestLookupRejectsUnknownName proves a typo fails with a name the user can
// act on rather than installing something they did not ask for.
func TestLookupRejectsUnknownName(t *testing.T) {
	t.Parallel()

	_, err := localmodel.Lookup("not-a-model")
	if !errors.Is(err, localmodel.ErrUnknownModel) {
		t.Fatalf("err = %v, want localmodel.ErrUnknownModel", err)
	}

	unknown, isUnknown := errors.AsType[*localmodel.UnknownModelError](err)
	if !isUnknown {
		t.Fatalf("err = %v, want a *localmodel.UnknownModelError carrying the name", err)
	}

	if unknown.Name != "not-a-model" {
		t.Errorf("name = %q, want the rejected name carried on the error", unknown.Name)
	}
}
