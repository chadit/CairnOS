package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/chadit/CairnOS/internal/store"
)

// Bridge-session conformance fixtures; goconst applies to library code in this
// repo's lint policy, so the repeated literals are named. The chat keys are
// Telegram chat ids as strings, the shape the bridge stores.
const (
	transportTelegram = "telegram"
	bridgeChatAlice   = "111111111"
	bridgeChatBob     = "222222222"
	bridgeChatShared  = "555555555"
)

// testBridgeSessionRoundTrip proves a binding persists and reads back: the
// chat-to-session link the Telegram loop resolves per turn so a conversation
// survives a serve restart (.agents/spec/parity/messaging-bridge.md, decision
// 1).
func testBridgeSessionRoundTrip[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()
	sessionID := store.NewID()

	bindBridge(t, db, sessionID)

	got, err := db.LookupBridgeSession(ctx, transportTelegram, bridgeChatAlice)
	if err != nil {
		t.Fatalf("lookup bridge session: %v", err)
	}

	if got != sessionID {
		t.Errorf("bound session = %q, want %q", got, sessionID)
	}
}

// testBridgeSessionRebindUpserts proves the /new path: rebinding the same chat
// overwrites the prior session id rather than failing on the primary key, so a
// second lookup returns the fresh session.
func testBridgeSessionRebindUpserts[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	first := store.NewID()
	second := store.NewID()

	bindBridge(t, db, first)
	bindBridge(t, db, second)

	got, err := db.LookupBridgeSession(ctx, transportTelegram, bridgeChatAlice)
	if err != nil {
		t.Fatalf("lookup after rebind: %v", err)
	}

	if got != second {
		t.Errorf("rebound session = %q, want the second binding %q", got, second)
	}
}

// testBridgeSessionNotFound proves LookupBridgeSession reports the not-found
// sentinel for a chat with no binding, the signal the bridge reads as first
// contact so it creates and binds a session.
func testBridgeSessionNotFound[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	_, err := db.LookupBridgeSession(ctx, transportTelegram, absentID)
	if !errors.Is(err, store.ErrBridgeSessionNotFound) {
		t.Errorf("LookupBridgeSession error = %v, want ErrBridgeSessionNotFound", err)
	}
}

// testBridgeSessionIsolatesByWorkspace proves a binding in one workspace is
// invisible to another, the tenancy boundary the bridge stamps before every
// bind and lookup the way the gRPC interceptor stamps a token's workspace
// (.agents/spec/parity/messaging-bridge.md, decision 1). Two tenants can bind
// the same chat key independently: the (workspace_id, transport, chat_key) key
// keeps the sessions separate.
func testBridgeSessionIsolatesByWorkspace[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)

	acmeCtx := store.ContextWithWorkspace(t.Context(), workspaceAcme)
	globexCtx := store.ContextWithWorkspace(t.Context(), workspaceGlobex)

	acmeSession := store.NewID()
	globexSession := store.NewID()

	// Both tenants bind the same chat key; the sessions must not collide.
	bindBridgeIn(t, db, acmeCtx, bridgeChatShared, acmeSession)
	bindBridgeIn(t, db, globexCtx, bridgeChatShared, globexSession)

	// Acme binds a key globex never touches, to prove the cross-tenant miss.
	bindBridgeIn(t, db, acmeCtx, bridgeChatBob, store.NewID())

	acmeGot, err := db.LookupBridgeSession(acmeCtx, transportTelegram, bridgeChatShared)
	if err != nil {
		t.Fatalf("acme lookup shared chat: %v", err)
	}

	if acmeGot != acmeSession {
		t.Errorf("acme bound session = %q, want %q; globex binding leaked across the tenant boundary", acmeGot, acmeSession)
	}

	globexGot, err := db.LookupBridgeSession(globexCtx, transportTelegram, bridgeChatShared)
	if err != nil {
		t.Fatalf("globex lookup shared chat: %v", err)
	}

	if globexGot != globexSession {
		t.Errorf("globex bound session = %q, want %q", globexGot, globexSession)
	}

	// Globex cannot resolve a chat bound only in acme: it reads as first contact
	// across the boundary rather than leaking acme's session id.
	if _, err := db.LookupBridgeSession(globexCtx, transportTelegram, bridgeChatBob); !errors.Is(err, store.ErrBridgeSessionNotFound) {
		t.Fatalf("globex LookupBridgeSession(acme-only chat) error = %v, want ErrBridgeSessionNotFound", err)
	}
}

// bindBridge binds Alice's Telegram chat to sessionID in the default workspace
// and asserts it persisted, so the cases stay focused on lookup and rebind
// rather than error plumbing.
func bindBridge(t *testing.T, db store.Store, sessionID string) {
	t.Helper()

	if err := db.BindBridgeSession(t.Context(), transportTelegram, bridgeChatAlice, sessionID); err != nil {
		t.Fatalf("bind bridge session %s/%s: %v", transportTelegram, bridgeChatAlice, err)
	}
}

// bindBridgeIn binds one Telegram chat through the workspace ctx carries, so
// the isolation case's bindings land in the right tenant.
//
//nolint:revive // t leads every helper here for the t.Fatalf calls; ctx follows to carry the workspace
func bindBridgeIn(t *testing.T, db store.Store, ctx context.Context, chatKey, sessionID string) {
	t.Helper()

	if err := db.BindBridgeSession(ctx, transportTelegram, chatKey, sessionID); err != nil {
		t.Fatalf("bind bridge session %s/%s: %v", transportTelegram, chatKey, err)
	}
}
