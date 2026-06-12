package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/chadit/CairnOS/internal/store"
)

// chatSessionColumns is the SELECT list chat-session queries share, in
// scanChatSession order.
const chatSessionColumns = "id, workspace_id, personality, created_at, updated_at"

// CreateSession persists a new conversation (.agents/spec/13-roadmap.md,
// Phase 9). A zero timestamp is stamped with the current time and an empty
// WorkspaceID becomes the request workspace, so the row lands in the caller's
// tenant.
func (s *Store) CreateSession(ctx context.Context, session *store.ChatSession) error {
	workspace := session.WorkspaceID
	if workspace == "" {
		workspace = store.WorkspaceFromContext(ctx)
	}

	now := time.Now()

	createdAt := session.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}

	updatedAt := session.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO chat_sessions (id, workspace_id, personality, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		session.ID, workspace, session.Personality, formatTime(createdAt), formatTime(updatedAt))
	if err != nil {
		return fmt.Errorf("insert chat session %s: %w", session.ID, err)
	}

	return nil
}

// GetSession fetches one session by id, scoped to the request workspace so a
// session in another workspace reads as not-found rather than leaking across
// the tenancy boundary (.agents/spec/12-ui-and-governance.md, "Multi-tenancy").
func (s *Store) GetSession(ctx context.Context, id string) (*store.ChatSession, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+chatSessionColumns+" FROM chat_sessions WHERE id = ? AND workspace_id = ?",
		id, store.WorkspaceFromContext(ctx))

	session, err := scanChatSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", store.ErrChatSessionNotFound, id)
	}

	if err != nil {
		return nil, fmt.Errorf("get chat session %s: %w", id, err)
	}

	return session, nil
}

// ListSessions returns the request workspace's sessions, most-recently-updated
// first, with the session id as tie-break so two sessions touched in the same
// instant still list in a stable order.
func (s *Store) ListSessions(ctx context.Context, filter store.ChatSessionFilter) ([]*store.ChatSession, error) {
	query := "SELECT " + chatSessionColumns +
		" FROM chat_sessions WHERE workspace_id = ? ORDER BY updated_at DESC, id DESC"

	args := []any{store.WorkspaceFromContext(ctx)}

	if filter.Limit > 0 {
		query += limitClause

		args = append(args, filter.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list chat sessions: %w", err)
	}
	defer closeQuietly(rows)

	var sessions []*store.ChatSession

	for rows.Next() {
		session, scanErr := scanChatSession(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("list chat sessions: %w", scanErr)
		}

		sessions = append(sessions, session)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list chat sessions: %w", err)
	}

	return sessions, nil
}

// AppendMessage records one message at the next sequence and stamps the
// session's updated_at. The insert computes the next seq in one statement, a
// SELECT over the workspace-scoped session row, so it never reads-then-writes
// across a transaction boundary: the single write statement serializes against
// the SQLite writer lock, which is what keeps two concurrent appends from
// claiming the same seq without a deferred-to-immediate lock upgrade that races
// under WAL. A zero rowsAffected means the session does not exist in the
// request workspace, which is the cross-tenant or missing-session case.
func (s *Store) AppendMessage(ctx context.Context, message *store.ChatMessage) error {
	createdAt := message.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	workspace := store.WorkspaceFromContext(ctx)

	result, err := s.db.ExecContext(ctx,
		`INSERT INTO chat_messages (session_id, seq, role, content, created_at)
		 SELECT s.id,
		        COALESCE((SELECT MAX(seq) FROM chat_messages WHERE session_id = s.id), 0) + 1,
		        ?, ?, ?
		 FROM chat_sessions s
		 WHERE s.id = ? AND s.workspace_id = ?`,
		message.Role, message.Content, formatTime(createdAt), message.SessionID, workspace)
	if err != nil {
		return fmt.Errorf("insert chat message for %s: %w", message.SessionID, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("insert chat message for %s: rows affected: %w", message.SessionID, err)
	}

	if affected == 0 {
		return fmt.Errorf("%w: %s", store.ErrChatSessionNotFound, message.SessionID)
	}

	if _, err := s.db.ExecContext(ctx,
		"UPDATE chat_sessions SET updated_at = ? WHERE id = ? AND workspace_id = ?",
		formatTime(createdAt), message.SessionID, workspace); err != nil {
		return fmt.Errorf("stamp chat session %s updated_at: %w", message.SessionID, err)
	}

	return nil
}

// ListMessages returns one session's messages ordered by seq, scoped to the
// request workspace so a token bound to one tenant never reads another's
// conversation.
func (s *Store) ListMessages(ctx context.Context, sessionID string) ([]*store.ChatMessage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.session_id, m.seq, m.role, m.content, m.created_at
		 FROM chat_messages m
		 JOIN chat_sessions s ON s.id = m.session_id
		 WHERE m.session_id = ? AND s.workspace_id = ?
		 ORDER BY m.seq`,
		sessionID, store.WorkspaceFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("list chat messages for %s: %w", sessionID, err)
	}
	defer closeQuietly(rows)

	var messages []*store.ChatMessage

	for rows.Next() {
		message, scanErr := scanChatMessage(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("list chat messages for %s: %w", sessionID, scanErr)
		}

		messages = append(messages, message)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list chat messages for %s: %w", sessionID, err)
	}

	return messages, nil
}

// scanChatSession reads one chatSessionColumns row back into a ChatSession.
func scanChatSession(row rowScanner) (*store.ChatSession, error) {
	var (
		session   store.ChatSession
		createdAt sql.NullTime
		updatedAt sql.NullTime
	)

	err := row.Scan(&session.ID, &session.WorkspaceID, &session.Personality, &createdAt, &updatedAt)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with their operation and session id
	}

	session.CreatedAt = parseTime(createdAt)
	session.UpdatedAt = parseTime(updatedAt)

	return &session, nil
}

// scanChatMessage reads one chat_messages row back into a ChatMessage.
func scanChatMessage(row rowScanner) (*store.ChatMessage, error) {
	var (
		message   store.ChatMessage
		createdAt sql.NullTime
	)

	err := row.Scan(&message.SessionID, &message.Seq, &message.Role, &message.Content, &createdAt)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with their operation and session id
	}

	message.CreatedAt = parseTime(createdAt)

	return &message, nil
}
