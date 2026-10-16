package builtins_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// gapKey is the config key that sets the spacing between two Claude probes.
const gapKey = "claude_gap"

// secondClaudeLauncher is a second Claude login on the same box, which is the
// shape the cache bites: both read the same config directory.
const secondClaudeLauncher = "claude-review"

// gapRecorder stands in for the wait. Probes are sequential, so a plain slice
// needs no lock, and allow decides whether the wait is reported as completed.
type gapRecorder struct {
	waits []time.Duration
	allow bool
}

// sleeper returns the seam the runner calls in place of a real wait.
func (g *gapRecorder) sleeper() builtins.UsageSleeper {
	return func(_ context.Context, gap time.Duration) bool {
		g.waits = append(g.waits, gap)

		return g.allow
	}
}

// twoClaudeLogins builds a runner over two Claude shims that both answer, with
// the recorder standing in for the gap.
func twoClaudeLogins(t *testing.T, recorder *gapRecorder, cfg map[string]any) *builtins.Usage {
	t.Helper()

	dir := t.TempDir()
	writeShim(t, dir, claudeLauncher, claudeScript)
	writeShim(t, dir, secondClaudeLauncher, claudeScript)

	cfg[claudeConfigKey] = []any{claudeLauncher, secondClaudeLauncher}
	cfg[codexConfigKey] = []any{}

	return newUsage(t, cfg, builtins.UsageOptions{Lookup: pathEnv(dir), Sleep: recorder.sleeper()})
}

func TestUsageSpacesTwoClaudeLoginsApart(t *testing.T) {
	t.Parallel()

	recorder := &gapRecorder{allow: true}
	usage := twoClaudeLogins(t, recorder, map[string]any{})

	result, err := usage.Execute(t.Context(), usageTask(nil))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	// One wait for two logins: the first probe has no cached answer to outlast.
	if len(recorder.waits) != 1 || recorder.waits[0] != 65*time.Second {
		t.Fatalf("waits = %v, want one wait of 65s", recorder.waits)
	}

	got := report(t, result)
	for _, launcher := range []string{claudeLauncher, secondClaudeLauncher} {
		if len(findLogin(t, got, launcher).Windows) == 0 {
			t.Fatalf("report %v, want %s to report windows", got, launcher)
		}
	}
}

func TestUsageSkipsTheGapWhenConfiguredToZero(t *testing.T) {
	t.Parallel()

	recorder := &gapRecorder{allow: true}
	usage := twoClaudeLogins(t, recorder, map[string]any{gapKey: int64(0)})

	if _, err := usage.Execute(t.Context(), usageTask(nil)); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if len(recorder.waits) != 0 {
		t.Fatalf("waits = %v, want none when the gap is zero", recorder.waits)
	}
}

func TestUsageHonoursAConfiguredGap(t *testing.T) {
	t.Parallel()

	recorder := &gapRecorder{allow: true}
	usage := twoClaudeLogins(t, recorder, map[string]any{gapKey: int64(90)})

	if _, err := usage.Execute(t.Context(), usageTask(nil)); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if len(recorder.waits) != 1 || recorder.waits[0] != 90*time.Second {
		t.Fatalf("waits = %v, want one wait of 90s", recorder.waits)
	}
}

func TestUsageDoesNotWaitBetweenProtocols(t *testing.T) {
	dir := t.TempDir()
	writeShim(t, dir, claudeLauncher, claudeScript)
	writeShim(t, dir, codexLauncher, codexScript)

	t.Parallel()

	recorder := &gapRecorder{allow: true}

	usage := newUsage(t, map[string]any{
		claudeConfigKey: []any{claudeLauncher},
		codexConfigKey:  []any{codexLauncher},
	}, builtins.UsageOptions{Lookup: pathEnv(dir), Sleep: recorder.sleeper()})

	if _, err := usage.Execute(t.Context(), usageTask(nil)); err != nil {
		t.Fatalf("execute: %v", err)
	}

	// Only Claude caches its answer, so a codex probe needs no spacing and a
	// single claude login has nothing to be spaced from.
	if len(recorder.waits) != 0 {
		t.Fatalf("waits = %v, want none across two protocols", recorder.waits)
	}
}

func TestUsageLeavesLaunchersUnprobedWhenTheGapIsCutShort(t *testing.T) {
	t.Parallel()

	recorder := &gapRecorder{allow: false}
	usage := twoClaudeLogins(t, recorder, map[string]any{})

	result, err := usage.Execute(t.Context(), usageTask(nil))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	got := report(t, result)

	if len(findLogin(t, got, claudeLauncher).Windows) == 0 {
		t.Fatalf("report %v, want the first login probed before the wait", got)
	}

	skipped := findLogin(t, got, secondClaudeLauncher)
	if skipped.Error == "" || len(skipped.Windows) != 0 {
		t.Fatalf("login %+v, want it recorded as not probed", skipped)
	}
}

func TestUsageRefusesANegativeGap(t *testing.T) {
	t.Parallel()

	_, err := builtins.NewUsage(map[string]any{gapKey: int64(-1)}, builtins.UsageOptions{})
	if err == nil {
		t.Fatalf("config accepted a negative %s", gapKey)
	}
}

func TestUsageEmptyLaunchersListIsAValidationError(t *testing.T) {
	t.Parallel()

	task := usageTask(map[string]any{usageLaunchers: []any{}})
	assertValidateCode(t, configuredUsage(t).Validate(task), "usage.bad_launchers")
}

