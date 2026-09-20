// Package reach probes what a scheduled workflow declares it needs to reach,
// so a host outage pauses the schedule instead of failing a run a minute.
//
// On 2026-09-14 the VPN to an Enterprise GitHub host dropped for four minutes.
// The reconciler workflow fired every minute regardless, each run died on the
// same dial error, the queue retried each one five times, and the operator got
// twenty tracebacks and two dead-letters for one cause. A workflow that names
// the host it talks to lets the daemon answer the question once: probe, skip
// the fire while the answer is no, say so once an hour, and say when it is
// back.
package reach

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/chadit/CairnOS/internal/workflow"
)

// Timing. The probe runs every minute, so a schedule waits at most one tick
// after the host returns, and a sub-minute schedule skips a few more fires on
// the way back. An hourly reminder keeps a long outage visible without becoming
// a heartbeat. A dial past five seconds is as good as refused for a host that
// answers in milliseconds.
const (
	defaultInterval = time.Minute
	repeatAfter     = time.Hour
	dialTimeout     = 5 * time.Second
)

// HoldPrefix marks a queue hold key as a reachability hold, so the worker's
// resume probe can tell it from a re-auth hold and ask this package instead of
// the credential store.
const HoldPrefix = "reach:"

// Config wires a Monitor. Only Notify has no useful default: nil means the
// monitor still gates fires but tells nobody. Dial and InterfaceUp exist so a
// fake can stand in for the network; production leaves them nil.
type Config struct {
	Notify      func(ctx context.Context, line string)
	Dial        func(ctx context.Context, addr string) error
	InterfaceUp func(name string) (bool, error)
	Now         func() time.Time
	Logger      *slog.Logger
	Interval    time.Duration
}

// state is what the monitor knows about one workflow's requirement. reason is
// empty while everything is reachable; since and notified only mean something
// during an outage. issued and applied order the probes: a result older than
// the last one applied is dropped, so a dial that hung through a recovery
// cannot land afterwards and report the outage back.
type state struct {
	since    time.Time
	notified time.Time
	reason   string
	spec     workflow.RequiresSpec
	issued   uint64
	applied  uint64
	probed   bool
}

// Monitor holds one reachability state per registered workflow and keeps it
// current on a timer. Every method is safe from any goroutine.
type Monitor struct {
	workflows map[string]*state
	cfg       Config
	mu        sync.Mutex
}

// New builds a Monitor, filling Config defaults.
func New(cfg Config) *Monitor {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultInterval
	}

	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	if cfg.Dial == nil {
		cfg.Dial = dialTCP
	}

	if cfg.InterfaceUp == nil {
		cfg.InterfaceUp = interfaceUp
	}

	return &Monitor{cfg: cfg, workflows: map[string]*state{}}
}

// Register records what the workflow named name needs. The name is the
// schedule's, not the file's, so the daemon and the worker key one entry even
// after the file's name is edited. A changed spec starts fresh, so a stale
// answer about the old host never gates the new one. A nil spec forgets the
// workflow.
func (m *Monitor) Register(name string, spec *workflow.RequiresSpec) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if spec == nil {
		delete(m.workflows, name)

		return
	}

	current, known := m.workflows[name]
	if known && sameSpec(&current.spec, spec) {
		return
	}

	m.workflows[name] = &state{spec: *spec}
}

// Blocked reports whether workflow name should not run right now, and why. It
// answers from the last probe, except for a workflow never probed, which it
// probes on the spot so the first fire after registration is not a guess. An
// unregistered name is never blocked.
func (m *Monitor) Blocked(ctx context.Context, name string) (string, bool) {
	reason, known, probed := m.cached(name)
	if !known {
		return "", false
	}

	if probed {
		return reason, reason != ""
	}

	return m.Check(ctx, name)
}

