package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/chadit/CairnOS/internal/store"
)

// versionColumns is the SELECT list every workflow-version query shares, in
// scanVersion order.
const versionColumns = "hash, workflow_name, definition_toml, created_at"

// SaveWorkflowVersion records one workflow-definition snapshot, the rollback
// point auto-apply captures before it edits a file
// (.agents/spec/13-roadmap.md, Phase 6). A zero CreatedAt is stamped with the
// current time. Re-saving the same hash is a no-op: the bytes are already
// captured, so ON CONFLICT DO NOTHING leaves the original snapshot and its
// timestamp untouched.
func (s *Store) SaveWorkflowVersion(ctx context.Context, version *store.WorkflowVersion) error {
	createdAt := version.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO workflow_versions (hash, workflow_name, definition_toml, created_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (hash) DO NOTHING`,
		version.Hash, version.WorkflowName, version.DefinitionTOML, formatTime(createdAt))
	if err != nil {
		return fmt.Errorf("save workflow version %s: %w", version.Hash, err)
	}

	return nil
}

// GetWorkflowVersion fetches one captured definition by its hash.
func (s *Store) GetWorkflowVersion(ctx context.Context, hash string) (*store.WorkflowVersion, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+versionColumns+" FROM workflow_versions WHERE hash = ?", hash)

	version, err := scanVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", store.ErrWorkflowVersionNotFound, hash)
	}

	if err != nil {
		return nil, fmt.Errorf("get workflow version %s: %w", hash, err)
	}

	return version, nil
}

// ListWorkflowVersions returns one workflow's captured definitions
// newest-first; the hash breaks ties so snapshots written in the same instant
// still list deterministically.
func (s *Store) ListWorkflowVersions(ctx context.Context, workflowName string) ([]*store.WorkflowVersion, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+versionColumns+" FROM workflow_versions WHERE workflow_name = ? ORDER BY created_at DESC, hash DESC",
		workflowName)
	if err != nil {
		return nil, fmt.Errorf("list workflow versions for %s: %w", workflowName, err)
	}
	defer closeQuietly(rows)

	var versions []*store.WorkflowVersion

	for rows.Next() {
		version, scanErr := scanVersion(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("list workflow versions for %s: %w", workflowName, scanErr)
		}

		versions = append(versions, version)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list workflow versions for %s: %w", workflowName, err)
	}

	return versions, nil
}

// scanVersion reads one versionColumns row back into a store.WorkflowVersion.
func scanVersion(row rowScanner) (*store.WorkflowVersion, error) {
	var (
		version   store.WorkflowVersion
		createdAt sql.NullTime
	)

	err := row.Scan(&version.Hash, &version.WorkflowName, &version.DefinitionTOML, &createdAt)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with their operation and hash
	}

	version.CreatedAt = parseTime(createdAt)

	return &version, nil
}
