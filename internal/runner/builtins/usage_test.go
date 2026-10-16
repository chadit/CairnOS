package builtins_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Fixture names the usage cases share. The launchers are the neutral wrapper
// names an operator would give two logins of the same CLI.
const (
	usageHostKey     = "host"
	usageLaunchers   = "launchers"
	claudeLauncher   = "claude-agent"
	codexLauncher    = "codex-agent"
	usageAction      = "windows"
	buildBox         = "build_box"
	buildBoxAlias    = "build-box"
	claudeConfigKey  = "claude"
	codexConfigKey   = "codex"
	usageTimeoutKey  = "timeout"
	remoteWrapper    = "/usr/local/bin/agent-env"
	usageClientToken = "9.9.9-test"

	// The window labels and plan names the decoders report, pinned here because
	// the package spells them in unexported constants.
	windowFiveHour = "5h"
	windowSevenDay = "7d"
	planMax        = "max"
	planProlite    = "prolite"

	// unknownBox is a host name no usage block declares, and sshOptionFlag the
	// ssh flag in front of each option the launcher passes.
	unknownBox    = "other_box"
	sshOptionFlag = "-o"
)

// claudeUsageLine is the get_usage answer the real CLI sends: a max plan with a
// spent five-hour window, a busier weekly one, and nulls for the windows the
// plan does not carry.
const claudeUsageLine = `{"type":"control_response","response":{"subtype":"success",` +
	`"request_id":"cairnos-usage","response":{"session":{"total_cost_usd":0},` +
	`"subscription_type":"max","rate_limits_available":true,"rate_limits":{` +
	`"five_hour":{"utilization":4,"resets_at":"2026-09-22T18:09:59.940354+00:00","limit_dollars":null},` +
	`"seven_day":{"utilization":41,"resets_at":"2026-09-26T06:59:59.940374+00:00"},` +
	`"seven_day_oauth_apps":null,"seven_day_opus":null,"seven_day_sonnet":null}}}}`

// codexUsageLine is the app-server's reply: one weekly window, no secondary.
const codexUsageLine = `{"id":2,"result":{"ordinaryUsageAllowed":true,"rateLimits":{"limitId":"codex",` +
	`"primary":{"usedPercent":24,"windowDurationMins":10080,"resetsAt":1790416865},` +
	`"secondary":null,"credits":{"balance":0},"planType":"prolite","rateLimitReachedType":null}},` +
	`"jsonrpc":"2.0"}`

// claudeAnswerScript drains stdin, then says what the CLI says: a system line,
// a fat initialize response, and the usage line.
const claudeAnswerScript = `cat > /dev/null
echo '{"type":"system","subtype":"init","tools":["Bash"]}'
echo '{"type":"control_response","response":{"subtype":"success","request_id":"cairnos-init","response":{"commands":[{"name":"compact"},{"name":"review"}]}}}'
echo '` + claudeUsageLine + `'`

// claudeScript is the answer plus an argv guard, so a dialog that stops asking
// for stream-json fails loudly instead of quietly reporting nothing.
const claudeScript = `case "$*" in
  *"--input-format stream-json"*) ;;
  *) echo "unexpected argv: $*" >&2; exit 64 ;;
esac
` + claudeAnswerScript

// codexScript mirrors the app-server: a notification first, then the reply once
// all three handshake lines have arrived. It records stdin beside itself,
// because the probe leaves the child's environment alone.
const codexScript = `echo '{"jsonrpc":"2.0","method":"configWarning","params":{"message":"stale key"}}'
n=0
while IFS= read -r line; do
  n=$((n+1))
  printf '%s\n' "$line" >> "$0.stdin"
  if [ "$n" -eq 1 ]; then echo '{"jsonrpc":"2.0","id":1,"result":{"userAgent":"codex"}}'; fi
  if [ "$n" -eq 3 ]; then break; fi
done
echo '{"jsonrpc":"2.0","method":"remoteControl/status/changed","params":{}}'
echo '` + codexUsageLine + `'`

