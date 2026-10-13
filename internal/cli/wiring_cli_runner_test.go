//go:build unix

package cli_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/runner/builtins"
)

// No built-in owns these block names, so only their own [runner.<name>] block
// can give them a runner.
const (
	ompWrapperBlock   = "omp_wrapper"
	codexWrapperBlock = "codex_wrapper"
	unownedBlock      = "unowned_wrapper"
)

// These are the arguments the omp and codex templates put ahead of the prompt.
const (
	printFlagArg  = "-p"
	execSubcmdArg = "exec"
)

// cliRunnerPrompt carries a space so a launch that split it into two arguments
// prints two fields instead of one.
const cliRunnerPrompt = "say hello"

// cliRunnerWorkflow asks the runner the first verb names for the prompt the
// second verb supplies.
const cliRunnerWorkflow = `
[workflow]
name = "cli_runner"

[action.ask]
runner = %q
params = { prompt = %q }
`

// echoArgvScript stands in for a wrapper script around a CLI tool. It prints
// each argument it was launched with inside its own angle brackets, then the
// host table entry appendHostBlocks renders, so one output shows the argument
// boundaries and the environment the wrapper got.
const echoArgvScript = "#!/bin/sh\nprintf '<%s>' \"$@\"\nprintf '|%s' \"$CAIRNOS_HOST_BUILD_BOX_HARNESS_CLAUDE\"\n"

// TestRunRegistersAConfigDeclaredCLIRunner covers a [runner.<name>] block whose
// type names a built-in CLI runner: the block's binary runs under the block's
// own name with that built-in's argv template and the rendered host table. A
// block named after its own type keeps configuring the built-in instead of
// tripping duplicate registration, and so does a typeless block named after a
// built-in.
func TestRunRegistersAConfigDeclaredCLIRunner(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		blockName  string
		runnerType string
		wantArgv   []string
	}{
		{
			name: "omp wrapper", blockName: ompWrapperBlock, runnerType: builtins.OMPName,
			wantArgv: []string{printFlagArg, cliRunnerPrompt},
		},
		{
			name: "codex wrapper", blockName: codexWrapperBlock, runnerType: builtins.CodexName,
			wantArgv: []string{execSubcmdArg, cliRunnerPrompt},
		},
		{
			name: "block named after its type", blockName: builtins.ClaudeName, runnerType: builtins.ClaudeName,
			wantArgv: []string{printFlagArg, cliRunnerPrompt},
		},
		{
			name: "typeless block named after a built-in", blockName: builtins.CodexName,
			wantArgv: []string{execSubcmdArg, cliRunnerPrompt},
		},
	}

	for i := range cases {
		testCase := &cases[i]

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			dir := initDir(t)
			appendHostBlocks(t, dir)
			appendCLIRunnerBlock(t, dir, testCase.blockName, testCase.runnerType)
			path := writeWorkflow(t, dir, fmt.Sprintf(cliRunnerWorkflow, testCase.blockName, cliRunnerPrompt))

			code, _, stderr := runCLI(t, runCmd, path, flagDirArg, dir)
			if code != cli.ExitOK {
				t.Fatalf("run exited %d: %s", code, stderr)
			}

			want := printedArgv(testCase.wantArgv) + "|" + wantLaunchPath
			if got := onlyActionOutput(t, dir); got != want {
				t.Fatalf("output = %q, want %q", got, want)
			}
		})
	}
}

// TestRunRefusesACLIBlockNamedAfterAnotherBuiltin pins the collision path: a
// block cannot take over a different built-in's name by choosing a CLI type,
// so wiring fails loudly rather than shadowing the codex runner.
func TestRunRefusesACLIBlockNamedAfterAnotherBuiltin(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	appendCLIRunnerBlock(t, dir, builtins.CodexName, builtins.OMPName)
	path := writeWorkflow(t, dir, fmt.Sprintf(cliRunnerWorkflow, builtins.CodexName, cliRunnerPrompt))

	code, _, stderr := runCLI(t, runCmd, path, flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("run exited %d, want %d (stderr: %s)", code, cli.ExitFailure, stderr)
	}

	if want := fmt.Sprintf("register %s runner %q", builtins.OMPName, builtins.CodexName); !strings.Contains(stderr, want) {
		t.Fatalf("stderr = %q, want the registration error %q", stderr, want)
	}
}

// offeredRunnerTypes is the type list the startup error offers for a block no
// runner claims.
const offeredRunnerTypes = "anthropic, antigravity, claude, codex, mcp_client, omp, openai, plugin, qwen"

// TestRunRefusesABlockNoRunnerClaims covers a block under a name no built-in
// has whose type gives it no runner either. Nothing would ever read it, so
// startup stops, and the error alone has to tell the operator which keys fix
// the block.
func TestRunRefusesABlockNoRunnerClaims(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		block string
		want  string
	}{
		{
			name:  "typeless block with a binary",
			block: "binary = \"/opt/wrapper\"\n",
			want: "unknown runner: [runner." + unownedBlock + "] sets no type and no built-in runner has that name; " +
				"add type = \"<built-in>\" for the tool it wraps (type is one of " + offeredRunnerTypes + ")",
		},
		{
			name:  "typeless block without a binary",
			block: "stdin = true\n",
			want: "unknown runner: [runner." + unownedBlock + "] sets no type and no built-in runner has that name; " +
				"add type = \"<built-in>\" for the tool it wraps, and binary = \"<path>\" for the program to launch " +
				"(type is one of " + offeredRunnerTypes + ")",
		},
		{
			name:  "misspelled type",
			block: "binary = \"/opt/wrapper\"\ntype   = \"claud\"\n",
			want: "unknown runner: [runner." + unownedBlock + "] sets type = \"claud\", which is not a runner type, " +
				"and no built-in runner has that name; set type to one of " + offeredRunnerTypes,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			dir := initDir(t)
			appendConfig(t, dir, fmt.Sprintf("\n[runner.%s]\n%s", unownedBlock, testCase.block))
			path := writeWorkflow(t, dir, fmt.Sprintf(cliRunnerWorkflow, unownedBlock, cliRunnerPrompt))

			code, _, stderr := runCLI(t, runCmd, path, flagDirArg, dir)
			if code != cli.ExitFailure {
				t.Fatalf("run exited %d, want %d (stderr: %s)", code, cli.ExitFailure, stderr)
			}

			if !strings.Contains(stderr, testCase.want) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, testCase.want)
			}
		})
	}
}

// appendCLIRunnerBlock adds a [runner.<blockName>] whose binary is a fresh
// echoArgvScript, the shape someone writes to declare a wrapper script. An empty
// runnerType leaves the type key out, the way a block that only configures a
// built-in is written.
func appendCLIRunnerBlock(t *testing.T, dir, blockName, runnerType string) {
	t.Helper()

	wrapper := filepath.Join(t.TempDir(), "wrapper")
	writeExecutable(t, wrapper, echoArgvScript)

	block := fmt.Sprintf("\n[runner.%s]\nbinary = %q\n", blockName, wrapper)
	if runnerType != "" {
		block += fmt.Sprintf("type   = %q\n", runnerType)
	}

	appendConfig(t, dir, block)
}

// printedArgv is what echoArgvScript prints for args.
func printedArgv(args []string) string {
	return "<" + strings.Join(args, "><") + ">"
}
