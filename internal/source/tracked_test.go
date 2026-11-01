package source_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/source"
	"github.com/chadit/CairnOS/internal/workflow"
)

// Paths the committed-file cases lay out: two workflows the glob matches, one
// file beside them it does not, one nested a level too deep for a single *, and
// the files the checkout gains without origin ever holding them.
const (
	workflowGlob   = "engines/*/cairnos/*.toml"
	firstWorkflow  = "engines/garmin/cairnos/sync.toml"
	secondWorkflow = "engines/mail/cairnos/digest.toml"
	notWorkflow    = "engines/garmin/README.md"
	tooDeep        = "engines/garmin/cairnos/old/sync.toml"
	plantedFile    = "engines/planted/cairnos/run.toml"
	stagedFile     = "engines/staged/cairnos/run.toml"
	localCommit    = "engines/local/cairnos/run.toml"
)

// commitFiles writes each path under the work tree, commits them together, and
// pushes, so origin holds them.
func (o origin) commitFiles(t *testing.T, paths ...string) {
	t.Helper()

	for _, rel := range paths {
		writeFixtureFile(t, filepath.Join(o.work, rel), firstContent)
	}

	git(t, o.work, append([]string{"add", "--"}, paths...)...)
	git(t, o.work, "commit", "-m", "add files")
	o.push(t)
}

// tamperedCheckout clones repo and then gives the checkout, without origin's
// knowledge, one file only on disk, one only in the index, and one committed
// locally on top of origin's tip, the planted commit dirty_reset = false keeps.
func tamperedCheckout(t *testing.T, repo origin) source.Spec {
	t.Helper()

	spec := specFor(t, repo, branchMain)
	mustEnsure(t, spec)
	setIdentity(t, spec.Dest)

	writeFixtureFile(t, filepath.Join(spec.Dest, localCommit), untrackedBody)
	git(t, spec.Dest, "add", "--", localCommit)
	git(t, spec.Dest, "commit", "-m", "planted")

	writeFixtureFile(t, filepath.Join(spec.Dest, plantedFile), untrackedBody)
	writeFixtureFile(t, filepath.Join(spec.Dest, stagedFile), untrackedBody)
	git(t, spec.Dest, "add", "--", stagedFile)

	return spec
}

func TestCommittedFilesListsOnlyOriginMatches(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	repo.commitFiles(t, firstWorkflow, secondWorkflow, notWorkflow, tooDeep)
	spec := tamperedCheckout(t, repo)

	got, err := source.CommittedFiles(t.Context(), source.GitCommand, spec, []string{workflowGlob})
	if err != nil {
		t.Fatalf("committed files: %v", err)
	}

	want := []string{secondWorkflow, firstWorkflow}
	slices.Sort(want)

	if !slices.Equal(got, want) {
		t.Fatalf("committed files = %q, want %q", got, want)
	}
}

func TestCommittedAs(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	repo.commitFiles(t, firstWorkflow, secondWorkflow)

	// A committed symlink is not the bytes a reviewer read, even when it points
	// at a committed file.
	link := "engines/link/cairnos/run.toml"
	if err := os.MkdirAll(filepath.Join(repo.work, filepath.Dir(link)), fixtureDirPerm); err != nil {
		t.Fatalf("create link dir: %v", err)
	}

	if err := os.Symlink(filepath.Join(repo.work, firstWorkflow), filepath.Join(repo.work, link)); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	git(t, repo.work, "add", link)
	git(t, repo.work, "commit", "-m", "add link")
	repo.push(t)

	spec := tamperedCheckout(t, repo)
	pushed := workflow.GitBlobID([]byte(firstContent))
	other := workflow.GitBlobID([]byte(untrackedBody))

	cases := []struct {
		want  map[string]string
		name  string
		match bool
	}{
		{name: "origin's bytes", want: map[string]string{firstWorkflow: pushed}, match: true},
		{name: "two files, both origin's bytes", want: map[string]string{firstWorkflow: pushed, secondWorkflow: pushed}, match: true},
		{name: "bytes origin never held", want: map[string]string{firstWorkflow: other}},
		{name: "one of two files with other bytes", want: map[string]string{firstWorkflow: pushed, secondWorkflow: other}},
		{name: "committed locally, not at origin", want: map[string]string{localCommit: other}},
		{name: "only on disk", want: map[string]string{plantedFile: other}},
		{name: "staged but never committed", want: map[string]string{stagedFile: other}},
		{name: "committed symlink", want: map[string]string{link: workflow.GitBlobID([]byte(filepath.Join(repo.work, firstWorkflow)))}},
		{name: "directory prefix of a committed file", want: map[string]string{"engines/garmin": pushed}},
		{name: "no files", want: map[string]string{}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got, err := source.CommittedAs(t.Context(), source.GitCommand, spec, testCase.want)
			if err != nil {
				t.Fatalf("committed as: %v", err)
			}

			if got != testCase.match {
				t.Fatalf("committed as %v = %v, want %v", testCase.want, got, testCase.match)
			}
		})
	}
}

// TestEnsureAllFetchesBeforeDiscovery covers the startup refresh: a file pushed
// while serve was down is listed after EnsureAll even though Ensure itself
// never fetches.
func TestEnsureAllFetchesBeforeDiscovery(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	spec := specFor(t, repo, branchMain)
	mustEnsure(t, spec)

	repo.commitFiles(t, firstWorkflow)

	if err := source.EnsureAll(t.Context(), source.GitCommand, []source.Spec{spec}, nil); err != nil {
		t.Fatalf("ensure all: %v", err)
	}

	got, err := source.CommittedFiles(t.Context(), source.GitCommand, spec, []string{workflowGlob})
	if err != nil {
		t.Fatalf("committed files: %v", err)
	}

	if !slices.Equal(got, []string{firstWorkflow}) {
		t.Fatalf("committed files after startup = %q, want %q", got, []string{firstWorkflow})
	}
}

// TestEnsureAllStartsWhenTheStartupFetchFails proves an unreachable origin
// costs a warning, not the boot, and the ref already on disk stays trusted.
func TestEnsureAllStartsWhenTheStartupFetchFails(t *testing.T) {
	t.Parallel()

	repo := newOrigin(t)
	repo.commitFiles(t, firstWorkflow)

	spec := specFor(t, repo, branchMain)
	mustEnsure(t, spec)

	if err := os.Rename(repo.bare, repo.bare+".gone"); err != nil {
		t.Fatalf("hide origin: %v", err)
	}

	var logs bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&logs, nil))

	if err := source.EnsureAll(t.Context(), source.GitCommand, []source.Spec{spec}, logger); err != nil {
		t.Fatalf("ensure all with origin unreachable: %v", err)
	}

	if !strings.Contains(logs.String(), "source fetch at startup failed") {
		t.Fatalf("no warning for the failed fetch:\n%s", logs.String())
	}

	got, err := source.CommittedFiles(t.Context(), source.GitCommand, spec, []string{workflowGlob})
	if err != nil {
		t.Fatalf("committed files: %v", err)
	}

	if !slices.Equal(got, []string{firstWorkflow}) {
		t.Fatalf("committed files = %q, want %q", got, []string{firstWorkflow})
	}
}