// sshRecorderScript records the argv it was handed and then answers as the
// remote CLI would, which is what lets a case read back the command ssh carried.
const sshRecorderScript = `for arg in "$@"; do printf '%s\n' "$arg" >> "$0.argv"; done
` + claudeAnswerScript

// usageTask builds one action for this runner.
func usageTask(params map[string]any) *runner.Task {
	return &runner.Task{
		RunID:      testRunID,
		ActionName: usageAction,
		Params:     params,
		Resolved:   true,
	}
}

// pathEnv aims the runner's lookups at a directory of shims, so a case never
// depends on what is installed on the machine running it.
func pathEnv(dir string) environ.Lookup {
	return environ.Map(map[string]string{pathVar: dir, "HOME": dir})
}

// newUsage builds the runner or fails the case, since every assertion below
// depends on it existing.
func newUsage(t *testing.T, cfg map[string]any, opts builtins.UsageOptions) *builtins.Usage {
	t.Helper()

	built, err := builtins.NewUsage(cfg, opts)
	if err != nil {
		t.Fatalf("new usage: %v", err)
	}

	return built
}

// report reads the runner's output back as a report, failing when the runner
// handed back anything but the JSON-shaped map a workflow reference reads.
func report(t *testing.T, result runner.Result) *builtins.UsageReport {
	t.Helper()

	fields, isMap := result.Output.(map[string]any)
	if !isMap {
		t.Fatalf("output = %#v, want a map", result.Output)
	}

	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode output: %v", err)
	}

	var typed builtins.UsageReport

	if err := json.Unmarshal(encoded, &typed); err != nil {
		t.Fatalf("decode output %s: %v", encoded, err)
	}

	return &typed
}

// findLogin locates one launcher's entry, failing when the report skipped it.
func findLogin(t *testing.T, got *builtins.UsageReport, launcher string) builtins.UsageLogin {
	t.Helper()

	for index := range got.Logins {
		if got.Logins[index].Launcher == launcher {
			return got.Logins[index]
		}
	}

	t.Fatalf("report %v has no entry for %s", got, launcher)

	return builtins.UsageLogin{}
}

func TestUsageReadsBothProtocolsLocally(t *testing.T) {
	dir := t.TempDir()
	writeShim(t, dir, claudeLauncher, claudeScript)
	writeShim(t, dir, codexLauncher, codexScript)

	t.Parallel()

	usage := newUsage(t, map[string]any{
		claudeConfigKey: []any{claudeLauncher},
		codexConfigKey:  []any{codexLauncher},
	}, builtins.UsageOptions{Lookup: pathEnv(dir)})

	result, err := usage.Execute(t.Context(), usageTask(nil))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	got := report(t, result)

	claude := findLogin(t, got, claudeLauncher)
	if claude.Plan != planMax {
		t.Fatalf("claude plan = %q, want max", claude.Plan)
	}

	wantWindows := []builtins.UsageWindow{
		{Name: windowFiveHour, UsedPercent: 4, ResetsAt: mustTime(t, "2026-09-22T18:09:59.940354Z")},
		{Name: windowSevenDay, UsedPercent: 41, ResetsAt: mustTime(t, "2026-09-26T06:59:59.940374Z")},
	}

	if !windowsEqual(claude.Windows, wantWindows) {
		t.Fatalf("claude windows = %+v, want %+v", claude.Windows, wantWindows)
	}

	codex := findLogin(t, got, codexLauncher)
	if codex.Plan != planProlite {
		t.Fatalf("codex plan = %q, want prolite", codex.Plan)
	}

	wantCodex := []builtins.UsageWindow{
		{Name: windowSevenDay, UsedPercent: 24, ResetsAt: time.Unix(1790416865, 0).UTC()},
	}

	if !windowsEqual(codex.Windows, wantCodex) {
		t.Fatalf("codex windows = %+v, want %+v", codex.Windows, wantCodex)
	}

	if result.Meta[claudeLauncher] != "5h 4% 7d 41%" {
		t.Fatalf("meta[%s] = %q, want %q", claudeLauncher, result.Meta[claudeLauncher], "5h 4% 7d 41%")
	}

	if result.Meta[codexLauncher] != "7d 24%" {
		t.Fatalf("meta[%s] = %q, want %q", codexLauncher, result.Meta[codexLauncher], "7d 24%")
	}

	if result.Usage.Latency <= 0 {
		t.Fatalf("latency = %v, want a positive duration", result.Usage.Latency)
	}
}

