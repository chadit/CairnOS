package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/reach"
	"github.com/chadit/CairnOS/internal/scheduler"
	"github.com/chadit/CairnOS/pkg/runner"
)

const gatedWorkflow = `
[workflow]
name     = "sample_app"
schedule = "* * * * *"

[workflow.requires]
urls = ["https://git.example.com/team/app"]
vpn  = "tun0"

[action.reconcile]
runner = "mock"
`

const (
	gatedName = "sample_app"
	gatedID   = "sched"
)

var (
	errRunFailed = errors.New("run sample_app: exit 1")
	errNoRoute   = errors.New("connect: connection refused")
)

// gateNet is the fake network behind a reach monitor: one switch for the VPN
// and one for the host, plus every line the monitor posted.
type gateNet struct {
	lines    []string
	hostDown bool
	vpnDown  bool
}

func (n *gateNet) monitor() *reach.Monitor {
	return reach.New(reach.Config{
		Notify: func(_ context.Context, line string) { n.lines = append(n.lines, line) },
		Dial: func(_ context.Context, _ string) error {
			if n.hostDown {
				return errNoRoute
			}

			return nil
		},
		InterfaceUp: func(_ string) (bool, error) { return !n.vpnDown, nil },
		Logger:      slog.New(slog.DiscardHandler),
	})
}

// recordingFire is the inputsRunner under the adapter: it counts runs and
// fails with whatever err it holds.
type recordingFire struct {
	err  error
	runs int
}

func (f *recordingFire) FireRun(_ context.Context, _ *scheduler.Schedule, _ map[string]any, _ string) error {
	f.runs++

	return f.err
}

// recordingEnqueue is the daemon-side inner runner: it counts fires.
type recordingEnqueue struct {
	fires int
}

func (r *recordingEnqueue) Fire(_ context.Context, _ *scheduler.Schedule) error {
	r.fires++

	return nil
}

// noCursors is the pollQueue a plain schedule never touches.
type noCursors struct{}

func (noCursors) Enqueue(_ context.Context, _ *scheduler.WorkItem) (bool, error) {
	return false, nil
}

func (noCursors) GetCursor(_ context.Context, _ string) (string, error) {
	return "", scheduler.ErrCursorNotFound
}

func (noCursors) SetCursor(_ context.Context, _, _ string) error {
	return nil
}

func writeGatedWorkflow(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "sample.toml")
	if err := os.WriteFile(path, []byte(gatedWorkflow), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	return path
}

// gatedSchedule is the daemon's view of the workflow at path.
func gatedSchedule(path string) *scheduler.Schedule {
	return &scheduler.Schedule{ID: gatedID, WorkflowPath: path, WorkflowName: gatedName}
}

func gatedItem(t *testing.T, path string) *scheduler.WorkItem {
	t.Helper()

	payload, err := json.Marshal(cli.SchedulePayload{WorkflowPath: path, WorkflowName: gatedName})
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}

	return &scheduler.WorkItem{ID: "item-1", DedupKey: "sched|fire", Payload: payload}
}

func TestDaemonSkipsTheFireWhileTheHostIsDown(t *testing.T) {
	t.Parallel()

	net := &gateNet{hostDown: true}
	inner := &recordingEnqueue{}
	gate := cli.NewDirectPollAwareRunner(inner, noCursors{}, net.monitor(), slog.New(slog.DiscardHandler))
	sched := gatedSchedule(writeGatedWorkflow(t))

	if err := gate.Fire(t.Context(), sched); err != nil {
		t.Fatalf("fire: %v", err)
	}

	if inner.fires != 0 {
		t.Fatalf("inner fired %d times while the host was down, want 0", inner.fires)
	}

	if len(net.lines) != 1 {
		t.Fatalf("posted %q, want the one pause line", net.lines)
	}

	net.hostDown = false

	if _, blocked := gate.Reach().Check(t.Context(), gatedName); blocked {
		t.Fatal("still blocked after the host came back")
	}

	if err := gate.Fire(t.Context(), sched); err != nil {
		t.Fatalf("fire after recovery: %v", err)
	}

	if inner.fires != 1 {
		t.Fatalf("inner fired %d times after recovery, want 1", inner.fires)
	}
}

func TestWorkerHoldsInsteadOfRunningWhileTheHostIsDown(t *testing.T) {
	t.Parallel()

	net := &gateNet{vpnDown: true}
	monitor := net.monitor()
	path := writeGatedWorkflow(t)

	// The daemon registers the workflow on its fire; the worker only knows the
	// name, so the registration has to happen first here too.
	gate := cli.NewDirectPollAwareRunner(&recordingEnqueue{}, noCursors{}, monitor, slog.New(slog.DiscardHandler))
	if err := gate.Fire(t.Context(), gatedSchedule(path)); err != nil {
		t.Fatalf("fire: %v", err)
	}

	fire := &recordingFire{}
	adapter := cli.NewQueueRunnerAdapter(fire, monitor)

	err := adapter.Run(t.Context(), gatedItem(t, path))
	if !reach.IsUnreachable(err) {
		t.Fatalf("run error = %v, want an unreachable hold", err)
	}

	if fire.runs != 0 {
		t.Fatalf("the workflow ran %d times while its VPN was down, want 0", fire.runs)
	}

	if !cli.Holdable(err) || cli.HoldKey(err) != reach.HoldPrefix+gatedName {
		t.Fatalf("holdable = %v, key = %q", cli.Holdable(err), cli.HoldKey(err))
	}
}

