package workflow_test

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// TestGitBlobIDMatchesGit pins the object IDs `git hash-object` prints for the
// same bytes, so the exec gate's comparison with ls-tree output can succeed.
func TestGitBlobIDMatchesGit(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"":        "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391",
		"hello\n": "ce013625030ba8dba906f756967f9e9ca394464a",
	}

	for data, want := range cases {
		if got := workflow.GitBlobID([]byte(data)); got != want {
			t.Fatalf("GitBlobID(%q) = %s, want %s", data, got, want)
		}
	}
}

// TestLoadRecordsDefinitionFiles covers what the exec gate reads: every action
// names the workflow file with the ID of the bytes Load read, and only the
// action that included a stdin_file names that file too.
func TestLoadRecordsDefinitionFiles(t *testing.T) {
	t.Parallel()

	flowText := includeFlow(`"./notes/digest.md"`)
	path := stageWorkflow(t, flowText, map[string]string{"notes/digest.md": promptText})

	flow, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	workflowFile := runner.DefinitionFile{Path: path, BlobID: workflow.GitBlobID([]byte(flowText))}
	prompt := runner.DefinitionFile{
		Path:   filepath.Join(filepath.Dir(path), "notes", "digest.md"),
		BlobID: workflow.GitBlobID([]byte(promptText)),
	}

	want := map[string][]runner.DefinitionFile{
		"fetch":     {workflowFile},
		"summarize": {workflowFile, prompt},
	}

	for name, files := range want {
		if got := flow.Actions[name].Definition; !slices.Equal(got, files) {
			t.Fatalf("action %s definition = %+v, want %+v", name, got, files)
		}
	}
}
