package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/chadit/CairnOS/internal/store"
)

// BindMessageThread upserts the root post for one thread key. ON CONFLICT lets a
// root that went missing be replaced in one statement instead of failing on the key.
func (s *Store) BindMessageThread(ctx context.Context, identity, target, key, rootID string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO message_threads (workspace_id, identity, target, thread_key, root_id, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (workspace_id, identity, target, thread_key) DO UPDATE SET
		     root_id = excluded.root_id,
		     updated_at = excluded.updated_at`,
		store.WorkspaceFromContext(ctx), identity, target, key, rootID, formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("bind message thread %s/%s/%s: %w", identity, target, key, err)
	}

	return nil
}

// LookupMessageThread reads the root post for one thread key. A key bound in
// another workspace reads as not found, so tenants never reply into each other's threads.
func (s *Store) LookupMessageThread(ctx context.Context, identity, target, key string) (string, error) {
	var rootID string

	err := s.db.QueryRowContext(ctx,
		`SELECT root_id FROM message_threads
		 WHERE workspace_id = ? AND identity = ? AND target = ? AND thread_key = ?`,
		store.WorkspaceFromContext(ctx), identity, target, key).Scan(&rootID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s/%s/%s", store.ErrMessageThreadNotFound, identity, target, key)
	}

	if err != nil {
		return "", fmt.Errorf("lookup message thread %s/%s/%s: %w", identity, target, key, err)
	}

	return rootID, nil
}
