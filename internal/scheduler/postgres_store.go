package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"
)

// pgSchedulesSchema is the spec's schedules table with timestamptz, plus IF NOT
// EXISTS so opening an existing database is idempotent across the nodes that
// share it. The table lives in the run store's database, so a multi-node
// deployment shares one schedule list and the advisory-lock coordinator picks
// which node fires it.
const pgSchedulesSchema = `
CREATE TABLE IF NOT EXISTS schedules (
    id            TEXT PRIMARY KEY,
    workflow_path TEXT NOT NULL,
    workflow_name TEXT NOT NULL,
    cron          TEXT NOT NULL,
    last_fired    TIMESTAMPTZ,
    next_fire     TIMESTAMPTZ NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL
);
`

// pgScheduleColumns is the SELECT list every Postgres schedule query shares,
// in scanPGSchedule order.
const pgScheduleColumns = "id, workflow_path, workflow_name, cron, last_fired, next_fire, created_at"

// Postgres is the multi-node Schedules backend: the same surface the sqlite
// backend offers, over pgxpool, so several nodes read one schedule list. The
// PostgresCoordinator gates which of them fires.
type Postgres struct {
	pool *pgxpool.Pool
}

// OpenPostgres connects to dsn, verifies the connection, and applies the
// schedules schema. The DSN carries credentials from CAIRNOS_STORE_DSN.
func OpenPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open schedules postgres pool: %w", err)
	}

	if pingErr := pool.Ping(ctx); pingErr != nil {
		pool.Close()

		return nil, fmt.Errorf("ping schedules postgres: %w", pingErr)
	}

	if _, execErr := pool.Exec(ctx, pgSchedulesSchema); execErr != nil {
		pool.Close()

		return nil, fmt.Errorf("create schedules schema: %w", execErr)
	}

	return &Postgres{pool: pool}, nil
}

// Pool exposes the underlying pool so the coordinator can share it.
func (s *Postgres) Pool() *pgxpool.Pool {
	return s.pool
}

// Close releases the connection pool.
func (s *Postgres) Close() error {
	s.pool.Close()

	return nil
}

// Add validates the cron expression, fills the id, next_fire, and created_at
// the caller left zero, and persists the schedule. Validation here means a
// row that parses can never reach the daemon's fire path.
func (s *Postgres) Add(ctx context.Context, sched *Schedule) (string, error) {
	if err := ValidateCron(sched.Cron); err != nil {
		return "", err
	}

	id := sched.ID
	if id == "" {
		id = ulid.Make().String()
	}

	now := time.Now()

	createdAt := sched.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}

	nextFire := sched.NextFire
	if nextFire.IsZero() {
		next, err := NextFire(sched.Cron, now)
		if err != nil {
			return "", err
		}

		nextFire = next
	}

	_, err := s.pool.Exec(ctx,
		`INSERT INTO schedules (id, workflow_path, workflow_name, cron, last_fired, next_fire, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, sched.WorkflowPath, sched.WorkflowName, sched.Cron,
		nullablePGTime(sched.LastFired), nextFire.UTC(), createdAt.UTC())
	if err != nil {
		return "", fmt.Errorf("insert schedule %s: %w", id, err)
	}

	return id, nil
}

// List returns every schedule, oldest first.
func (s *Postgres) List(ctx context.Context) ([]Schedule, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+pgScheduleColumns+" FROM schedules ORDER BY created_at ASC, id ASC")
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	defer rows.Close()

	var scheds []Schedule

	for rows.Next() {
		sched, scanErr := scanPGSchedule(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("list schedules: %w", scanErr)
		}

		scheds = append(scheds, sched)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}

	return scheds, nil
}

// Remove deletes schedules whose id or workflow name matches key; zero rows
// is ErrScheduleNotFound.
func (s *Postgres) Remove(ctx context.Context, idOrName string) error {
	tag, err := s.pool.Exec(ctx,
		"DELETE FROM schedules WHERE id = $1 OR workflow_name = $2", idOrName, idOrName)
	if err != nil {
		return fmt.Errorf("remove schedule %s: %w", idOrName, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrScheduleNotFound, idOrName)
	}

	return nil
}

// MarkFired stamps one schedule's last_fired and next_fire after a fire.
func (s *Postgres) MarkFired(ctx context.Context, id string, lastFired, nextFire time.Time) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE schedules SET last_fired = $1, next_fire = $2 WHERE id = $3",
		lastFired.UTC(), nextFire.UTC(), id)
	if err != nil {
		return fmt.Errorf("mark schedule %s fired: %w", id, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrScheduleNotFound, id)
	}

	return nil
}

// nullablePGTime renders a timestamp for a nullable column, mapping the zero
// time ("never fired") to a SQL NULL.
func nullablePGTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}

	return value.UTC()
}

// scanPGSchedule reads one pgScheduleColumns row back into a Schedule.
func scanPGSchedule(rows pgx.Rows) (Schedule, error) {
	var (
		sched     Schedule
		lastFired *time.Time
		nextFire  time.Time
		createdAt time.Time
	)

	err := rows.Scan(&sched.ID, &sched.WorkflowPath, &sched.WorkflowName, &sched.Cron,
		&lastFired, &nextFire, &createdAt)
	if err != nil {
		return Schedule{}, fmt.Errorf("scan schedule: %w", err)
	}

	if lastFired != nil {
		sched.LastFired = *lastFired
	}

	sched.NextFire = nextFire
	sched.CreatedAt = createdAt

	return sched, nil
}
