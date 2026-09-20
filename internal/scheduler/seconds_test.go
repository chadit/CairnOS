package scheduler_test

import (
	"errors"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/chadit/CairnOS/internal/scheduler"
)

// Sub-minute fixtures: the cadence these tests are about, and the gap it
// promises between fires.
const (
	thirtySecCron     = "*/30 * * * * *"
	thirtySecInterval = 30 * time.Second
	fastScheduleID    = "fast"
)

// TestNextFireHonorsSecondsField pins the six-field surface: a leading seconds
// field lands fires inside a minute, and the five-field forms it sits in front
// of keep the second-zero meaning they have always had.
func TestNextFireHonorsSecondsField(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, 3, 10, 10, 30, 15, 0, time.UTC)

	cases := []struct {
		want time.Time
		name string
		expr string
	}{
		{
			name: "thirty second step fires on the next half minute",
			expr: thirtySecCron,
			want: time.Date(2026, 3, 10, 10, 30, 30, 0, time.UTC),
		},
		{
			name: "exact seconds value waits for that second",
			expr: "45 * * * * *",
			want: time.Date(2026, 3, 10, 10, 30, 45, 0, time.UTC),
		},
		{
			name: "six field form with a minute step still fires on second zero",
			expr: "0 */5 * * * *",
			want: time.Date(2026, 3, 10, 10, 35, 0, 0, time.UTC),
		},
		{
			name: "five field form is unchanged by the optional seconds field",
			expr: fiveMinCron,
			want: time.Date(2026, 3, 10, 10, 35, 0, 0, time.UTC),
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := scheduler.NextFire(tt.expr, from)
			if err != nil {
				t.Fatalf("NextFire(%q) = %v", tt.expr, err)
			}

			if !got.Equal(tt.want) {
				t.Fatalf("NextFire(%q) = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}
}

// TestNextFireStepsThirtySecondsApart walks consecutive fires rather than one,
// because a cadence that drifts only shows up between fires.
func TestNextFireStepsThirtySecondsApart(t *testing.T) {
	t.Parallel()

	mark := time.Date(2026, 3, 10, 10, 30, 0, 0, time.UTC)

	for range 4 {
		next, err := scheduler.NextFire(thirtySecCron, mark)
		if err != nil {
			t.Fatalf("NextFire(%q) = %v", thirtySecCron, err)
		}

		if gap := next.Sub(mark); gap != thirtySecInterval {
			t.Fatalf("fire after %v came %s later, want %s", mark, gap, thirtySecInterval)
		}

		mark = next
	}
}

// TestValidateCronRejectsRunawayRates covers the floor that makes the seconds
// field safe to accept, and checks the report names the rate so an operator can
// see what was wrong with the expression.
func TestValidateCronRejectsRunawayRates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		expr string
	}{
		{name: "every second", expr: "* * * * * *"},
		{name: "every five seconds", expr: "*/5 * * * * *"},
		{name: "two seconds one apart", expr: "0,1 * * * * *"},
		{name: "tight pair inside an hourly window", expr: "0,1 0 * * * *"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := scheduler.ValidateCron(tt.expr)
			if !errors.Is(err, scheduler.ErrTooFrequent) {
				t.Fatalf("ValidateCron(%q) = %v, want ErrTooFrequent", tt.expr, err)
			}
		})
	}
}

// TestValidateCronAcceptsRatesAtOrAboveTheFloor guards the other side of the
// floor: a ten-second cadence is the fastest allowed and a schedule that merely
// has two fires in one minute is ordinary.
func TestValidateCronAcceptsRatesAtOrAboveTheFloor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		expr string
	}{
		{name: "exactly the floor", expr: "*/10 * * * * *"},
		{name: "thirty seconds", expr: thirtySecCron},
		{name: "once a minute on a late second", expr: "1 * * * * *"},
		{name: "five field expression", expr: fiveMinCron},
		{name: "shorthand", expr: hourlyCron},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := scheduler.ValidateCron(tt.expr); err != nil {
				t.Fatalf("ValidateCron(%q) = %v, want nil", tt.expr, err)
			}
		})
	}
}

