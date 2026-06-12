// Package scheduler runs workflows on cron schedules
// (.agents/spec/11-deployment-and-config.md, "Scheduler (Phase 0b)").
// Schedules persist in SQLite so they survive restarts, next-fire math goes
// through robfig/cron, and the daemon is single-node but fires through a
// Coordinator interface so the Phase 4 leader lock drops in without a
// rewrite (.agents/spec/14-open-decisions.md, S2).
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// pollInterval caps how long the daemon sleeps between store reads, so a
// schedule added by another process (the CLI writing to the shared SQLite
// file) is noticed without a restart.
const pollInterval = time.Minute

// backlogCap bounds the missed-occurrence count in the skip log line; past
// a thousand the exact number stops being interesting.
const backlogCap = 1000

// Schedule is one persisted scheduled workflow. A zero LastFired means the
// schedule has never fired.
type Schedule struct {
	ID           string
	WorkflowPath string
	WorkflowName string
	Cron         string
	LastFired    time.Time
	NextFire     time.Time
	CreatedAt    time.Time
}

// Schedules is the persistence surface the daemon and the CLI share. The
// Phase 0b implementation is SQLite; a Postgres-backed one arrives with
// multi-node scaling.
type Schedules interface {
	// Add validates the schedule's cron expression, stamps id, next_fire,
	// and created_at, and persists it, returning the id.
	Add(ctx context.Context, sched *Schedule) (string, error)

	// List returns every schedule, oldest first.
	List(ctx context.Context) ([]Schedule, error)

	// Remove deletes schedules matching the id or workflow name, reporting
	// ErrScheduleNotFound when nothing matched.
	Remove(ctx context.Context, idOrName string) error

	// MarkFired stamps one schedule's last_fired and next_fire after a fire.
	MarkFired(ctx context.Context, id string, lastFired, nextFire time.Time) error
}

// Runner fires one scheduled workflow. The CLI wires this to workflow.Load
// plus engine.RunWorkflow; daemon tests substitute a recording fake.
type Runner interface {
	Fire(ctx context.Context, sched *Schedule) error
}

// Coordinator decides whether this node may fire schedules right now. Phase
// 0b is single-node, so SoloCoordinator is the only implementation; the
// Phase 4 Postgres advisory lock implements the same method
// (.agents/spec/14-open-decisions.md, S2).
type Coordinator interface {
	Leader(ctx context.Context) (bool, error)
}

// SoloCoordinator is the single-node Coordinator: always the leader.
type SoloCoordinator struct{}

// Leader always reports true; one node owns all firing. The ctx check keeps
// a canceled daemon from firing on its way out.
func (SoloCoordinator) Leader(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("solo coordinator: %w", err)
	}

	return true, nil
}

// Config wires one Scheduler.
type Config struct {
	// Store persists schedules; required.
	Store Schedules

	// Runner fires due workflows; required.
	Runner Runner

	// Coordinator gates firing; nil means SoloCoordinator.
	Coordinator Coordinator

	// Clock supplies the current time. Nil means time.Now; tests inject a
	// fake to drive fire decisions without real waiting.
	Clock func() time.Time

	// Logger receives fire, skip, and error events; nil means slog.Default.
	Logger *slog.Logger
}

// Scheduler is the Phase 0b daemon: it watches the schedules table, sleeps
// until the earliest next_fire, fires due workflows once each, and stamps
// the timestamps. Fire errors are logged and never stop the loop.
type Scheduler struct {
	store  Schedules
	runner Runner
	coord  Coordinator
	clock  func() time.Time
	logger *slog.Logger
}

// New builds a Scheduler, filling Config defaults.
func New(cfg Config) *Scheduler {
	if cfg.Coordinator == nil {
		cfg.Coordinator = SoloCoordinator{}
	}

	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}

	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	return &Scheduler{
		store:  cfg.Store,
		runner: cfg.Runner,
		coord:  cfg.Coordinator,
		clock:  cfg.Clock,
		logger: cfg.Logger,
	}
}