// Check probes workflow name now and reports the fresh answer, recording and
// notifying like the timer would. The worker calls it after a run fails, so a
// mid-run outage is announced on the run that hit it. A probe cut short by ctx
// proves nothing about the host: it records nothing and the cached answer
// stands.
func (m *Monitor) Check(ctx context.Context, name string) (string, bool) {
	spec, seq, known := m.issue(name)
	if !known {
		return "", false
	}

	reason := m.probe(ctx, &spec)
	if ctx.Err() != nil {
		cachedReason, _, _ := m.cached(name)

		return cachedReason, cachedReason != ""
	}

	m.record(ctx, name, reason, seq)

	return reason, reason != ""
}

// Resumable reports whether a queue item held under key may run again: true
// once the workflow's requirement is reachable, and true for a key this monitor
// does not know, since the worker registers and re-checks the workflow before
// running it, so a released item is gated again rather than run blind.
func (m *Monitor) Resumable(key string) bool {
	name, isReach := strings.CutPrefix(key, HoldPrefix)
	if !isReach {
		return false
	}

	reason, known, _ := m.cached(name)

	return !known || reason == ""
}

// Run probes every registered workflow on the interval until ctx cancels.
func (m *Monitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.tick(ctx)
		}
	}
}

// cached reads one workflow's stored answer under the lock: the reason it is
// down, whether the name is registered, and whether it has been probed yet.
func (m *Monitor) cached(name string) (string, bool, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	current, known := m.workflows[name]
	if !known {
		return "", false, false
	}

	return current.reason, true, current.probed
}

// issue copies one workflow's requirement out from under the lock and stamps
// the probe about to run, so a dial never happens while the lock is held and
// its result can be ordered against every other probe of the same workflow.
func (m *Monitor) issue(name string) (workflow.RequiresSpec, uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	current, known := m.workflows[name]
	if !known {
		return workflow.RequiresSpec{}, 0, false
	}

	current.issued++

	return current.spec, current.issued, true
}

// tick probes every registered workflow once. A probe the shutdown cut short
// is dropped for the same reason Check drops it.
func (m *Monitor) tick(ctx context.Context) {
	m.mu.Lock()
	names := make([]string, 0, len(m.workflows))

	for name := range m.workflows {
		names = append(names, name)
	}
	m.mu.Unlock()

	for _, name := range names {
		spec, seq, known := m.issue(name)
		if !known {
			continue
		}

		reason := m.probe(ctx, &spec)
		if ctx.Err() != nil {
			return
		}

		m.record(ctx, name, reason, seq)
	}
}

// probe answers why spec is unreachable, or "" when it is. The VPN is checked
// first because a down tunnel explains every dial failure behind it, and the
// operator needs to hear about the tunnel, not the symptom.
func (m *Monitor) probe(ctx context.Context, spec *workflow.RequiresSpec) string {
	if spec.VPN != "" {
		up, err := m.cfg.InterfaceUp(spec.VPN)
		if err != nil || !up {
			return fmt.Sprintf("VPN %s is offline, so %s cannot be reached", spec.VPN, joinURLs(spec.URLs))
		}
	}

	for _, raw := range spec.URLs {
		if err := m.cfg.Dial(ctx, dialAddr(raw)); err != nil {
			return fmt.Sprintf("%s cannot be reached (%v)", raw, err)
		}
	}

	return ""
}

// record applies one probe result and posts whichever line the transition
// calls for. The line is built under the lock and posted outside it, so a
// slow notifier never stalls a Blocked call on the fire path.
func (m *Monitor) record(ctx context.Context, name, reason string, seq uint64) {
	line := m.apply(name, reason, seq)
	if line == "" {
		return
	}

	m.cfg.Logger.Warn("reachability", "line", line)

	if m.cfg.Notify != nil {
		m.cfg.Notify(ctx, line)
	}
}

