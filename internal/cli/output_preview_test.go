package cli_test

import (
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/store"
)

// structuredOutputWorkflow emits a table-valued output, the shape a stored map
// comes back in from the run store.
const structuredOutputWorkflow = `
[workflow]
name = "structured"

[action.report]
runner = "mock"
params = { output = { host = "build_box", ok = true } }
`

func TestRunsShowPreviewsAStructuredOutputAsJSON(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	path := writeWorkflow(t, dir, structuredOutputWorkflow)

	if code, _, stderr := runCLI(t, runCmd, path, flagDirArg, dir); code != cli.ExitOK {
		t.Fatalf("run exited %d: %s", code, stderr)
	}

	runs, err := openRunStore(t, dir).ListRuns(t.Context(), store.ListFilter{})
	if err != nil || len(runs) != 1 {
		t.Fatalf("list runs: %v (%d rows)", err, len(runs))
	}

	code, stdout, stderr := runCLI(t, "runs", "show", runs[0].ID, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("runs show exited %d: %s", code, stderr)
	}

	if want := `{"host":"build_box","ok":true}`; !strings.Contains(stdout, want) {
		t.Fatalf("runs show =\n%s\nwant the output previewed as %s", stdout, want)
	}

	if strings.Contains(stdout, "map[") {
		t.Fatalf("runs show =\n%s\nstill prints Go map syntax", stdout)
	}
}
