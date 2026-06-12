package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
)

// costCmd is the subcommand under test.
const costCmd = "cost"

// costedWorkflow carries mock costs so the aggregation has real numbers:
// two actions at $0.25 each, one run = $0.50.
const costedWorkflow = `
[workflow]
name = "billable"

[action.first]
runner = "mock"
params = { cost_usd = 0.25 }

[action.second]
runner = "mock"
depends_on = ["first"]
params = { cost_usd = 0.25 }
`

func TestCostAggregatesSeededRun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	flowPath := filepath.Join(dir, "billable.toml")
	if err := os.WriteFile(flowPath, []byte(costedWorkflow), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	var runOut, runErr bytes.Buffer

	if code := cli.New(&runOut, &runErr).Run(t.Context(), []string{runCmd, flowPath, flagDirArg, dir}); code != cli.ExitOK {
		t.Fatalf("seed run exit = %d\nstderr: %s", code, runErr.String())
	}

	var stdout, stderr bytes.Buffer

	if code := cli.New(&stdout, &stderr).Run(t.Context(), []string{costCmd, flagDirArg, dir}); code != cli.ExitOK {
		t.Fatalf("cost exit = %d\nstderr: %s", code, stderr.String())
	}

	out := stdout.String()

	if !strings.Contains(out, "billable") {
		t.Fatalf("cost output %q missing the workflow row", out)
	}

	if !strings.Contains(out, "$0.5000") {
		t.Fatalf("cost output %q missing the $0.5000 aggregate", out)
	}

	if !strings.Contains(out, "TOTAL") {
		t.Fatalf("cost output %q missing the total row", out)
	}
}

func TestCostFiltersByWorkflowAndWindow(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	flowPath := filepath.Join(dir, "billable.toml")
	if err := os.WriteFile(flowPath, []byte(costedWorkflow), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	var runOut, runErr bytes.Buffer

	if code := cli.New(&runOut, &runErr).Run(t.Context(), []string{runCmd, flowPath, flagDirArg, dir}); code != cli.ExitOK {
		t.Fatalf("seed run exit = %d\nstderr: %s", code, runErr.String())
	}

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "other workflow matches nothing",
			args: []string{costCmd, "--workflow", "ghost", flagDirArg, dir},
			want: "no runs matched",
		},
		{
			name: "recent window includes the run",
			args: []string{costCmd, "--since", "1h", flagDirArg, dir},
			want: "billable",
		},
		{
			name: "bad since flag is a usage error",
			args: []string{costCmd, "--since", "soonish", flagDirArg, dir},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer

			code := cli.New(&stdout, &stderr).Run(t.Context(), tt.args)

			if tt.want == "" {
				if code != cli.ExitUsage {
					t.Fatalf("exit = %d, want usage error", code)
				}

				return
			}

			if code != cli.ExitOK {
				t.Fatalf("exit = %d\nstderr: %s", code, stderr.String())
			}

			if !strings.Contains(stdout.String(), tt.want) {
				t.Fatalf("output %q missing %q", stdout.String(), tt.want)
			}
		})
	}
}
