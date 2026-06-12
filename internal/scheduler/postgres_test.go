package scheduler_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chadit/CairnOS/internal/scheduler"
)

// pgDSNEnv names the environment variable pointing the advisory-lock tests at
// a live Postgres. Unset means skip, so the scheduler package's tests stay
// green on a machine with no Postgres (the same env-gate the store backend
// uses).
const pgDSNEnv = "CAIRNOS_TEST_POSTGRES_DSN"

// openPool dials a pool against the test DSN, skipping when none is set.
func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("%s not set; skipping postgres advisory-lock tests", pgDSNEnv)
	}

	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}

	t.Cleanup(pool.Close)

	return pool
}

// TestPostgresCoordinatorElectsSingleLeader is the contention test the spec
// asks for: two coordinators against the same database race for the lock, and
// exactly one wins. After the winner releases, the loser can take leadership,
// which proves the lock is released, not leaked.
func TestPostgresCoordinatorContendForLeadership(t *testing.T) {
	t.Parallel()

	// Two separate pools stand in for two nodes: a session advisory lock is
	// connection-scoped, so each coordinator must hold its own connection.
	poolA := openPool(t)
	poolB := openPool(t)
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
	if err := winner.Close(ctx); err != nil {
		t.Fatalf("winner close: %v", err)
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

// TestPostgresCoordinatorClosePastLeadershipIsSafe checks Close on a
// coordinator that never led is a no-op, not a panic or error.
func TestPostgresCoordinatorCloseWithoutLeadershipIsSafe(t *testing.T) {
	t.Parallel()

	pool := openPool(t)

	coord := scheduler.NewPostgresCoordinator(pool)

	if err := coord.Close(t.Context()); err != nil {
		t.Fatalf("close without leadership = %v, want nil", err)
	}
}

// TestPostgresCoordinatorRejectsCanceledContext proves Leader honors a
// canceled context instead of borrowing a connection, with no Postgres
// required.
func TestPostgresCoordinatorRejectsCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	coord := scheduler.NewPostgresCoordinator(nil)

	if _, err := coord.Leader(ctx); err == nil {
		t.Fatal("Leader with a canceled context returned no error")
	}
}
