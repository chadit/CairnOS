package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/store/sqlite"
	"github.com/chadit/CairnOS/internal/workflow"
)

// Subcommand, flag, and layout literals the tests reuse, named so goconst
// stays quiet and a typo fails loudly in one place.
const (
	runCmd       = "run"
	memoryCmd    = "memory"
	flagDirArg   = "--dir"
	flagInputArg = "--input"

	dataDirName     = "data"
	examplesDirName = "examples"
	helloFileName   = "hello.toml"
	dbFileName      = "cairnos.db"
)

// staleAge puts an inserted run past the default 90d retention window.
const staleAge = 100 * 24 * time.Hour

const failingWorkflow = `
[workflow]
name = "doomed"

[action.boom]
runner = "mock"
params = { fail_class = "permanent" }
`

const typedWorkflow = `
[workflow]
name = "typed"

[workflow.inputs.count]
type     = "int"
required = true

[workflow.inputs.tags]
type     = "string[]"
required = true

[action.emit]
runner = "mock"
params = { output = "count is ${{ inputs.count }}" }
`

const badWorkflowText = `
[workflow]
name = "Bad Name"
`

// runCLI drives one invocation against captured streams.
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	code := cli.New(&stdout, &stderr).Run(t.Context(), args)

	return code, stdout.String(), stderr.String()
}

// initDir scaffolds a fresh CairnOS home in a temp directory.
func initDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	code, _, stderr := runCLI(t, "init", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("init exited %d: %s", code, stderr)
	}

	return dir
}

// writeWorkflow drops a workflow fixture into the home's workflows folder.
func writeWorkflow(t *testing.T, dir, content string) string {
	t.Helper()

	path := filepath.Join(dir, "workflows", "test.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	return path
}

// openRunStore opens the scaffolded home's run store directly.
func openRunStore(t *testing.T, dir string) *sqlite.Store {
	t.Helper()

	db, err := sqlite.Open(filepath.Join(dir, dataDirName, dbFileName))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	return db
}

func TestInitScaffoldsRunnableLayout(t *testing.T) {
	t.Parallel()

	dir := initDir(t)

	for _, sub := range []string{dataDirName, "workflows", examplesDirName} {
		info, err := os.Stat(filepath.Join(dir, sub))
		if err != nil || !info.IsDir() {
			t.Fatalf("missing scaffold directory %s: %v", sub, err)
		}
	}

	flow, err := workflow.Load(filepath.Join(dir, examplesDirName, helloFileName))
	if err != nil {
		t.Fatalf("example workflow does not parse: %v", err)
	}

	if flow.Name != "hello" || len(flow.Actions) != 2 {
		t.Fatalf("example = %q with %d actions, want hello with 2", flow.Name, len(flow.Actions))
	}

	cfg, err := config.Load(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatalf("scaffolded config does not load: %v", err)
	}

	// The rendered file must round-trip the compiled-in defaults, which also
	// proves the duration formatting ("90d", "10m") parses back.
	def := config.Default()
	if cfg.Engine.MaxConcurrentActions != def.Engine.MaxConcurrentActions {
		t.Fatalf("max_concurrent_actions = %d, want %d",
			cfg.Engine.MaxConcurrentActions, def.Engine.MaxConcurrentActions)
	}

	if cfg.Engine.DefaultTimeout != def.Engine.DefaultTimeout {
		t.Fatalf("default_timeout = %v, want %v", cfg.Engine.DefaultTimeout, def.Engine.DefaultTimeout)
	}

	if cfg.Retention.Runs != def.Retention.Runs || cfg.Retention.Outputs != def.Retention.Outputs {
		t.Fatalf("retention = %v/%v, want %v/%v",
			cfg.Retention.Runs, cfg.Retention.Outputs, def.Retention.Runs, def.Retention.Outputs)
	}
}

func TestRunExecutesHelloEndToEnd(t *testing.T) {
	t.Parallel()

	dir := initDir(t)

	code, stdout, stderr := runCLI(t, runCmd,
		filepath.Join(dir, examplesDirName, helloFileName), flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("run exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, "echo: hello, world") || !strings.Contains(stdout, "succeeded") {
		t.Fatalf("summary missing interpolated output or status:\n%s", stdout)
	}

	db := openRunStore(t, dir)

	runs, err := db.ListRuns(t.Context(), store.ListFilter{})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}

	if len(runs) != 1 || runs[0].Status != store.RunSucceeded {
		t.Fatalf("persisted runs = %+v, want one succeeded row", runs)
	}

	if runs[0].WorkflowHash == "" {
		t.Fatal("persisted run carries no workflow hash; Load should have stamped it")
	}
}

func TestRunFailingWorkflowExitsOne(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	path := writeWorkflow(t, dir, failingWorkflow)

	code, stdout, _ := runCLI(t, runCmd, path, flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("run exited %d, want %d", code, cli.ExitFailure)
	}

	if !strings.Contains(stdout, "failed") {
		t.Fatalf("summary does not report the failure:\n%s", stdout)
	}
}

func TestRunCoercesTypedInputs(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	path := writeWorkflow(t, dir, typedWorkflow)

	code, stdout, stderr := runCLI(t, runCmd, path,
		flagInputArg, "count=5", flagInputArg, "tags=a,b", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("run exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, "count is 5") {
		t.Fatalf("int input did not interpolate:\n%s", stdout)
	}

	code, _, stderr = runCLI(t, runCmd, path,
		flagInputArg, "count=five", flagInputArg, "tags=a,b", flagDirArg, dir)
	if code != cli.ExitUsage {
		t.Fatalf("uncoercible input exited %d, want %d (stderr: %s)", code, cli.ExitUsage, stderr)
	}
}

func TestValidateAcceptsExample(t *testing.T) {
	t.Parallel()

	dir := initDir(t)

	code, stdout, stderr := runCLI(t, "validate", filepath.Join(dir, examplesDirName, helloFileName))
	if code != cli.ExitOK {
		t.Fatalf("validate exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, "valid") {
		t.Fatalf("stdout = %q, want valid", stdout)
	}
}

func TestValidateRejectsBadFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "bad.toml")
	if err := os.WriteFile(path, []byte(badWorkflowText), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	code, _, stderr := runCLI(t, "validate", path)
	if code != cli.ExitUsage {
		t.Fatalf("validate exited %d, want %d", code, cli.ExitUsage)
	}

	if !strings.Contains(stderr, workflow.CodeBadName) {
		t.Fatalf("stderr does not name the canonical code:\n%s", stderr)
	}
}

func TestRunsListAndShowDisplayRun(t *testing.T) {
	t.Parallel()

	dir := initDir(t)

	code, _, stderr := runCLI(t, runCmd,
		filepath.Join(dir, examplesDirName, helloFileName), flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("run exited %d: %s", code, stderr)
	}

	db := openRunStore(t, dir)

	runs, err := db.ListRuns(t.Context(), store.ListFilter{})
	if err != nil || len(runs) != 1 {
		t.Fatalf("list runs: %v (%d rows)", err, len(runs))
	}

	runID := runs[0].ID

	code, stdout, _ := runCLI(t, "runs", "list", flagDirArg, dir)
	if code != cli.ExitOK || !strings.Contains(stdout, runID) || !strings.Contains(stdout, "hello") {
		t.Fatalf("runs list (exit %d) missing the run:\n%s", code, stdout)
	}

	code, stdout, _ = runCLI(t, "runs", "show", runID, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("runs show exited %d", code)
	}

	for _, want := range []string{"greet", "echo", "succeeded", runID} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("runs show missing %q:\n%s", want, stdout)
		}
	}
}

