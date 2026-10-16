package scheduler_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/chadit/CairnOS/internal/scheduler"
)

// Shared fixture values; goconst applies to tests in this repo's lint
// policy.
const (
	hourlyCron   = "@hourly"
	fiveMinCron  = "*/5 * * * *"
	flowName     = "hello"
	flowPath     = "/tmp/hello.toml"
	dbFileName   = "schedules.db"
	settleSleep  = time.Millisecond
	hourInterval = time.Hour

	// behindScheduleID names the schedule the catch-up tests leave overdue.
	behindScheduleID = "behind"
)

// errFireBoom is the failure the failing-runner tests inject.
var errFireBoom = errors.New("fire boom")

// memStore is an in-memory Schedules fake so daemon tests run inside a
// synctest bubble without SQLite's background machinery.
type memStore struct {
	scheds map[string]scheduler.Schedule
	mu     sync.Mutex
}

func newMemStore() *memStore {
	return &memStore{scheds: map[string]scheduler.Schedule{}}
}

func (m *memStore) Add(_ context.Context, sched *scheduler.Schedule) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.scheds[sched.ID] = *sched

	return sched.ID, nil
}

func (m *memStore) List(_ context.Context) ([]scheduler.Schedule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]scheduler.Schedule, 0, len(m.scheds))
	for id := range m.scheds {
		out = append(out, m.scheds[id])
	}

	return out, nil
}

func (m *memStore) Remove(_ context.Context, idOrName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.scheds[idOrName]; !ok {
		return scheduler.ErrScheduleNotFound
	}

	delete(m.scheds, idOrName)

	return nil
}

func (m *memStore) MarkFired(_ context.Context, id string, lastFired, nextFire time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	sched, ok := m.scheds[id]
	if !ok {
		return scheduler.ErrScheduleNotFound
	}

	sched.LastFired = lastFired
	sched.NextFire = nextFire
	m.scheds[id] = sched

	return nil
}

func (m *memStore) get(t *testing.T, id string) scheduler.Schedule {
	t.Helper()

	m.mu.Lock()
	defer m.mu.Unlock()

	sched, ok := m.scheds[id]
	if !ok {
		t.Fatalf("schedule %s missing from fake store", id)
	}

	return sched
}

// recordRunner records every Fire call, with the instant it happened so cadence
// tests can measure the gap between fires, and optionally fails them all.
type recordRunner struct {
	fireErr error
	fired   []string
	firedAt []time.Time
	mu      sync.Mutex
}

func (r *recordRunner) Fire(_ context.Context, sched *scheduler.Schedule) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.fired = append(r.fired, sched.ID)
	r.firedAt = append(r.firedAt, time.Now())

	return r.fireErr
}

// times returns a copy of the instants Fire ran at.
func (r *recordRunner) times() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.firedAt)
}

func (r *recordRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.fired)
}

// startDaemon launches sched.Run and returns a stop function that cancels
// it and asserts the clean (nil) exit the loop promises on cancellation.
func startDaemon(t *testing.T, sched *scheduler.Scheduler) func() {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() {
		done <- sched.Run(ctx)
	}()

	return func() {
		cancel()

		if err := <-done; err != nil {
			t.Errorf("Run returned %v on cancel, want nil", err)
		}
	}
}

func TestNextFireMath(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, 3, 10, 10, 30, 0, 0, time.UTC) // a Tuesday

	cases := []struct {
		want time.Time
		name string
		expr string
	}{
		{
			name: "hourly shorthand fires at the next top of hour",
			expr: hourlyCron,
			want: time.Date(2026, 3, 10, 11, 0, 0, 0, time.UTC),
		},
		{
			name: "daily shorthand fires at the next midnight",
			expr: "@daily",
			want: time.Date(2026, 3, 11, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "weekly shorthand fires the next Sunday midnight",
			expr: "@weekly",
			want: time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "monthly shorthand fires the first of next month",
			expr: "@monthly",
			want: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "five field step expression fires on the next boundary",
			expr: fiveMinCron,
			want: time.Date(2026, 3, 10, 10, 35, 0, 0, time.UTC),
		},
		{
			name: "exact five field expression fires tomorrow when today passed",
			expr: "15 9 * * *",
			want: time.Date(2026, 3, 11, 9, 15, 0, 0, time.UTC),
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

func TestValidateCronRejectsBadExpressions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		expr string
	}{
		{name: "unsupported shorthand", expr: "@yearly"},
		{name: "free text", expr: "every five minutes"},
		{name: "four fields", expr: "* * * *"},
		{name: "seven fields", expr: "* * * * * * *"},
		{name: "seconds out of range", expr: "75 * * * * *"},
		{name: "seconds step out of range", expr: "*/75 * * * * *"},
		{name: "minute out of range", expr: "61 * * * *"},
		{name: "empty", expr: ""},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := scheduler.ValidateCron(tt.expr); !errors.Is(err, scheduler.ErrBadCron) {
				t.Fatalf("ValidateCron(%q) = %v, want ErrBadCron", tt.expr, err)
			}
		})
	}
}

