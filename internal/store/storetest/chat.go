package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/store"
)

// Chat conformance fixtures; goconst applies to library code in this repo's
// lint policy, so the repeated literals are named.
const (
	personalityAssistant = "assistant"
	personalityOps       = "ops"
	chatHello            = "hello there"
	chatReply            = "hi, how can I help"
	chatFollowUp         = "what is the budget"
)

// testChatSessionRoundTrip proves a session and its ordered messages persist
// and read back, the conversation-context-persisted contract Phase 9 rests on
// (.agents/spec/13-roadmap.md, Phase 9).
func testChatSessionRoundTrip(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()
	created := time.Date(2026, 6, 11, 9, 0, 0, microFixtureNS, time.UTC)

	sessionID := newID()

	err := db.CreateSession(ctx, &store.ChatSession{
		ID:          sessionID,
		Personality: personalityAssistant,
		CreatedAt:   created,
		UpdatedAt:   created,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	got, err := db.GetSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}

	if got.Personality != personalityAssistant {
		t.Errorf("personality = %q, want %q", got.Personality, personalityAssistant)
	}

	if got.WorkspaceID != store.DefaultWorkspace {
		t.Errorf("workspace = %q, want default", got.WorkspaceID)
	}

	if !got.CreatedAt.Equal(created) {
		t.Errorf("created_at = %v, want %v", got.CreatedAt, created)
	}

	appendMessage(t, db, sessionID, store.ChatRoleUser, chatHello)
	appendMessage(t, db, sessionID, store.ChatRoleAssistant, chatReply)
	appendMessage(t, db, sessionID, store.ChatRoleUser, chatFollowUp)

	messages, err := db.ListMessages(ctx, sessionID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}

	wantRoles := []string{store.ChatRoleUser, store.ChatRoleAssistant, store.ChatRoleUser}
	wantContent := []string{chatHello, chatReply, chatFollowUp}

	if len(messages) != len(wantRoles) {
		t.Fatalf("listed %d messages, want %d", len(messages), len(wantRoles))
	}

	for idx, message := range messages {
		if message.Seq != idx+1 {
			t.Errorf("message %d seq = %d, want %d: the store assigns ordered sequences", idx, message.Seq, idx+1)
		}

		if message.Role != wantRoles[idx] || message.Content != wantContent[idx] {
			t.Errorf("message %d = %s/%q, want %s/%q", idx, message.Role, message.Content, wantRoles[idx], wantContent[idx])
		}

		if message.CreatedAt.IsZero() {
			t.Errorf("message %d created_at not persisted", idx)
		}
	}
}

// testChatSessionNotFound proves GetSession and AppendMessage report the
// not-found sentinel for an unknown session, so callers classify it instead
// of seeing a silent empty answer.
func testChatSessionNotFound(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	if _, err := db.GetSession(ctx, "missing"); !errors.Is(err, store.ErrChatSessionNotFound) {
		t.Errorf("GetSession error = %v, want ErrChatSessionNotFound", err)
	}

	appendErr := db.AppendMessage(ctx, &store.ChatMessage{
		SessionID: "missing",
		Role:      store.ChatRoleUser,
		Content:   chatHello,
	})
	if !errors.Is(appendErr, store.ErrChatSessionNotFound) {
		t.Errorf("AppendMessage error = %v, want ErrChatSessionNotFound", appendErr)
	}
}

// testChatSessionListNewestFirst proves ListSessions orders by updated_at,
// not creation, so appending to an older session floats it to the top: the
// "most recent conversation first" behavior a session picker needs.
func testChatSessionListNewestFirst(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()
	base := time.Date(2026, 6, 11, 8, 0, 0, 0, time.UTC)

	older := newID()
	newer := newID()

	seedSession(t, db, older, personalityAssistant, base)
	seedSession(t, db, newer, personalityOps, base.Add(time.Hour))

	// Touch the older session after the newer one was created; its updated_at
	// jumps ahead, so it must now list first.
	appendMessage(t, db, older, store.ChatRoleUser, chatHello)

	sessions, err := db.ListSessions(ctx, store.ChatSessionFilter{})
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}

	if len(sessions) != twoUnits {
		t.Fatalf("listed %d sessions, want 2", len(sessions))
	}

	if sessions[0].ID != older {
		t.Errorf("first session = %s, want %s: a touched session sorts newest by updated_at", sessions[0].ID, older)
	}

	limited, err := db.ListSessions(ctx, store.ChatSessionFilter{Limit: 1})
	if err != nil {
		t.Fatalf("list sessions with limit: %v", err)
	}

	if len(limited) != 1 || limited[0].ID != older {
		t.Errorf("limited sessions = %v, want the single newest %s", sessionIDs(limited), older)
	}
}

