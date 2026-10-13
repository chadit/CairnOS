package builtins_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/shellquote"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Config keys and values of the report-file source.
const (
	usageScriptKey     = "script"
	usageOutputKey     = "output"
	usageIntervalKey   = "interval"
	usageReportHost    = "host"
	reportInterval     = int64(900)
	reportFileName     = "usage.json"
	reportScriptName   = "usage-report"
	remoteReportScript = "/usr/local/bin/usage-report"
	altLauncher        = "claude-alt"
	quietBox           = "quiet_box"
	codeUsageNoReport  = "usage.unavailable"
	claudeTool         = "claude"
)

// usageReportJSON is a cairnos.usage.v1 report: a codex account with one
// banked reset still to expire and one long gone, a claude account, and one
// the script could not read. The verb fills written_at and every read_at. The
// codex 5h window's remaining_percent is the one field the format leaves out,
// so a reader that refused unknown fields fails here.
const usageReportJSON = `{"schema": "cairnos.usage.v1", "written_at": %[1]d,
 "accounts": [
  {"tool": "codex", "wrapper": "codex-agent", "read_at": %[1]d.5, "plan": "prolite", "error": null,
   "windows": [
    {"name": "5h", "used_percent": 12.5, "remaining_percent": 87.5, "resets_at": 1790010000},
    {"name": "weekly", "used_percent": 40, "resets_at": 1790400000}],
   "reset_credit_expiries": [4102444800, 1700000000], "score": 14.3},
  {"tool": "claude", "wrapper": "claude-agent", "read_at": %[1]d, "plan": "max", "error": null,
   "windows": [
    {"name": "5h", "used_percent": 3, "resets_at": 1790005000}],
   "reset_credit_expiries": [], "score": 9.5},
  {"tool": "claude", "wrapper": "claude-alt", "read_at": %[1]d, "plan": null,
   "error": "HTTP 401: token expired", "windows": [], "reset_credit_expiries": [], "score": null}]}`

// recordRunScript is a report script that only notes that it ran, so a case can
// tell whether a request started it.
const recordRunScript = `printf 'run\n' >> "$0.runs"`

// writeUsageReport writes the fixture report into dir, stamped writtenAt.
func writeUsageReport(t *testing.T, dir string, writtenAt time.Time) string {
	t.Helper()

	path := filepath.Join(dir, reportFileName)

	if err := os.WriteFile(path, fmt.Appendf(nil, usageReportJSON, writtenAt.Unix()), 0o600); err != nil {
		t.Fatalf("write usage report: %v", err)
	}

	return path
}

// reportConfig is a report-file block over script, reading output.
func reportConfig(script, output string) map[string]any {
	return map[string]any{
		usageScriptKey:   []any{script, "--out", "{output}"},
		usageOutputKey:   output,
		usageIntervalKey: reportInterval,
	}
}

// assertRunnerError checks the class and code of a classified failure and
// returns its message for the cases that also check the wording.
func assertRunnerError(t *testing.T, err error, class runner.ErrorClass, code string) string {
	t.Helper()

	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified {
		t.Fatalf("error %v is not a runner.Error", err)
	}

	if classified.Class != class || classified.Code != code {
		t.Fatalf("class/code = %v/%q, want %v/%q (%s)", classified.Class, classified.Code, class, code, classified.Message)
	}

	return classified.Message
}

