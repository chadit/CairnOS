package postgres

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/store"
)

// runColumns is the SELECT list every run query shares, in scanRun order.
const runColumns = "id, workflow_name, workflow_hash, workspace_id, status, inputs_json, trigger, error_json, created_at, started_at, finished_at, parent_run_id"

// CreateRun persists a new run in its initial state. A zero CreatedAt is
// stamped with the current time and an empty WorkspaceID becomes "default",
// matching the sqlite backend so callers only fill what they care about.
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

	if inputs == nil {
		// Match the schema's NOT NULL DEFAULT '{}' even on explicit inserts.
		inputs = []byte("{}")
	}

	runErr, err := marshalError(run.Error)
	if err != nil {
		return err
	}

	_, err = s.pool.Exec(ctx,
		`INSERT INTO runs (id, workflow_name, workflow_hash, workspace_id, status, inputs_json, trigger, error_json, created_at, started_at, finished_at, parent_run_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		run.ID, run.WorkflowName, run.WorkflowHash, workspace, string(run.Status), inputs, run.Trigger,
		runErr, createdAt.UTC(), nullableTime(run.StartedAt), nullableTime(run.FinishedAt), run.ParentRunID)
	if err != nil {
		return duplicateRunErr(run.ID, err)
	}

	return nil
}

// UpdateRunStatus moves a run through its lifecycle. The CASE expressions
// stamp started_at only on the first transition to running and finished_at
// only on terminal states, all in one statement so concurrent updaters on
// different nodes cannot interleave a read with a write.
func (s *Store) UpdateRunStatus(ctx context.Context, id string, status store.RunStatus, runErr *cairnerr.Error) error {
	encodedErr, err := marshalError(runErr)
	if err != nil {
		return err
	}

	now := time.Now().UTC()

	tag, err := s.pool.Exec(ctx,
		`UPDATE runs
		 SET status = $1,
		     error_json = $2,
		     started_at = CASE WHEN $3 AND started_at IS NULL THEN $4 ELSE started_at END,
		     finished_at = CASE WHEN $5 THEN $6 ELSE finished_at END
		 WHERE id = $7`,
		string(status), encodedErr, status == store.RunRunning, now, isTerminal(status), now, id)
	if err != nil {
		return fmt.Errorf("update run %s status: %w", id, err)
	}

	if tag.RowsAffected() == 0 {
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
// reads as not-found rather than crossing the tenancy boundary
// (.agents/spec/12-ui-and-governance.md, "Multi-tenancy").
func (s *Store) GetRun(ctx context.Context, id string) (*store.Run, error) {
	row := s.pool.QueryRow(ctx,
		"SELECT "+runColumns+" FROM runs WHERE id = $1 AND workspace_id = $2",
		id, store.WorkspaceFromContext(ctx))

	run, err := scanRun(row)
	if isNoRows(err) {
		return nil, fmt.Errorf("%w: %s", ErrRunNotFound, id)
	}

	if err != nil {
		return nil, fmt.Errorf("get run %s: %w", id, err)
	}

	return run, nil
}

// ListRuns returns runs newest-first, narrowed by the filter. Newest-first
// uses created_at with the ULID id as tie-break, so runs created in the same
// instant still list deterministically, matching the sqlite backend.
func (s *Store) ListRuns(ctx context.Context, filter store.ListFilter) ([]*store.Run, error) {
	builder := newClauses()

	// Every list is scoped to the request workspace first, so a token bound to
	// one workspace never sees another's run history.
	builder.add("workspace_id = ", store.WorkspaceFromContext(ctx))

	if filter.WorkflowName != "" {
		builder.add("workflow_name = ", filter.WorkflowName)
	}

	if filter.Status != "" {
		builder.add("status = ", string(filter.Status))
	}

	query := "SELECT " + runColumns + " FROM runs" + builder.where() + " ORDER BY created_at DESC, id DESC"
	if filter.Limit > 0 {
		query += " LIMIT " + builder.add("", filter.Limit)
	}

	rows, err := s.pool.Query(ctx, query, builder.args()...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()

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

// rowScanner abstracts pgx.Row and pgx.Rows so scanRun serves both GetRun
// and ListRuns.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanRun reads one runColumns row back into a store.Run.
func scanRun(row rowScanner) (*store.Run, error) {
	var (
		run         store.Run
		status      string
		inputs      []byte
		runErr      []byte
		createdAt   time.Time
		startedAt   *time.Time
		finishedAt  *time.Time
		parentRunID *string
	)

	err := row.Scan(&run.ID, &run.WorkflowName, &run.WorkflowHash, &run.WorkspaceID, &status,
		&inputs, &run.Trigger, &runErr, &createdAt, &startedAt, &finishedAt, &parentRunID)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with their operation and run id
	}

	run.Status = store.RunStatus(status)

	// A pre-upgrade row (added by the guarded ALTER) reads NULL here, which is
	// the same "root run" the empty string means, so both map to "".
	if parentRunID != nil {
		run.ParentRunID = *parentRunID
	}

	if err := unmarshalJSON(inputs, &run.Inputs); err != nil {
		return nil, err
	}

	if err := unmarshalError(runErr, &run.Error); err != nil {
		return nil, err
	}

	run.CreatedAt = createdAt
	run.StartedAt = timeFromNull(startedAt)
	run.FinishedAt = timeFromNull(finishedAt)

	return &run, nil
}

// clauseBuilder accumulates WHERE fragments and their bound values, handing
// out $N placeholders so Postgres's positional parameters line up with the
// args slice. Postgres needs $1, $2... where sqlite used ?, so a small
// builder keeps the dynamic-filter queries readable.
type clauseBuilder struct {
	clauses []string
	values  []any
}

// newClauses starts an empty builder.
func newClauses() *clauseBuilder {
	return &clauseBuilder{}
}

// add binds value, returns its $N placeholder, and (when prefix is a column
// comparison like "status = ") records the full clause for the WHERE. A bare
// "" prefix binds a value (a LIMIT) without adding a WHERE clause.
func (b *clauseBuilder) add(prefix string, value any) string {
	b.values = append(b.values, value)
	placeholder := "$" + strconv.Itoa(len(b.values))

	if prefix != "" {
		b.clauses = append(b.clauses, prefix+placeholder)
	}

	return placeholder
}

// where renders the accumulated clauses, empty when there are none.
func (b *clauseBuilder) where() string {
	if len(b.clauses) == 0 {
		return ""
	}

	return " WHERE " + strings.Join(b.clauses, " AND ")
}

// args returns the bound values in placeholder order.
func (b *clauseBuilder) args() []any {
	return b.values
}
