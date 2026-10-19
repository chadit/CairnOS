package cli_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
)

// Schedule fixture literals, named per the repo's goconst-in-tests policy.
const (
	scheduleCmd      = "schedule"
	scheduledName    = "ticker"
	scheduledCron    = "@hourly"
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

	code, stdout, _ := runCLI(t, scheduleCmd, listArg, flagDirArg, dir)
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

	code, stdout, _ = runCLI(t, scheduleCmd, listArg, flagDirArg, dir)
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

	code, stdout, _ := runCLI(t, scheduleCmd, listArg, flagDirArg, dir)
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

const pollAddKnownSource = `
[workflow]
name     = "poller_known"
schedule = "@hourly"

[workflow.poll]
source = "github_issues"

[action.tick]
runner = "mock"
params = { output = "tick" }
`

const pollAddUnknownSource = `
[workflow]
name     = "poller_unknown"
schedule = "@hourly"

[workflow.poll]
source = "not_a_real_source"

[action.tick]
runner = "mock"
params = { output = "tick" }
`

func TestScheduleAddWarnsOnUnknownPollSource(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	path := writeWorkflow(t, dir, pollAddUnknownSource)

	// An unknown source parses fine (it fails only at fire time), so add still
	// succeeds; the warning is the operator's early heads-up.
	code, _, stderr := runCLI(t, scheduleCmd, scheduleArgAdd, path, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("schedule add exited %d, want OK with a warning: %s", code, stderr)
	}

	if !strings.Contains(stderr, "unknown poll source") {
		t.Fatalf("stderr missing the unknown-source warning:\n%s", stderr)
	}
}

func TestScheduleAddAcceptsKnownPollSourceQuietly(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	path := writeWorkflow(t, dir, pollAddKnownSource)

	code, stdout, stderr := runCLI(t, scheduleCmd, scheduleArgAdd, path, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("schedule add exited %d: %s", code, stderr)
	}

	if strings.Contains(stderr, "unknown poll source") {
		t.Fatalf("known poll source warned unexpectedly:\n%s", stderr)
	}

	if strings.TrimSpace(stdout) == "" {
		t.Fatal("schedule add printed no id for a valid poll workflow")
	}
}

// The workflow template and the assertions that look for them share these
// required-input fixture names. title is a key every github_issues event
// carries, and issues_repo is not.
const (
	requiredInputFlowName = "needs_input"
	emittedInputName      = "title"
	unemittedInputName    = "issues_repo"
	githubIssuesPoll      = "[workflow.poll]\nsource = \"github_issues\"\n"
)

// inputsWorkflow is an hourly workflow. The verbs take the workflow name and
// the TOML tables that set up the case, such as input declarations and a poll
// source.
const inputsWorkflow = `
[workflow]
name     = %q
schedule = "@hourly"

%s
[action.tick]
runner = "mock"
params = { output = "tick" }
`

// requiredInput declares a required string input, appending extraKeys such as
// a default to its table.
func requiredInput(name, extraKeys string) string {
	return fmt.Sprintf("[workflow.inputs.%s]\ntype = \"string\"\nrequired = true\n%s\n", name, extraKeys)
}

// writeInputsWorkflow writes an inputsWorkflow built from tables into a fresh
// data dir and returns the dir and the workflow path.
func writeInputsWorkflow(t *testing.T, tables ...string) (string, string) {
	t.Helper()

	dir := initDir(t)

	return dir, writeWorkflow(t, dir, fmt.Sprintf(inputsWorkflow, requiredInputFlowName, strings.Join(tables, "\n")))
}

// TestScheduleAddRejectsInputsNoFireSupplies covers workflows every fire would
// fail: a plain scheduled run supplies no inputs, and a github_issues poll fire
// supplies only the issue fields. Add refuses each, names every missing input
// in sorted order, and stores nothing.
func TestScheduleAddRejectsInputsNoFireSupplies(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		missing string
		tables  []string
	}{
		{
			name:    "plain schedule with a required input",
			missing: unemittedInputName,
			tables:  []string{requiredInput(unemittedInputName, "")},
		},
		{
			name:    "plain schedule names every missing input sorted",
			missing: "alpha, zeta",
			tables:  []string{requiredInput("zeta", ""), requiredInput("alpha", "")},
		},
		{
			name:    "poll source never emits the input",
			missing: unemittedInputName,
			tables:  []string{requiredInput(emittedInputName, ""), requiredInput(unemittedInputName, ""), githubIssuesPoll},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			dir, path := writeInputsWorkflow(t, testCase.tables...)

			code, _, stderr := runCLI(t, scheduleCmd, scheduleArgAdd, path, flagDirArg, dir)
			if code != cli.ExitUsage {
				t.Fatalf("schedule add exited %d, want %d (stderr: %s)", code, cli.ExitUsage, stderr)
			}

			if !strings.Contains(stderr, ": "+testCase.missing+"; give each one a default") {
				t.Fatalf("stderr does not name exactly %q and the default fix:\n%s", testCase.missing, stderr)
			}

			code, stdout, _ := runCLI(t, scheduleCmd, listArg, flagDirArg, dir)
			if code != cli.ExitOK || strings.Contains(stdout, requiredInputFlowName) {
				t.Fatalf("rejected workflow was stored (exit %d):\n%s", code, stdout)
			}
		})
	}
}

// TestScheduleAddAcceptsInputsAFireFills covers inputs that never fail a fire:
// one with a default, an optional one, and one the poll source emits. A poll
// source no wiring builds is not checked, since add cannot know its keys; add
// warns about that source instead.
func TestScheduleAddAcceptsInputsAFireFills(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		tables []string
	}{
		{
			name:   "required input with a default",
			tables: []string{requiredInput(unemittedInputName, `default = "owner/repo"`)},
		},
		{
			name:   "optional input with no default",
			tables: []string{"[workflow.inputs.note]\ntype = \"string\"\n"},
		},
		{
			name:   "poll source emits the input",
			tables: []string{requiredInput(emittedInputName, ""), githubIssuesPoll},
		},
		{
			name:   "unknown poll source is not checked",
			tables: []string{requiredInput(unemittedInputName, ""), "[workflow.poll]\nsource = \"not_a_real_source\"\n"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			dir, path := writeInputsWorkflow(t, testCase.tables...)

			addSchedule(t, dir, path)

			code, stdout, _ := runCLI(t, scheduleCmd, listArg, flagDirArg, dir)
			if code != cli.ExitOK || !strings.Contains(stdout, requiredInputFlowName) {
				t.Fatalf("schedule list (exit %d) missing the workflow:\n%s", code, stdout)
			}
		})
	}
}