func TestUsageReadsAReportFile(t *testing.T) {
	dir := t.TempDir()
	script := writeShim(t, dir, reportScriptName, recordRunScript)
	writtenAt := time.Now().Truncate(time.Second)
	output := writeUsageReport(t, dir, writtenAt)

	t.Parallel()

	usage := newUsage(t, reportConfig(script, output), builtins.UsageOptions{Lookup: pathEnv(dir)})

	result, err := usage.Execute(t.Context(), usageTask(nil))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	got := report(t, result)

	if !got.WrittenAt.Equal(writtenAt) || got.Notice != "" || !got.HasErrors {
		t.Fatalf("written_at/notice/has_errors = %v/%q/%v, want %v, no notice, and the failed account flagged",
			got.WrittenAt, got.Notice, got.HasErrors, writtenAt)
	}

	codex := findLogin(t, got, codexLauncher)
	wantWindows := []builtins.UsageWindow{
		{Name: "5h", UsedPercent: 12.5, ResetsAt: time.Unix(1790010000, 0)},
		{Name: "weekly", UsedPercent: 40, ResetsAt: time.Unix(1790400000, 0)},
	}

	if codex.Protocol != codexConfigKey || codex.Plan != "prolite" || !windowsEqual(codex.Windows, wantWindows) {
		t.Fatalf("codex login = %+v, want the codex protocol, prolite, and both windows", codex)
	}

	if codex.Score == nil || *codex.Score != 14.3 {
		t.Fatalf("codex score = %v, want 14.3", codex.Score)
	}

	// The 2023 expiry had passed long before this read, so only the 2100 one counts.
	if len(codex.ResetCredits) != 1 || !codex.ResetCredits[0].Equal(time.Unix(4102444800, 0)) {
		t.Fatalf("codex reset credits = %v, want only the unexpired one", codex.ResetCredits)
	}

	if claude := findLogin(t, got, claudeLauncher); claude.Protocol != claudeConfigKey || claude.Plan != "max" {
		t.Fatalf("claude login = %+v, want the claude protocol on a max plan", claude)
	}

	if failed := findLogin(t, got, altLauncher); failed.Error != "HTTP 401: token expired" || len(failed.Windows) != 0 {
		t.Fatalf("failed login = %+v, want its error and no windows", failed)
	}

	for _, want := range []string{"SCORE", "CREDITS", "| 14.3 ", "| 9.5 "} {
		if !strings.Contains(got.Summary, want) {
			t.Fatalf("summary =\n%s\nwant it to contain %q", got.Summary, want)
		}
	}
}

func TestUsageRequestReadsTheFileWithoutRunningTheScript(t *testing.T) {
	dir := t.TempDir()
	script := writeShim(t, dir, reportScriptName, recordRunScript)
	output := writeUsageReport(t, dir, time.Now())

	t.Parallel()

	usage := newUsage(t, reportConfig(script, output), builtins.UsageOptions{Lookup: pathEnv(dir)})

	got, err := usage.Report(t.Context(), usageTask(nil))
	if err != nil {
		t.Fatalf("report: %v", err)
	}

	if len(got.Logins) != 3 {
		t.Fatalf("logins = %+v, want the three accounts in the file", got.Logins)
	}

	if _, err := os.Stat(script + ".runs"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run record stat = %v, want no record: a request must not run the script", err)
	}

	if gap, logins := usage.ClaudeGap(usageTask(nil)); logins != 0 {
		t.Fatalf("claude gap = %v over %d logins, want no wait when nothing is probed", gap, logins)
	}
}

func TestUsageFlagsAReadingOlderThanTwoIntervalsAndATimeout(t *testing.T) {
	// The block sets no timeout, so the script's five-minute default applies and
	// the bound is two 900-second intervals plus 300 seconds.
	bound := 2*time.Duration(reportInterval)*time.Second + 5*time.Minute

	cases := []struct {
		name      string
		age       time.Duration
		wantStale bool
	}{
		{name: "a minute inside the bound is fresh", age: bound - time.Minute},
		{name: "a minute past the bound is stale", age: bound + time.Minute, wantStale: true},
	}

	t.Parallel()

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			output := writeUsageReport(t, dir, time.Now().Add(-tt.age))
			usage := newUsage(t, reportConfig("/bin/true", output), builtins.UsageOptions{Lookup: pathEnv(dir)})

			got, err := usage.Report(t.Context(), usageTask(nil))
			if err != nil {
				t.Fatalf("report: %v", err)
			}

			if tt.wantStale != strings.HasPrefix(got.Notice, "stale:") {
				t.Fatalf("notice = %q, want stale %v", got.Notice, tt.wantStale)
			}

			// The notice leads the summary, so a chat reply that copies the
			// table copies the warning with it.
			if tt.wantStale && !strings.HasPrefix(got.Summary, got.Notice+"\n```") {
				t.Fatalf("summary =\n%s\nwant it to open with the notice", got.Summary)
			}
		})
	}
}

// mergedReportJSON is a file a codex-only run just rewrote: written_at and the
// codex reading fill the first verb, and the claude entry it kept fills the
// second.
const mergedReportJSON = `{"schema": "cairnos.usage.v1", "written_at": %[1]d, "accounts": [
 {"tool": "codex", "wrapper": "codex-agent", "read_at": %[1]d, "plan": "prolite",
  "error": null, "windows": [], "reset_credit_expiries": [], "score": 12.0},
 {"tool": "claude", "wrapper": "claude-agent", "read_at": %[2]d, "plan": "max",
  "error": null, "windows": [], "reset_credit_expiries": [], "score": 9.5}]}`

