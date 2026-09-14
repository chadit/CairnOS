package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/chadit/CairnOS/internal/store"
)

// chatSessionColumns is the SELECT list chat-session queries share, in
// scanChatSession order.
const chatSessionColumns = "id, workspace_id, personality, created_at, updated_at"

// CreateSession persists a new conversation (.agents/spec/13-roadmap.md,
// Phase 9), keeping identical defaulting to the sqlite backend: a zero
// timestamp becomes now and an empty WorkspaceID becomes the request
// workspace.
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

	_, err := s.pool.Exec(ctx,
		`INSERT INTO chat_sessions (id, workspace_id, personality, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		session.ID, workspace, session.Personality, createdAt.UTC(), updatedAt.UTC())
	if err != nil {
		return fmt.Errorf("insert chat session %s: %w", session.ID, err)
	}

	return nil
}

// GetSession fetches one session by id, scoped to the request workspace so a
// session in another workspace reads as not-found.
func (s *Store) GetSession(ctx context.Context, id string) (*store.ChatSession, error) {
	row := s.pool.QueryRow(ctx,
		"SELECT "+chatSessionColumns+" FROM chat_sessions WHERE id = $1 AND workspace_id = $2",
		id, store.WorkspaceFromContext(ctx))

	session, err := scanChatSession(row)
	if isNoRows(err) {
		return nil, fmt.Errorf("%w: %s", store.ErrChatSessionNotFound, id)
	}

	if err != nil {
		return nil, fmt.Errorf("get chat session %s: %w", id, err)
	}

	return session, nil
}

// ListSessions returns the request workspace's sessions, most-recently-updated
// first, with the id as tie-break for a stable order.
func (s *Store) ListSessions(ctx context.Context, filter store.ChatSessionFilter) ([]*store.ChatSession, error) {
	query := "SELECT " + chatSessionColumns +
		" FROM chat_sessions WHERE workspace_id = $1 ORDER BY updated_at DESC, id DESC"

	args := []any{store.WorkspaceFromContext(ctx)}

	if filter.Limit > 0 {
		query += " LIMIT $2"

		args = append(args, filter.Limit)
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list chat sessions: %w", err)
	}
	defer rows.Close()

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
// session's updated_at inside one transaction, the same shape the sqlite
// backend uses, so a concurrent append never claims the same seq.
func (s *Store) AppendMessage(ctx context.Context, message *store.ChatMessage) error {
	createdAt := message.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin append message tx: %w", err)
	}
	defer rollbackQuietly(ctx, tx)

	seq, err := nextChatSeq(ctx, tx, message.SessionID, store.WorkspaceFromContext(ctx))
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO chat_messages (session_id, seq, role, content, created_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		message.SessionID, seq, message.Role, message.Content, createdAt.UTC())
	if err != nil {
		return fmt.Errorf("insert chat message %s/%d: %w", message.SessionID, seq, err)
	}

	if _, err := tx.Exec(ctx,
		"UPDATE chat_sessions SET updated_at = $1 WHERE id = $2 AND workspace_id = $3",
		createdAt.UTC(), message.SessionID, store.WorkspaceFromContext(ctx)); err != nil {
		return fmt.Errorf("stamp chat session %s updated_at: %w", message.SessionID, err)
	}

	message.Seq = seq

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit append message: %w", err)
	}

	return nil
}

// nextChatSeq resolves the next 1-based sequence for one session, first proving
// the session exists in the request workspace so a cross-tenant append fails
// rather than orphaning a message.
func nextChatSeq(ctx context.Context, tx pgx.Tx, sessionID, workspace string) (int, error) {
	var present int

	err := tx.QueryRow(ctx,
		"SELECT 1 FROM chat_sessions WHERE id = $1 AND workspace_id = $2",
		sessionID, workspace).Scan(&present)
	if isNoRows(err) {
		return 0, fmt.Errorf("%w: %s", store.ErrChatSessionNotFound, sessionID)
	}

	if err != nil {
		return 0, fmt.Errorf("check chat session %s: %w", sessionID, err)
	}

	var maxSeq *int

	if err := tx.QueryRow(ctx,
		"SELECT MAX(seq) FROM chat_messages WHERE session_id = $1", sessionID).Scan(&maxSeq); err != nil {
		return 0, fmt.Errorf("next chat seq for %s: %w", sessionID, err)
	}

	if maxSeq == nil {
		return 1, nil
	}

	return *maxSeq + 1, nil
}

// ListMessages returns one session's messages ordered by seq, scoped to the
// request workspace.
func (s *Store) ListMessages(ctx context.Context, sessionID string) ([]*store.ChatMessage, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT m.session_id, m.seq, m.role, m.content, m.created_at
		 FROM chat_messages m
		 JOIN chat_sessions s ON s.id = m.session_id
		 WHERE m.session_id = $1 AND s.workspace_id = $2
		 ORDER BY m.seq`,
		sessionID, store.WorkspaceFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("list chat messages for %s: %w", sessionID, err)
	}
	defer rows.Close()

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