// TestDaemonFiresTwiceInsideOneMinute is the end-to-end cadence check: the loop
// has to sleep to the half-minute boundary rather than to its poll interval,
// which is what a 30-second schedule needs and what the minute-capped sleep
// could not give it.
func TestDaemonFiresTwiceInsideOneMinute(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		store := newMemStore()
		runner := &recordRunner{}
		start := time.Now()

		first, err := scheduler.NextFire(thirtySecCron, start)
		if err != nil {
			t.Fatalf("first fire: %v", err)
		}

		if _, addErr := store.Add(t.Context(), &scheduler.Schedule{
			ID: fastScheduleID, WorkflowName: flowName, Cron: thirtySecCron, NextFire: first,
		}); addErr != nil {
			t.Fatalf("seed store: %v", addErr)
		}

		stop := startDaemon(t, scheduler.New(scheduler.Config{Store: store, Runner: runner}))
		defer stop()

		synctest.Sleep(first.Sub(start) + thirtySecInterval + settleSleep)

		fires := runner.times()
		if len(fires) != 2 {
			t.Fatalf("fired %d times in the minute after the first boundary, want 2", len(fires))
		}

		if gap := fires[1].Sub(fires[0]); gap != thirtySecInterval {
			t.Fatalf("fires landed %s apart, want %s", gap, thirtySecInterval)
		}
	})
}

// TestDaemonCatchUpStaysOneFireForSubMinuteSchedule checks the downtime policy
// at the cadence that makes it matter: an hour behind is 120 occurrences for a
// 30-second schedule, and the daemon owes exactly one of them.
func TestDaemonCatchUpStaysOneFireForSubMinuteSchedule(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		store := newMemStore()
		runner := &recordRunner{}
		now := time.Now()

		if _, err := store.Add(t.Context(), &scheduler.Schedule{
			ID: "behind", WorkflowName: flowName, Cron: thirtySecCron,
			NextFire: now.Add(-hourInterval),
		}); err != nil {
			t.Fatalf("seed store: %v", err)
		}

		stop := startDaemon(t, scheduler.New(scheduler.Config{Store: store, Runner: runner}))
		defer stop()

		synctest.Sleep(settleSleep)

		if got := runner.count(); got != 1 {
			t.Fatalf("an hour of missed occurrences fired %d times, want exactly 1", got)
		}

		stamped := store.get(t, "behind")

		wantNext, err := scheduler.NextFire(thirtySecCron, now)
		if err != nil {
			t.Fatalf("next fire: %v", err)
		}

		if !stamped.NextFire.Equal(wantNext) {
			t.Fatalf("next_fire = %v, want recomputed-from-now %v", stamped.NextFire, wantNext)
		}
	})
}

// TestQueueKeepsBothFiresInsideOneMinute drives the real store, because a key
// or a column that rounded to the minute would silently merge two occurrences
// into one queued run and the daemon would never notice.
func TestQueueKeepsBothFiresInsideOneMinute(t *testing.T) {
	t.Parallel()

	queue, err := scheduler.OpenSQLiteQueue(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("open sqlite queue: %v", err)
	}

	t.Cleanup(func() { _ = queue.Close() })

	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)

	for _, fire := range []time.Time{base, base.Add(thirtySecInterval)} {
		inserted, enqErr := queue.Enqueue(t.Context(), &scheduler.WorkItem{
			DedupKey: scheduler.DedupKeyFor(fastScheduleID, fire),
			Payload:  []byte(`{"workflow":"hello"}`),
		})
		if enqErr != nil {
			t.Fatalf("enqueue fire %v: %v", fire, enqErr)
		}

		if !inserted {
			t.Fatalf("fire %v did not land its own row", fire)
		}
	}

	stats, err := queue.Stats(t.Context(), base.Add(time.Minute))
	if err != nil {
		t.Fatalf("stats: %v", err)
	}

	if stats.Ready != 2 {
		t.Fatalf("ready = %d after two fires inside one minute, want 2", stats.Ready)
	}
}
