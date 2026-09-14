package reach_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/reach"
	"github.com/chadit/CairnOS/internal/workflow"
)

const (
	repoURL      = "https://git.example.com/operator/Imagetool-Work"
	hostAddr     = "git.example.com:443"
	workflowName = "prflow_app"
)

var errRefused = errors.New("dial tcp 203.0.113.10:443: connect: connection refused")

// fakeNet stands in for the network: which dial addresses fail, whether the
// VPN interface is up, and every address the monitor asked about. A dial can
// be made to block until released, which is how a slow probe is staged.
type fakeNet struct {
	now     time.Time
	refuse  map[string]error
	hold    chan struct{}
	reached chan struct{}
	dialed  []string
	posted  []string
	mu      sync.Mutex
	vpnUp   bool
	ifaceOK bool
}

func newFakeNet() *fakeNet {
	return &fakeNet{
		refuse:  map[string]error{},
		reached: make(chan struct{}, 1),
		vpnUp:   true,
		ifaceOK: true,
		now:     time.Date(2026, 9, 14, 17, 58, 0, 0, time.UTC),
	}
}

func (f *fakeNet) dial(ctx context.Context, addr string) error {
	f.mu.Lock()
	f.dialed = append(f.dialed, addr)
	hold, err := f.hold, f.refuse[addr]
	f.mu.Unlock()

	// Signal that a probe reached the network so a test can stage the next step
	// against a dial already in flight instead of polling for one.
	select {
	case f.reached <- struct{}{}:
	default:
	}

	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return fmt.Errorf("dial %s: %w", addr, ctx.Err())
		}
	}

	return err
}

func (f *fakeNet) interfaceUp(_ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.ifaceOK {
		return false, errors.New("route ip+net: no such network interface")
	}

	return f.vpnUp, nil
}

func (f *fakeNet) notify(_ context.Context, line string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.posted = append(f.posted, line)
}

func (f *fakeNet) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.now
}

func (f *fakeNet) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.now = f.now.Add(d)
}

// setHostRefused makes the repository host's dial fail with err, or succeed
// again when err is nil.
func (f *fakeNet) setHostRefused(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err == nil {
		delete(f.refuse, hostAddr)

		return
	}

	f.refuse[hostAddr] = err
}

func (f *fakeNet) setVPN(up bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.vpnUp = up
}

// stall makes every following dial block until the returned release is
// called. Dials that start after passthrough run normally; a dial already
// blocked stays blocked until release.
func (f *fakeNet) stall() (func(), func()) {
	f.mu.Lock()
	defer f.mu.Unlock()

	hold := make(chan struct{})
	f.hold = hold

	var once sync.Once

	passthrough := func() {
		f.mu.Lock()
		defer f.mu.Unlock()

		f.hold = nil
	}

	release := func() {
		passthrough()
		once.Do(func() { close(hold) })
	}

	return release, passthrough
}

func (f *fakeNet) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.posted...)
}

func (f *fakeNet) addresses() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.dialed...)
}

func newMonitor(net *fakeNet) *reach.Monitor {
	return reach.New(reach.Config{
		Notify:      net.notify,
		Dial:        net.dial,
		InterfaceUp: net.interfaceUp,
		Now:         net.clock,
	})
}

func requiring(vpn string) *workflow.RequiresSpec {
	return &workflow.RequiresSpec{URLs: []string{repoURL}, VPN: vpn}
}

func TestUnregisteredWorkflowIsNeverBlocked(t *testing.T) {
	t.Parallel()

	monitor := newMonitor(newFakeNet())

	if reason, blocked := monitor.Blocked(t.Context(), "nobody"); blocked {
		t.Fatalf("blocked = true (%q), want an unknown workflow to run", reason)
	}
}

func TestFirstBlockedCallProbesRatherThanGuessing(t *testing.T) {
	t.Parallel()

	net := newFakeNet()
	net.setHostRefused(errRefused)
	monitor := newMonitor(net)
	monitor.Register(workflowName, requiring(""))

	reason, blocked := monitor.Blocked(t.Context(), workflowName)
	if !blocked {
		t.Fatal("blocked = false, want the first call to probe and find the host down")
	}

	if !strings.Contains(reason, repoURL) || !strings.Contains(reason, "connection refused") {
		t.Fatalf("reason = %q, want the URL and the dial error", reason)
	}

	if got := net.addresses(); len(got) != 1 || got[0] != hostAddr {
		t.Fatalf("dialed %v, want the URL's host on port 443", got)
	}
}

