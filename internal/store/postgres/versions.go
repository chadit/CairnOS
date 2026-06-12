package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/chadit/CairnOS/internal/store"
)

// versionColumns is the SELECT list every workflow-version query shares, in
// scanVersion order.
const versionColumns = "hash, workflow_name, definition_toml, created_at"

// SaveWorkflowVersion records one workflow-definition snapshot, matching the
// sqlite backend: a zero CreatedAt becomes the current time, and re-saving the
// same hash is a no-op so a definition captured twice keeps its first
// timestamp (.agents/spec/13-roadmap.md, Phase 6).
func (s *Store) SaveWorkflowVersion(ctx context.Context, version *store.WorkflowVersion) error {
	createdAt := version.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	_, err := s.pool.Exec(ctx,
		`INSERT INTO workflow_versions (hash, workflow_name, definition_toml, created_at)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (hash) DO NOTHING`,
		version.Hash, version.WorkflowName, version.DefinitionTOML, createdAt.UTC())
	if err != nil {
		return fmt.Errorf("save workflow version %s: %w", version.Hash, err)
	}

	return nil
}

// GetWorkflowVersion fetches one captured definition by its hash.
func (s *Store) GetWorkflowVersion(ctx context.Context, hash string) (*store.WorkflowVersion, error) {
	row := s.pool.QueryRow(ctx,
		"SELECT "+versionColumns+" FROM workflow_versions WHERE hash = $1", hash)

	version, err := scanVersion(row)
	if isNoRows(err) {
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
	rows, err := s.pool.Query(ctx,
		"SELECT "+versionColumns+" FROM workflow_versions WHERE workflow_name = $1 ORDER BY created_at DESC, hash DESC",
		workflowName)
	if err != nil {
		return nil, fmt.Errorf("list workflow versions for %s: %w", workflowName, err)
	}
	defer rows.Close()

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
		createdAt time.Time
	)

	err := row.Scan(&version.Hash, &version.WorkflowName, &version.DefinitionTOML, &createdAt)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with their operation and hash
	}

	version.CreatedAt = createdAt

	return &version, nil
}
