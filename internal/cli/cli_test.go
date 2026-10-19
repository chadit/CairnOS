package cli_test

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/bitwarden"
	"github.com/chadit/CairnOS/internal/bwinstall"
	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/store/sqlite"
	"github.com/chadit/CairnOS/internal/workflow"
)

// Subcommand, flag, and layout literals the tests reuse, named so goconst
// stays quiet and a typo fails loudly in one place.
const (
	runCmd        = "run"
	memoryCmd     = "memory"
	serveCmd      = "serve"
	flagDirArg    = "--dir"
	flagInputArg  = "--input"
	flagDryRunArg = "--dry-run"
	listArg       = "list"

	dataDirName     = "data"
	examplesDirName = "examples"
	helloFileName   = "hello.toml"

	// helloWorkflowName is the name the scaffolded hello.toml declares.
	helloWorkflowName = "hello"
	dbFileName        = "cairnos.db"
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

	return runPinnedCLI(t, testBinaryDigest(t), args...)
}

// runPinnedCLI drives one invocation with the Bitwarden release pinned to a
// given digest. A case about the refusal passes a digest nothing matches.
func runPinnedCLI(t *testing.T, binarySum string, args ...string) (int, string, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	code := pinnedApp(t, &stdout, &stderr, binarySum).Run(t.Context(), args)

	return code, stdout.String(), stderr.String()
}

// pinnedApp builds the App every run helper drives. A command that resolves bw
// refuses anything but the pinned digest, so a helper building an App without
// passing through here would skip that refusal. The asset URL is one nothing
// serves, and the stepping clock spends the readiness budget in polls, not on a
// wall clock a busy host runs past.
func pinnedApp(t *testing.T, stdout, stderr *bytes.Buffer, binarySum string) *cli.App {
	t.Helper()

	return cli.New(stdout, stderr).
		WithBitwardenPin(unreachableAsset, binarySum).
		WithBitwardenClock(bitwarden.NewSteppingClock(bwReadyPolls))
}

// runEnvCLI drives one invocation over a staged environment instead of the
// process one, so a case that needs PATH or a token set can still run beside
// every other case rather than serializing the whole package.
func runEnvCLI(t *testing.T, env map[string]string, args ...string) (int, string, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	code := pinnedApp(t, &stdout, &stderr, testBinaryDigest(t)).
		WithEnv(environ.Map(env)).
		Run(t.Context(), args)

	return code, stdout.String(), stderr.String()
}

// pathEnvKey is the variable the command search reads, staged per case.
const pathEnvKey = "PATH"

// cliTrigger is the trigger a run started from this binary records.
const cliTrigger = "cli"

// pathEnv is the staged environment for a case that puts one directory in front
// of the command's PATH search. Nothing else is inherited, so a bw on the
// developer's own machine can never answer for the fake.
func pathEnv(dir string) map[string]string {
	return map[string]string{pathEnvKey: dir}
}

// fakeBridgeToken stands in for a bridge credential. It is not a real token and
// never leaves the process; the bridges only check that the variable is set.
const fakeBridgeToken = "fake-not-a-real-token" // #nosec G101 -- fixture, not a credential

// tokenEnv is the staged environment for a case that needs one variable set.
func tokenEnv(name, value string) map[string]string {
	return map[string]string{name: value}
}

// withSourcePath adds the host's PATH to a staged environment. A config that
// carries a [source.<name>] block is refused unless the version-control binary
// sits on the PATH the loader is handed, so a case about such a config has to
// pass a real one. Reading the process PATH changes nothing, so the case still
// runs beside every other one.
func withSourcePath(env map[string]string) map[string]string {
	staged := maps.Clone(env)
	staged[pathEnvKey] = os.Getenv(pathEnvKey)

	return staged
}

// runUnpinnedEnvCLI drives one invocation on a platform this build pins no
// release for, which runtime.GOOS makes unreachable any other way, over a
// staged environment so the case still runs beside every other one.
func runUnpinnedEnvCLI(t *testing.T, env map[string]string, args ...string) (int, string, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	code := cli.New(&stdout, &stderr).
		WithoutBitwardenPin().
		WithBitwardenClock(bitwarden.NewSteppingClock(bwReadyPolls)).
		WithEnv(environ.Map(env)).
		Run(t.Context(), args)

	return code, stdout.String(), stderr.String()
}

// testBinaryDigest hashes the binary running this suite. stageFakeBW stages its
// fake bw as a symlink to this same file, so this is the digest of the binary
// those cases actually put on PATH. The result is cached because every
// invocation asks for it and the test binary runs to tens of megabytes.
func testBinaryDigest(t *testing.T) string {
	t.Helper()

	digest, err := cachedTestBinaryDigest()
	if err != nil {
		t.Fatalf("hash the test binary: %v", err)
	}

	return digest
}

// cachedTestBinaryDigest hashes the test binary once per process.
//
//nolint:gochecknoglobals // a read-only cache of one immutable value, computed on first use
var cachedTestBinaryDigest = sync.OnceValues(func() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate the test binary: %w", err)
	}

	return bwinstall.FileDigest(self)
})

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

