package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/chadit/CairnOS/internal/store"
)

// actionColumns is the SELECT list every action query shares, in
// scanActionResult order. The ar. prefix lets the workspace-scoped list join
// runs without ambiguous column names.
const actionColumns = "ar.run_id, ar.action_name, ar.runner, ar.status, ar.attempt, ar.output_json, ar.usage_json, ar.meta_json, ar.error_json, ar.started_at, ar.finished_at"

// UpsertActionResult writes an action's current state. The engine calls it
// on every transition, so the upsert overwrites the whole row: the latest
// write is the action's truth and a crash mid-run loses nothing already
// flushed.
func (s *Store) UpsertActionResult(ctx context.Context, result *store.ActionResult) error {
	output, err := marshalJSON(result.Output, result.Output == nil)
	if err != nil {
		return err
	}

	usage, err := marshalUsage(result.Usage)
	if err != nil {
		return err
	}

	meta, err := marshalJSON(result.Meta, result.Meta == nil)
	if err != nil {
		return err
	}

	actionErr, err := marshalError(result.Error)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO action_results (run_id, action_name, runner, status, attempt, output_json, usage_json, meta_json, error_json, started_at, finished_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (run_id, action_name) DO UPDATE SET
		     runner = excluded.runner,
		     status = excluded.status,
		     attempt = excluded.attempt,
		     output_json = excluded.output_json,
		     usage_json = excluded.usage_json,
		     meta_json = excluded.meta_json,
		     error_json = excluded.error_json,
		     started_at = excluded.started_at,
		     finished_at = excluded.finished_at`,
		result.RunID, result.ActionName, result.Runner, string(result.Status), result.Attempt,
		output, usage, meta, actionErr, formatTime(result.StartedAt), formatTime(result.FinishedAt))
	if err != nil {
		return missingRunErr(result.RunID, result.ActionName, err)
	}

	return nil
}

// ListActionResults returns every action row for a run, ordered by action
// name so callers see a stable listing. The join to runs scopes the result
// to the request workspace, so an action row whose run belongs to another
// workspace is invisible.
func (s *Store) ListActionResults(ctx context.Context, runID string) ([]*store.ActionResult, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+actionColumns+` FROM action_results ar
		 JOIN runs r ON r.id = ar.run_id
		 WHERE ar.run_id = ? AND r.workspace_id = ?
		 ORDER BY ar.action_name`,
		runID, store.WorkspaceFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("list action results for run %s: %w", runID, err)
	}
	defer closeQuietly(rows)

	var results []*store.ActionResult

	for rows.Next() {
		result, scanErr := scanActionResult(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("list action results for run %s: %w", runID, scanErr)
		}

		results = append(results, result)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list action results for run %s: %w", runID, err)
	}

	return results, nil
}

// scanActionResult reads one actionColumns row back into a
// store.ActionResult.
func scanActionResult(row rowScanner) (*store.ActionResult, error) {
	var (
		result     store.ActionResult
		status     string
		output     sql.NullString
		usage      sql.NullString
		meta       sql.NullString
		actionErr  sql.NullString
		startedAt  sql.NullTime
		finishedAt sql.NullTime
	)

	err := row.Scan(&result.RunID, &result.ActionName, &result.Runner, &status, &result.Attempt,
		&output, &usage, &meta, &actionErr, &startedAt, &finishedAt)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with their operation and run id
	}

	result.Status = store.ActionStatus(status)

	if unmarshalJSONErr := unmarshalJSON(output, &result.Output); unmarshalJSONErr != nil {
		return nil, unmarshalJSONErr
	}

	result.Usage, err = unmarshalUsage(usage)
	if err != nil {
		return nil, err
	}

	if err := unmarshalJSON(meta, &result.Meta); err != nil {
		return nil, err
	}

	if err := unmarshalError(actionErr, &result.Error); err != nil {
		return nil, err
	}

	result.StartedAt = parseTime(startedAt)
	result.FinishedAt = parseTime(finishedAt)

	return &result, nil
}
