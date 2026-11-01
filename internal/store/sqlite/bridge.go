package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/chadit/CairnOS/internal/store"
)

// BindBridgeSession upserts the (transport, chatKey) -> sessionID binding in
// the request workspace. ON CONFLICT makes a rebind (the bridge's /new path)
// overwrite the prior session id in one statement, so switching a chat to a
// fresh conversation never fails on the primary key. The workspace stamp keeps
// a chat bound in one tenant invisible from another.
func (s *Store) BindBridgeSession(ctx context.Context, transport, chatKey, sessionID string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO bridge_sessions (workspace_id, transport, chat_key, session_id, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (workspace_id, transport, chat_key) DO UPDATE SET
		     session_id = excluded.session_id,
		     updated_at = excluded.updated_at`,
		store.WorkspaceFromContext(ctx), transport, chatKey, sessionID, formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("bind bridge session %s/%s: %w", transport, chatKey, err)
	}

	return nil
}

// LookupBridgeSession returns the bound session id for (transport, chatKey) in
// the request workspace, or ErrBridgeSessionNotFound when no binding exists.
// The workspace scope means a chat bound in another tenant reads as not-found
// rather than leaking the session id across the boundary, so the bridge treats
// it as first contact and creates + binds a fresh session.
func (s *Store) LookupBridgeSession(ctx context.Context, transport, chatKey string) (string, error) {
	var sessionID string

	err := s.db.QueryRowContext(ctx,
		"SELECT session_id FROM bridge_sessions WHERE workspace_id = ? AND transport = ? AND chat_key = ?",
		store.WorkspaceFromContext(ctx), transport, chatKey).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s/%s", store.ErrBridgeSessionNotFound, transport, chatKey)
	}

	if err != nil {
		return "", fmt.Errorf("lookup bridge session %s/%s: %w", transport, chatKey, err)
	}

	return sessionID, nil
}
