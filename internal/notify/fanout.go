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

// Poster is the single-string sink a Fanout drains onto. *DiscordNotifier
// satisfies it through Post, and so does the Mattermost sink from
// MattermostSink. The interface keeps fan-out tests off a live webhook and lets
// serve pick which destinations exist.
type Poster interface {
	Post(ctx context.Context, content string) error
}

// Sink is one destination plus the event names it wants, as serve declares it.
// Events is the same six-name vocabulary config validates (held, run_failed,
// dead_lettered, budget_denied, connectivity, source_dirty), so two sinks can
// subscribe to different slices of it: Discord to everything, Mattermost to the
// held alert only, say.
type Sink struct {
	Poster Poster
	Events []string
}

// sink is a Sink with its event list turned into a set, which is the form Emit
// and Run ask about on every line.
type sink struct {
	poster   Poster
	selected map[string]struct{}
}

// selects reports whether this sink wants event.
func (s *sink) selects(event string) bool {
	_, ok := s.selected[event]

	return ok
}

// queuedLine is one enqueued notification. The event travels with the line
// because the sinks disagree about which events they want, so the drainer has
// to re-ask per sink rather than filtering once at Emit.
type queuedLine struct {
	event string
	line  string
}

// Fanout posts selected engine and queue events to every sink, off the producer
// goroutine (D8). Producers call Emit from any goroutine; a single drainer,
// started by Run, posts each queued line to the sinks that selected its event.
// A nil *Fanout is the "nothing configured" state where every method is a no-op,
// so serve wires producers without nil guards.
type Fanout struct {
	logger *slog.Logger

	// lines drops on overflow so a slow or failing destination never blocks an
	// engine, queue, or chat goroutine: a dropped notification changes no run,
	// queue, or chat outcome.
	lines chan queuedLine

	sinks []sink
}

// NewFanout builds a fan-out over the given sinks. A nil logger falls back to
// slog.Default. The result is inert until Run drains it. Sinks come last and
// variadic so serve appends the destinations it has configured and passes them
// in one call.
func NewFanout(logger *slog.Logger, sinks ...Sink) *Fanout {
	if logger == nil {
		logger = slog.Default()
	}

	built := make([]sink, 0, len(sinks))

	for _, declared := range sinks {
		selected := make(map[string]struct{}, len(declared.Events))
		for _, event := range declared.Events {
			selected[event] = struct{}{}
		}

		built = append(built, sink{poster: declared.Poster, selected: selected})
	}

	return &Fanout{
		sinks:  built,
		lines:  make(chan queuedLine, fanoutBuffer),
		logger: logger,
	}
}

// Selects reports whether any sink wants event, so a producer can skip building
// a line no one posts. A nil fan-out selects nothing.
func (f *Fanout) Selects(event string) bool {
	if f == nil {
		return false
	}

	for idx := range f.sinks {
		if f.sinks[idx].selects(event) {
			return true
		}
	}

	return false
}

// Emit queues line for event when some sink selected it, dropping it with a warn
// log when the buffer is full so the calling producer never blocks on a slow
// destination. A canceled ctx skips the enqueue, since the notification is moot.
// Safe from any goroutine and a no-op on a nil fan-out or an unselected event.
func (f *Fanout) Emit(ctx context.Context, event, line string) {
	if !f.Selects(event) {
		return
	}

	select {
	case f.lines <- queuedLine{event: event, line: line}:
	case <-ctx.Done():
	default:
		f.logger.Warn("notify fan-out buffer full; dropping event line", "event", event)
	}
}

// Run drains queued lines and posts each until ctx cancels. A delivery failure
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
		case queued := <-f.lines:
			f.post(ctx, queued)
		}
	}
}

// post delivers one queued line to every sink that selected its event. Each sink
// is attempted independently so a failing destination costs the others nothing:
// a Mattermost identity that never registered must not silence the Discord
// webhook.
func (f *Fanout) post(ctx context.Context, queued queuedLine) {
	for idx := range f.sinks {
		target := &f.sinks[idx]
		if !target.selects(queued.event) {
			continue
		}

		if err := target.poster.Post(ctx, queued.line); err != nil {
			f.logger.Error("notify fan-out post failed", "event", queued.event, "error", err)
		}
	}
}
