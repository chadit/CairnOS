package builtins_test

import (
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// harnessVar is one rendered entry of the host table, spelled the way
// hostenv.Render spells it, and inheritedVar stands in for something already in
// serve's own environment.
const (
	harnessVar    = "CAIRNOS_HOST_BUILD_BOX_HARNESS_CLAUDE"
	harnessPath   = "~/.local/bin/claude-agent"
	inheritedVar  = "CAIRNOS_TEST_INHERITED"
	inheritedText = "from-serve"
)

// hostTable is what wiring hands a factory: the rendered slice, already in
// KEY=VALUE form.
func hostTable() []string {
	return []string{harnessVar + "=" + harnessPath}
}

// serveEnv stands in for the environment serve itself runs under. Passing it as
// a slice is what replaces t.Setenv here: the child sees the variable and no
// other case in the process does.
func serveEnv() []string {
	return []string{inheritedVar + "=" + inheritedText}
}

// echoShim writes a script that prints one variable, so a case reads the child's
// own view of its environment rather than the parent's.
func echoShim(t *testing.T, name string) string {
	t.Helper()

	return writeShim(t, t.TempDir(), name, `printf '%s' "$`+harnessVar+`"`)
}

// TestExecPassesHostTableToChild covers FR6 for the exec runner: a script the
// engine launches can read another machine's tool paths without being told them
// in its own argv.
func TestExecPassesHostTableToChild(t *testing.T) {
	shim := echoShim(t, "printhost")

	t.Parallel()

	built, err := builtins.ExecFactoryWithHostEnv(hostTable(), nil)(execConfig(nil))
	if err != nil {
		t.Fatalf("build exec with a host table: %v", err)
	}

	result, err := built.Execute(t.Context(), execTask(map[string]any{cmdKey: shim}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if result.Output != harnessPath {
		t.Fatalf("output = %#v, want the rendered launch path %q", result.Output, harnessPath)
	}
}

// TestExecWithoutHostTableLeavesTheChildAlone proves the no-hosts case changes
// nothing: the variable the shim reads is simply absent.
func TestExecWithoutHostTableLeavesTheChildAlone(t *testing.T) {
	shim := echoShim(t, "printhost")

	t.Parallel()

	built, err := builtins.ExecFactoryWithHostEnv(nil, nil)(execConfig(nil))
	if err != nil {
		t.Fatalf("build exec without a host table: %v", err)
	}

	result, err := built.Execute(t.Context(), execTask(map[string]any{cmdKey: shim}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if result.Output != "" {
		t.Fatalf("output = %#v, want nothing when no host is configured", result.Output)
	}
}

// TestExecRejectsAReservedEnvKey covers FR4. Two writers for one variable would
// make the winner depend on append order, so the config table is refused the
// prefix outright rather than being quietly overridden.
func TestExecRejectsAReservedEnvKey(t *testing.T) {
	t.Parallel()

	cfg := execConfig(map[string]any{
		envKey: map[string]any{harnessVar: "/somewhere/else/claude"},
	})

	_, err := builtins.NewExec(cfg)
	if err == nil {
		t.Fatal("NewExec accepted an env key reserved for the host table")
	}

	message := err.Error()
	if !strings.Contains(message, harnessVar) || !strings.Contains(message, "reserved") {
		t.Fatalf("error %q does not name the reserved key", message)
	}
}

// TestCLIPassesHostTableToChild covers FR6 for the wrapped CLI runners, which
// set no environment at all before this.
func TestCLIPassesHostTableToChild(t *testing.T) {
	shim := echoShim(t, "printhost")

	t.Parallel()

	built, err := builtins.CLIFactory(builtins.CodexName, builtins.CLITool{Binary: shim}, hostTable(), nil)(nil)
	if err != nil {
		t.Fatalf("build cli with a host table: %v", err)
	}

	result, err := built.Execute(t.Context(), cliTask())
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	output, isString := result.Output.(string)
	if !isString || output != harnessPath {
		t.Fatalf("output = %#v, want the rendered launch path %q", result.Output, harnessPath)
	}
}

// TestCLIWithoutHostTableStillInheritsServeEnvironment pins the half that must
// not regress: these runners exist to drive tools whose logins live in serve's
// own environment, so a nil table has to leave that environment untouched.
func TestCLIWithoutHostTableStillInheritsServeEnvironment(t *testing.T) {
	shim := writeShim(t, t.TempDir(), "printinherited", `printf '%s' "$`+inheritedVar+`"`)

	t.Parallel()

	built, err := builtins.CLIFactory(builtins.CodexName, builtins.CLITool{Binary: shim}, nil, serveEnv())(nil)
	if err != nil {
		t.Fatalf("build cli without a host table: %v", err)
	}

	result, err := built.Execute(t.Context(), cliTask())
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	output, isString := result.Output.(string)
	if !isString || output != inheritedText {
		t.Fatalf("output = %#v, want the inherited value %q", result.Output, inheritedText)
	}
}

// TestCLIWithHostTableKeepsInheritedEnvironment proves the table is added to
// serve's environment rather than replacing it, which is the difference between
// a wrapped tool that can still find its login and one that cannot.
func TestCLIWithHostTableKeepsInheritedEnvironment(t *testing.T) {
	shim := writeShim(t, t.TempDir(), "printboth",
		`printf '%s|%s' "$`+inheritedVar+`" "$`+harnessVar+`"`)

	t.Parallel()

	built, err := builtins.CLIFactory(builtins.CodexName, builtins.CLITool{Binary: shim}, hostTable(), serveEnv())(nil)
	if err != nil {
		t.Fatalf("build cli with a host table: %v", err)
	}

	result, err := built.Execute(t.Context(), cliTask())
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	want := inheritedText + "|" + harnessPath
	if output, isString := result.Output.(string); !isString || output != want {
		t.Fatalf("output = %#v, want %q", result.Output, want)
	}
}

// TestSandboxArgvCarriesNoHostTable covers the FR6 exclusion. sandbox_exec hands
// the container no environment by design, and the host table does not get to be
// the exception: nothing the runner builds passes a variable across that
// boundary.
func TestSandboxArgvCarriesNoHostTable(t *testing.T) {
	dir, argvFile := stageRecordingStub(t)

	t.Parallel()

	argv := executeRecording(t, dir, sandboxConfig(nil), sandboxTask(), argvFile)

	for index, arg := range argv {
		if arg == "-e" || arg == "--env" {
			t.Fatalf("argv passes an environment flag at %d: %v", index, argv)
		}

		if strings.Contains(arg, "CAIRNOS_HOST") {
			t.Fatalf("argv carries a host variable at %d: %v", index, argv)
		}
	}
}

// assertRunnerName keeps the factory cases honest about which runner they built,
// since every one of them goes through the registry contract rather than a
// concrete constructor.
func assertRunnerName(t *testing.T, built runner.Runner, want string) {
	t.Helper()

	if got := built.Name(); got != want {
		t.Fatalf("runner name = %q, want %q", got, want)
	}
}

// TestHostEnvFactoriesBuildTheRightRunners is the one-line guard that the two
// new factories still register what their names say.
func TestHostEnvFactoriesBuildTheRightRunners(t *testing.T) {
	t.Parallel()

	execRunner, err := builtins.ExecFactoryWithHostEnv(hostTable(), nil)(execConfig(nil))
	if err != nil {
		t.Fatalf("build exec: %v", err)
	}

	assertRunnerName(t, execRunner, builtins.ExecName)

	cliRunner, err := builtins.CLIFactory(builtins.CodexName, builtins.CLITool{Binary: binTrue}, hostTable(), nil)(nil)
	if err != nil {
		t.Fatalf("build cli: %v", err)
	}

	assertRunnerName(t, cliRunner, builtins.CodexName)
}
