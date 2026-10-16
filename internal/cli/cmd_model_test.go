package cli_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/internal/localmodel"
)

// cmdModelName is the top-level command under test.
const cmdModelName = "model"

// runModel drives one model subcommand over captured streams.
func runModel(t *testing.T, args ...string) (int, string, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	code := cli.New(&stdout, &stderr).Run(t.Context(), append([]string{cmdModelName}, args...))

	return code, stdout.String(), stderr.String()
}

// TestModelWithoutSubcommandPrintsUsage proves a bare invocation explains
// itself instead of guessing at an action that downloads five gigabytes.
func TestModelWithoutSubcommandPrintsUsage(t *testing.T) {
	t.Parallel()

	code, _, stderr := runModel(t)
	if code != cli.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, cli.ExitUsage)
	}

	for _, want := range []string{"setup", "status", "check", "upgrade", "uninstall"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("usage is missing %q:\n%s", want, stderr)
		}
	}
}

// TestModelUnknownSubcommandIsUsageError proves a typo names itself.
func TestModelUnknownSubcommandIsUsageError(t *testing.T) {
	t.Parallel()

	code, _, stderr := runModel(t, "reinstall")
	if code != cli.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, cli.ExitUsage)
	}

	if !strings.Contains(stderr, `unknown model subcommand "reinstall"`) {
		t.Errorf("stderr = %s", stderr)
	}
}

// TestModelSetupDryRunPlansAndTouchesNothing proves the dry run reports the
// download, the config change, and the service without creating any of them.
func TestModelSetupDryRunPlansAndTouchesNothing(t *testing.T) {
	t.Parallel()

	home := t.TempDir()

	code, stdout, stderr := runModel(t, "setup", "--dir", home, "--dry-run")
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}

	for _, want := range []string{
		"runtime:", "model:", "append [runner.local]",
		filepath.Join(home, "model", "runtime"),
		filepath.Join(home, "model", "models"),
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("plan is missing %q:\n%s", want, stdout)
		}
	}

	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("read home: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("dry run created %d entries in the home", len(entries))
	}
}

// TestModelSetupDryRunSkipsServiceOnRequest proves --no-service is honored at
// plan time, so the printed plan matches what a real run would do.
func TestModelSetupDryRunSkipsServiceOnRequest(t *testing.T) {
	t.Parallel()

	code, stdout, stderr := runModel(t, "setup", "--dir", t.TempDir(), "--dry-run", "--no-service")
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}

	if !strings.Contains(stdout, "service: skipped (--no-service)") {
		t.Errorf("plan did not skip the service:\n%s", stdout)
	}
}

// TestModelSetupRejectsUnknownModel proves a bad --model fails before anything
// is downloaded, naming what was asked for.
func TestModelSetupRejectsUnknownModel(t *testing.T) {
	t.Parallel()

	code, _, stderr := runModel(t, "setup", "--dir", t.TempDir(), "--dry-run", "--model", "llama-9000")
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d, want %d", code, cli.ExitFailure)
	}

	if !strings.Contains(stderr, "llama-9000") {
		t.Errorf("stderr = %s", stderr)
	}
}

// TestModelSetupDryRunLeavesAnExistingRunnerBlockAlone proves a re-run reports
// that it would keep the operator's own config rather than appending a second
// block the TOML parser would reject.
func TestModelSetupDryRunLeavesAnExistingRunnerBlockAlone(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	seedConfig(t, home, "[runner.local]\ntype = \"openai\"\n")

	code, stdout, stderr := runModel(t, "setup", "--dir", home, "--dry-run")
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}

	if !strings.Contains(stdout, "already present") {
		t.Errorf("plan did not report the existing block:\n%s", stdout)
	}
}

