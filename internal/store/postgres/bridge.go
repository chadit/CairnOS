package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/chadit/CairnOS/internal/store"
)

// BindBridgeSession upserts the (transport, chatKey) -> sessionID binding in
// the request workspace, keeping the sqlite backend's semantics: ON CONFLICT
// makes a rebind (the bridge's /new path) overwrite the prior session id in one
// statement (.agents/spec/parity/messaging-bridge.md, decision 1). The
// workspace stamp keeps a chat bound in one tenant invisible from another.
func (s *Store) BindBridgeSession(ctx context.Context, transport, chatKey, sessionID string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO bridge_sessions (workspace_id, transport, chat_key, session_id, updated_at)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (workspace_id, transport, chat_key) DO UPDATE SET
		     session_id = excluded.session_id,
		     updated_at = excluded.updated_at`,
		store.WorkspaceFromContext(ctx), transport, chatKey, sessionID, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("bind bridge session %s/%s: %w", transport, chatKey, err)
	}

	return nil
}

// LookupBridgeSession returns the bound session id for (transport, chatKey) in
// the request workspace, or ErrBridgeSessionNotFound when no binding exists,
// matching the sqlite backend. The workspace scope keeps a chat bound in another
// tenant reading as not-found rather than leaking its session id.
func (s *Store) LookupBridgeSession(ctx context.Context, transport, chatKey string) (string, error) {
	var sessionID string

	err := s.pool.QueryRow(ctx,
		"SELECT session_id FROM bridge_sessions WHERE workspace_id = $1 AND transport = $2 AND chat_key = $3",
		store.WorkspaceFromContext(ctx), transport, chatKey).Scan(&sessionID)
	if isNoRows(err) {
		return "", fmt.Errorf("%w: %s/%s", store.ErrBridgeSessionNotFound, transport, chatKey)
	}

	if err != nil {
		return "", fmt.Errorf("lookup bridge session %s/%s: %w", transport, chatKey, err)
	}

	return sessionID, nil
}
