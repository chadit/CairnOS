package notify

import (
	"context"
	"log/slog"
)

// fanoutBuffer sizes the fan-out's line channel. A single-operator surface
// rarely queues more than a handful of notifications between drains, so 64
// absorbs a burst without letting a slow webhook grow memory without bound; past
// it, lines drop rather than block a producer.
const fanoutBuffer = 64

// Poster is the single-string webhook sink a Fanout drains onto.
// *DiscordNotifier satisfies it through Post. The interface keeps fan-out tests
// off a live webhook and lets serve swap the sink.
type Poster interface {
	Post(ctx context.Context, content string) error
}

// Fanout posts selected engine and queue events to a Discord webhook, one line
// each, off the producer goroutine (.agents/spec/parity/messaging-bridge.md,
// decision 7). Producers call Emit from any goroutine; a single drainer started
// by Run posts each queued line. The buffer drops on overflow so a slow or
// failing webhook never blocks an engine, queue, or chat goroutine: a dropped
// notification changes no run, queue, or chat outcome. A nil *Fanout is the
// "no webhook configured" state; every method is a no-op on it, so serve wires
// producers without a nil guard at each call site.
type Fanout struct {
	poster   Poster
	selected map[string]struct{}
	lines    chan string
	logger   *slog.Logger
}

// NewFanout builds a fan-out over poster that emits only the named events. A nil
// logger falls back to slog.Default. The result is inert until Run drains it.
func NewFanout(poster Poster, events []string, logger *slog.Logger) *Fanout {
	selected := make(map[string]struct{}, len(events))
	for _, event := range events {
		selected[event] = struct{}{}
	}

	if logger == nil {
		logger = slog.Default()
	}

	return &Fanout{
		poster:   poster,
		selected: selected,
		lines:    make(chan string, fanoutBuffer),
		logger:   logger,
	}
}

// Selects reports whether event is in the configured set, so a producer can skip
// building a line no one posts. A nil fan-out selects nothing.
func (f *Fanout) Selects(event string) bool {
	if f == nil {
		return false
	}

	_, ok := f.selected[event]

	return ok
}

// Emit queues line for event when event is selected, dropping it with a warn log
// when the buffer is full so the calling producer goroutine never blocks on a
// slow webhook. A ctx already canceled skips the enqueue: the producing
// operation is gone, so its notification is moot. Safe from any goroutine and a
// no-op on a nil fan-out or an unselected event.
func (f *Fanout) Emit(ctx context.Context, event, line string) {
	if !f.Selects(event) {
		return
	}

	select {
	case f.lines <- line:
	case <-ctx.Done():
	default:
		f.logger.Warn("discord fan-out buffer full; dropping event line", "event", event)
	}
}

// Run drains queued lines and posts each until ctx cancels. A webhook failure
// logs and never propagates: a dropped notification changes no run, queue, or
// chat outcome. A nil fan-out returns at once, so the serve drain needs no nil
// guard around the goroutine.
func (f *Fanout) Run(ctx context.Context) {
	if f == nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case line := <-f.lines:
			if err := f.poster.Post(ctx, line); err != nil {
				f.logger.Error("discord fan-out post failed", "error", err)
			}
		}
	}
}
