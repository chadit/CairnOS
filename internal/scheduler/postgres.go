package scheduler

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// advisoryLockKey is the fixed 64-bit key every CairnOS node uses for the
// single-scheduler advisory lock (.agents/spec/14-open-decisions.md, S2). It
// is an arbitrary constant; what matters is that every node agrees on it, so
// at most one node holds the lock at a time. Postgres session advisory locks
// share one key space across the whole database, so this value just has to
// not collide with another application's advisory lock on the same database.
// "CRNS" in ASCII as the high bytes keeps it recognizable in pg_locks.
const advisoryLockKey int64 = 0x43524e53_00000001

// PostgresCoordinator gates scheduling so exactly one node fires schedules at
// a time, the multi-node leader election from
// .agents/spec/11-deployment-and-config.md ("Scaling") and
// .agents/spec/14-open-decisions.md (S2). It takes a Postgres session-scoped
// advisory lock (pg_try_advisory_lock) on advisoryLockKey: whichever node
// holds the lock is the leader, the rest are followers that fire nothing.
//
// The lock is connection-scoped, not transaction-scoped, so the coordinator
// holds one dedicated pooled connection for the lock's whole lifetime.
// Releasing that connection (or losing it to a network partition or a node
// crash) drops the lock, and another node's next Leader call acquires it.
// That is the failover path: no heartbeat, no lease timer, just Postgres
// releasing a dead session's locks.
type PostgresCoordinator struct {
	pool *pgxpool.Pool

	// mu guards conn and held so concurrent Leader and Close calls do not
	// race on the dedicated connection.
	mu sync.Mutex
	// conn is the dedicated connection that owns the advisory lock while this
	// node is the leader; nil when this node does not hold the lock.
	conn *pgxpool.Conn
	// held mirrors conn != nil for readability at the call sites.
	held bool
}

// NewPostgresCoordinator builds a coordinator over an existing pool. The pool
// is the same one the store uses; the coordinator borrows one connection from
// it while it is the leader. The caller owns the pool's lifetime; Close here
// only releases the lock connection, never the pool.
func NewPostgresCoordinator(pool *pgxpool.Pool) *PostgresCoordinator {
	return &PostgresCoordinator{pool: pool}
}

// Leader reports whether this node may fire schedules right now. It is the
// Coordinator contract the daemon calls before every fire. When this node
// already holds the lock, Leader confirms the connection is still alive and
// returns true. Otherwise it borrows a connection and tries the lock: a
// successful pg_try_advisory_lock makes this node the leader, a failed one
// leaves it a follower. pg_try_advisory_lock never blocks, so a follower's
// Leader call returns at once.
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

	_, unlockErr := c.conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", advisoryLockKey)
	c.dropConn()

	if unlockErr != nil {
		return fmt.Errorf("postgres coordinator: advisory unlock: %w", unlockErr)
	}

	return nil
}

// confirmStillLeader checks the held lock connection is still usable. A
// dropped connection (partition, server restart) means the lock is gone, so
// it releases the dead handle and reports follower; the next Leader call
// retries the acquire. Holding the connection across calls is what keeps the
// session-scoped lock alive between fires.
// confirmStillLeader reports whether the held lock is still good. A dead
// connection is a lost lock, not a hard failure: it returns false so the
// daemon simply skips this pass, and a later Leader call re-acquires.
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

	if scanErr := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", advisoryLockKey).Scan(&locked); scanErr != nil {
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
