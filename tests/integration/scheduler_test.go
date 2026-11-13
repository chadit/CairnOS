//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"github.com/chadit/CairnOS/internal/scheduler"
	"github.com/chadit/CairnOS/internal/scheduler/schedulertest"
	"github.com/chadit/CairnOS/tests/integration"
)

// openSchedulerPool dials a fresh pool against the shared container. A session
// advisory lock is connection-scoped, so each coordinator under test needs its
// own pool.
func openSchedulerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	pool, err := pgxpool.New(t.Context(), integration.DSN(t))
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}

	t.Cleanup(pool.Close)

	return pool
}

// newPostgresQueue opens a work queue in its own schema so parallel conformance
// subtests never share one work_item table. The schema is a fresh lowercased
// ULID (a safe identifier that starts with a letter); cleanup drops it.
func newPostgresQueue(t *testing.T) *scheduler.PostgresQueue {
	t.Helper()

	ctx := t.Context()
	schema := "cairnos_queue_test_" + strings.ToLower(ulid.Make().String())

	cfg, err := pgxpool.ParseConfig(integration.DSN(t))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}

	// search_path pins the unqualified work_item table to this test's schema, so
	// NewPostgresQueue creates and queries it there.
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}

	if _, execErr := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); execErr != nil {
		pool.Close()
		t.Fatalf("create schema: %v", execErr)
	}

	queue, err := scheduler.NewPostgresQueue(ctx, pool)
	if err != nil {
		pool.Close()
		t.Fatalf("new postgres queue: %v", err)
	}

	t.Cleanup(func() {
		dropCtx := context.WithoutCancel(ctx)

		if _, dropErr := pool.Exec(dropCtx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); dropErr != nil {
			t.Errorf("drop test schema: %v", dropErr)
		}

		pool.Close()
	})

	return queue
}

// TestSchedulerQueueConformance runs the shared work-queue contract against a
// live Postgres, the backend counterpart to the sqlite unit test. The poll
// cursor cases ride the same suite, so both backends are held to one contract.
func TestSchedulerQueueConformance(t *testing.T) {
	t.Parallel()

	schedulertest.RunWorkQueueConformance(t, newPostgresQueue)
}

// TestSchedulerPollCursorSurvivesReopen proves the Postgres poll cursor is
// durable across a pool close/reopen against the same schema, the backend
// counterpart to the sqlite reopen test: a daemon restart re-polls from where it
// left off instead of backfilling.
func TestSchedulerPollCursorSurvivesReopen(t *testing.T) {
	t.Parallel()

	const (
		scheduleID = "sched-reopen"
		cursor     = "2026-06-13T12:00:00Z"
	)

	ctx := t.Context()
	schema := "cairnos_cursor_test_" + strings.ToLower(ulid.Make().String())

	cfg, err := pgxpool.ParseConfig(integration.DSN(t))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}

	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"

	setupPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open setup pool: %v", err)
	}

	if _, innerErr := setupPool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); innerErr != nil {
		setupPool.Close()
		t.Fatalf("create schema: %v", innerErr)
	}

	setupPool.Close()

	t.Cleanup(func() {
		dropCtx := context.WithoutCancel(ctx)

		dropPool, dropErr := pgxpool.NewWithConfig(dropCtx, cfg)
		if dropErr != nil {
			t.Errorf("open drop pool: %v", dropErr)

			return
		}
		defer dropPool.Close()

		if _, execErr := dropPool.Exec(dropCtx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); execErr != nil {
			t.Errorf("drop test schema: %v", execErr)
		}
	})

	firstPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open first pool: %v", err)
	}

	first, err := scheduler.NewPostgresQueue(ctx, firstPool)
	if err != nil {
		firstPool.Close()
		t.Fatalf("new first queue: %v", err)
	}

	if setCursorErr := first.SetCursor(ctx, scheduleID, cursor); setCursorErr != nil {
		firstPool.Close()
		t.Fatalf("set cursor: %v", setCursorErr)
	}

	firstPool.Close()

	secondPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("reopen pool: %v", err)
	}
	defer secondPool.Close()

	second, err := scheduler.NewPostgresQueue(ctx, secondPool)
	if err != nil {
		t.Fatalf("new second queue: %v", err)
	}

	got, err := second.GetCursor(ctx, scheduleID)
	if err != nil {
		t.Fatalf("get cursor after reopen: %v", err)
	}

	if got != cursor {
		t.Errorf("cursor after reopen = %q, want %q", got, cursor)
	}
}