func TestUsageSendsTheCodexHandshakeInOrder(t *testing.T) {
	dir := t.TempDir()
	shim := writeShim(t, dir, codexLauncher, codexScript)

	t.Parallel()

	usage := newUsage(t, map[string]any{
		claudeConfigKey: []any{},
		codexConfigKey:  []any{codexLauncher},
	}, builtins.UsageOptions{Lookup: pathEnv(dir), Version: usageClientToken})

	if _, err := usage.Execute(t.Context(), usageTask(nil)); err != nil {
		t.Fatalf("execute: %v", err)
	}

	lines := readLines(t, shim+".stdin")
	if len(lines) != 3 {
		t.Fatalf("stdin lines = %#v, want three", lines)
	}

	if !strings.Contains(lines[0], `"method":"initialize"`) ||
		!strings.Contains(lines[0], `"version":"`+usageClientToken+`"`) {
		t.Fatalf("handshake line = %q, want an initialize carrying the version", lines[0])
	}

	if !strings.Contains(lines[1], `"method":"initialized"`) {
		t.Fatalf("second line = %q, want the initialized notification", lines[1])
	}

	if !strings.Contains(lines[2], `"method":"account/rateLimits/read"`) {
		t.Fatalf("third line = %q, want the rate-limit request", lines[2])
	}
}