func TestUsageFlagsOnlyTheAccountsReadTooLongAgo(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	output := filepath.Join(dir, reportFileName)
	now := time.Now()

	if err := os.WriteFile(output, fmt.Appendf(nil, mergedReportJSON, now.Unix(), now.Add(-3*time.Hour).Unix()), 0o600); err != nil {
		t.Fatalf("write report: %v", err)
	}

	usage := newUsage(t, reportConfig("/bin/true", output), builtins.UsageOptions{Lookup: pathEnv(dir)})

	got, err := usage.Report(t.Context(), usageTask(nil))
	if err != nil {
		t.Fatalf("report: %v", err)
	}

	// A fresh written_at must not hide the claude reading the script kept.
	if !strings.HasPrefix(got.Notice, "stale: "+claudeLauncher+" last read 3h0m0s ago") ||
		strings.Contains(got.Notice, codexLauncher) {
		t.Fatalf("notice = %q, want only %s flagged", got.Notice, claudeLauncher)
	}
}

func TestUsageReportsAMissingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	output := filepath.Join(dir, reportFileName)
	usage := newUsage(t, reportConfig("/bin/true", output), builtins.UsageOptions{Lookup: pathEnv(dir)})

	_, err := usage.Execute(t.Context(), usageTask(nil))

	if message := assertRunnerError(t, err, runner.ClassTransient, codeUsageNoReport); !strings.Contains(message, output) {
		t.Fatalf("message = %q, want it to name %s", message, output)
	}
}

func TestUsageRefusesAReportItCannotRead(t *testing.T) {
	emptyWrapper := fmt.Sprintf("accounts[1] (tool %q) has an empty wrapper", claudeTool)
	emptyTool := fmt.Sprintf("accounts[1] (wrapper %q) has an empty tool", claudeLauncher)

	cases := []struct {
		name     string
		body     string
		wantText string
	}{
		{
			name:     "unknown schema",
			body:     `{"schema": "cairnos.usage.v2", "written_at": 1790000000, "accounts": []}`,
			wantText: `"cairnos.usage.v2"`,
		},
		{name: "missing schema", body: `{"written_at": 1790000000, "accounts": []}`, wantText: `schema ""`},
		{name: "not json", body: `usage: usage-report [-h]`, wantText: "invalid character"},
		{
			name:     "account without a wrapper",
			body:     reportWithSecondAccount(fmt.Sprintf(`{"tool": %q, "windows": []}`, claudeTool)),
			wantText: emptyWrapper,
		},
		{
			name:     "account with a blank wrapper",
			body:     reportWithSecondAccount(fmt.Sprintf(`{"tool": %q, "wrapper": "  ", "windows": []}`, claudeTool)),
			wantText: emptyWrapper,
		},
		{
			name:     "account without a tool",
			body:     reportWithSecondAccount(fmt.Sprintf(`{"wrapper": %q, "windows": []}`, claudeLauncher)),
			wantText: emptyTool,
		},
		{
			name:     "account with a blank tool",
			body:     reportWithSecondAccount(fmt.Sprintf(`{"tool": " ", "wrapper": %q, "windows": []}`, claudeLauncher)),
			wantText: emptyTool,
		},
	}

	t.Parallel()

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			output := filepath.Join(dir, reportFileName)

			if err := os.WriteFile(output, []byte(tt.body), 0o600); err != nil {
				t.Fatalf("write report: %v", err)
			}

			usage := newUsage(t, reportConfig("/bin/true", output), builtins.UsageOptions{Lookup: pathEnv(dir)})

			_, err := usage.Execute(t.Context(), usageTask(nil))

			if message := assertRunnerError(t, err, runner.ClassPermanent, "usage.bad_report"); !strings.Contains(message, tt.wantText) {
				t.Fatalf("message = %q, want it to contain %q", message, tt.wantText)
			}
		})
	}
}

func TestUsageReportSelectsAnAccountWithPaddedNames(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	output := filepath.Join(dir, reportFileName)
	padded := fmt.Sprintf(`{"tool": " %s ", "wrapper": "  %s ", "windows": []}`, claudeTool, claudeLauncher)

	if err := os.WriteFile(output, []byte(reportWithSecondAccount(padded)), 0o600); err != nil {
		t.Fatalf("write report: %v", err)
	}

	usage := newUsage(t, reportConfig("/bin/true", output), builtins.UsageOptions{Lookup: pathEnv(dir)})

	got, err := usage.Report(t.Context(), usageTask(map[string]any{usageLaunchers: []any{claudeLauncher}}))
	if err != nil {
		t.Fatalf("report: %v", err)
	}

	if len(got.Logins) != 1 || got.Logins[0].Launcher != claudeLauncher || got.Logins[0].Protocol != claudeTool {
		t.Fatalf("logins = %+v, want only %s speaking %s", got.Logins, claudeLauncher, claudeTool)
	}
}

