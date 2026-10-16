package storetest

import (
	"context"
	"errors"
	"fmt"
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
	chatSystemFraming    = "you are a terse ops assistant"
)

// Search fixtures. The strong message repeats the term so it outranks the weak
// one under both bm25 and ts_rank; the weak message is byte-identical in two
// sessions so equal scores force the (session_id, seq) tie-break. The quoted
// and decoy pair catches an unsanitized FTS5 query treating a literal OR as an
// operator.
const (
	chatSearchTerm      = "budget"
	chatSearchStrong    = "budget review of the budget against the budget cap"
	chatSearchWeak      = "is the budget final"
	chatSearchMiss      = "nothing to see here"
	chatSearchQuoted    = `say "hello" OR else`
	chatSearchDecoy     = "else what"
	chatSearchOperators = `"hello" OR else`
)

// testChatSessionRoundTrip proves a session and its ordered messages persist
// and read back, the conversation-context-persisted contract Phase 9 rests on
// (.agents/spec/13-roadmap.md, Phase 9).
func testChatSessionRoundTrip[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()
	created := time.Date(2026, 6, 11, 9, 0, 0, microFixtureNS, time.UTC)

	sessionID := store.NewID()

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

// testChatSessionSeedsSystemRole proves a system-role message persists and
// reads back in sequence order alongside the user and assistant rows, the row
// the OpenAI-compatible adapter seeds when a client sends a `system` message
// (.agents/spec/parity/openai-compat.md). The role column is a plain string, so
// this is the store proving it accepts the value with no schema change.
func testChatSessionSeedsSystemRole[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()
	created := time.Date(2026, 6, 13, 9, 0, 0, microFixtureNS, time.UTC)

	sessionID := store.NewID()
	seedSession(t, db, sessionID, personalityAssistant, created)

	appendMessage(t, db, sessionID, store.ChatRoleSystem, chatSystemFraming)
	appendMessage(t, db, sessionID, store.ChatRoleUser, chatHello)
	appendMessage(t, db, sessionID, store.ChatRoleAssistant, chatReply)

	messages, err := db.ListMessages(ctx, sessionID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}

	wantRoles := []string{store.ChatRoleSystem, store.ChatRoleUser, store.ChatRoleAssistant}
	wantContent := []string{chatSystemFraming, chatHello, chatReply}

	if len(messages) != len(wantRoles) {
		t.Fatalf("listed %d messages, want %d", len(messages), len(wantRoles))
	}

	for idx, message := range messages {
		if message.Seq != idx+1 {
			t.Errorf("message %d seq = %d, want %d", idx, message.Seq, idx+1)
		}

		if message.Role != wantRoles[idx] || message.Content != wantContent[idx] {
			t.Errorf("message %d = %s/%q, want %s/%q", idx, message.Role, message.Content, wantRoles[idx], wantContent[idx])
		}
	}
}

// testChatSessionNotFound proves GetSession and AppendMessage report the
// not-found sentinel for an unknown session, so callers classify it instead
// of seeing a silent empty answer.
func testChatSessionNotFound[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	if _, err := db.GetSession(ctx, absentID); !errors.Is(err, store.ErrChatSessionNotFound) {
		t.Errorf("GetSession error = %v, want ErrChatSessionNotFound", err)
	}

	appendErr := db.AppendMessage(ctx, &store.ChatMessage{
		SessionID: absentID,
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
func testChatSessionListNewestFirst[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()
	base := time.Date(2026, 6, 11, 8, 0, 0, 0, time.UTC)

	older := store.NewID()
	newer := store.NewID()

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
func testChatSessionIsolatesByWorkspace[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	base := time.Date(2026, 6, 11, 8, 0, 0, 0, time.UTC)

	acmeCtx := store.ContextWithWorkspace(t.Context(), workspaceAcme)
	globexCtx := store.ContextWithWorkspace(t.Context(), workspaceGlobex)

	acmeSession := store.NewID()
	globexSession := store.NewID()

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

// testChatSearchRanksBestMatchFirst proves SearchMessages returns matching
// messages content-intact, best match first, with equal-relevance ties broken
// on (session_id, seq) so the order is stable across runs. bm25 and ts_rank
// disagree on scores, so ordering is only asserted where both rankers agree.
func testChatSearchRanksBestMatchFirst[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	sessionA, sessionB := seedChatSearchCorpus(t, db)

	hits, err := db.SearchMessages(t.Context(), store.ChatSearchQuery{Text: chatSearchTerm})
	if err != nil {
		t.Fatalf("search messages: %v", err)
	}

	assertChatHits(t, hits, []chatHit{
		{session: sessionA, seq: twoUnits, content: chatSearchStrong},
		{session: sessionA, seq: 1, content: chatSearchWeak},
		{session: sessionB, seq: 1, content: chatSearchWeak},
	})

	for idx, hit := range hits {
		if hit.CreatedAt.IsZero() {
			t.Errorf("hit %d created_at not populated", idx)
		}
	}
}

// testChatSearchFiltersBySession proves the SessionID filter narrows hits to
// one session, and that naming a missing session matches nothing rather than
// reporting not-found: it is a filter, not a lookup.
func testChatSearchFiltersBySession[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	_, sessionB := seedChatSearchCorpus(t, db)

	hits, err := db.SearchMessages(t.Context(), store.ChatSearchQuery{Text: chatSearchTerm, SessionID: sessionB})
	if err != nil {
		t.Fatalf("search messages with session filter: %v", err)
	}

	assertChatHits(t, hits, []chatHit{{session: sessionB, seq: 1, content: chatSearchWeak}})

	missing, err := db.SearchMessages(t.Context(), store.ChatSearchQuery{Text: chatSearchTerm, SessionID: absentID})
	if err != nil {
		t.Fatalf("search messages with missing session filter: %v", err)
	}

	if len(missing) != 0 {
		t.Errorf("missing-session filter returned %d hits, want 0", len(missing))
	}
}

// testChatSearchLimitsResults proves Limit caps the hit count from the top of
// the ranking while zero returns everything, the same contract as
// ChatSessionFilter.Limit.
func testChatSearchLimitsResults[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	sessionA, sessionB := seedChatSearchCorpus(t, db)

	limited, err := db.SearchMessages(t.Context(), store.ChatSearchQuery{Text: chatSearchTerm, Limit: twoUnits})
	if err != nil {
		t.Fatalf("search messages with limit: %v", err)
	}

	assertChatHits(t, limited, []chatHit{
		{session: sessionA, seq: twoUnits, content: chatSearchStrong},
		{session: sessionA, seq: 1, content: chatSearchWeak},
	})

	everything, err := db.SearchMessages(t.Context(), store.ChatSearchQuery{Text: chatSearchTerm})
	if err != nil {
		t.Fatalf("search messages without limit: %v", err)
	}

	assertChatHits(t, everything, []chatHit{
		{session: sessionA, seq: twoUnits, content: chatSearchStrong},
		{session: sessionA, seq: 1, content: chatSearchWeak},
		{session: sessionB, seq: 1, content: chatSearchWeak},
	})
}

// testChatSearchIsolatesByWorkspace proves a search from one workspace never
// sees another's messages, even when both hold the same matching content: the
// tenancy boundary every chat read honors
// (.agents/spec/12-ui-and-governance.md, "Multi-tenancy").
func testChatSearchIsolatesByWorkspace[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	base := time.Date(2026, 6, 12, 8, 0, 0, 0, time.UTC)

	acmeCtx := store.ContextWithWorkspace(t.Context(), workspaceAcme)
	globexCtx := store.ContextWithWorkspace(t.Context(), workspaceGlobex)

	acmeSession := store.NewID()
	globexSession := store.NewID()

	createSessionIn(t, db, acmeCtx, acmeSession, base)
	createSessionIn(t, db, globexCtx, globexSession, base)

	appendMessageIn(t, db, acmeCtx, acmeSession)
	appendMessageIn(t, db, globexCtx, globexSession)

	acmeHits, err := db.SearchMessages(acmeCtx, store.ChatSearchQuery{Text: chatSearchTerm})
	if err != nil {
		t.Fatalf("search acme messages: %v", err)
	}

	assertChatHits(t, acmeHits, []chatHit{{session: acmeSession, seq: 1, content: chatSearchWeak}})

	globexHits, err := db.SearchMessages(globexCtx, store.ChatSearchQuery{Text: chatSearchTerm})
	if err != nil {
		t.Fatalf("search globex messages: %v", err)
	}

	assertChatHits(t, globexHits, []chatHit{{session: globexSession, seq: 1, content: chatSearchWeak}})
}

// testChatSearchTreatsQuerySyntaxLiterally proves a query carrying quotes and
// an OR matches literally instead of smuggling FTS operators in. If the OR
// reached FTS5 as an operator, the decoy message (which only shares the last
// word) would come back too.
func testChatSearchTreatsQuerySyntaxLiterally[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	base := time.Date(2026, 6, 12, 8, 0, 0, 0, time.UTC)

	sessionID := store.NewID()
	seedSession(t, db, sessionID, personalityAssistant, base)

	appendMessage(t, db, sessionID, store.ChatRoleUser, chatSearchQuoted)
	appendMessage(t, db, sessionID, store.ChatRoleUser, chatSearchDecoy)

	hits, err := db.SearchMessages(t.Context(), store.ChatSearchQuery{Text: chatSearchOperators})
	if err != nil {
		t.Fatalf("search with operator syntax: %v", err)
	}

	assertChatHits(t, hits, []chatHit{{session: sessionID, seq: 1, content: chatSearchQuoted}})
}

// testChatSearchEmptyTextReturnsNothing proves an empty or blank query
// returns no rows and no error at the store. Rejecting it loudly is the API
// layer's job; the store just stays quiet.
func testChatSearchEmptyTextReturnsNothing[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	base := time.Date(2026, 6, 12, 8, 0, 0, 0, time.UTC)

	sessionID := store.NewID()
	seedSession(t, db, sessionID, personalityAssistant, base)
	appendMessage(t, db, sessionID, store.ChatRoleUser, chatSearchWeak)

	for _, text := range []string{"", "   "} {
		hits, err := db.SearchMessages(t.Context(), store.ChatSearchQuery{Text: text})
		if err != nil {
			t.Fatalf("search with blank text %q: %v", text, err)
		}

		if len(hits) != 0 {
			t.Errorf("blank text %q returned %d hits, want 0", text, len(hits))
		}
	}
}

// seedChatSearchCorpus stages the two-session search fixture: session A holds
// a weak match, a strong match, and a miss; session B holds one message
// byte-identical to A's weak match. It returns the session ids in creation
// order, which is also lexicographic order (ULIDs are monotonic), so callers
// can assert the (session_id, seq) tie-break.
func seedChatSearchCorpus(t *testing.T, db store.Store) (string, string) {
	t.Helper()

	base := time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC)

	sessionA := store.NewID()
	sessionB := store.NewID()

	seedSession(t, db, sessionA, personalityAssistant, base)
	seedSession(t, db, sessionB, personalityAssistant, base)

	appendMessage(t, db, sessionA, store.ChatRoleUser, chatSearchWeak)
	appendMessage(t, db, sessionA, store.ChatRoleAssistant, chatSearchStrong)
	appendMessage(t, db, sessionA, store.ChatRoleUser, chatSearchMiss)
	appendMessage(t, db, sessionB, store.ChatRoleUser, chatSearchWeak)

	return sessionA, sessionB
}

// chatHit is the session, sequence, and content identity of one expected search hit.
type chatHit struct {
	session string
	content string
	seq     int
}

// assertChatHits compares search results against the expected hits in order,
// failing with the full got/want shape so a ranking or tie-break regression
// reads directly from the failure message.
func assertChatHits(t *testing.T, got []*store.ChatMessage, want []chatHit) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("search returned %d hits %v, want %d %v", len(got), chatHitIDs(got), len(want), want)
	}

	for idx, hit := range want {
		message := got[idx]
		if message.SessionID != hit.session || message.Seq != hit.seq || message.Content != hit.content {
			t.Errorf("hit %d = %s/%d %q, want %s/%d %q",
				idx, message.SessionID, message.Seq, message.Content, hit.session, hit.seq, hit.content)
		}
	}
}

// chatHitIDs renders search hits as (session, seq) pairs for a failure message.
func chatHitIDs(messages []*store.ChatMessage) []string {
	ids := make([]string, len(messages))
	for i, message := range messages {
		ids[i] = fmt.Sprintf("%s/%d", message.SessionID, message.Seq)
	}

	return ids
}

// appendMessageIn records the weak-match user message through the workspace
// ctx carries, so the isolation case's messages land in the right tenant.
//
//nolint:revive // t leads every helper here for the t.Fatalf calls; ctx follows to carry the workspace
func appendMessageIn(t *testing.T, db store.Store, ctx context.Context, sessionID string) {
	t.Helper()

	err := db.AppendMessage(ctx, &store.ChatMessage{
		SessionID: sessionID,
		Role:      store.ChatRoleUser,
		Content:   chatSearchWeak,
	})
	if err != nil {
		t.Fatalf("append message to %s: %v", sessionID, err)
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