func TestUsageOverSSHBuildsTheRemoteCommand(t *testing.T) {
	dir := t.TempDir()
	shim := writeShim(t, dir, "ssh-probe", sshRecorderScript)

	t.Parallel()

	usage := newUsage(t, map[string]any{
		claudeConfigKey: []any{"~/.local/bin/" + claudeLauncher},
		codexConfigKey:  []any{},
	}, builtins.UsageOptions{
		Hosts:  map[string]builtins.UsageHost{buildBox: {SSH: buildBoxAlias, Wrapper: remoteWrapper}},
		Lookup: pathEnv(dir),
		SSH:    "ssh-probe",
	})

	result, err := usage.Execute(t.Context(), usageTask(map[string]any{usageHostKey: buildBox}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	got := report(t, result)
	if got.Host != buildBox {
		t.Fatalf("report host = %q, want %q", got.Host, buildBox)
	}

	argv := readLines(t, shim+".argv")
	wantFlags := []string{"-T", sshOptionFlag, "BatchMode=yes", sshOptionFlag, "ConnectTimeout=10", buildBoxAlias, argTerminator}

	if len(argv) != len(wantFlags)+1 {
		t.Fatalf("ssh argv = %#v, want the flags plus one remote command", argv)
	}

	for index, want := range wantFlags {
		if argv[index] != want {
			t.Fatalf("ssh argv[%d] = %q, want %q", index, argv[index], want)
		}
	}

	// The wrapper takes the launcher as its own argv, and the tilde is handed to
	// the far side's HOME rather than expanded here.
	command := argv[len(argv)-1]
	wantPrefix := "exec '" + remoteWrapper + `' "$HOME"/'.local/bin/` + claudeLauncher + "'"

	if !strings.HasPrefix(command, wantPrefix) {
		t.Fatalf("remote command = %q, want it to start with %q", command, wantPrefix)
	}

	if !strings.Contains(command, "'--input-format' 'stream-json'") {
		t.Fatalf("remote command = %q, want the quoted protocol argv", command)
	}
}

func TestUsageOverSSHWithoutAWrapperRunsTheLauncherDirectly(t *testing.T) {
	dir := t.TempDir()
	shim := writeShim(t, dir, "ssh-plain", sshRecorderScript)

	t.Parallel()

	usage := newUsage(t, map[string]any{
		claudeConfigKey: []any{claudeLauncher},
		codexConfigKey:  []any{},
	}, builtins.UsageOptions{
		Hosts:  map[string]builtins.UsageHost{buildBox: {SSH: buildBoxAlias}},
		Lookup: pathEnv(dir),
		SSH:    "ssh-plain",
	})

	if _, err := usage.Execute(t.Context(), usageTask(map[string]any{usageHostKey: buildBox})); err != nil {
		t.Fatalf("execute: %v", err)
	}

	argv := readLines(t, shim+".argv")
	command := argv[len(argv)-1]

	if !strings.HasPrefix(command, "exec '"+claudeLauncher+"'") {
		t.Fatalf("remote command = %q, want it to exec the bare launcher", command)
	}
}

func TestUsageRecordsOneMissingLauncherAndKeepsGoing(t *testing.T) {
	dir := t.TempDir()
	writeShim(t, dir, codexLauncher, codexScript)

	t.Parallel()

	usage := newUsage(t, map[string]any{
		claudeConfigKey: []any{claudeLauncher},
		codexConfigKey:  []any{codexLauncher},
	}, builtins.UsageOptions{Lookup: pathEnv(dir)})

	result, err := usage.Execute(t.Context(), usageTask(nil))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	got := report(t, result)

	missing := findLogin(t, got, claudeLauncher)
	if missing.Error == "" {
		t.Fatalf("login %+v, want an error for the missing launcher", missing)
	}

	live := findLogin(t, got, codexLauncher)
	if live.Error != "" || len(live.Windows) == 0 {
		t.Fatalf("login %+v, want the working launcher to report windows", live)
	}
}

func TestUsageFailsPermanentlyWhenEveryLauncherIsMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	usage := newUsage(t, map[string]any{
		claudeConfigKey: []any{claudeLauncher},
		codexConfigKey:  []any{codexLauncher},
	}, builtins.UsageOptions{Lookup: pathEnv(dir)})

	_, err := usage.Execute(t.Context(), usageTask(nil))
	assertSweepFailure(t, err, runner.ClassPermanent)
}

func TestUsageRecordsARefusedLogin(t *testing.T) {
	dir := pairedShims(t, claudeRefusalScript, codexScript)

	t.Parallel()

	got := pairedProbe(t, dir)

	login := findLogin(t, got, claudeLauncher)
	if !strings.Contains(login.Error, "login expired") {
		t.Fatalf("login error = %q, want the CLI's own reason", login.Error)
	}

	if len(findLogin(t, got, codexLauncher).Windows) == 0 {
		t.Fatalf("report %v, want the second login still reporting", got)
	}
}

func TestUsageRecordsAPlanWithNoRateLimits(t *testing.T) {
	dir := pairedShims(t, `cat > /dev/null
echo '{"type":"control_response","response":{"subtype":"success","request_id":"cairnos-usage","response":{"subscription_type":"free","rate_limits_available":false,"rate_limits":null}}}'`, codexScript)

	t.Parallel()

	got := pairedProbe(t, dir)

	login := findLogin(t, got, claudeLauncher)
	if !strings.Contains(login.Error, "no rate limit windows") {
		t.Fatalf("login error = %q, want it to say the plan reports no windows", login.Error)
	}
}

func TestUsageRecordsAJSONRPCError(t *testing.T) {
	dir := pairedShims(t, claudeScript, `echo '{"jsonrpc":"2.0","method":"configWarning"}'
n=0
while IFS= read -r line; do
  n=$((n+1))
  if [ "$n" -eq 3 ]; then break; fi
done
echo '{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"not signed in"}}'`)

	t.Parallel()

	got := pairedProbe(t, dir)

	login := findLogin(t, got, codexLauncher)
	if !strings.Contains(login.Error, "not signed in") {
		t.Fatalf("login error = %q, want the app-server's own message", login.Error)
	}
}

func TestUsageSkipsMalformedLinesBeforeTheAnswer(t *testing.T) {
	dir := t.TempDir()
	writeShim(t, dir, claudeLauncher, `cat > /dev/null
echo 'not json at all'
echo '{"type":"system"}'
echo '{'
echo '{"type":"control_response","response":{"subtype":"success","request_id":"other","response":{}}}'
echo '`+claudeUsageLine+`'`)

	t.Parallel()

	usage := newUsage(t, map[string]any{
		claudeConfigKey: []any{claudeLauncher},
		codexConfigKey:  []any{},
	}, builtins.UsageOptions{Lookup: pathEnv(dir)})

	result, err := usage.Execute(t.Context(), usageTask(nil))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	login := findLogin(t, report(t, result), claudeLauncher)
	if len(login.Windows) != 2 {
		t.Fatalf("windows = %+v, want the two the answer carries", login.Windows)
	}
}

func TestUsageRecordsALauncherThatNeverAnswers(t *testing.T) {
	dir := pairedShims(t, `cat > /dev/null
echo "wrapper could not find the cli" >&2
exit 3`, codexScript)

	t.Parallel()

	got := pairedProbe(t, dir)

	login := findLogin(t, got, claudeLauncher)
	if !strings.Contains(login.Error, "wrapper could not find the cli") {
		t.Fatalf("login error = %q, want the launcher's stderr tail in it", login.Error)
	}
}

func TestUsageHonoursTheConfiguredTimeout(t *testing.T) {
	dir := t.TempDir()
	writeShim(t, dir, claudeLauncher, "sleep 300")
	writeShim(t, dir, codexLauncher, codexScript)

	t.Parallel()

	usage := newUsage(t, map[string]any{
		claudeConfigKey: []any{claudeLauncher},
		codexConfigKey:  []any{codexLauncher},
		usageTimeoutKey: int64(1),
	}, builtins.UsageOptions{Lookup: pathEnv(dir)})

	start := time.Now()

	result, err := usage.Execute(t.Context(), usageTask(nil))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	// Returning at all is half the assertion: the probe waits for the child to
	// be reaped, so a shim still sleeping would hold this for five minutes.
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("execute took %v, want the one-second bound to end it", elapsed)
	}

	login := findLogin(t, report(t, result), claudeLauncher)
	if !strings.Contains(login.Error, "timed out") {
		t.Fatalf("login error = %q, want it to name the timeout", login.Error)
	}
}

