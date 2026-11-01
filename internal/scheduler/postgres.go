package scheduler

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// advisoryLockKey is the fixed 64-bit key every CairnOS node uses for the
// single-scheduler advisory lock (S2). The value is arbitrary; what matters is
// that every node agrees on it. Postgres advisory locks share one key space per
// database, so it also has to miss another application's key. "CRNS" in ASCII
// keeps it recognizable in pg_locks.
const advisoryLockKey int64 = 0x43524e53_00000001

// PostgresCoordinator gates scheduling so exactly one node fires schedules at a
// time (S2). The node holding the pg_try_advisory_lock on advisoryLockKey
// leads; the rest fire nothing.
//
// The lock is connection-scoped, so the coordinator holds a dedicated
// connection while it leads. Losing it drops the lock: no heartbeat, no lease
// timer, just Postgres releasing a dead session's locks.
type PostgresCoordinator struct {
	pool *pgxpool.Pool

	// conn is the dedicated connection that owns the advisory lock while this
	// node is the leader; nil when this node does not hold the lock.
	conn *pgxpool.Conn

	// lockKey is the 64-bit advisory-lock key this coordinator elects on.
	// Different roles (the scheduler leader, the hive primary) pass different
	// keys so they elect independently: one node can be the scheduler leader
	// and another the hive primary.
	lockKey int64

	// mu guards conn and held so concurrent Leader and Close calls do not
	// race on the dedicated connection.
	mu sync.Mutex

	// held mirrors conn != nil for readability at the call sites.
	held bool
}

// NewPostgresCoordinator builds a coordinator over an existing pool, electing
// on the default scheduler lock key. The pool is the same one the store uses;
// the coordinator borrows one connection from it while it is the leader. The
// caller owns the pool's lifetime; Close here only releases the lock
// connection, never the pool.
func NewPostgresCoordinator(pool *pgxpool.Pool) *PostgresCoordinator {
	return NewPostgresCoordinatorWithKey(pool, advisoryLockKey)
}

// NewPostgresCoordinatorWithKey builds a coordinator that elects on a caller-
// chosen advisory-lock key, so a second role (the hive primary) can elect
// independently of the scheduler leader over the same database.
func NewPostgresCoordinatorWithKey(pool *pgxpool.Pool, lockKey int64) *PostgresCoordinator {
	return &PostgresCoordinator{pool: pool, lockKey: lockKey}
}

// Leader reports whether this node may fire schedules right now, the
// Coordinator contract the daemon calls before every fire. When this node
// already holds the lock, Leader confirms the connection is still alive.
// Otherwise it borrows a connection and tries the lock. pg_try_advisory_lock
// never blocks, so a follower's Leader call returns at once.
func (c *PostgresCoordinator) Leader(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("postgres coordinator: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.held {
		return c.confirmStillLeader(ctx), nil
	}

	return c.tryAcquire(ctx)
}

// Close releases the advisory lock and returns the dedicated connection to
// the pool, so another node can take leadership promptly instead of waiting
// for this node's session to time out. It is safe to call when this node is
// not the leader. The pool itself is the caller's to close.
func (c *PostgresCoordinator) Close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.held {
		return nil
	}

	_, unlockErr := c.conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", c.lockKey)
	c.dropConn()

	if unlockErr != nil {
		return fmt.Errorf("postgres coordinator: advisory unlock: %w", unlockErr)
	}

	return nil
}

// confirmStillLeader reports whether the held lock is still good. A dead
// connection (partition, server restart) means the lock is gone, not a hard
// failure: it drops the dead handle and returns false so the daemon skips this
// pass, and a later Leader call re-acquires. Holding the connection across
// calls is what keeps the session-scoped lock alive between fires.
func (c *PostgresCoordinator) confirmStillLeader(ctx context.Context) bool {
	if _, err := c.conn.Exec(ctx, "SELECT 1"); err != nil {
		c.dropConn()

		return false
	}

	return true
}

// tryAcquire borrows a connection and attempts the advisory lock on it. On
// success the connection is retained so the session-scoped lock survives
// between Leader calls; on failure the connection goes back to the pool and
// this node stays a follower.
func (c *PostgresCoordinator) tryAcquire(ctx context.Context) (bool, error) {
	conn, err := c.pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("postgres coordinator: acquire connection: %w", err)
	}

	var locked bool

	if scanErr := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", c.lockKey).Scan(&locked); scanErr != nil {
		conn.Release()

		return false, fmt.Errorf("postgres coordinator: try advisory lock: %w", scanErr)
	}

	if !locked {
		conn.Release()

		return false, nil
	}

	c.conn = conn
	c.held = true

	return true, nil
}

// dropConn returns the lock connection to the pool and clears the held flag.
// A connection that already errored is released so the pool can discard it;
// the caller holds c.mu.
func (c *PostgresCoordinator) dropConn() {
	if c.conn != nil {
		c.conn.Release()
		c.conn = nil
	}

	c.held = false
}

// compile-time assertion that PostgresCoordinator implements Coordinator.
var _ Coordinator = (*PostgresCoordinator)(nil)
