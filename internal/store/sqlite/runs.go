package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/store"
)

// runColumns is the SELECT list every run query shares, in scanRun order.
const runColumns = "id, workflow_name, workflow_hash, workspace_id, status, inputs_json, trigger, error_json, created_at, started_at, finished_at, parent_run_id"

// CreateRun persists a new run in its initial state. A zero CreatedAt is
// stamped with the current time and an empty WorkspaceID becomes "default",
// so callers only fill what they care about.
func (s *Store) CreateRun(ctx context.Context, run *store.Run) error {
	workspace := run.WorkspaceID
	if workspace == "" {
		workspace = store.DefaultWorkspace
	}

	createdAt := run.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	inputs, err := marshalJSON(run.Inputs, run.Inputs == nil)
	if err != nil {
		return err
	}

	if !inputs.Valid {
		// Match the schema's NOT NULL DEFAULT '{}' even on explicit inserts.
		inputs = sql.NullString{String: "{}", Valid: true}
	}

	runErr, err := marshalError(run.Error)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO runs (id, workflow_name, workflow_hash, workspace_id, status, inputs_json, trigger, error_json, created_at, started_at, finished_at, parent_run_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.WorkflowName, run.WorkflowHash, workspace, string(run.Status), inputs, run.Trigger,
		runErr, formatTime(createdAt), formatTime(run.StartedAt), formatTime(run.FinishedAt), run.ParentRunID)
	if err != nil {
		return duplicateRunErr(run.ID, err)
	}

	return nil
}

// UpdateRunStatus moves a run through its lifecycle. The CASE expressions
// stamp started_at only on the first transition to running and finished_at
// only on terminal states, all in one statement so concurrent updates cannot
// interleave between a read and a write.
func (s *Store) UpdateRunStatus(ctx context.Context, id string, status store.RunStatus, runErr *cairnerr.Error) error {
	encodedErr, err := marshalError(runErr)
	if err != nil {
		return err
	}

	now := formatTime(time.Now())

	result, err := s.db.ExecContext(ctx,
		`UPDATE runs
		 SET status = ?,
		     error_json = ?,
		     started_at = CASE WHEN ? AND started_at IS NULL THEN ? ELSE started_at END,
		     finished_at = CASE WHEN ? THEN ? ELSE finished_at END
		 WHERE id = ?`,
		string(status), encodedErr, status == store.RunRunning, now, isTerminal(status), now, id)
	if err != nil {
		return fmt.Errorf("update run %s status: %w", id, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update run %s status: rows affected: %w", id, err)
	}

	if affected == 0 {
		return fmt.Errorf("%w: %s", ErrRunNotFound, id)
	}

	return nil
}

// isTerminal reports whether status ends a run's lifecycle.
func isTerminal(status store.RunStatus) bool {
	return status == store.RunSucceeded || status == store.RunFailed || status == store.RunCanceled
}

// GetRun fetches one run by id, including its terminal error if any. The
// query is scoped to the request workspace, so a run in another workspace
// reads as not-found rather than leaking across the tenancy boundary
// (.agents/spec/12-ui-and-governance.md, "Multi-tenancy").
func (s *Store) GetRun(ctx context.Context, id string) (*store.Run, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+runColumns+" FROM runs WHERE id = ? AND workspace_id = ?",
		id, store.WorkspaceFromContext(ctx))

	run, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrRunNotFound, id)
	}

	if err != nil {
		return nil, fmt.Errorf("get run %s: %w", id, err)
	}

	return run, nil
}

// ListRuns returns runs newest-first, narrowed by the filter. Newest-first
// uses created_at with the ULID id as tie-break, so runs created in the same
// nanosecond still list deterministically.
func (s *Store) ListRuns(ctx context.Context, filter store.ListFilter) ([]*store.Run, error) {
	// Every list is scoped to the request workspace first, so a token bound to
	// one workspace never sees another's run history.
	clauses := []string{"workspace_id = ?"}
	args := []any{store.WorkspaceFromContext(ctx)}

	if filter.WorkflowName != "" {
		clauses = append(clauses, "workflow_name = ?")
		args = append(args, filter.WorkflowName)
	}

	if filter.Status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, string(filter.Status))
	}

	// The workspace clause always present means WHERE is unconditional now.
	// #nosec G202 -- every clause is a constant string; values bind as parameters.
	query := "SELECT " + runColumns + " FROM runs WHERE " + strings.Join(clauses, " AND ")

	query += " ORDER BY created_at DESC, id DESC"

	if filter.Limit > 0 {
		query += limitClause

		args = append(args, filter.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer closeQuietly(rows)

	var runs []*store.Run

	for rows.Next() {
		run, scanErr := scanRun(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("list runs: %w", scanErr)
		}

		runs = append(runs, run)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}

	return runs, nil
}

// rowScanner abstracts *sql.Row and *sql.Rows so scanRun serves both GetRun
// and ListRuns.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanRun reads one runColumns row back into a store.Run.
func scanRun(row rowScanner) (*store.Run, error) {
	var (
		run         store.Run
		status      string
		inputs      sql.NullString
		runErr      sql.NullString
		createdAt   sql.NullTime
		startedAt   sql.NullTime
		finishedAt  sql.NullTime
		parentRunID sql.NullString
	)

	err := row.Scan(&run.ID, &run.WorkflowName, &run.WorkflowHash, &run.WorkspaceID, &status,
		&inputs, &run.Trigger, &runErr, &createdAt, &startedAt, &finishedAt, &parentRunID)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with their operation and run id
	}

	run.Status = store.RunStatus(status)
	// A pre-upgrade row (added by the guarded ALTER) reads NULL here, which is
	// the same "root run" the empty string means, so both map to "".
	run.ParentRunID = parentRunID.String

	if err := unmarshalJSON(inputs, &run.Inputs); err != nil {
		return nil, err
	}

	if err := unmarshalError(runErr, &run.Error); err != nil {
		return nil, err
	}

	run.CreatedAt = parseTime(createdAt)
	run.StartedAt = parseTime(startedAt)
	run.FinishedAt = parseTime(finishedAt)

	return &run, nil
}

// closeQuietly closes rows where the read path already reports errors via
// rows.Err, making a second error from Close redundant.
func closeQuietly(rows *sql.Rows) {
	_ = rows.Close()
}