// SearchMessages runs tsvector search over the workspace's transcripts, best
// match first with (session_id, seq) as the tie-break, matching the sqlite
// backend's FTS5 semantics. plainto_tsquery treats input as plain words, the
// injection safety ftsPhrase gives sqlite, and 'simple' matches
// internal/memory. The doubled session parameter keeps that filter optional
// without runtime SQL assembly; a NULL limit means "all".
func (s *Store) SearchMessages(ctx context.Context, query store.ChatSearchQuery) ([]*store.ChatMessage, error) {
	// The sqlite backend returns nothing on empty text before touching SQL;
	// short-circuiting here keeps the backends behaviorally identical.
	terms := strings.TrimSpace(query.Text)
	if terms == "" {
		return nil, nil
	}

	rows, err := s.pool.Query(ctx,
		`SELECT m.session_id, m.seq, m.role, m.content, m.created_at
		 FROM chat_messages m
		 JOIN chat_sessions s ON s.id = m.session_id
		 WHERE to_tsvector('simple', m.content) @@ plainto_tsquery('simple', $1)
		   AND s.workspace_id = $2
		   AND ($3 = '' OR m.session_id = $3)
		 ORDER BY ts_rank(to_tsvector('simple', m.content), plainto_tsquery('simple', $1)) DESC,
		          m.session_id, m.seq
		 LIMIT $4`,
		terms, store.WorkspaceFromContext(ctx), query.SessionID, limitOrNull(query.Limit))
	if err != nil {
		return nil, fmt.Errorf("search chat messages: %w", err)
	}

	return collectRows(rows, "search chat messages", scanChatMessage)
}

// limitOrNull maps "no limit requested" onto a NULL LIMIT (all rows).
func limitOrNull(limit int) any {
	if limit <= 0 {
		return nil
	}

	return limit
}

// scanChatSession reads one chatSessionColumns row back into a ChatSession.
func scanChatSession(row rowScanner) (*store.ChatSession, error) {
	var (
		session   store.ChatSession
		createdAt time.Time
		updatedAt time.Time
	)

	err := row.Scan(&session.ID, &session.WorkspaceID, &session.Personality, &createdAt, &updatedAt)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with their operation and session id
	}

	session.CreatedAt = createdAt
	session.UpdatedAt = updatedAt

	return &session, nil
}

// scanChatMessage reads one chat_messages row back into a ChatMessage.
func scanChatMessage(row rowScanner) (*store.ChatMessage, error) {
	var (
		message   store.ChatMessage
		createdAt time.Time
	)

	err := row.Scan(&message.SessionID, &message.Seq, &message.Role, &message.Content, &createdAt)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with their operation and session id
	}

	message.CreatedAt = createdAt

	return &message, nil
}
