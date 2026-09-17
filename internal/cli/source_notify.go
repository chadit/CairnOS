package cli

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/notify"
)

// sourceDirtyRepeat is how long one source stays quiet after a skip line goes
// out. A dirty tree is not news every five minutes, and twice a day is often
// enough that nobody forgets it. The window lives in memory, so a serve restart
// clears it and the next skipped sync posts again.
const sourceDirtyRepeat = 12 * time.Hour

// unknownHostname stands in when the OS will not say. The line is worth sending
// without it, since the channel it lands in still narrows down the host.
const unknownHostname = "unknown-host"

// SourceDirtyNotifier turns skipped syncs into source_dirty lines, one per
// source per sourceDirtyRepeat. It owns the rate limit because the fan-out has
// none. The fan-out arrives after the runner registry, so the zero value is a
// working no-op and SetFanout supplies it later. Serve is the only caller; the
// export exists so the package's external tests can drive it.
type SourceDirtyNotifier struct {
	now      func() time.Time
	fanout   *notify.Fanout
	notified map[string]time.Time
	hostname string
	mu       sync.Mutex
}

// NewSourceDirtyNotifier reads the hostname once rather than per line: it does
// not change under a running process, and a lookup failure should not cost a
// notification. now may be nil for the wall clock; a test passes its own so the
// 12-hour window is exercised without waiting out.
func NewSourceDirtyNotifier(now func() time.Time) *SourceDirtyNotifier {
	if now == nil {
		now = time.Now
	}

	host, err := os.Hostname()
	if err != nil || host == "" {
		host = unknownHostname
	}

	return &SourceDirtyNotifier{
		hostname: host,
		now:      now,
		notified: make(map[string]time.Time),
	}
}

// SetFanout hands the notifier the destination serve builds after the registry.
// Keeping the two steps apart is what lets the startup order stay as it is.
func (n *SourceDirtyNotifier) SetFanout(fanout *notify.Fanout) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.fanout = fanout
}

// Skipped emits one line for a source whose tree was left alone, unless this
// source has already been reported inside the window.
func (n *SourceDirtyNotifier) Skipped(ctx context.Context, source, dest, reason string) {
	fanout, ok := n.claim(source)
	if !ok {
		return
	}

	fanout.Emit(ctx, config.NotifyEventSourceDirty, sourceDirtyLine(n.hostname, source, dest, reason))
}

// Synced clears the window for a source that synced without a skip, so a tree
// that is put right and dirtied again is reported again rather than staying
// silent for the rest of the 12 hours.
func (n *SourceDirtyNotifier) Synced(source string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	delete(n.notified, source)
}

// claim records the send and answers with the fan-out to use, or reports that
// this source is still inside its window. With no fan-out it claims nothing,
// since recording a send would start a 12-hour silence for a notification nobody
// got. The emit happens outside the lock, so a slow destination never holds up
// another source's sync.
func (n *SourceDirtyNotifier) claim(source string) (*notify.Fanout, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.fanout == nil {
		return nil, false
	}

	now := n.now()
	if n.silenced(source, now) {
		return nil, false
	}

	n.notified[source] = now

	return n.fanout, true
}

// silenced reports whether source already sent inside the repeat window. It
// takes now rather than reading the clock, so the window is anchored on the same
// instant the caller goes on to record.
func (n *SourceDirtyNotifier) silenced(source string, now time.Time) bool {
	last, seen := n.notified[source]

	return seen && now.Sub(last) < sourceDirtyRepeat
}

// sourceDirtyLine names the host, because a notification channel collects lines
// from every box an operator runs and the source name alone does not say which
// one needs a look.
func sourceDirtyLine(hostname, source, dest, reason string) string {
	return fmt.Sprintf("CairnOS on %s: source %q at %s left alone: %s; fix the tree or set dirty_reset = true",
		hostname, source, dest, reason)
}
