package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/chadit/CairnOS/internal/store"
)

// approvalColumns is the SELECT list approval queries share, in scanApproval
// order.
const approvalColumns = "run_id, action_name, workspace_id, status, message, actor, comment, created_at, decided_at"

// CreateApproval records a new pending approval gate. A zero CreatedAt is
// stamped with the current time, an empty WorkspaceID becomes the request
// workspace, and an empty Status becomes pending, so the engine only fills what
// it cares about.
func (s *Store) CreateApproval(ctx context.Context, approval *store.Approval) error {
	workspace := approval.WorkspaceID
	if workspace == "" {
		workspace = store.WorkspaceFromContext(ctx)
	}

	status := approval.Status
	if status == "" {
		status = store.ApprovalPending
	}

	createdAt := approval.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO run_approvals (run_id, action_name, workspace_id, status, message, actor, comment, created_at, decided_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		approval.RunID, approval.ActionName, workspace, string(status), approval.Message,
		approval.Actor, approval.Comment, formatTime(createdAt), formatTime(approval.DecidedAt))
	if err != nil {
		return fmt.Errorf("insert approval %s/%s: %w", approval.RunID, approval.ActionName, err)
	}

	return nil
}

// GetApproval fetches one gate by (runID, actionName), scoped to the request
// workspace so an approval in another workspace reads as not-found rather than
// leaking across the tenancy boundary.
func (s *Store) GetApproval(ctx context.Context, runID, actionName string) (*store.Approval, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+approvalColumns+" FROM run_approvals WHERE run_id = ? AND action_name = ? AND workspace_id = ?",
		runID, actionName, store.WorkspaceFromContext(ctx))

	approval, err := scanApproval(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s/%s", store.ErrApprovalNotFound, runID, actionName)
	}

	if err != nil {
		return nil, fmt.Errorf("get approval %s/%s: %w", runID, actionName, err)
	}

	return approval, nil
}

// ListPendingApprovals returns a run's still-pending gates ordered by action
// name, scoped to the request workspace so a name-omitted decision only ever
// sees the caller's own tenant.
func (s *Store) ListPendingApprovals(ctx context.Context, runID string) ([]*store.Approval, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+approvalColumns+
			" FROM run_approvals WHERE run_id = ? AND workspace_id = ? AND status = ? ORDER BY action_name",
		runID, store.WorkspaceFromContext(ctx), string(store.ApprovalPending))
	if err != nil {
		return nil, fmt.Errorf("list pending approvals for %s: %w", runID, err)
	}

	return collectRows(rows, "list pending approvals for "+runID, scanApproval)
}

// DecideApproval moves a pending gate to approved or denied in one statement,
// so a concurrent second decision cannot both win: the status = pending guard
// makes the update a no-op once the gate is decided. A zero rowsAffected means
// the gate is missing, already decided, or in another workspace, all of which
// read as not-found.
func (s *Store) DecideApproval(ctx context.Context, runID, actionName string, status store.ApprovalStatus, actor, comment string) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE run_approvals
		 SET status = ?, actor = ?, comment = ?, decided_at = ?
		 WHERE run_id = ? AND action_name = ? AND workspace_id = ? AND status = ?`,
		string(status), actor, comment, formatTime(time.Now()),
		runID, actionName, store.WorkspaceFromContext(ctx), string(store.ApprovalPending))
	if err != nil {
		return fmt.Errorf("decide approval %s/%s: %w", runID, actionName, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("decide approval %s/%s: rows affected: %w", runID, actionName, err)
	}

	if affected == 0 {
		return fmt.Errorf("%w: %s/%s", store.ErrApprovalNotFound, runID, actionName)
	}

	return nil
}

// scanApproval reads one approvalColumns row back into a store.Approval.
func scanApproval(row rowScanner) (*store.Approval, error) {
	var (
		approval  store.Approval
		status    string
		createdAt sql.NullTime
		decidedAt sql.NullTime
	)

	err := row.Scan(&approval.RunID, &approval.ActionName, &approval.WorkspaceID, &status,
		&approval.Message, &approval.Actor, &approval.Comment, &createdAt, &decidedAt)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with their operation and ids
	}

	approval.Status = store.ApprovalStatus(status)
	approval.CreatedAt = parseTime(createdAt)
	approval.DecidedAt = parseTime(decidedAt)

	return &approval, nil
}