// reportWithSecondAccount is a report whose first account is valid, so
// whatever the reader does with second comes from second itself.
func reportWithSecondAccount(second string) string {
	return `{"schema": "cairnos.usage.v1", "written_at": 1790000000, "accounts": [
 {"tool": "codex", "wrapper": "codex-agent", "windows": []}, ` + second + `]}`
}

func TestUsageReportNarrowsToTheNamedLaunchers(t *testing.T) {
	dir := t.TempDir()
	output := writeUsageReport(t, dir, time.Now())

	t.Parallel()

	usage := newUsage(t, reportConfig("/bin/true", output), builtins.UsageOptions{Lookup: pathEnv(dir)})

	got, err := usage.Report(t.Context(), usageTask(map[string]any{usageLaunchers: []any{codexLauncher}}))
	if err != nil {
		t.Fatalf("report: %v", err)
	}

	if len(got.Logins) != 1 || got.Logins[0].Launcher != codexLauncher || got.HasErrors {
		t.Fatalf("logins = %+v, want only %s", got.Logins, codexLauncher)
	}

	_, err = usage.Report(t.Context(), usageTask(map[string]any{usageLaunchers: []any{"codex-typo"}}))
	assertRunnerError(t, err, runner.ClassPermanent, "usage.unknown_launcher")

	// The file lives on this machine, so a request cannot ask for another one.
	_, err = usage.Report(t.Context(), usageTask(map[string]any{usageHostKey: buildBox}))
	assertRunnerError(t, err, runner.ClassPermanent, "usage.unknown_host")
}

func TestUsageConfigKeepsTheTwoSourcesApart(t *testing.T) {
	t.Parallel()

	hosts := map[string]builtins.UsageHost{buildBox: {SSH: buildBoxAlias}, quietBox: {}}
	withScript := func(extra map[string]any) map[string]any {
		cfg := reportConfig("/bin/true", "/tmp/usage.json")
		maps.Copy(cfg, extra)

		return cfg
	}

	// Each case is a valid block with one change, or an empty block with one key,
	// and both of those build, so a refusal comes from the case's own change.
	cases := []struct {
		cfg  map[string]any
		name string
	}{
		{name: "script beside a launcher list", cfg: withScript(map[string]any{claudeConfigKey: []any{claudeLauncher}})},
		{name: "script beside claude_gap", cfg: withScript(map[string]any{"claude_gap": int64(0)})},
		{name: "output without script", cfg: map[string]any{usageOutputKey: "/tmp/usage.json"}},
		{name: "host without script", cfg: map[string]any{usageReportHost: buildBox}},
		{name: "script without output", cfg: map[string]any{usageScriptKey: []any{"/bin/true"}, usageIntervalKey: reportInterval}},
		{name: "script without interval", cfg: map[string]any{usageScriptKey: []any{"/bin/true"}, usageOutputKey: "/tmp/usage.json"}},
		{name: "zero interval", cfg: withScript(map[string]any{usageIntervalKey: int64(0)})},
		{name: "empty script", cfg: withScript(map[string]any{usageScriptKey: []any{}})},
		{name: "relative output", cfg: withScript(map[string]any{usageOutputKey: "usage.json"})},
		{name: "unknown host", cfg: withScript(map[string]any{usageReportHost: "other_box"})},
		{name: "host without an alias", cfg: withScript(map[string]any{usageReportHost: quietBox})},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := builtins.NewUsage(tt.cfg, builtins.UsageOptions{Hosts: hosts}); !errors.Is(err, builtins.ErrConfigValue) {
				t.Fatalf("err = %v, want a config error", err)
			}
		})
	}

	if interval := newUsage(t, withScript(nil), builtins.UsageOptions{}).RefreshInterval(); interval != 15*time.Minute {
		t.Fatalf("refresh interval = %v, want 15m", interval)
	}

	if interval := newUsage(t, map[string]any{}, builtins.UsageOptions{}).RefreshInterval(); interval != 0 {
		t.Fatalf("launcher block refresh interval = %v, want none", interval)
	}
}