func TestOutageIsAnnouncedOnceThenRepeatedHourly(t *testing.T) {
	t.Parallel()

	net := newFakeNet()
	net.setHostRefused(errRefused)
	monitor := newMonitor(net)
	monitor.Register(workflowName, requiring(""))

	for range 3 {
		_, _ = monitor.Check(t.Context(), workflowName)
	}

	if lines := net.lines(); len(lines) != 1 || !strings.HasPrefix(lines[0], `CairnOS: workflow "prflow_app" paused: `) {
		t.Fatalf("lines = %q, want one pause line for three failed probes", lines)
	}

	net.advance(59 * time.Minute)

	_, _ = monitor.Check(t.Context(), workflowName)

	if lines := net.lines(); len(lines) != 1 {
		t.Fatalf("lines = %q, want no reminder before an hour has passed", lines)
	}

	net.advance(time.Minute)

	_, _ = monitor.Check(t.Context(), workflowName)

	lines := net.lines()
	if len(lines) != 2 || !strings.Contains(lines[1], "still paused after 1h0m0s") {
		t.Fatalf("lines = %q, want an hourly reminder naming the outage length", lines)
	}
}

func TestRecoveryIsAnnouncedWithTheOutageLength(t *testing.T) {
	t.Parallel()

	net := newFakeNet()
	net.setHostRefused(errRefused)
	monitor := newMonitor(net)
	monitor.Register(workflowName, requiring(""))

	_, _ = monitor.Check(t.Context(), workflowName)

	net.advance(4 * time.Minute)
	net.setHostRefused(nil)

	if _, blocked := monitor.Check(t.Context(), workflowName); blocked {
		t.Fatal("blocked = true after the host came back")
	}

	lines := net.lines()
	if len(lines) != 2 {
		t.Fatalf("lines = %q, want the pause and the recovery", lines)
	}

	want := `CairnOS: workflow "prflow_app" resumed: ` + repoURL + " reachable again after 4m0s"
	if lines[1] != want {
		t.Fatalf("recovery line = %q, want %q", lines[1], want)
	}

	if _, blocked := monitor.Check(t.Context(), workflowName); blocked || len(net.lines()) != 2 {
		t.Fatalf("a second healthy probe posted again: %q", net.lines())
	}
}

func TestDownVPNIsReportedInsteadOfTheDialItCauses(t *testing.T) {
	t.Parallel()

	net := newFakeNet()
	net.setVPN(false)
	net.setHostRefused(errRefused)
	monitor := newMonitor(net)
	monitor.Register(workflowName, requiring("tun0"))

	reason, blocked := monitor.Check(t.Context(), workflowName)
	if !blocked {
		t.Fatal("blocked = false with the VPN down")
	}

	if !strings.HasPrefix(reason, "VPN tun0 is offline") || !strings.Contains(reason, repoURL) {
		t.Fatalf("reason = %q, want the VPN named first and the URL it fronts", reason)
	}

	if len(net.addresses()) != 0 {
		t.Fatalf("dialed %v, want no dial while the VPN is down", net.addresses())
	}
}

func TestMissingVPNInterfaceReadsAsDown(t *testing.T) {
	t.Parallel()

	net := newFakeNet()
	net.ifaceOK = false
	monitor := newMonitor(net)
	monitor.Register(workflowName, requiring("tun0"))

	if reason, blocked := monitor.Check(t.Context(), workflowName); !blocked || !strings.HasPrefix(reason, "VPN tun0 is offline") {
		t.Fatalf("blocked = %v (%q), want a removed interface to read as offline", blocked, reason)
	}
}

func TestResumableFollowsTheProbe(t *testing.T) {
	t.Parallel()

	net := newFakeNet()
	net.setHostRefused(errRefused)
	monitor := newMonitor(net)
	monitor.Register(workflowName, requiring(""))

	_, _ = monitor.Check(t.Context(), workflowName)

	key := reach.HoldKey(reach.NewUnreachable(workflowName, "down", nil))
	if key != reach.HoldPrefix+workflowName {
		t.Fatalf("hold key = %q", key)
	}

	if monitor.Resumable(key) {
		t.Fatal("resumable while the host is down")
	}

	net.setHostRefused(nil)

	_, _ = monitor.Check(t.Context(), workflowName)

	if !monitor.Resumable(key) {
		t.Fatal("not resumable after the host came back")
	}

	if !monitor.Resumable(reach.HoldPrefix + "forgotten") {
		t.Fatal("a key this monitor never saw must resume, since the worker re-checks before running")
	}

	if monitor.Resumable("codex") {
		t.Fatal("a re-auth key is not this monitor's to answer")
	}
}

