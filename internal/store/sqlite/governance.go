package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/chadit/CairnOS/internal/store"
)

// auditColumns is the SELECT list every audit query shares, in scanAudit
// order.
const auditColumns = "id, actor, action, target, detail, created_at"

// pauseColumns is the SELECT list every workflow-pause query shares, in
// scanPause order.
const pauseColumns = "workflow_name, actor, reason, paused_at"

// AppendAudit persists one governance audit row. A zero ID gets a fresh ULID
// and a zero CreatedAt the current time, so callers fill only what they care
// about. Audit rows are append-only and survive the retention sweep. The row is
// stamped with the request workspace so the audit trail is per tenant.
func (s *Store) AppendAudit(ctx context.Context, entry *store.AuditEntry) error {
	id := entry.ID
	if id == "" {
		id = ulid.Make().String()
	}

	createdAt := entry.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (id, workspace_id, actor, action, target, detail, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, store.WorkspaceFromContext(ctx), entry.Actor, entry.Action, entry.Target, entry.Detail, formatTime(createdAt))
	if err != nil {
		return fmt.Errorf("append audit %s: %w", id, err)
	}

	return nil
}

// ListAudit returns audit rows newest-first, narrowed by the filter, scoped to
// the request workspace so a token bound to one tenant reads only its own
// audit trail. The id breaks ties so rows written in the same instant still
// list deterministically.
func (s *Store) ListAudit(ctx context.Context, filter store.AuditFilter) ([]*store.AuditEntry, error) {
	query := "SELECT " + auditColumns + " FROM audit_log WHERE workspace_id = ? ORDER BY created_at DESC, id DESC"

	args := []any{store.WorkspaceFromContext(ctx)}

	if filter.Limit > 0 {
		query += limitClause

		args = append(args, filter.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}

	return collectRows(rows, "list audit", scanAudit)
}

// SetWorkflowPause records or overwrites the pause flag for one workflow in the
// request workspace. A zero PausedAt becomes the current time. Re-pausing an
// already paused workflow overwrites the actor, reason, and timestamp rather
// than failing. Two tenants can pause the same workflow name independently: the
// (workspace_id, workflow_name) key keeps the flags separate.
func (s *Store) SetWorkflowPause(ctx context.Context, pause *store.WorkflowPause) error {
	pausedAt := pause.PausedAt
	if pausedAt.IsZero() {
		pausedAt = time.Now()
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO workflow_pauses (workspace_id, workflow_name, actor, reason, paused_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (workspace_id, workflow_name) DO UPDATE SET
		     actor = excluded.actor,
		     reason = excluded.reason,
		     paused_at = excluded.paused_at`,
		store.WorkspaceFromContext(ctx), pause.WorkflowName, pause.Actor, pause.Reason, formatTime(pausedAt))
	if err != nil {
		return fmt.Errorf("pause workflow %s: %w", pause.WorkflowName, err)
	}

	return nil
}

// ClearWorkflowPause removes one workflow's pause flag in the request
// workspace, reporting whether a flag was present to clear so resume can tell a
// real lift from a no-op. One tenant's resume never clears another's pause of
// the same workflow name.
func (s *Store) ClearWorkflowPause(ctx context.Context, workflowName string) (bool, error) {
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM workflow_pauses WHERE workspace_id = ? AND workflow_name = ?",
		store.WorkspaceFromContext(ctx), workflowName)
	if err != nil {
		return false, fmt.Errorf("resume workflow %s: %w", workflowName, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("resume workflow %s: rows affected: %w", workflowName, err)
	}

	return affected > 0, nil
}

// GetWorkflowPause fetches one workflow's pause flag in the request workspace,
// reporting whether the workflow is paused. A missing row is the unpaused case,
// not an error. The workspace scope means a workflow paused in one tenant reads
// as unpaused in another.
func (s *Store) GetWorkflowPause(ctx context.Context, workflowName string) (*store.WorkflowPause, bool, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+pauseColumns+" FROM workflow_pauses WHERE workspace_id = ? AND workflow_name = ?",
		store.WorkspaceFromContext(ctx), workflowName)

	pause, err := scanPause(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("get workflow pause %s: %w", workflowName, err)
	}

	return pause, true, nil
}

// ListWorkflowPauses returns the request workspace's paused workflows, newest
// pause first, so one tenant never sees another's pauses.
func (s *Store) ListWorkflowPauses(ctx context.Context) ([]*store.WorkflowPause, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+pauseColumns+" FROM workflow_pauses WHERE workspace_id = ? ORDER BY paused_at DESC, workflow_name ASC",
		store.WorkspaceFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("list workflow pauses: %w", err)
	}

	return collectRows(rows, "list workflow pauses", scanPause)
}

// SpendSince sums estimated cost_usd across every action result of the named
// workflow's runs created at or after since. A workflow with no runs in the
// window sums to zero. COALESCE keeps an all-NULL sum from returning NULL.
// The workspace clause scopes the budget to the request tenant, so two
// workspaces running the same workflow name keep separate budgets.
func (s *Store) SpendSince(ctx context.Context, workflowName string, since time.Time) (float64, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(CAST(json_extract(ar.usage_json, '$.cost_usd') AS REAL)), 0)
		 FROM action_results ar
		 JOIN runs r ON r.id = ar.run_id
		 WHERE r.workflow_name = ? AND r.workspace_id = ? AND r.created_at >= ?`,
		workflowName, store.WorkspaceFromContext(ctx), formatTime(since))

	var spend float64
	if err := row.Scan(&spend); err != nil {
		return 0, fmt.Errorf("spend since for %s: %w", workflowName, err)
	}

	return spend, nil
}

// scanAudit reads one auditColumns row back into a store.AuditEntry.
func scanAudit(row rowScanner) (*store.AuditEntry, error) {
	var (
		entry     store.AuditEntry
		createdAt sql.NullTime
	)

	err := row.Scan(&entry.ID, &entry.Actor, &entry.Action, &entry.Target, &entry.Detail, &createdAt)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with their operation
	}

	entry.CreatedAt = parseTime(createdAt)

	return &entry, nil
}

// scanPause reads one pauseColumns row back into a store.WorkflowPause.
func scanPause(row rowScanner) (*store.WorkflowPause, error) {
	var (
		pause    store.WorkflowPause
		pausedAt sql.NullTime
	)

	err := row.Scan(&pause.WorkflowName, &pause.Actor, &pause.Reason, &pausedAt)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with their operation
	}

	pause.PausedAt = parseTime(pausedAt)

	return &pause, nil
}