// writingScript stands in for a report script: it records the run, then writes
// the fixture report to the path after --out through a rename, so a run the test
// cuts short never leaves half a file behind.
func writingScript(writtenAt time.Time) string {
	return recordRunScript + "\nprintf '%s' " + shellquote.Word(fmt.Sprintf(usageReportJSON, writtenAt.Unix())) +
		` > "$2.tmp" && mv "$2.tmp" "$2"`
}

func TestUsageRefreshRunsTheScriptOnEveryTick(t *testing.T) {
	dir := t.TempDir()
	script := writeShim(t, dir, reportScriptName, writingScript(time.Now()))

	t.Parallel()

	output := filepath.Join(dir, reportFileName)
	usage := newUsage(t, reportConfig(script, output), builtins.UsageOptions{Lookup: pathEnv(dir)})

	ctx, cancel := context.WithCancel(t.Context())
	ticks := make(chan time.Time)
	done := make(chan struct{})

	go func() {
		defer close(done)

		usage.RunRefresh(ctx, ticks, slog.New(slog.DiscardHandler))
	}()

	// The loop runs once on start and receives a tick only after a run ends, so
	// two sends mean at least two finished runs.
	ticks <- time.Now()

	ticks <- time.Now()

	cancel()
	<-done

	if runs := readLines(t, script+".runs"); len(runs) < 2 {
		t.Fatalf("script ran %d times, want at least 2", len(runs))
	}

	got, err := usage.Report(t.Context(), usageTask(nil))
	if err != nil {
		t.Fatalf("report after the refresh: %v", err)
	}

	if len(got.Logins) != 3 || got.Notice != "" {
		t.Fatalf("report = %+v, want the three accounts the script wrote, fresh", got)
	}
}

func TestUsageReportOverSSHRunsAndReadsOnTheScriptHost(t *testing.T) {
	dir := t.TempDir()
	// The recorder prints the report for every call, which is what the cat a
	// request sends would print; the refresh ignores it.
	shim := writeShim(t, dir, "ssh-report", `for arg in "$@"; do printf '%s\n' "$arg" >> "$0.argv"; done
printf '%s' `+shellquote.Word(fmt.Sprintf(usageReportJSON, time.Now().Unix())))

	t.Parallel()

	cfg := map[string]any{
		usageScriptKey:   []any{remoteReportScript, "--out", "{output}"},
		usageOutputKey:   "~/.cache/" + reportFileName,
		usageIntervalKey: reportInterval,
		usageReportHost:  buildBox,
	}
	usage := newUsage(t, cfg, builtins.UsageOptions{
		Hosts:  map[string]builtins.UsageHost{buildBox: {SSH: buildBoxAlias, Wrapper: remoteWrapper}},
		Lookup: pathEnv(dir),
		SSH:    "ssh-report",
	})

	got, err := usage.Report(t.Context(), usageTask(nil))
	if err != nil {
		t.Fatalf("report: %v", err)
	}

	if got.Host != buildBox || len(got.Logins) != 3 {
		t.Fatalf("report host/logins = %q/%d, want %s and the three accounts", got.Host, len(got.Logins), buildBox)
	}

	ctx, cancel := context.WithCancel(t.Context())
	ticks := make(chan time.Time)
	done := make(chan struct{})

	go func() {
		defer close(done)

		usage.RunRefresh(ctx, ticks, slog.New(slog.DiscardHandler))
	}()

	ticks <- time.Now()

	cancel()
	<-done

	// Each call records eight lines: the ssh flags, the alias, the terminator,
	// and one remote command. The read came first, then the refresh.
	argv := readLines(t, shim+".argv")
	if len(argv) < 16 {
		t.Fatalf("ssh argv = %#v, want a read and a refresh", argv)
	}

	wantRead := `exec cat -- "$HOME"/'.cache/usage.json'`
	// The absolute script path is quoted whole while the output's "~/" becomes
	// "$HOME", so the remote shell expands only the part meant for it.
	wantRefresh := "exec '" + remoteWrapper + "' '" + remoteReportScript + `' '--out' "$HOME"/'.cache/usage.json'`

	if argv[5] != buildBoxAlias || argv[7] != wantRead || argv[15] != wantRefresh {
		t.Fatalf("ssh calls = %#v, want the read %q then the refresh %q on %s", argv, wantRead, wantRefresh, buildBoxAlias)
	}
}
