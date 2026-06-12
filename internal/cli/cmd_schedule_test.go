package cli_test

import (
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
)

// Schedule fixture literals, named per the repo's goconst-in-tests policy.
const (
	scheduleCmd      = "schedule"
	scheduledName    = "ticker"
	scheduledCron    = "@hourly"
	scheduleArgList  = "list"
	scheduleArgAdd   = "add"
	scheduleArgEvict = "remove"
)

const scheduledWorkflow = `
[workflow]
name     = "ticker"
schedule = "@hourly"

[action.tick]
runner = "mock"
params = { output = "tick" }
`

// addSchedule runs schedule add for path and returns the printed id.
func addSchedule(t *testing.T, dir, path string) string {
	t.Helper()

	code, stdout, stderr := runCLI(t, scheduleCmd, scheduleArgAdd, path, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("schedule add exited %d: %s", code, stderr)
	}

	id := strings.TrimSpace(stdout)
	if id == "" {
		t.Fatal("schedule add printed no id")
	}

	return id
}

func TestScheduleLifecycle(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	path := writeWorkflow(t, dir, scheduledWorkflow)
	id := addSchedule(t, dir, path)

	code, stdout, _ := runCLI(t, scheduleCmd, scheduleArgList, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("schedule list exited %d", code)
	}

	for _, want := range []string{id, scheduledName, scheduledCron, "NEXT FIRE"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("schedule list missing %q:\n%s", want, stdout)
		}
	}

	code, stdout, _ = runCLI(t, scheduleCmd, scheduleArgEvict, id, flagDirArg, dir)
	if code != cli.ExitOK || !strings.Contains(stdout, id) {
		t.Fatalf("schedule remove (exit %d) output:\n%s", code, stdout)
	}

	code, stdout, _ = runCLI(t, scheduleCmd, scheduleArgList, flagDirArg, dir)
	if code != cli.ExitOK || strings.Contains(stdout, id) {
		t.Fatalf("removed schedule still listed (exit %d):\n%s", code, stdout)
	}
}

func TestScheduleRemoveByWorkflowName(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	path := writeWorkflow(t, dir, scheduledWorkflow)
	id := addSchedule(t, dir, path)

	code, _, _ := runCLI(t, scheduleCmd, scheduleArgEvict, scheduledName, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("schedule remove by name exited %d", code)
	}

	code, stdout, _ := runCLI(t, scheduleCmd, scheduleArgList, flagDirArg, dir)
	if code != cli.ExitOK || strings.Contains(stdout, id) {
		t.Fatalf("schedule survived remove-by-name (exit %d):\n%s", code, stdout)
	}
}

func TestScheduleRemoveUnknownFails(t *testing.T) {
	t.Parallel()

	dir := initDir(t)

	code, _, stderr := runCLI(t, scheduleCmd, scheduleArgEvict, "no-such-schedule", flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("schedule remove exited %d, want %d", code, cli.ExitFailure)
	}

	if !strings.Contains(stderr, "not found") {
		t.Fatalf("stderr does not explain the miss:\n%s", stderr)
	}
}

func TestScheduleAddRejectsWorkflowWithoutScheduleKey(t *testing.T) {
	t.Parallel()

	dir := initDir(t)

	// The hello example parses fine but declares no schedule key, so there
	// is nothing to put on the calendar.
	code, _, stderr := runCLI(t, scheduleCmd, scheduleArgAdd,
		dir+"/"+examplesDirName+"/"+helloFileName, flagDirArg, dir)
	if code != cli.ExitUsage {
		t.Fatalf("schedule add exited %d, want %d", code, cli.ExitUsage)
	}

	if !strings.Contains(stderr, "no schedule key") {
		t.Fatalf("stderr does not explain the missing key:\n%s", stderr)
	}
}

func TestScheduleAddRejectsUnparseableWorkflow(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	path := writeWorkflow(t, dir, badWorkflowText)

	code, _, _ := runCLI(t, scheduleCmd, scheduleArgAdd, path, flagDirArg, dir)
	if code != cli.ExitUsage {
		t.Fatalf("schedule add exited %d, want %d", code, cli.ExitUsage)
	}
}
