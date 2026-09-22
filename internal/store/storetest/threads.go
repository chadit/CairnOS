package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/chadit/CairnOS/internal/store"
)

// Message-thread conformance fixtures: one identity posting to one channel under
// one key. The isolation case changes one part of that triple at a time.
const (
	threadIdentity = "ops"
	threadTarget   = "#usage"
	threadKey      = "usage:build_box"
)

// testMessageThreadRoundTrip proves a bound root reads back, which is what lets
// the next run reply in the thread the first one opened.
func testMessageThreadRoundTrip(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	rootID := store.NewID()

	bindThread(t.Context(), t, db, rootID)

	if got := lookupThread(t, db); got != rootID {
		t.Errorf("bound root = %q, want %q", got, rootID)
	}
}

// testMessageThreadRebindUpserts proves a replacement root overwrites the old
// one, the path the runner takes when the original root post was deleted.
func testMessageThreadRebindUpserts(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	second := store.NewID()

	bindThread(t.Context(), t, db, store.NewID())
	bindThread(t.Context(), t, db, second)

	if got := lookupThread(t, db); got != second {
		t.Errorf("rebound root = %q, want the second binding %q", got, second)
	}
}

// testMessageThreadIsolatesEveryKeyPart proves a root is found only under the
// exact identity, target, key, and workspace that bound it.
func testMessageThreadIsolatesEveryKeyPart(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	bindThread(store.ContextWithWorkspace(t.Context(), workspaceAcme), t, db, store.NewID())

	misses := []struct {
		name, workspace, identity, target, key string
	}{
		{name: "other workspace", workspace: workspaceGlobex, identity: threadIdentity, target: threadTarget, key: threadKey},
		{name: "other identity", workspace: workspaceAcme, identity: "deploy", target: threadTarget, key: threadKey},
		{name: "other target", workspace: workspaceAcme, identity: threadIdentity, target: "#alerts", key: threadKey},
		{name: "other key", workspace: workspaceAcme, identity: threadIdentity, target: threadTarget, key: "usage:other"},
	}

	for _, miss := range misses {
		ctx := store.ContextWithWorkspace(t.Context(), miss.workspace)

		_, err := db.LookupMessageThread(ctx, miss.identity, miss.target, miss.key)
		if !errors.Is(err, store.ErrMessageThreadNotFound) {
			t.Errorf("%s: LookupMessageThread error = %v, want ErrMessageThreadNotFound", miss.name, err)
		}
	}
}

// bindThread binds rootID under the fixture triple, failing the case on error so
// the cases stay about lookup.
func bindThread(ctx context.Context, t *testing.T, db store.Store, rootID string) {
	t.Helper()

	if err := db.BindMessageThread(ctx, threadIdentity, threadTarget, threadKey, rootID); err != nil {
		t.Fatalf("bind message thread %s: %v", threadKey, err)
	}
}

// lookupThread reads the fixture root, failing the case when it is absent.
func lookupThread(t *testing.T, db store.Store) string {
	t.Helper()

	rootID, err := db.LookupMessageThread(t.Context(), threadIdentity, threadTarget, threadKey)
	if err != nil {
		t.Fatalf("lookup message thread %s: %v", threadKey, err)
	}

	return rootID
}