// openSchedules opens a schedules store and registers its cleanup.
func openSchedules(t *testing.T, path string) *scheduler.SQLite {
	t.Helper()

	store, err := scheduler.OpenSQLite(path)
	if err != nil {
		t.Fatalf("open schedules store: %v", err)
	}

	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close schedules store: %v", err)
		}
	})

	return store
}

// addHourly persists the shared hourly fixture schedule.
func addHourly(t *testing.T, store *scheduler.SQLite) string {
	t.Helper()

	id, err := store.Add(t.Context(), &scheduler.Schedule{
		WorkflowPath: flowPath,
		WorkflowName: flowName,
		Cron:         hourlyCron,
	})
	if err != nil {
		t.Fatalf("add schedule: %v", err)
	}

	return id
}

func TestSQLiteScheduleRoundTrip(t *testing.T) {
	t.Parallel()

	store := openSchedules(t, filepath.Join(t.TempDir(), dbFileName))
	before := time.Now()
	id := addHourly(t, store)

	if id == "" {
		t.Fatal("add returned an empty id")
	}

	scheds, err := store.List(t.Context())
	if err != nil || len(scheds) != 1 {
		t.Fatalf("list = %v (%d rows), want 1 row", err, len(scheds))
	}

	sched := scheds[0]
	if sched.ID != id || sched.WorkflowPath != flowPath || sched.WorkflowName != flowName || sched.Cron != hourlyCron {
		t.Fatalf("round-tripped schedule = %+v", sched)
	}

	if !sched.LastFired.IsZero() {
		t.Fatalf("fresh schedule has last_fired %v, want zero", sched.LastFired)
	}

	if !sched.NextFire.After(before) {
		t.Fatalf("next_fire %v is not in the future of %v", sched.NextFire, before)
	}

	if err := store.Remove(t.Context(), id); err != nil {
		t.Fatalf("remove by id: %v", err)
	}

	if err := store.Remove(t.Context(), id); !errors.Is(err, scheduler.ErrScheduleNotFound) {
		t.Fatalf("second remove = %v, want ErrScheduleNotFound", err)
	}
}

func TestSQLiteMarkFiredSurvivesReopen(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), dbFileName)

	first, err := scheduler.OpenSQLite(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	id := addHourly(t, first)

	fired := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	next := fired.Add(hourInterval)

	if markFiredErr := first.MarkFired(t.Context(), id, fired, next); markFiredErr != nil {
		t.Fatalf("mark fired: %v", markFiredErr)
	}

	// Reopen the same file: restart-safe means the stamped row survives.
	if closeErr := first.Close(); closeErr != nil {
		t.Fatalf("close: %v", closeErr)
	}

	reopened := openSchedules(t, path)

	scheds, err := reopened.List(t.Context())
	if err != nil || len(scheds) != 1 {
		t.Fatalf("list after reopen = %v (%d rows), want 1 row", err, len(scheds))
	}

	if !scheds[0].LastFired.Equal(fired) || !scheds[0].NextFire.Equal(next) {
		t.Fatalf("stamps did not survive the reopen: %+v", scheds[0])
	}
}

func TestSQLiteRemoveByWorkflowName(t *testing.T) {
	t.Parallel()

	store, err := scheduler.OpenSQLite(filepath.Join(t.TempDir(), dbFileName))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Errorf("close: %v", closeErr)
		}
	})

	if _, addErr := store.Add(t.Context(), &scheduler.Schedule{
		WorkflowPath: flowPath, WorkflowName: flowName, Cron: hourlyCron,
	}); addErr != nil {
		t.Fatalf("add: %v", addErr)
	}

	if removeErr := store.Remove(t.Context(), flowName); removeErr != nil {
		t.Fatalf("remove by name: %v", removeErr)
	}

	scheds, err := store.List(t.Context())
	if err != nil || len(scheds) != 0 {
		t.Fatalf("list after remove = %v (%d rows), want none", err, len(scheds))
	}
}

func TestSQLiteAddRejectsBadCron(t *testing.T) {
	t.Parallel()

	store, err := scheduler.OpenSQLite(filepath.Join(t.TempDir(), dbFileName))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Errorf("close: %v", closeErr)
		}
	})

	_, err = store.Add(t.Context(), &scheduler.Schedule{
		WorkflowPath: flowPath, WorkflowName: flowName, Cron: "not a cron",
	})
	if !errors.Is(err, scheduler.ErrBadCron) {
		t.Fatalf("add = %v, want ErrBadCron", err)
	}

	scheds, listErr := store.List(t.Context())
	if listErr != nil || len(scheds) != 0 {
		t.Fatalf("rejected schedule persisted: %v (%d rows)", listErr, len(scheds))
	}
}

