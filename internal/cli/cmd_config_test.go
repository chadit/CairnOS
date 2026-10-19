package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
)

const (
	configCmd       = "config"
	allowActionCmd  = "allow-action"
	allowedName     = "report"
	restartNotice   = "restart cairnos to apply"
	configFileMode  = 0o640
	unparsableCase  = "unparsable file"
	noExecCase      = "no exec section"
	brokenConf      = "[runner.exec\nallow_actions = ["
	serverOnlyConf  = "[server]\nport = 8080\n"
	configUsageText = "usage: cairnos config"
	addUsageText    = "allow-action add <name>"
	oneLineExecConf = "# exec gate\n[runner.exec]\nallow_actions = [\"reconcile\"] # reviewed\ntimeout = 540\n"
)

// stageConfig writes config.toml into a fresh CairnOS home with a mode other
// than the atomic writer's default, so a case can see the mode survive.
func stageConfig(t *testing.T, body string) (string, string) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, cli.ConfigFileName)

	if err := os.WriteFile(path, []byte(body), configFileMode); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := os.Chmod(path, configFileMode); err != nil {
		t.Fatalf("chmod config: %v", err)
	}

	return dir, path
}

func readText(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(data)
}

func TestConfigAllowActionAdd(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		config     string
		action     string
		wantConfig string
		wantOut    string
		wantErr    string
		wantCode   int
	}{
		{
			name:       "one-line array",
			config:     oneLineExecConf,
			action:     allowedName,
			wantConfig: "# exec gate\n[runner.exec]\nallow_actions = [\"reconcile\", \"report\"] # reviewed\ntimeout = 540\n",
			wantOut:    "added report to [runner.exec] allow_actions",
			wantErr:    restartNotice,
			wantCode:   cli.ExitOK,
		},
		{
			name:       "multi-line array",
			config:     "[runner.exec]\nallow_actions = [\n  \"reconcile\",\n]\ntimeout = 540\n",
			action:     allowedName,
			wantConfig: "[runner.exec]\nallow_actions = [\n  \"reconcile\",\n  \"report\",\n]\ntimeout = 540\n",
			wantOut:    "added report",
			wantErr:    restartNotice,
			wantCode:   cli.ExitOK,
		},
		{
			name:       "already listed",
			config:     oneLineExecConf,
			action:     "reconcile",
			wantConfig: oneLineExecConf,
			wantOut:    "reconcile is already in [runner.exec] allow_actions",
			wantCode:   cli.ExitOK,
		},
		{
			name:       "name outside the action grammar",
			config:     oneLineExecConf,
			action:     "Bad-Name",
			wantConfig: oneLineExecConf,
			wantErr:    `"Bad-Name" is not an action name`,
			wantCode:   cli.ExitUsage,
		},
		{
			name:       unparsableCase,
			config:     brokenConf,
			action:     allowedName,
			wantConfig: brokenConf,
			wantErr:    "left unchanged: invalid configuration",
			wantCode:   cli.ExitFailure,
		},
		{
			name:       noExecCase,
			config:     serverOnlyConf,
			action:     allowedName,
			wantConfig: serverOnlyConf,
			wantErr:    "no [runner.exec] table: add a [runner.exec] block with a timeout first",
			wantCode:   cli.ExitFailure,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			dir, path := stageConfig(t, testCase.config)

			code, stdout, stderr := runCLI(t, configCmd, allowActionCmd, "add", testCase.action, flagDirArg, dir)
			if code != testCase.wantCode {
				t.Fatalf("exit = %d, want %d (stderr %q)", code, testCase.wantCode, stderr)
			}

			if got := readText(t, path); got != testCase.wantConfig {
				t.Fatalf("config:\n%s\nwant:\n%s", got, testCase.wantConfig)
			}

			if !strings.Contains(stdout, testCase.wantOut) {
				t.Fatalf("stdout = %q, want it to contain %q", stdout, testCase.wantOut)
			}

			if !strings.Contains(stderr, testCase.wantErr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, testCase.wantErr)
			}

			if testCase.wantErr == "" && stderr != "" {
				t.Fatalf("stderr = %q, want nothing when no change was made", stderr)
			}
		})
	}
}

func TestConfigAllowActionAddKeepsModeAndSymlink(t *testing.T) {
	t.Parallel()

	targetDir, target := stageConfig(t, oneLineExecConf)

	home := t.TempDir()
	link := filepath.Join(home, cli.ConfigFileName)

	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	code, _, stderr := runCLI(t, configCmd, allowActionCmd, "add", allowedName, flagDirArg, home)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}

	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config link was replaced (err %v)", err)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}

	if info.Mode().Perm() != configFileMode {
		t.Fatalf("mode = %o, want %o", info.Mode().Perm(), configFileMode)
	}

	if !strings.Contains(readText(t, target), `"reconcile", "report"`) {
		t.Fatalf("target in %s was not edited", targetDir)
	}
}

func TestConfigAllowActionList(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		config   string
		wantOut  string
		wantErr  string
		wantCode int
	}{
		{name: "listed names", config: "[runner.exec]\nallow_actions = [\"b\", \"a\"]\ntimeout = 5\n", wantOut: "b\na\n", wantCode: cli.ExitOK},
		{name: noExecCase, config: serverOnlyConf, wantErr: "exec refuses every action", wantCode: cli.ExitOK},
		{name: unparsableCase, config: brokenConf, wantErr: "invalid configuration", wantCode: cli.ExitFailure},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			dir, _ := stageConfig(t, testCase.config)

			code, stdout, stderr := runCLI(t, configCmd, allowActionCmd, listArg, flagDirArg, dir)
			if code != testCase.wantCode {
				t.Fatalf("exit = %d, want %d (stderr %q)", code, testCase.wantCode, stderr)
			}

			if stdout != testCase.wantOut {
				t.Fatalf("stdout = %q, want %q", stdout, testCase.wantOut)
			}

			if !strings.Contains(stderr, testCase.wantErr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, testCase.wantErr)
			}
		})
	}
}

func TestConfigUsageErrors(t *testing.T) {
	t.Parallel()

	missingHome := filepath.Join(t.TempDir(), "absent")

	cases := []struct {
		name     string
		wantErr  string
		args     []string
		wantCode int
	}{
		{name: "no family", args: []string{configCmd}, wantErr: configUsageText, wantCode: cli.ExitUsage},
		{name: "unknown family", args: []string{configCmd, "server"}, wantErr: configUsageText, wantCode: cli.ExitUsage},
		{name: "no verb", args: []string{configCmd, allowActionCmd}, wantErr: addUsageText, wantCode: cli.ExitUsage},
		{name: "unknown verb", args: []string{configCmd, allowActionCmd, "remove"}, wantErr: `unknown config allow-action subcommand "remove"`, wantCode: cli.ExitUsage},
		{name: "add without a name", args: []string{configCmd, allowActionCmd, "add"}, wantErr: addUsageText, wantCode: cli.ExitUsage},
		{name: "list with a stray argument", args: []string{configCmd, allowActionCmd, listArg, "extra"}, wantErr: "allow-action list", wantCode: cli.ExitUsage},
		{name: "missing config file", args: []string{configCmd, allowActionCmd, listArg, flagDirArg, missingHome}, wantErr: "find config", wantCode: cli.ExitFailure},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			code, _, stderr := runCLI(t, testCase.args...)
			if code != testCase.wantCode {
				t.Fatalf("exit = %d, want %d (stderr %q)", code, testCase.wantCode, stderr)
			}

			if !strings.Contains(stderr, testCase.wantErr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, testCase.wantErr)
			}
		})
	}
}
