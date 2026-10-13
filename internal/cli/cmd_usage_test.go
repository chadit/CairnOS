package cli_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/runner/builtins"
)

// Launcher names the usage cases configure: one login per protocol.
const (
	usageCmd            = "usage"
	usageClaudeLauncher = "claude-agent"
	usageCodexLauncher  = "codex-agent"
	usageSecondClaude   = "claude-alt"
)

// usageClaudeShim drains the request lines, then answers the way Claude Code
// does: an init response, then a get_usage answer for a max plan with two windows.
const usageClaudeShim = `cat > /dev/null
echo '{"type":"system","subtype":"init","tools":["Bash"]}'
echo '{"type":"control_response","response":{"subtype":"success","request_id":"cairnos-init","response":{"commands":[]}}}'
echo '{"type":"control_response","response":{"subtype":"success","request_id":"cairnos-usage","response":{"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":2,"resets_at":"2026-09-22T18:50:00+00:00"},"seven_day":{"utilization":23,"resets_at":"2026-09-24T12:00:00+00:00"}}}}}'`

// usageCodexShim answers the Codex app-server handshake: the initialize reply
// after the first line, then the rate-limit reply once all three have arrived.
const usageCodexShim = `n=0
while IFS= read -r line; do
  n=$((n+1))
  if [ "$n" -eq 1 ]; then echo '{"jsonrpc":"2.0","id":1,"result":{"userAgent":"codex"}}'; fi
  if [ "$n" -eq 3 ]; then break; fi
done
echo '{"id":2,"result":{"rateLimits":{"primary":{"usedPercent":24,"windowDurationMins":10080,"resetsAt":1790416865},"secondary":null,"planType":"prolite"}},"jsonrpc":"2.0"}'`

// usageHome scaffolds a home with the given usage block and one host, returning it
// and the shim directory. Call it before t.Parallel: a fork racing a write fails the exec.
func usageHome(t *testing.T, usageBlock string) (string, string) {
	t.Helper()

	dir := initDir(t)
	shims := t.TempDir()

	for name, body := range map[string]string{
		usageClaudeLauncher: usageClaudeShim, usageSecondClaude: usageClaudeShim, usageCodexLauncher: usageCodexShim,
	} {
		// #nosec G306 -- the shim must be executable; it lives in t.TempDir.
		if err := os.WriteFile(filepath.Join(shims, name), []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
			t.Fatalf("write %s shim: %v", name, err)
		}
	}

	block := "\n" + usageBlock + "\n[host.build_box]\ntype = \"worker\"\nssh  = \"build-box\"\n"

	appendConfig(t, dir, block)

	return dir, shims
}

// bothLoginsBlock configures one login per protocol, so no Claude gap applies.
const bothLoginsBlock = "[runner.usage]\nclaude = [\"" + usageClaudeLauncher + "\"]\ncodex  = [\"" + usageCodexLauncher + "\"]\n"

func TestUsagePrintsOneRowPerWindow(t *testing.T) {
	dir, shims := usageHome(t, bothLoginsBlock)

	t.Parallel()

	code, stdout, stderr := runEnvCLI(t, pathEnv(shims), usageCmd, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("usage exited %d: %s", code, stderr)
	}

	want := "LOGIN         PLAN     WINDOW  USED  RESETS\n" +
		"claude-agent  max      5h      2%    2026-09-22 18:50 UTC\n" +
		"                       7d      23%   2026-09-24 12:00 UTC\n" +
		"codex-agent   prolite  7d      24%   2026-09-26 10:01 UTC\n"

	if stdout != want {
		t.Fatalf("stdout =\n%s\nwant\n%s", stdout, want)
	}

	if stderr != "" {
		t.Fatalf("stderr = %q, want nothing when a single Claude login pays no gap", stderr)
	}
}

func TestUsageJSONRoundTripsToTheReport(t *testing.T) {
	dir, shims := usageHome(t, bothLoginsBlock)

	t.Parallel()

	code, stdout, stderr := runEnvCLI(t, pathEnv(shims), usageCmd, "--json", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("usage --json exited %d: %s", code, stderr)
	}

	var report builtins.UsageReport

	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode %s: %v", stdout, err)
	}

	if len(report.Logins) != 2 || report.Logins[0].Plan != "max" || report.Logins[1].Plan != "prolite" {
		t.Fatalf("logins = %+v, want max then prolite", report.Logins)
	}

	if report.HasErrors || !strings.Contains(report.Summary, "| codex-agent ") {
		t.Fatalf("report = %+v, want a clean report whose summary names both logins", report)
	}
}

