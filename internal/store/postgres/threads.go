package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/chadit/CairnOS/internal/store"
)

// BindMessageThread upserts the root post for one thread key, matching the
// sqlite backend: a root that went missing is replaced in one statement.
func (s *Store) BindMessageThread(ctx context.Context, identity, target, key, rootID string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO message_threads (workspace_id, identity, target, thread_key, root_id, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (workspace_id, identity, target, thread_key) DO UPDATE SET
		     root_id = excluded.root_id,
		     updated_at = excluded.updated_at`,
		store.WorkspaceFromContext(ctx), identity, target, key, rootID, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("bind message thread %s/%s/%s: %w", identity, target, key, err)
	}

	return nil
}

// LookupMessageThread reads the root post for one thread key. A key bound in
// another workspace reads as not found, so tenants never reply into each other's threads.
func (s *Store) LookupMessageThread(ctx context.Context, identity, target, key string) (string, error) {
	var rootID string

	err := s.pool.QueryRow(ctx,
		`SELECT root_id FROM message_threads
		 WHERE workspace_id = $1 AND identity = $2 AND target = $3 AND thread_key = $4`,
		store.WorkspaceFromContext(ctx), identity, target, key).Scan(&rootID)
	if isNoRows(err) {
		return "", fmt.Errorf("%w: %s/%s/%s", store.ErrMessageThreadNotFound, identity, target, key)
	}

	if err != nil {
		return "", fmt.Errorf("lookup message thread %s/%s/%s: %w", identity, target, key, err)
	}

	return rootID, nil
}