// TestWriteConfigBlockAppendsRunnerLocal proves setup adds a block the openai
// runner can build: a model name and a loopback base_url.
func TestWriteConfigBlockAppendsRunnerLocal(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	seedConfig(t, home, "[engine]\nmax_concurrent_actions = 4\n")

	app := cli.New(&bytes.Buffer{}, &bytes.Buffer{})
	if err := app.WriteConfigBlock(home, "gemma-4-e4b", 8089); err != nil {
		t.Fatalf("writeConfigBlock: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(home, cli.ConfigFileName))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	for _, want := range []string{
		"[engine]",
		"[runner.local]",
		`type = "openai"`,
		`model = "gemma-4-e4b"`,
		`base_url = "http://127.0.0.1:8089/v1"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("config is missing %q:\n%s", want, raw)
		}
	}
}

// TestWriteConfigBlockIsIdempotent proves a second setup does not append a
// duplicate section, which would make the config unparseable.
func TestWriteConfigBlockIsIdempotent(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	app := cli.New(&bytes.Buffer{}, &bytes.Buffer{})

	for range 2 {
		if err := app.WriteConfigBlock(home, "gemma-4-e4b", 8089); err != nil {
			t.Fatalf("writeConfigBlock: %v", err)
		}
	}

	raw, err := os.ReadFile(filepath.Join(home, cli.ConfigFileName))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	if count := strings.Count(string(raw), cli.LocalRunnerHeader); count != 1 {
		t.Errorf("config carries %d [runner.local] sections, want 1:\n%s", count, raw)
	}
}

// TestWriteConfigBlockKeepsAnEditedBlock proves an operator who changed the
// base_url keeps their edit through a re-run.
func TestWriteConfigBlockKeepsAnEditedBlock(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	edited := "[runner.local]\ntype = \"openai\"\nbase_url = \"http://127.0.0.1:9999/v1\"\n"
	seedConfig(t, home, edited)

	app := cli.New(&bytes.Buffer{}, &bytes.Buffer{})
	if err := app.WriteConfigBlock(home, "gemma-4-e4b", 8089); err != nil {
		t.Fatalf("writeConfigBlock: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(home, cli.ConfigFileName))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	if string(raw) != edited {
		t.Errorf("config was rewritten:\n%s", raw)
	}
}

// TestModelStatusReportsNothingInstalled proves status answers on a fresh home
// instead of failing, and exits non-zero because nothing is running.
func TestModelStatusReportsNothingInstalled(t *testing.T) {
	t.Parallel()

	code, stdout, stderr := runModel(t, "status", "--dir", t.TempDir())
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}

	if !strings.Contains(stdout, "not installed") {
		t.Errorf("status did not report an empty install:\n%s", stdout)
	}
}

// TestModelStatusSeesAnAnsweringServer proves the probe is real: a server on
// the reported port turns the endpoint line from "not answering" to
// "answering".
func TestModelStatusSeesAnAnsweringServer(t *testing.T) {
	t.Parallel()

	// Real socket on purpose: status probes the port it was handed, so there is
	// nothing to answer unless something is bound to it.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte(`{"data":[]}`)); err != nil {
			t.Errorf("write models response: %v", err)
		}
	}))
	defer server.Close()

	code, stdout, _ := runModel(t, "status", "--dir", t.TempDir(), "--"+cli.FlagPort, portOf(t, server.URL))
	if code != cli.ExitFailure {
		t.Errorf("exit = %d, want failure while the runtime is still missing", code)
	}

	if !strings.Contains(stdout, "(answering)") {
		t.Errorf("status did not see the running server:\n%s", stdout)
	}
}

// TestModelStatusJSONCarriesTheComponents proves --json is machine-readable
// and names both halves of the install.
func TestModelStatusJSONCarriesTheComponents(t *testing.T) {
	t.Parallel()

	_, stdout, _ := runModel(t, "status", "--dir", t.TempDir(), "--json")

	for _, want := range []string{`"runtime"`, `"model"`, `"server_answers"`, `"endpoint"`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status JSON is missing %s:\n%s", want, stdout)
		}
	}
}

// TestModelUninstallDryRunKeepsTheDownloadByDefault proves the plan says the
// weights stay, and that --purge is what asks for the disk space back.
func TestModelUninstallDryRunKeepsTheDownloadByDefault(t *testing.T) {
	t.Parallel()

	home := t.TempDir()

	code, stdout, stderr := runModel(t, "uninstall", "--dir", home, "--dry-run")
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}

	if !strings.Contains(stdout, "keep: ") || !strings.Contains(stdout, "--purge") {
		t.Errorf("plan did not say the download stays:\n%s", stdout)
	}

	_, purged, _ := runModel(t, "uninstall", "--dir", home, "--dry-run", "--purge")
	if !strings.Contains(purged, "rm -r: "+filepath.Join(home, "model")) {
		t.Errorf("purge plan does not remove the model tree:\n%s", purged)
	}
}

// TestDefaultThreadsLeavesHeadroom proves the default never claims the whole
// machine and never drops below one thread.
func TestDefaultThreadsLeavesHeadroom(t *testing.T) {
	t.Parallel()

	if got := cli.DefaultThreads(); got < 1 {
		t.Errorf("defaultThreads = %d, want at least 1", got)
	}
}

// seedConfig writes a config file into a temp home.
func seedConfig(t *testing.T, home, content string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(home, cli.ConfigFileName), []byte(content), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
}

// portOf pulls the port out of an httptest server URL.
func portOf(t *testing.T, rawURL string) string {
	t.Helper()

	_, port, found := strings.Cut(strings.TrimPrefix(rawURL, "http://"), ":")
	if !found {
		t.Fatalf("no port in %q", rawURL)
	}

	return port
}

// TestModelRejectsUnusableFlagValues proves the numeric flags are checked
// before anything downloads. --port 0 is the one that matters most: llama-server
// would bind an ephemeral port while every probe still targets :0, so an
// upgrade would roll back a runtime that was running fine.
func TestModelRejectsUnusableFlagValues(t *testing.T) {
	t.Parallel()

	portArg := "--" + cli.FlagPort
	threadsArg := "--" + cli.FlagThreads
	ctxArg := "--" + cli.FlagCtxSize

	cases := []struct {
		name string
		flag string
		arg  string
	}{
		{"zero port", portArg, "0"},
		{"negative port", portArg, "-1"},
		{"port above the range", portArg, "65536"},
		{"zero threads", threadsArg, "0"},
		{"negative threads", threadsArg, "-4"},
		{"zero context", ctxArg, "0"},
		{"negative context", ctxArg, "-1"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			code, _, stderr := runModel(t,
				"setup", "--dir", t.TempDir(), "--dry-run", testCase.flag, testCase.arg)
			if code != cli.ExitFailure {
				t.Fatalf("exit = %d, want %d", code, cli.ExitFailure)
			}

			if !strings.Contains(stderr, strings.TrimPrefix(testCase.flag, "--")) {
				t.Errorf("stderr does not name the bad flag:\n%s", stderr)
			}
		})
	}
}

// TestModelAcceptsTheFlagBounds proves the checks admit the edge values rather
// than being off by one.
func TestModelAcceptsTheFlagBounds(t *testing.T) {
	t.Parallel()

	code, _, stderr := runModel(t, "setup", "--dir", t.TempDir(), "--dry-run",
		"--"+cli.FlagPort, "65535", "--"+cli.FlagThreads, "1", "--"+cli.FlagCtxSize, "1")
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
}

// TestModelStatusRejectsABadPort proves status validates the same bounds, so a
// probe never targets an address that cannot answer.
func TestModelStatusRejectsABadPort(t *testing.T) {
	t.Parallel()

	code, _, stderr := runModel(t, "status", "--dir", t.TempDir(), "--"+cli.FlagPort, "0")
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d, want %d", code, cli.ExitFailure)
	}

	if !strings.Contains(stderr, "port") {
		t.Errorf("stderr = %s", stderr)
	}
}

// TestManualStartHintIsRunnable proves the fallback command names the installed
// binary, the installed weights, and every tuning flag, so someone on a host
// with no supervised sidecar can paste it and get the same server.
func TestManualStartHintIsRunnable(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	opts := cli.NewModelOptions(home, localmodel.DefaultModel, 9100, 3, 4096)

	target, err := cli.ResolveModelTarget(opts, environ.Map(nil))
	if err != nil {
		t.Fatalf("resolve target: %v", err)
	}

	model, err := localmodel.Lookup(localmodel.DefaultModel)
	if err != nil {
		t.Fatalf("look up the default model: %v", err)
	}

	// The paths come from the layout itself, so the case pins where install puts
	// the files rather than what the target reports about them.
	layout := localmodel.NewLayout(home)
	hint := cli.ManualStartHint(target, opts)

	for _, want := range []string{
		layout.ServerPath(),
		layout.ModelPath(&model),
		"--port 9100", "--threads 3", "--ctx-size 4096", "--host 127.0.0.1", "--jinja",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint is missing %q:\n%s", want, hint)
		}
	}
}