func TestRegisterWithoutRequiresForgetsTheWorkflow(t *testing.T) {
	t.Parallel()

	net := newFakeNet()
	net.setHostRefused(errRefused)
	monitor := newMonitor(net)
	monitor.Register(workflowName, requiring(""))

	_, _ = monitor.Check(t.Context(), workflowName)

	monitor.Register(workflowName, nil)

	if _, blocked := monitor.Blocked(t.Context(), workflowName); blocked {
		t.Fatal("still blocked after the requirement was removed from the workflow")
	}
}

func TestChangedRequirementDropsTheOldAnswer(t *testing.T) {
	t.Parallel()

	net := newFakeNet()
	net.setHostRefused(errRefused)
	monitor := newMonitor(net)
	monitor.Register(workflowName, requiring(""))

	_, _ = monitor.Check(t.Context(), workflowName)

	// The same spec again must not reset anything: the outage clock keeps
	// running and no second pause line goes out.
	monitor.Register(workflowName, requiring(""))

	if reason, blocked := monitor.Blocked(t.Context(), workflowName); !blocked || len(net.lines()) != 1 {
		t.Fatalf("blocked = %v (%q), lines = %q; re-registering the same spec changed the answer", blocked, reason, net.lines())
	}

	// A new host is a new question, probed fresh rather than answered with the
	// old host's refusal.
	monitor.Register(workflowName, &workflow.RequiresSpec{URLs: []string{"https://other.example.com/repo"}})

	if reason, blocked := monitor.Blocked(t.Context(), workflowName); blocked {
		t.Fatalf("blocked = true (%q) for a reachable replacement host", reason)
	}
}

func TestAStaleProbeCannotUndoALaterOne(t *testing.T) {
	t.Parallel()

	net := newFakeNet()
	net.setHostRefused(errRefused)
	monitor := newMonitor(net)
	monitor.Register(workflowName, requiring(""))

	// One probe hangs on the refusing host, as a blackholed tunnel makes them
	// do, while a later probe sees the host back and records the recovery.
	release, passthrough := net.stall()
	stale := make(chan struct{})

	go func() {
		defer close(stale)

		_, _ = monitor.Check(t.Context(), workflowName)
	}()

	// Let the stalled probe reach the network before the host comes back, so
	// the recovery belongs to the later probe.
	<-net.reached

	net.setHostRefused(nil)
	passthrough()

	if _, blocked := monitor.Check(t.Context(), workflowName); blocked {
		t.Fatal("the fresh probe found the host up and still reported blocked")
	}

	release()
	<-stale

	if _, blocked := monitor.Blocked(t.Context(), workflowName); blocked {
		t.Fatal("the stale refusal landed after the recovery and blocked the workflow again")
	}

	if lines := net.lines(); len(lines) != 0 {
		t.Fatalf("lines = %q, want nothing: the outage was never recorded before the recovery", lines)
	}
}

func TestACancelledProbeRecordsNothing(t *testing.T) {
	t.Parallel()

	net := newFakeNet()
	monitor := newMonitor(net)
	monitor.Register(workflowName, requiring(""))

	if _, blocked := monitor.Check(t.Context(), workflowName); blocked {
		t.Fatal("blocked with the host up")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if reason, blocked := monitor.Check(ctx, workflowName); blocked {
		t.Fatalf("blocked = true (%q) on a canceled context, want the cached healthy answer", reason)
	}

	if lines := net.lines(); len(lines) != 0 {
		t.Fatalf("lines = %q, want no outage fabricated from a cancellation", lines)
	}
}

func TestRunProbesOnTheInterval(t *testing.T) {
	t.Parallel()

	net := newFakeNet()
	net.setHostRefused(errRefused)
	monitor := reach.New(reach.Config{
		Notify:      net.notify,
		Dial:        net.dial,
		InterfaceUp: net.interfaceUp,
		Now:         net.clock,
		Interval:    5 * time.Millisecond,
	})
	monitor.Register(workflowName, requiring(""))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		monitor.Run(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	deadline := time.After(2 * time.Second)

	for len(net.lines()) == 0 {
		select {
		case <-deadline:
			t.Fatal("Run never probed and announced the outage")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestUnreachableErrorCarriesTheRunFailure(t *testing.T) {
	t.Parallel()

	cause := errors.New("run prflow_app: exit 1")
	err := reach.NewUnreachable(workflowName, "host down", cause)

	if !reach.IsUnreachable(err) || !errors.Is(err, cause) {
		t.Fatal("the error must read as unreachable and still unwrap to the run failure")
	}

	if reach.IsUnreachable(cause) || reach.HoldKey(cause) != "" {
		t.Fatal("a plain run failure is not unreachable")
	}

	if got := err.Error(); !strings.Contains(got, "host down") || !strings.Contains(got, "exit 1") {
		t.Fatalf("Error() = %q, want the reason and the cause", got)
	}
}