// Run drives the daemon loop until ctx is canceled, which is the clean way
// to stop it (Run returns nil). Each pass fires every due schedule once,
// then sleeps until the earliest next_fire, capped at pollInterval so
// externally added schedules are picked up.
func (s *Scheduler) Run(ctx context.Context) error {
	if s.store == nil {
		return ErrNoStore
	}

	if s.runner == nil {
		return ErrNoRunner
	}

	for {
		wait, err := s.pass(ctx)
		if err != nil {
			// Store reads failing is an environment problem, not a reason to
			// die; log and retry on the next poll.
			s.logger.Error("scheduler pass", "error", err)

			wait = pollInterval
		}

		if !sleepCtx(ctx, wait) {
			return nil
		}
	}
}

// pass fires every due schedule and returns how long to sleep before the
// next pass.
func (s *Scheduler) pass(ctx context.Context) (time.Duration, error) {
	scheds, err := s.store.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("list schedules: %w", err)
	}

	now := s.clock()
	wait := pollInterval

	for idx := range scheds {
		sched := &scheds[idx]

		if sched.NextFire.After(now) {
			if until := sched.NextFire.Sub(now); until < wait {
				wait = until
			}

			continue
		}

		s.fire(ctx, sched, now)
	}

	return wait, nil
}

// fire runs one due schedule exactly once and stamps its timestamps. A
// next_fire that fell multiple intervals behind (the process was down) does
// not fan out a burst: the backlog is logged and dropped, and next_fire
// recomputes from now (.agents/spec/14-open-decisions.md, S8).
func (s *Scheduler) fire(ctx context.Context, sched *Schedule, now time.Time) {
	leader, err := s.coord.Leader(ctx)
	if err != nil {
		s.logger.Error("scheduler leader check", "schedule", sched.ID, "error", err)

		return
	}

	if !leader {
		return
	}

	if missed := missedOccurrences(sched.Cron, sched.NextFire, now); missed > 0 {
		s.logger.Warn("skipping missed schedule occurrences; firing once",
			"schedule", sched.ID, "workflow", sched.WorkflowName, "missed", missed)
	}

	s.fireOnce(ctx, sched)

	next, err := NextFire(sched.Cron, now)
	if err != nil {
		// Unreachable for stored rows (Add validated the expression), kept
		// loud in case a row was edited by hand.
		s.logger.Error("scheduler next fire", "schedule", sched.ID, "error", err)

		return
	}

	if err := s.store.MarkFired(ctx, sched.ID, now, next); err != nil {
		s.logger.Error("scheduler mark fired", "schedule", sched.ID, "error", err)
	}
}

// fireOnce calls the runner and logs the outcome. A failing run still
// counts as fired: the caller stamps next_fire regardless, so a broken
// workflow is not re-fired every poll.
func (s *Scheduler) fireOnce(ctx context.Context, sched *Schedule) {
	if err := s.runner.Fire(ctx, sched); err != nil {
		s.logger.Error("scheduled run failed",
			"schedule", sched.ID, "workflow", sched.WorkflowName, "error", err)

		return
	}

	s.logger.Info("scheduled run fired",
		"schedule", sched.ID, "workflow", sched.WorkflowName)
}

// missedOccurrences counts fire times in (nextFire, now] beyond the first
// due one, capped at backlogCap. Zero means the schedule is merely due, not
// behind.
func missedOccurrences(cronExpr string, nextFire, now time.Time) int {
	var missed int

	mark := nextFire

	for missed < backlogCap {
		next, err := NextFire(cronExpr, mark)
		if err != nil || next.After(now) {
			break
		}

		missed++
		mark = next
	}

	return missed
}

// sleepCtx waits out wait or the context, reporting false on cancellation
// so the caller exits its loop.
func sleepCtx(ctx context.Context, wait time.Duration) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
