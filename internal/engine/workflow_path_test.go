package engine_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// workflowPathTOML runs one local_fn action that records the task it receives.
const workflowPathTOML = `
[workflow]
name = "where"

[action.look]
runner = "local_fn"
params = { fn = "capture" }
`

// TestRunCarriesDefinitionToTheRunner proves the exec gate's input reaches a
// runner: a workflow loaded from a file hands every task that file's absolute
// path and the object ID of the bytes Load read, and one parsed from bytes
// hands none.
func TestRunCarriesDefinitionToTheRunner(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "where.toml")
	if err := os.WriteFile(path, []byte(workflowPathTOML), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	loaded, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	parsed, err := workflow.Parse([]byte(workflowPathTOML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	cases := []struct {
		flow *workflow.Workflow
		name string
		want []runner.DefinitionFile
	}{
		{name: "loaded from a file", flow: loaded, want: []runner.DefinitionFile{
			{Path: path, BlobID: workflow.GitBlobID([]byte(workflowPathTOML))},
		}},
		{name: "parsed from bytes", flow: parsed},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			sink := &paramSink{}
			eng, _ := newEngineWithRunners(t, baseConfig(), map[string]runner.Factory{
				builtins.LocalFnName: builtins.LocalFnFactory(map[string]builtins.Func{fnCapture: sink.capture}),
			})

			wantRunStatus(t, mustRun(t, eng, testCase.flow, nil), store.RunSucceeded)

			if got := sink.seenDefinition(); !slices.Equal(got, testCase.want) {
				t.Fatalf("task definition = %+v, want %+v", got, testCase.want)
			}
		})
	}
}