func TestUsageNarrowsToTheRequestedLaunchers(t *testing.T) {
	dir := t.TempDir()
	writeShim(t, dir, claudeLauncher, claudeScript)

	t.Parallel()

	usage := newUsage(t, map[string]any{
		claudeConfigKey: []any{claudeLauncher},
		codexConfigKey:  []any{codexLauncher},
	}, builtins.UsageOptions{Lookup: pathEnv(dir)})

	result, err := usage.Execute(t.Context(), usageTask(map[string]any{
		usageLaunchers: []any{claudeLauncher},
	}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	got := report(t, result)
	if len(got.Logins) != 1 || got.Logins[0].Launcher != claudeLauncher {
		t.Fatalf("logins = %+v, want only %s", got.Logins, claudeLauncher)
	}
}

func TestUsageReportStringIsOneLinePerLogin(t *testing.T) {
	dir := t.TempDir()
	writeShim(t, dir, claudeLauncher, claudeScript)

	t.Parallel()

	usage := newUsage(t, map[string]any{
		claudeConfigKey: []any{claudeLauncher},
		codexConfigKey:  []any{},
	}, builtins.UsageOptions{Lookup: pathEnv(dir)})

	result, err := usage.Execute(t.Context(), usageTask(nil))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	want := claudeLauncher + ": 5h 4% 7d 41%"
	if got := report(t, result).String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

// claudeRefusalScript is the CLI answering with a failure of its own, which is
// what an expired login looks like on the wire.
const claudeRefusalScript = `cat > /dev/null
echo '{"type":"control_response","response":{"subtype":"error","request_id":"cairnos-usage","error":"login expired"}}'`

// pairedShims writes one shim per protocol and returns their directory. It runs
// before t.Parallel for the reason writeShim documents: a fork racing an open
// write descriptor fails the exec.
func pairedShims(t *testing.T, claudeBody, codexBody string) string {
	t.Helper()

	dir := t.TempDir()
	writeShim(t, dir, claudeLauncher, claudeBody)
	writeShim(t, dir, codexLauncher, codexBody)

	return dir
}

// pairedProbe runs both shims and returns the report. Pairing a failing
// launcher with a working one keeps the action green, so the failure is
// readable in the report rather than folded into a swept-up error.
func pairedProbe(t *testing.T, dir string) *builtins.UsageReport {
	t.Helper()

	usage := newUsage(t, map[string]any{
		claudeConfigKey: []any{claudeLauncher},
		codexConfigKey:  []any{codexLauncher},
	}, builtins.UsageOptions{Lookup: pathEnv(dir)})

	result, err := usage.Execute(t.Context(), usageTask(nil))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	return report(t, result)
}

// mustTime parses an RFC 3339 fixture or fails the case, since a window
// assertion means nothing without the instant it compares against.
func mustTime(t *testing.T, text string) time.Time {
	t.Helper()

	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}

	return parsed.UTC()
}

// windowsEqual compares two window slices on the fields the report promises.
func windowsEqual(got, want []builtins.UsageWindow) bool {
	if len(got) != len(want) {
		return false
	}

	for index, window := range got {
		if window.Name != want[index].Name || window.UsedPercent != want[index].UsedPercent {
			return false
		}

		if !window.ResetsAt.Equal(want[index].ResetsAt) {
			return false
		}
	}

	return true
}

// assertSweepFailure checks the error a run reports when no login answered.
func assertSweepFailure(t *testing.T, err error, class runner.ErrorClass) {
	t.Helper()

	const code = "usage.unavailable"

	if err == nil {
		t.Fatalf("execute returned no error, want %s", code)
	}

	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified {
		t.Fatalf("error %v is not a runner.Error", err)
	}

	if classified.Code != code {
		t.Fatalf("code = %q, want %q", classified.Code, code)
	}

	if classified.Class != class {
		t.Fatalf("class = %v, want %v", classified.Class, class)
	}
}

func TestUsageSummaryIsAFencedTableOfEveryWindow(t *testing.T) {
	dir := pairedShims(t, claudeScript, codexScript)

	t.Parallel()

	got := pairedProbe(t, dir)

	want := "```\n" +
		"| LOGIN        | PLAN    | WINDOW | USED | RESETS               |\n" +
		"|--------------|---------|--------|------|----------------------|\n" +
		"| claude-agent | max     | 5h     | 4%   | 2026-09-22 18:09 UTC |\n" +
		"|              |         | 7d     | 41%  | 2026-09-26 06:59 UTC |\n" +
		"| codex-agent  | prolite | 7d     | 24%  | 2026-09-26 10:01 UTC |\n" +
		"```"

	if got.Summary != want {
		t.Fatalf("summary =\n%s\nwant\n%s", got.Summary, want)
	}

	if got.HasErrors {
		t.Fatalf("has_errors = true for a report where every login answered")
	}
}

func TestUsageSummaryCarriesAFailedLoginAcrossTheWindowColumns(t *testing.T) {
	dir := pairedShims(t, claudeRefusalScript, codexScript)

	t.Parallel()

	got := pairedProbe(t, dir)

	if !got.HasErrors {
		t.Fatalf("has_errors = false, want true when %s was refused", claudeLauncher)
	}

	errorRow := "| claude-agent |         | error: "
	if !strings.Contains(got.Summary, errorRow) || !strings.Contains(got.Summary, "login expired |\n") {
		t.Fatalf("summary =\n%s\nwant a row starting %q that ends with the CLI's reason", got.Summary, errorRow)
	}

	if !strings.Contains(got.Summary, "| codex-agent  | prolite | 7d     | 24%  |") {
		t.Fatalf("summary =\n%s\nwant the working login's window row", got.Summary)
	}
}