// TestSchedulerCoordinatorContendForLeadership races two coordinators against the
// same database for the lock; exactly one wins. After the winner releases, the
// loser can take leadership, which proves the lock is released, not leaked.
func TestSchedulerCoordinatorContendForLeadership(t *testing.T) {
	t.Parallel()

	// Two separate pools stand in for two nodes: a session advisory lock is
	// connection-scoped, so each coordinator must hold its own connection.
	poolA := openSchedulerPool(t)
	poolB := openSchedulerPool(t)
	ctx := t.Context()

	coordA := scheduler.NewPostgresCoordinator(poolA)
	coordB := scheduler.NewPostgresCoordinator(poolB)

	leaderA, err := coordA.Leader(ctx)
	if err != nil {
		t.Fatalf("coordinator A leader: %v", err)
	}

	leaderB, err := coordB.Leader(ctx)
	if err != nil {
		t.Fatalf("coordinator B leader: %v", err)
	}

	if leaderA == leaderB {
		t.Fatalf("both coordinators reported leader=%t, want exactly one leader", leaderA)
	}

	winner, loser := coordA, coordB
	if leaderB {
		winner, loser = coordB, coordA
	}

	// The leader stays the leader across repeated calls; the held connection
	// keeps the session lock alive.
	stillLeader, err := winner.Leader(ctx)
	if err != nil {
		t.Fatalf("winner re-check leader: %v", err)
	}

	if !stillLeader {
		t.Fatal("winner lost leadership on a repeat Leader call")
	}

	// The follower stays a follower while the winner holds the lock.
	stillFollower, err := loser.Leader(ctx)
	if err != nil {
		t.Fatalf("loser re-check leader: %v", err)
	}

	if stillFollower {
		t.Fatal("follower became leader while the winner still held the lock")
	}

	// The winner releasing the lock is what lets the follower take over: the
	// failover path with no lease timer.
	if closeErr := winner.Close(ctx); closeErr != nil {
		t.Fatalf("winner close: %v", closeErr)
	}

	promoted, err := loser.Leader(ctx)
	if err != nil {
		t.Fatalf("loser leader after winner released: %v", err)
	}

	if !promoted {
		t.Fatal("follower did not acquire the lock after the leader released it")
	}

	if err := loser.Close(ctx); err != nil {
		t.Fatalf("loser close: %v", err)
	}
}

// TestSchedulerCoordinatorsDistinctKeysElectIndependently proves the
// parameterized key: two coordinators on different advisory-lock keys both win at
// once, so the hive primary and the scheduler leader elect independently over the
// same database. The high-byte keys differ from the default so this runs
// alongside the contention test against one database.
func TestSchedulerCoordinatorsDistinctKeysElectIndependently(t *testing.T) {
	t.Parallel()

	const (
		keyA int64 = 0x43524e53_70000001
		keyB int64 = 0x43524e53_70000002
	)

	pool := openSchedulerPool(t)
	ctx := t.Context()

	coordA := scheduler.NewPostgresCoordinatorWithKey(pool, keyA)
	coordB := scheduler.NewPostgresCoordinatorWithKey(pool, keyB)

	leaderA, err := coordA.Leader(ctx)
	if err != nil {
		t.Fatalf("coordinator A leader: %v", err)
	}

	leaderB, err := coordB.Leader(ctx)
	if err != nil {
		t.Fatalf("coordinator B leader: %v", err)
	}

	if !leaderA || !leaderB {
		t.Fatalf("distinct-key coordinators leaderA=%t leaderB=%t, want both true (independent locks)", leaderA, leaderB)
	}

	if err := coordA.Close(ctx); err != nil {
		t.Fatalf("close A: %v", err)
	}

	if err := coordB.Close(ctx); err != nil {
		t.Fatalf("close B: %v", err)
	}
}

// TestSchedulerCoordinatorCloseWithoutLeadershipIsSafe checks Close on a
// coordinator that never led is a no-op, not a panic or error.
func TestSchedulerCoordinatorCloseWithoutLeadershipIsSafe(t *testing.T) {
	t.Parallel()

	pool := openSchedulerPool(t)

	coord := scheduler.NewPostgresCoordinator(pool)

	if err := coord.Close(t.Context()); err != nil {
		t.Fatalf("close without leadership = %v, want nil", err)
	}
}
