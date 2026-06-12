package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
)

const (
	pauseCmd  = "pause"
	resumeCmd = "resume"
	auditCmd  = "audit"
	budgetCmd = "budget"
)

// budgetedFlow carries a month budget plus a costed action so the budget
// command has both a limit and seeded spend to report.
const budgetedFlow = `
[workflow]
name = "billable"

[workflow.budget]
period = "month"
limit_usd = 10.0
on_exceed = "pause"

[action.work]
runner = "mock"
params = { cost_usd = 0.25 }
`

// writeWorkflowFile drops a workflow into the home's workflows directory so
// name-based commands (budget) can resolve it, returning the file path.
func writeWorkflowFile(t *testing.T, dir, name, content string) string {
	t.Helper()

	workflowsDir := filepath.Join(dir, "workflows")
	if err := os.MkdirAll(workflowsDir, 0o750); err != nil {
		t.Fatalf("create workflows dir: %v", err)
	}

	path := filepath.Join(workflowsDir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	return path
}

func TestPauseResumeAndAuditTrail(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if code, _, stderr := runCLI(t, pauseCmd, "billable", "--reason", "manual hold", flagDirArg, dir); code != cli.ExitOK {
		t.Fatalf("pause exit = %d\nstderr: %s", code, stderr)
	}

	if code, _, stderr := runCLI(t, resumeCmd, "billable", flagDirArg, dir); code != cli.ExitOK {
		t.Fatalf("resume exit = %d\nstderr: %s", code, stderr)
	}

	code, stdout, stderr := runCLI(t, auditCmd, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("audit exit = %d\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stdout, "pause") || !strings.Contains(stdout, "resume") {
		t.Fatalf("audit output %q missing the pause and resume rows", stdout)
	}

	if !strings.Contains(stdout, "manual hold") {
		t.Fatalf("audit output %q missing the pause reason detail", stdout)
	}
}

func TestResumeUnpausedIsNoOp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	code, stdout, stderr := runCLI(t, resumeCmd, "never-paused", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("resume of unpaused workflow exit = %d\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stdout, "was not paused") {
		t.Fatalf("resume output %q missing the not-paused notice", stdout)
	}
}

func TestBudgetShowsSpendAndLimit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	flowPath := writeWorkflowFile(t, dir, "billable.toml", budgetedFlow)

	// One run seeds $0.25 of spend in the current month.
	if code, _, stderr := runCLI(t, runCmd, flowPath, flagDirArg, dir); code != cli.ExitOK {
		t.Fatalf("seed run exit = %d\nstderr: %s", code, stderr)
	}

	code, stdout, stderr := runCLI(t, budgetCmd, "billable", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("budget exit = %d\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stdout, "$0.2500 of $10.0000") {
		t.Fatalf("budget output %q missing the spend-versus-limit line", stdout)
	}

	if !strings.Contains(stdout, "Paused:   false") {
		t.Fatalf("budget output %q missing the unpaused state", stdout)
	}
}

func TestBudgetReportsNoneForBudgetlessWorkflow(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeWorkflowFile(t, dir, "plain.toml", "[workflow]\nname = \"plain\"\n[action.a]\nrunner = \"mock\"\n")

	code, stdout, stderr := runCLI(t, budgetCmd, "plain", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("budget exit = %d\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stdout, "Budget:   none") {
		t.Fatalf("budget output %q missing the none notice", stdout)
	}
}
