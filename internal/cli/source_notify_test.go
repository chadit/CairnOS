package cli_test

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/notify"
)

// The source every notifier case reports on, and the reason it carries.
const (
	notifySourceName = "automation"
	notifySourceDest = "/opt/src/automation"
	notifySkipReason = "uncommitted changes"
)

// notifyPostWait bounds the wait for a line to travel through the drainer to the
// capture server. It is generous because a slow machine failing this case would
// say nothing about the cooldown it exists to prove.
const notifyPostWait = 2 * time.Second

// testClock is the injected wall clock. A case moves it instead of waiting out a
// 12-hour window.
type testClock struct {
	at time.Time
}

// now satisfies the clock seam NewSourceDirtyNotifier takes.
func (c *testClock) now() time.Time {
	return c.at
}

// sourceDirtyFanout builds a fan-out selecting only source_dirty over the
// webhook URL the caller has already put in the environment, which is what a
// [notify.discord] events list naming that event produces.
func sourceDirtyFanout(t *testing.T) *notify.Fanout {
	t.Helper()

	cfg := config.Default()
	cfg.Notify.Discord.Events = []string{config.NotifyEventSourceDirty}

	fanout := cli.BuildFanout(&cfg, nil, slog.New(slog.DiscardHandler))
	if fanout == nil {
		t.Fatal("cli.BuildFanout returned nil with the webhook env set")
	}

	return fanout
}

// startSourceNotifier starts the drainer and builds a notifier on a fixed clock.
// The fan-out is left for the case to hand over, since when that happens is
// itself one of the things under test.
func startSourceNotifier(t *testing.T, fanout *notify.Fanout) (*cli.SourceDirtyNotifier, *testClock, context.Context) {
	t.Helper()

	ctx := drainFanout(t, fanout)
	clock := &testClock{at: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)}

	return cli.NewSourceDirtyNotifier(clock.now), clock, ctx
}

// waitForSkipLine returns the next posted line or fails the case.
func waitForSkipLine(t *testing.T, bodies <-chan string) string {
	t.Helper()

	select {
	case line := <-bodies:
		return line
	case <-time.After(notifyPostWait):
		t.Fatal("timed out waiting for a source_dirty line to post")

		return ""
	}
}

// TestSourceDirtyLineNamesHostSourceAndFix covers FR9's wording: an operator
// reading one line in a shared channel has to learn which box, which source,
// what is wrong, and what to do about it.
func TestSourceDirtyLineNamesHostSourceAndFix(t *testing.T) {
	// t.Setenv forbids t.Parallel; the webhook env is process-global.
	server, bodies := fanoutCaptureServer(t)
	t.Setenv(cli.NotifyDiscordWebhookEnv, server.URL)

	fanout := sourceDirtyFanout(t)
	notifier, _, ctx := startSourceNotifier(t, fanout)

	notifier.SetFanout(fanout)
	notifier.Skipped(ctx, notifySourceName, notifySourceDest, notifySkipReason)

	line := waitForSkipLine(t, bodies)

	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("read the hostname the notifier also reads: %v", err)
	}

	for _, want := range []string{host, notifySourceName, notifySourceDest, notifySkipReason, "dirty_reset = true"} {
		if !strings.Contains(line, want) {
			t.Fatalf("line %q does not name %q", line, want)
		}
	}
}

// TestSourceDirtyNotifierHoldsItsWindow covers the rate limit FR9 asks for: a
// tree skipped every five minutes is worth one line, not 288 a day.
func TestSourceDirtyNotifierHoldsItsWindow(t *testing.T) {
	server, bodies := fanoutCaptureServer(t)
	t.Setenv(cli.NotifyDiscordWebhookEnv, server.URL)

	fanout := sourceDirtyFanout(t)
	notifier, clock, ctx := startSourceNotifier(t, fanout)

	notifier.SetFanout(fanout)
	notifier.Skipped(ctx, notifySourceName, notifySourceDest, notifySkipReason)
	waitForSkipLine(t, bodies)

	clock.at = clock.at.Add(11 * time.Hour)

	notifier.Skipped(ctx, notifySourceName, notifySourceDest, notifySkipReason)
	assertNoSecondPost(t, bodies)

	clock.at = clock.at.Add(2 * time.Hour)

	notifier.Skipped(ctx, notifySourceName, notifySourceDest, notifySkipReason)

	if line := waitForSkipLine(t, bodies); !strings.Contains(line, notifySourceName) {
		t.Fatalf("line past the window = %q, want the source named again", line)
	}
}

// TestSourceDirtyNotifierReportsAgainAfterAGoodSync is the other half of FR9: a
// tree that is fixed and dirtied again inside the window is news, so the clean
// sync in between has to clear the silence.
func TestSourceDirtyNotifierReportsAgainAfterAGoodSync(t *testing.T) {
	server, bodies := fanoutCaptureServer(t)
	t.Setenv(cli.NotifyDiscordWebhookEnv, server.URL)

	fanout := sourceDirtyFanout(t)
	notifier, clock, ctx := startSourceNotifier(t, fanout)

	notifier.SetFanout(fanout)
	notifier.Skipped(ctx, notifySourceName, notifySourceDest, notifySkipReason)
	waitForSkipLine(t, bodies)

	notifier.Synced(notifySourceName)

	clock.at = clock.at.Add(time.Minute)

	notifier.Skipped(ctx, notifySourceName, notifySourceDest, notifySkipReason)

	if line := waitForSkipLine(t, bodies); !strings.Contains(line, notifySkipReason) {
		t.Fatalf("line after a clean sync = %q, want the reason named again", line)
	}
}

// TestSourceDirtyNotifierIsQuietBeforeItsFanout covers the startup gap serve
// leaves between building the registry and building the fan-out. A skip in that
// window posts nothing, and must not spend the source's next 12 hours either.
func TestSourceDirtyNotifierIsQuietBeforeItsFanout(t *testing.T) {
	server, bodies := fanoutCaptureServer(t)
	t.Setenv(cli.NotifyDiscordWebhookEnv, server.URL)

	fanout := sourceDirtyFanout(t)
	notifier, _, ctx := startSourceNotifier(t, fanout)

	notifier.Skipped(ctx, notifySourceName, notifySourceDest, notifySkipReason)
	assertNoSecondPost(t, bodies)

	notifier.SetFanout(fanout)
	notifier.Skipped(ctx, notifySourceName, notifySourceDest, notifySkipReason)

	if line := waitForSkipLine(t, bodies); !strings.Contains(line, notifySourceName) {
		t.Fatalf("first line after the fan-out arrived = %q, want the source named", line)
	}
}