// testChatSessionIsolatesByWorkspace proves a session created in one workspace
// is invisible to another, the tenancy boundary every chat read honors
// (.agents/spec/12-ui-and-governance.md, "Multi-tenancy").
func testChatSessionIsolatesByWorkspace(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	base := time.Date(2026, 6, 11, 8, 0, 0, 0, time.UTC)

	acmeCtx := store.ContextWithWorkspace(t.Context(), workspaceAcme)
	globexCtx := store.ContextWithWorkspace(t.Context(), workspaceGlobex)

	acmeSession := newID()
	globexSession := newID()

	createSessionIn(t, db, acmeCtx, acmeSession, base)
	createSessionIn(t, db, globexCtx, globexSession, base.Add(time.Hour))

	acmeSessions, err := db.ListSessions(acmeCtx, store.ChatSessionFilter{})
	if err != nil {
		t.Fatalf("list acme sessions: %v", err)
	}

	if len(acmeSessions) != 1 || acmeSessions[0].ID != acmeSession {
		t.Fatalf("acme sessions = %v, want only %s", sessionIDs(acmeSessions), acmeSession)
	}

	if _, err := db.GetSession(acmeCtx, globexSession); !errors.Is(err, store.ErrChatSessionNotFound) {
		t.Fatalf("acme GetSession(globex session) error = %v, want ErrChatSessionNotFound", err)
	}

	// Acme cannot append to the globex session: the cross-tenant write fails
	// rather than orphaning a message under the wrong workspace.
	appendErr := db.AppendMessage(acmeCtx, &store.ChatMessage{
		SessionID: globexSession,
		Role:      store.ChatRoleUser,
		Content:   chatHello,
	})
	if !errors.Is(appendErr, store.ErrChatSessionNotFound) {
		t.Fatalf("acme AppendMessage(globex session) error = %v, want ErrChatSessionNotFound", appendErr)
	}
}

// appendMessage records one message and asserts it persisted, so the cases
// stay focused on ordering and content rather than error plumbing.
func appendMessage(t *testing.T, db store.Store, sessionID, role, content string) {
	t.Helper()

	err := db.AppendMessage(t.Context(), &store.ChatMessage{
		SessionID: sessionID,
		Role:      role,
		Content:   content,
	})
	if err != nil {
		t.Fatalf("append message to %s: %v", sessionID, err)
	}
}

// seedSession stages one session with the given id, personality, and creation
// time in the default workspace.
func seedSession(t *testing.T, db store.Store, id, personality string, created time.Time) {
	t.Helper()

	err := db.CreateSession(t.Context(), &store.ChatSession{
		ID:          id,
		Personality: personality,
		CreatedAt:   created,
		UpdatedAt:   created,
	})
	if err != nil {
		t.Fatalf("seed session %s: %v", id, err)
	}
}

// createSessionIn stages one session in the workspace ctx carries, stamping
// WorkspaceID the way the chat service does, so the isolation reads find the
// row in the right tenant.
//
//nolint:revive // t leads every helper here for the t.Fatalf calls; ctx follows to carry the workspace
func createSessionIn(t *testing.T, db store.Store, ctx context.Context, id string, created time.Time) {
	t.Helper()

	err := db.CreateSession(ctx, &store.ChatSession{
		ID:          id,
		WorkspaceID: store.WorkspaceFromContext(ctx),
		Personality: personalityAssistant,
		CreatedAt:   created,
		UpdatedAt:   created,
	})
	if err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
}

// sessionIDs extracts session ids for a failure message.
func sessionIDs(sessions []*store.ChatSession) []string {
	ids := make([]string, len(sessions))
	for i, session := range sessions {
		ids[i] = session.ID
	}

	return ids
}
