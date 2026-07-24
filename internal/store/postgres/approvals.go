package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/chadit/CairnOS/internal/store"
)

// approvalColumns is the SELECT list approval queries share, in scanApproval
// order.
const approvalColumns = "run_id, action_name, workspace_id, status, message, actor, comment, created_at, decided_at"

// CreateApproval records a new pending approval gate, keeping identical
// defaulting to the sqlite backend: a zero CreatedAt becomes now, an empty
// WorkspaceID becomes the request workspace, and an empty Status becomes
// pending (.agents/spec/parity/workflow-language.md, "Human approval").
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

	_, err := s.pool.Exec(ctx,
		`INSERT INTO run_approvals (run_id, action_name, workspace_id, status, message, actor, comment, created_at, decided_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		approval.RunID, approval.ActionName, workspace, string(status), approval.Message,
		approval.Actor, approval.Comment, createdAt.UTC(), nullableTime(approval.DecidedAt))
	if err != nil {
		return fmt.Errorf("insert approval %s/%s: %w", approval.RunID, approval.ActionName, err)
	}

	return nil
}

// GetApproval fetches one gate by (runID, actionName), scoped to the request
// workspace so an approval in another workspace reads as not-found.
func (s *Store) GetApproval(ctx context.Context, runID, actionName string) (*store.Approval, error) {
	row := s.pool.QueryRow(ctx,
		"SELECT "+approvalColumns+" FROM run_approvals WHERE run_id = $1 AND action_name = $2 AND workspace_id = $3",
		runID, actionName, store.WorkspaceFromContext(ctx))

	approval, err := scanApproval(row)
	if isNoRows(err) {
		return nil, fmt.Errorf("%w: %s/%s", store.ErrApprovalNotFound, runID, actionName)
	}

	if err != nil {
		return nil, fmt.Errorf("get approval %s/%s: %w", runID, actionName, err)
	}

	return approval, nil
}

// ListPendingApprovals returns a run's still-pending gates ordered by action
// name, scoped to the request workspace.
func (s *Store) ListPendingApprovals(ctx context.Context, runID string) ([]*store.Approval, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+approvalColumns+
			" FROM run_approvals WHERE run_id = $1 AND workspace_id = $2 AND status = $3 ORDER BY action_name",
		runID, store.WorkspaceFromContext(ctx), string(store.ApprovalPending))
	if err != nil {
		return nil, fmt.Errorf("list pending approvals for %s: %w", runID, err)
	}

	return collectRows(rows, "list pending approvals for "+runID, scanApproval)
}

// DecideApproval moves a pending gate to approved or denied in one statement,
// the status = pending guard making a second concurrent decision a no-op. A
// zero rows-affected means the gate is missing, already decided, or in another
// workspace, all of which read as not-found, matching the sqlite backend.
func (s *Store) DecideApproval(ctx context.Context, runID, actionName string, status store.ApprovalStatus, actor, comment string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE run_approvals
		 SET status = $1, actor = $2, comment = $3, decided_at = $4
		 WHERE run_id = $5 AND action_name = $6 AND workspace_id = $7 AND status = $8`,
		string(status), actor, comment, time.Now().UTC(),
		runID, actionName, store.WorkspaceFromContext(ctx), string(store.ApprovalPending))
	if err != nil {
		return fmt.Errorf("decide approval %s/%s: %w", runID, actionName, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s/%s", store.ErrApprovalNotFound, runID, actionName)
	}

	return nil
}

// scanApproval reads one approvalColumns row back into a store.Approval.
func scanApproval(row rowScanner) (*store.Approval, error) {
	var (
		approval  store.Approval
		status    string
		createdAt time.Time
		decidedAt *time.Time
	)

	err := row.Scan(&approval.RunID, &approval.ActionName, &approval.WorkspaceID, &status,
		&approval.Message, &approval.Actor, &approval.Comment, &createdAt, &decidedAt)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with their operation and ids
	}

	approval.Status = store.ApprovalStatus(status)
	approval.CreatedAt = createdAt
	approval.DecidedAt = timeFromNull(decidedAt)

	return &approval, nil
}
