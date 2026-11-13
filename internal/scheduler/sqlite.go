package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// schedulesSchema is the schedules table, plus IF NOT EXISTS so opening
// an existing database is idempotent. It shares the run store's database file
// with disjoint tables, the same arrangement internal/memory uses.
const schedulesSchema = `
CREATE TABLE IF NOT EXISTS schedules (
    id            TEXT PRIMARY KEY,
    workflow_path TEXT NOT NULL,
    workflow_name TEXT NOT NULL,
    cron          TEXT NOT NULL,
    last_fired    TIMESTAMP,
    next_fire     TIMESTAMP NOT NULL,
    created_at    TIMESTAMP NOT NULL
);
`

// scheduleColumns is the SELECT list every schedule query shares, in
// scanSchedule order.
const scheduleColumns = "id, workflow_path, workflow_name, cron, last_fired, next_fire, created_at"

// storedTimeLayout is RFC 3339 UTC with fixed-width nanoseconds, matching
// the run and memory stores so lexicographic order tracks chronological
// order across every table in the shared file.
const storedTimeLayout = "2006-01-02T15:04:05.000000000Z"

// SQLite is a Schedules backend. Safe for concurrent use through
// WAL mode and busy_timeout, which also lets the CLI add and remove
// schedules while the daemon holds the file open.
type SQLite struct {
	db *sql.DB
}

// OpenSQLite opens (creating if needed) the schedules database at path and
// applies the schema. Same pragma choices as the run store: WAL so readers
// do not block the writer, busy_timeout so concurrent writers wait instead
// of failing with SQLITE_BUSY.
func OpenSQLite(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open schedules database %q: %w", path, err)
	}

	// Schema creation is process-lifetime initialization with no request to
	// inherit a context from; OpenSQLite carries no ctx on purpose.
	setupCtx := context.Background()

	if _, execErr := db.ExecContext(setupCtx, schedulesSchema); execErr != nil {
		return nil, fmt.Errorf("create schedules schema: %w", errors.Join(execErr, db.Close()))
	}

	return &SQLite{db: db}, nil
}

// Close releases the underlying database.
func (s *SQLite) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close schedules database: %w", err)
	}

	return nil
}

// Add validates the cron expression, fills the id, next_fire, and
// created_at the caller left zero, and persists the schedule. Validation
// here means a row that parses can never reach the daemon's fire path.
func (s *SQLite) Add(ctx context.Context, sched *Schedule) (string, error) {
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

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO schedules (id, workflow_path, workflow_name, cron, last_fired, next_fire, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, sched.WorkflowPath, sched.WorkflowName, sched.Cron,
		nullableTime(sched.LastFired), storedTime(nextFire), storedTime(createdAt))
	if err != nil {
		return "", fmt.Errorf("insert schedule %s: %w", id, err)
	}

	return id, nil
}

// List returns every schedule, oldest first; created_at is stored in a
// lexicographically ordered layout, so the index walk is chronological.
func (s *SQLite) List(ctx context.Context) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+scheduleColumns+" FROM schedules ORDER BY created_at ASC, id ASC")
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	defer closeRows(rows)

	var scheds []Schedule

	for rows.Next() {
		sched, scanErr := scanSchedule(rows)
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

// Remove deletes schedules whose id or workflow name matches key. Matching
// by name may delete several rows when one workflow was scheduled twice;
// zero rows is ErrScheduleNotFound.
func (s *SQLite) Remove(ctx context.Context, idOrName string) error {
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM schedules WHERE id = ? OR workflow_name = ?", idOrName, idOrName)
	if err != nil {
		return fmt.Errorf("remove schedule %s: %w", idOrName, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("remove schedule %s: rows affected: %w", idOrName, err)
	}

	if affected == 0 {
		return fmt.Errorf("%w: %s", ErrScheduleNotFound, idOrName)
	}

	return nil
}

// MarkFired stamps one schedule's last_fired and next_fire after a fire.
func (s *SQLite) MarkFired(ctx context.Context, id string, lastFired, nextFire time.Time) error {
	result, err := s.db.ExecContext(ctx,
		"UPDATE schedules SET last_fired = ?, next_fire = ? WHERE id = ?",
		storedTime(lastFired), storedTime(nextFire), id)
	if err != nil {
		return fmt.Errorf("mark schedule %s fired: %w", id, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark schedule %s fired: rows affected: %w", id, err)
	}

	if affected == 0 {
		return fmt.Errorf("%w: %s", ErrScheduleNotFound, id)
	}

	return nil
}

// storedTime renders a timestamp in the shared fixed-width layout.
func storedTime(value time.Time) string {
	return value.UTC().Format(storedTimeLayout)
}

// nullableTime renders a timestamp for a nullable column, mapping the zero
// time ("never fired") to NULL.
func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}

	return storedTime(value)
}

// scanSchedule reads one scheduleColumns row back into a Schedule.
func scanSchedule(rows *sql.Rows) (Schedule, error) {
	var (
		sched     Schedule
		lastFired sql.NullTime
		nextFire  sql.NullTime
		createdAt sql.NullTime
	)

	err := rows.Scan(&sched.ID, &sched.WorkflowPath, &sched.WorkflowName, &sched.Cron,
		&lastFired, &nextFire, &createdAt)
	if err != nil {
		return Schedule{}, fmt.Errorf("scan schedule: %w", err)
	}

	sched.LastFired = lastFired.Time
	sched.NextFire = nextFire.Time
	sched.CreatedAt = createdAt.Time

	return sched, nil
}

// closeRows closes rows where the read path already reports errors via
// rows.Err, making a second error from Close redundant.
func closeRows(rows *sql.Rows) {
	_ = rows.Close()
}