func TestMemoryLifecycle(t *testing.T) {
	t.Parallel()

	dir := initDir(t)

	code, stdout, stderr := runCLI(t, memoryCmd, "add",
		"--scope", "global", "--content", "prefer table tests", "--tags", "style,go", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("memory add exited %d: %s", code, stderr)
	}

	id := strings.TrimSpace(stdout)
	if id == "" {
		t.Fatal("memory add printed no id")
	}

	code, stdout, _ = runCLI(t, memoryCmd, "get", id, flagDirArg, dir)
	if code != cli.ExitOK || !strings.Contains(stdout, "prefer table tests") {
		t.Fatalf("memory get (exit %d) missing content:\n%s", code, stdout)
	}

	code, stdout, _ = runCLI(t, memoryCmd, "search", "--text", "table", flagDirArg, dir)
	if code != cli.ExitOK || !strings.Contains(stdout, id) {
		t.Fatalf("memory search (exit %d) missing the entry:\n%s", code, stdout)
	}

	code, _, _ = runCLI(t, memoryCmd, "rm", id, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("memory rm exited %d", code)
	}

	code, _, _ = runCLI(t, memoryCmd, "get", id, flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("memory get after rm exited %d, want %d", code, cli.ExitFailure)
	}
}

func TestPruneDeletesOldRuns(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	db := openRunStore(t, dir)

	stale := &store.Run{
		ID:           "stale-run",
		WorkflowName: "old",
		Status:       store.RunSucceeded,
		Trigger:      "cli",
		CreatedAt:    time.Now().Add(-staleAge),
	}
	if err := db.CreateRun(t.Context(), stale); err != nil {
		t.Fatalf("insert stale run: %v", err)
	}

	code, stdout, stderr := runCLI(t, "prune", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("prune exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, "deleted 1 run(s)") {
		t.Fatalf("prune stats missing the deletion:\n%s", stdout)
	}

	runs, err := db.ListRuns(t.Context(), store.ListFilter{})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}

	if len(runs) != 0 {
		t.Fatalf("%d run(s) survived the prune", len(runs))
	}
}

func TestHealthOnFreshInit(t *testing.T) {
	t.Parallel()

	dir := initDir(t)

	code, stdout, stderr := runCLI(t, "health", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("health exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, "ok") {
		t.Fatalf("stdout = %q, want ok", stdout)
	}
}