func TestWorkerRegistersTheWorkflowItselfAfterARestart(t *testing.T) {
	t.Parallel()

	// No daemon fire has happened: the monitor is as empty as it is right after
	// serve restarts and the sweep resumes an item that was held before.
	net := &gateNet{hostDown: true}
	fire := &recordingFire{err: errRunFailed}
	adapter := cli.NewQueueRunnerAdapter(fire, net.monitor())

	err := adapter.Run(t.Context(), gatedItem(t, writeGatedWorkflow(t)))
	if !reach.IsUnreachable(err) {
		t.Fatalf("run error = %v, want a hold even though nothing fired first", err)
	}

	if fire.runs != 0 {
		t.Fatalf("the workflow ran %d times against a down host, want 0", fire.runs)
	}
}

func TestRunFailureIsReprobedAndHeldWhenTheHostDroppedMidRun(t *testing.T) {
	t.Parallel()

	net := &gateNet{}
	monitor := net.monitor()
	path := writeGatedWorkflow(t)
	gate := cli.NewDirectPollAwareRunner(&recordingEnqueue{}, noCursors{}, monitor, slog.New(slog.DiscardHandler))

	if err := gate.Fire(t.Context(), gatedSchedule(path)); err != nil {
		t.Fatalf("fire: %v", err)
	}

	fire := &recordingFire{err: errRunFailed}
	adapter := cli.NewQueueRunnerAdapter(fire, monitor)

	// The host is up when the item is claimed and gone by the time the run
	// reports back: the worst case, where a probe-before-run alone would still
	// have burned a delivery.
	net.hostDown = true

	err := adapter.Run(t.Context(), gatedItem(t, path))
	if !reach.IsUnreachable(err) || !errors.Is(err, errRunFailed) {
		t.Fatalf("run error = %v, want an unreachable hold wrapping the run failure", err)
	}

	if len(net.lines) != 1 {
		t.Fatalf("posted %q, want the outage announced on the run that hit it", net.lines)
	}
}

func TestRunFailureWithTheHostUpStaysAnOrdinaryFailure(t *testing.T) {
	t.Parallel()

	net := &gateNet{}
	monitor := net.monitor()
	path := writeGatedWorkflow(t)
	gate := cli.NewDirectPollAwareRunner(&recordingEnqueue{}, noCursors{}, monitor, slog.New(slog.DiscardHandler))

	if err := gate.Fire(t.Context(), gatedSchedule(path)); err != nil {
		t.Fatalf("fire: %v", err)
	}

	adapter := cli.NewQueueRunnerAdapter(&recordingFire{err: errRunFailed}, monitor)

	err := adapter.Run(t.Context(), gatedItem(t, path))
	if reach.IsUnreachable(err) || !errors.Is(err, errRunFailed) {
		t.Fatalf("run error = %v, want the run failure itself, not a hold", err)
	}

	if cli.Holdable(err) {
		t.Fatal("an ordinary failure must not be held")
	}
}

func TestResumeProbeRoutesReachKeysToTheMonitor(t *testing.T) {
	t.Parallel()

	net := &gateNet{hostDown: true}
	monitor := net.monitor()
	path := writeGatedWorkflow(t)
	gate := cli.NewDirectPollAwareRunner(&recordingEnqueue{}, noCursors{}, monitor, slog.New(slog.DiscardHandler))

	if err := gate.Fire(t.Context(), gatedSchedule(path)); err != nil {
		t.Fatalf("fire: %v", err)
	}

	var reauthAsked bool

	probe := cli.ResumeProbe(monitor, func(_ string, _ time.Time) bool {
		reauthAsked = true

		return true
	})

	key := reach.HoldPrefix + gatedName
	if probe(key, time.Now()) {
		t.Fatal("a reach hold resumed while the host was still down")
	}

	if reauthAsked {
		t.Fatal("a reach key reached the credential probe")
	}

	net.hostDown = false
	_, _ = monitor.Check(t.Context(), gatedName)

	if !probe(key, time.Now()) {
		t.Fatal("a reach hold stayed held after the host came back")
	}

	if !probe("codex", time.Now()) || !reauthAsked {
		t.Fatal("a re-auth key must go to the credential probe")
	}

	if cli.ResumeProbe(monitor, nil)("codex", time.Now()) {
		t.Fatal("a re-auth key with no credential probe must stay held")
	}
}

func TestHoldableStillCoversReauth(t *testing.T) {
	t.Parallel()

	reauth := runner.NewError(runner.ClassNeedsReauth, "codex.needs_reauth", "run codex login")
	if !cli.Holdable(reauth) || cli.HoldKey(reauth) != "codex" {
		t.Fatalf("holdable = %v, key = %q for a re-auth error", cli.Holdable(reauth), cli.HoldKey(reauth))
	}
}

func TestReachHoldsPostNothingFromTheWorker(t *testing.T) {
	t.Parallel()

	var innerCalls int

	onHold := cli.SkipReachHolds(func(_ context.Context, _ *scheduler.WorkItem, _ error) { innerCalls++ })

	onHold(t.Context(), &scheduler.WorkItem{}, reach.NewUnreachable("w", "down", nil))

	if innerCalls != 0 {
		t.Fatal("a reach hold reached the re-auth alert")
	}

	onHold(t.Context(), &scheduler.WorkItem{}, runner.NewError(runner.ClassNeedsReauth, "codex.needs_reauth", "login"))

	if innerCalls != 1 {
		t.Fatal("a re-auth hold must still alert")
	}

	if cli.SkipReachHolds(nil) != nil {
		t.Fatal("a nil alert must stay nil so the worker skips it")
	}
}