func TestUsageNarrowsToTheNamedLaunchers(t *testing.T) {
	dir, shims := usageHome(t, bothLoginsBlock)

	t.Parallel()

	code, stdout, stderr := runEnvCLI(t, pathEnv(shims), usageCmd, "--launchers", usageCodexLauncher, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("usage exited %d: %s", code, stderr)
	}

	if strings.Contains(stdout, usageClaudeLauncher) || !strings.Contains(stdout, usageCodexLauncher) {
		t.Fatalf("stdout =\n%s\nwant only %s", stdout, usageCodexLauncher)
	}
}

func TestUsageNotesTheClaudeGapBeforeProbing(t *testing.T) {
	block := "[runner.usage]\nclaude_gap = 1\nclaude = [\"" + usageClaudeLauncher + "\", \"" +
		usageSecondClaude + "\"]\ncodex = []\n"
	dir, shims := usageHome(t, block)

	t.Parallel()

	code, _, stderr := runEnvCLI(t, pathEnv(shims), usageCmd, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("usage exited %d: %s", code, stderr)
	}

	if want := "probing 2 Claude logins 1s apart, about 1m total\n"; stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
}

func TestUsageRefusesBadSelections(t *testing.T) {
	cases := []struct {
		name    string
		wantErr string
		args    []string
	}{
		{name: "unknown host", args: []string{"--host", "no_such_box"}, wantErr: "no [host.no_such_box] block is configured"},
		{name: "unconfigured launcher", args: []string{"--launchers", "claude-spare"}, wantErr: "launcher claude-spare is not listed"},
	}

	dir, shims := usageHome(t, bothLoginsBlock)

	t.Parallel()

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			args := append([]string{usageCmd, flagDirArg, dir}, testCase.args...)

			code, stdout, stderr := runEnvCLI(t, pathEnv(shims), args...)
			if code != cli.ExitFailure {
				t.Fatalf("usage exited %d, want %d; stdout %s", code, cli.ExitFailure, stdout)
			}

			if !strings.Contains(stderr, testCase.wantErr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, testCase.wantErr)
			}
		})
	}
}

func TestUsageFailsWhenNoLoginReports(t *testing.T) {
	dir, _ := usageHome(t, bothLoginsBlock)

	t.Parallel()

	code, stdout, stderr := runEnvCLI(t, pathEnv(t.TempDir()), usageCmd, flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("usage exited %d with no launchers on PATH, want %d; stdout %s", code, cli.ExitFailure, stdout)
	}

	if !strings.Contains(stderr, "no login reported usage") {
		t.Fatalf("stderr = %q, want the runner's reason", stderr)
	}
}

// usageReportFile is a one-account cairnos.usage.v1 report. The verb fills
// written_at and the account's read_at.
const usageReportFile = `{"schema": "cairnos.usage.v1", "written_at": %[1]d, "accounts": [
 {"tool": "codex", "wrapper": "codex-agent", "read_at": %[1]d, "plan": "prolite", "error": null,
  "windows": [{"name": "weekly", "used_percent": 24, "resets_at": 1790416865}],
  "reset_credit_expiries": [4102444800], "score": 14.3}]}`

func TestUsageReadsAStaleScriptReportWithoutRunningTheScript(t *testing.T) {
	files := t.TempDir()
	script := filepath.Join(files, "usage-report")
	output := filepath.Join(files, "usage.json")

	// #nosec G306 -- the script must be executable; it lives in t.TempDir.
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'run\\n' >> \"$0.runs\"\n"), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}

	// Three hours is well past two 900-second intervals plus the script's timeout.
	report := fmt.Appendf(nil, usageReportFile, time.Now().Add(-3*time.Hour).Unix())
	if err := os.WriteFile(output, report, 0o600); err != nil {
		t.Fatalf("write report: %v", err)
	}

	block := "[runner.usage]\nscript = [\"" + script + "\", \"--out\", \"{output}\"]\noutput = \"" + output + "\"\ninterval = 900\n"
	dir, shims := usageHome(t, block)

	t.Parallel()

	code, stdout, stderr := runEnvCLI(t, pathEnv(shims), usageCmd, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("usage exited %d: %s", code, stderr)
	}

	notice, table, _ := strings.Cut(stdout, "\n")
	if !strings.HasPrefix(notice, "stale: "+usageCodexLauncher+" last read 3h0m0s ago") {
		t.Fatalf("first line = %q, want the stale notice above the table", notice)
	}

	want := "LOGIN        PLAN     WINDOW  USED  RESETS                SCORE  CREDITS\n" +
		"codex-agent  prolite  weekly  24%   2026-09-26 10:01 UTC  14.3   1\n"

	if table != want {
		t.Fatalf("table =\n%s\nwant\n%s", table, want)
	}

	if _, err := os.Stat(script + ".runs"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run record stat = %v, want none: cairnos usage must read the file, not run the script", err)
	}
}