// apply stores one probe result and returns the line it earns: the outage
// when it begins, a reminder once an hour has passed, the recovery with how
// long it took, or nothing. A result older than the last applied, or for a
// workflow forgotten meanwhile, changes nothing.
func (m *Monitor) apply(name, reason string, seq uint64) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	current, known := m.workflows[name]
	if !known || seq < current.applied {
		return ""
	}

	now := m.cfg.Now()
	wasDown := current.reason != ""
	current.applied = seq
	current.probed = true
	current.reason = reason

	switch {
	case reason != "" && !wasDown:
		current.since = now
		current.notified = now

		return fmt.Sprintf("CairnOS: workflow %q paused: %s", name, reason)
	case reason != "" && now.Sub(current.notified) >= repeatAfter:
		current.notified = now

		return fmt.Sprintf("CairnOS: workflow %q still paused after %s: %s",
			name, outageFor(now.Sub(current.since)), reason)
	case reason == "" && wasDown:
		return fmt.Sprintf("CairnOS: workflow %q resumed: %s reachable again after %s",
			name, joinURLs(current.spec.URLs), outageFor(now.Sub(current.since)))
	default:
		return ""
	}
}

// UnreachableError is the run error a gated worker returns instead of running
// a workflow whose requirement is down. The worker holds the item on it, and
// Cause carries the run failure when the outage was found after one.
type UnreachableError struct {
	Cause    error
	Workflow string
	Reason   string
}

// NewUnreachable builds the error for workflow name; cause may be nil.
func NewUnreachable(name, reason string, cause error) *UnreachableError {
	return &UnreachableError{Workflow: name, Reason: reason, Cause: cause}
}

func (e *UnreachableError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("workflow %q: %s: %v", e.Workflow, e.Reason, e.Cause)
	}

	return fmt.Sprintf("workflow %q: %s", e.Workflow, e.Reason)
}

// Unwrap exposes the run failure, when there was one, to errors.Is and As.
func (e *UnreachableError) Unwrap() error {
	return e.Cause
}

// IsUnreachable reports whether err is, or wraps, an UnreachableError.
func IsUnreachable(err error) bool {
	_, ok := errors.AsType[*UnreachableError](err)

	return ok
}

// HoldKey is the queue hold key for err, naming the workflow whose requirement
// the item waits on, or "" when err is not an UnreachableError.
func HoldKey(err error) string {
	unreachable, ok := errors.AsType[*UnreachableError](err)
	if !ok {
		return ""
	}

	return HoldPrefix + unreachable.Workflow
}

// sameSpec reports whether two requirements name the same VPN and URLs.
func sameSpec(stored, incoming *workflow.RequiresSpec) bool {
	return stored.VPN == incoming.VPN && slices.Equal(stored.URLs, incoming.URLs)
}

// dialAddr is the host:port a URL's probe dials: the URL's own port when it
// names one, else 443 for https and 80 for http. Validation already made the
// URL absolute with a host, so a parse failure here cannot happen for a loaded
// workflow.
func dialAddr(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	if parsed.Port() != "" {
		return parsed.Host
	}

	port := "443"
	if parsed.Scheme == "http" {
		port = "80"
	}

	return net.JoinHostPort(parsed.Hostname(), port)
}

// dialTCP is the production Dial: one TCP connect, closed as soon as it opens.
// A refused or unrouted port is exactly the failure the runs were dying on.
func dialTCP(ctx context.Context, addr string) error {
	dialer := &net.Dialer{Timeout: dialTimeout}

	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}

	if err := conn.Close(); err != nil {
		return fmt.Errorf("close %s: %w", addr, err)
	}

	return nil
}

// interfaceUp is the production InterfaceUp. OpenVPN removes tun0 when the
// tunnel drops, so a missing interface reads as down rather than as an error
// worth surfacing separately.
func interfaceUp(name string) (bool, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return false, fmt.Errorf("interface %s: %w", name, err)
	}

	return iface.Flags&net.FlagUp != 0, nil
}

// joinURLs renders a spec's URLs for a line, comma separated.
func joinURLs(urls []string) string {
	return strings.Join(urls, ", ")
}

// outageFor renders a duration the way an operator reads one: whole minutes,
// with anything shorter called out rather than printed as seconds.
func outageFor(d time.Duration) string {
	if d < time.Minute {
		return "under a minute"
	}

	return d.Round(time.Minute).String()
}