func TestDaemonFiresDueScheduleExactlyOnce(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		store := newMemStore()
		runner := &recordRunner{}
		now := time.Now()

		if _, err := store.Add(t.Context(), &scheduler.Schedule{
			ID: "due", WorkflowName: flowName, Cron: hourlyCron, NextFire: now,
		}); err != nil {
			t.Fatalf("seed store: %v", err)
		}

		stop := startDaemon(t, scheduler.New(scheduler.Config{Store: store, Runner: runner}))
		defer stop()

		synctest.Sleep(settleSleep)

		if got := runner.count(); got != 1 {
			t.Fatalf("fired %d times, want exactly 1", got)
		}

		stamped := store.get(t, "due")
		if !stamped.LastFired.Equal(now) {
			t.Fatalf("last_fired = %v, want %v", stamped.LastFired, now)
		}

		wantNext, err := scheduler.NextFire(hourlyCron, now)
		if err != nil {
			t.Fatalf("next fire: %v", err)
		}

		if !stamped.NextFire.Equal(wantNext) {
			t.Fatalf("next_fire = %v, want %v", stamped.NextFire, wantNext)
		}

		// Just before the next boundary nothing new fires; crossing it fires
		// exactly one more time. The settle sleep above already elapsed, so
		// subtract it twice to stop short of the boundary.
		synctest.Sleep(wantNext.Sub(now) - 2*settleSleep)

		if got := runner.count(); got != 1 {
			t.Fatalf("fired %d times before the boundary, want still 1", got)
		}

		synctest.Sleep(2 * settleSleep)

		if got := runner.count(); got != 2 {
			t.Fatalf("fired %d times after the boundary, want 2", got)
		}
	})
}

func TestDaemonFiresMissedScheduleOnceWithoutBacklog(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		store := newMemStore()
		runner := &recordRunner{}
		now := time.Now()

		// Three hourly intervals passed while the process was "down": the
		// spec's policy is one fire, no burst (.agents/spec/14, S8).
		if _, err := store.Add(t.Context(), &scheduler.Schedule{
			ID: behindScheduleID, WorkflowName: flowName, Cron: hourlyCron,
			NextFire: now.Add(-3 * hourInterval),
		}); err != nil {
			t.Fatalf("seed store: %v", err)
		}

		stop := startDaemon(t, scheduler.New(scheduler.Config{Store: store, Runner: runner}))
		defer stop()

		synctest.Sleep(settleSleep)

		if got := runner.count(); got != 1 {
			t.Fatalf("missed schedule fired %d times, want exactly 1", got)
		}

		stamped := store.get(t, behindScheduleID)

		wantNext, err := scheduler.NextFire(hourlyCron, now)
		if err != nil {
			t.Fatalf("next fire: %v", err)
		}

		if !stamped.NextFire.Equal(wantNext) {
			t.Fatalf("next_fire = %v, want recomputed-from-now %v", stamped.NextFire, wantNext)
		}
	})
}

func TestDaemonSurvivesFailingRuns(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		store := newMemStore()
		runner := &recordRunner{fireErr: errFireBoom}
		now := time.Now()

		if _, err := store.Add(t.Context(), &scheduler.Schedule{
			ID: "doomed", WorkflowName: flowName, Cron: hourlyCron, NextFire: now,
		}); err != nil {
			t.Fatalf("seed store: %v", err)
		}

		stop := startDaemon(t, scheduler.New(scheduler.Config{Store: store, Runner: runner}))
		defer stop()

		synctest.Sleep(settleSleep)

		if got := runner.count(); got != 1 {
			t.Fatalf("fired %d times, want 1", got)
		}

		// A failed run still stamps next_fire, so the loop neither dies nor
		// re-fires every poll; the next boundary fires again.
		synctest.Sleep(hourInterval)

		if got := runner.count(); got != 2 {
			t.Fatalf("daemon fired %d times across the next boundary, want 2 (loop alive)", got)
		}
	})
}

func TestDaemonStopsPromptlyOnCancel(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		store := newMemStore()
		runner := &recordRunner{}

		if _, err := store.Add(t.Context(), &scheduler.Schedule{
			ID: "idle", WorkflowName: flowName, Cron: hourlyCron,
			NextFire: time.Now().Add(hourInterval),
		}); err != nil {
			t.Fatalf("seed store: %v", err)
		}

		// startDaemon's stop cancels and fails the test if Run does not
		// return nil; synctest hangs the bubble if Run never returns, so
		// completing at all proves promptness.
		stop := startDaemon(t, scheduler.New(scheduler.Config{Store: store, Runner: runner}))

		synctest.Sleep(settleSleep)
		stop()

		if got := runner.count(); got != 0 {
			t.Fatalf("idle schedule fired %d times, want 0", got)
		}
	})
}

func TestRunRejectsMissingWiring(t *testing.T) {
	t.Parallel()

	cases := []struct {
		cfg  scheduler.Config
		want error
		name string
	}{
		{
			name: "missing store",
			cfg:  scheduler.Config{Runner: &recordRunner{}},
			want: scheduler.ErrNoStore,
		},
		{
			name: "missing runner",
			cfg:  scheduler.Config{Store: newMemStore()},
			want: scheduler.ErrNoRunner,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := scheduler.New(tt.cfg).Run(t.Context()); !errors.Is(err, tt.want) {
				t.Fatalf("Run = %v, want %v", err, tt.want)
			}
		})
	}
}