func TestUsageSweepOfRefusalsIsPermanent(t *testing.T) {
	dir := t.TempDir()
	writeShim(t, dir, claudeLauncher, claudeRefusalScript)

	t.Parallel()

	usage := newUsage(t, map[string]any{
		claudeConfigKey: []any{claudeLauncher},
		codexConfigKey:  []any{},
	}, builtins.UsageOptions{Lookup: pathEnv(dir)})

	// A login the CLI turned down is settled: another run meets the same answer.
	_, err := usage.Execute(t.Context(), usageTask(nil))
	assertSweepFailure(t, err, runner.ClassPermanent)
}

func TestUsageSweepOfMissingWindowsIsPermanent(t *testing.T) {
	dir := t.TempDir()
	writeShim(t, dir, claudeLauncher, `cat > /dev/null
echo '{"type":"control_response","response":{"subtype":"success","request_id":"cairnos-usage","response":{"subscription_type":"free","rate_limits_available":false,"rate_limits":null}}}'`)

	t.Parallel()

	usage := newUsage(t, map[string]any{
		claudeConfigKey: []any{claudeLauncher},
		codexConfigKey:  []any{},
	}, builtins.UsageOptions{Lookup: pathEnv(dir)})

	_, err := usage.Execute(t.Context(), usageTask(nil))
	assertSweepFailure(t, err, runner.ClassPermanent)
}

func TestUsageSweepOfStartFailuresIsTransient(t *testing.T) {
	dir := t.TempDir()
	writeShim(t, dir, claudeLauncher, `echo "no route to the box" >&2
exit 255`)

	t.Parallel()

	usage := newUsage(t, map[string]any{
		claudeConfigKey: []any{claudeLauncher},
		codexConfigKey:  []any{},
	}, builtins.UsageOptions{Lookup: pathEnv(dir)})

	// A hop that died carries no verdict about the login, so it is worth a retry.
	_, err := usage.Execute(t.Context(), usageTask(nil))
	assertSweepFailure(t, err, runner.ClassTransient)
}

func TestUsageRecordsAnUnreadableWindow(t *testing.T) {
	t.Parallel()

	dir := pairedShims(t, `cat > /dev/null
echo '{"type":"control_response","response":{"subtype":"success","request_id":"cairnos-usage","response":{"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":4,"resets_at":1790416865}}}}}'`, codexScript)

	login := findLogin(t, pairedProbe(t, dir), claudeLauncher)
	if !strings.Contains(login.Error, "rate_limits.five_hour") {
		t.Fatalf("login error = %q, want it to name the key that would not decode", login.Error)
	}
}

// blockingScript writes the stderr line its caller should see before it marks
// itself running, so a cancel never lands ahead of that line, then blocks.
const blockingScript = `echo "wrapper is still waiting on the vpn" >&2
echo started > "$0.running"
sleep 300`

// waitForFile blocks until the shim reports it is running, so a case can cancel
// mid-probe without guessing at a delay.
func waitForFile(t *testing.T, path string) {
	t.Helper()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	deadline := time.After(30 * time.Second)

	for {
		if _, err := os.Stat(path); err == nil {
			return
		}

		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatalf("%s never appeared, so the shim did not start", path)
		}
	}
}

func TestUsageReportsACancellationRatherThanATimeout(t *testing.T) {
	dir := t.TempDir()
	shim := writeShim(t, dir, claudeLauncher, blockingScript)

	t.Parallel()

	usage := newUsage(t, map[string]any{
		claudeConfigKey: []any{claudeLauncher},
		codexConfigKey:  []any{},
	}, builtins.UsageOptions{Lookup: pathEnv(dir)})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() {
		_, execErr := usage.Execute(ctx, usageTask(nil))
		done <- execErr
	}()

	waitForFile(t, shim+".running")
	cancel()

	err := <-done
	assertSweepFailure(t, err, runner.ClassTransient)

	// A run somebody stopped says nothing about the CLI, so it must not read as
	// the CLI being slow, and the launcher's own stderr still has to survive.
	message := err.Error()
	if !strings.Contains(message, "canceled") || strings.Contains(message, "timed out") {
		t.Fatalf("error = %v, want it to report the cancellation", err)
	}

	if !strings.Contains(message, "wrapper is still waiting on the vpn") {
		t.Fatalf("error = %v, want the launcher's stderr tail in it", err)
	}
}

func TestUsageTimeoutKeepsTheStderrTail(t *testing.T) {
	dir := t.TempDir()
	writeShim(t, dir, claudeLauncher, blockingScript)

	t.Parallel()

	usage := newUsage(t, map[string]any{
		claudeConfigKey: []any{claudeLauncher},
		codexConfigKey:  []any{},
		usageTimeoutKey: int64(1),
	}, builtins.UsageOptions{Lookup: pathEnv(dir)})

	_, err := usage.Execute(t.Context(), usageTask(nil))
	assertSweepFailure(t, err, runner.ClassTransient)

	message := err.Error()
	if !strings.Contains(message, "timed out") {
		t.Fatalf("error = %v, want it to name the timeout", err)
	}

	if !strings.Contains(message, "wrapper is still waiting on the vpn") {
		t.Fatalf("error = %v, want the launcher's stderr tail in it", err)
	}
}