// appendConfig adds a block to the scaffolded config, keeping what init wrote so
// the block sits beside the defaults the way an operator's edit would.
func appendConfig(t *testing.T, dir, block string) {
	t.Helper()

	cfgPath := filepath.Join(dir, cli.ConfigFileName)

	existing, err := os.ReadFile(cfgPath) // #nosec G304 -- test-owned scaffold path
	if err != nil {
		t.Fatalf("read scaffolded config: %v", err)
	}

	if err := os.WriteFile(cfgPath, append(existing, block...), 0o600); err != nil {
		t.Fatalf("append to the scaffolded config: %v", err)
	}
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

// onlyActionOutput returns the stored output of the single action in the single
// run the home holds. It fails unless that run is stored as succeeded, because
// a zero exit alone does not prove the store recorded the outcome.
func onlyActionOutput(t *testing.T, dir string) any {
	t.Helper()

	db := openRunStore(t, dir)

	runs, err := db.ListRuns(t.Context(), store.ListFilter{})
	if err != nil || len(runs) != 1 || runs[0].Status != store.RunSucceeded {
		t.Fatalf("list runs = %+v, %v; want one succeeded run", runs, err)
	}

	results, err := db.ListActionResults(t.Context(), runs[0].ID)
	if err != nil || len(results) != 1 {
		t.Fatalf("action results = %+v, %v; want one result", results, err)
	}

	return results[0].Output
}

// TestInitScaffoldsRunnableLayout checks that the rendered config round-trips
// the compiled-in defaults, which also proves the duration formatting ("90d",
// "10m") parses back.
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

	if flow.Name != helloWorkflowName || len(flow.Actions) != 2 {
		t.Fatalf("example = %q with %d actions, want hello with 2", flow.Name, len(flow.Actions))
	}

	cfg, err := config.Load(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatalf("scaffolded config does not load: %v", err)
	}

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
	if code != cli.ExitOK || !strings.Contains(stdout, runID) || !strings.Contains(stdout, helloWorkflowName) {
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

const replayableWorkflow = `
[workflow]
name = "replayme"

[action.greet]
runner = "mock"
params = { output = "hi" }
`

const replayableWorkflowEdited = `
[workflow]
name = "replayme"

[action.greet]
runner = "mock"
params = { output = "hi there" }
`

// TestRunsReplayHashGuard proves the replay hash check: after the workflow file
// changes, a bare replay is refused with the mismatch explained, and --force
// overrides it and mints a new run.
func TestRunsReplayHashGuard(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	path := writeWorkflow(t, dir, replayableWorkflow)

	code, _, stderr := runCLI(t, runCmd, path, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("run exited %d: %s", code, stderr)
	}

	db := openRunStore(t, dir)

	runs, err := db.ListRuns(t.Context(), store.ListFilter{})
	if err != nil || len(runs) != 1 {
		t.Fatalf("list runs: %v (%d rows)", err, len(runs))
	}

	runID := runs[0].ID

	writeWorkflow(t, dir, replayableWorkflowEdited)

	code, _, stderr = runCLI(t, "runs", "replay", runID, flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("replay of a changed workflow exited %d, want %d (stderr: %s)", code, cli.ExitFailure, stderr)
	}

	if !strings.Contains(stderr, "changed since run") {
		t.Fatalf("replay error does not explain the hash mismatch:\n%s", stderr)
	}

	code, stdout, stderr := runCLI(t, "runs", "replay", runID, "--force", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("forced replay exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, "replaying run "+runID) {
		t.Fatalf("forced replay did not report the new run:\n%s", stdout)
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

// TestMemoryExportImportRoundTrip edits an exported file on disk, which is the
// point of file-based memory: grep and edit offline, then import the change
// back. A second import with no further edits must report everything unchanged.
func TestMemoryExportImportRoundTrip(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	exportDir := filepath.Join(t.TempDir(), "export")

	code, stdout, stderr := runCLI(t, memoryCmd, "add",
		"--scope", "global", "--content", "prefer table tests", "--tags", "style,go", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("memory add exited %d: %s", code, stderr)
	}

	id := strings.TrimSpace(stdout)
	if id == "" {
		t.Fatal("memory add printed no id")
	}

	code, stdout, stderr = runCLI(t, memoryCmd, "export", "--out", exportDir, flagDirArg, dir)
	if code != cli.ExitOK || !strings.Contains(stdout, "exported 1 entries") {
		t.Fatalf("memory export (exit %d) stdout=%q stderr=%q", code, stdout, stderr)
	}

	mdPath := filepath.Join(exportDir, id+".md")

	data, err := os.ReadFile(mdPath) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("read exported file: %v", err)
	}

	edited := strings.Replace(string(data), "prefer table tests", "prefer external test packages", 1)
	if err := os.WriteFile(mdPath, []byte(edited), 0o600); err != nil {
		t.Fatalf("rewrite exported file: %v", err)
	}

	code, stdout, stderr = runCLI(t, memoryCmd, "import", "--from", exportDir, flagDirArg, dir)
	if code != cli.ExitOK || !strings.Contains(stdout, "updated 1") {
		t.Fatalf("memory import (exit %d) stdout=%q stderr=%q", code, stdout, stderr)
	}

	code, stdout, _ = runCLI(t, memoryCmd, "get", id, flagDirArg, dir)
	if code != cli.ExitOK || !strings.Contains(stdout, "prefer external test packages") {
		t.Fatalf("memory get (exit %d) missing the offline edit:\n%s", code, stdout)
	}

	code, stdout, _ = runCLI(t, memoryCmd, "import", "--from", exportDir, flagDirArg, dir)
	if code != cli.ExitOK || !strings.Contains(stdout, "unchanged 1") {
		t.Fatalf("second import (exit %d) is not a no-op:\n%s", code, stdout)
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
		Trigger:      cliTrigger,
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
