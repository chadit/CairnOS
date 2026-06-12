// Package authtest is the shared behavioral spec for auth.Store
// implementations. RunConformance is one suite every backend runs against a
// fresh store: the sqlite backend calls it, and the postgres backend calls
// it when a test DSN is configured. One spec, two backends, so a key CRUD or
// denylist-window difference between them fails a test instead of going
// unnoticed (.agents/spec/08-auth-and-access.md).
package authtest

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/auth"
)

// Denylist fixture names shared across the table cases; unrelated stands in
// for any value with no denylist row.
const (
	deniedJTI        = "jti-1"
	deniedKey        = "key-1"
	unrelated        = "other"
	testLabel        = "ci-key"
	workspaceAcme    = "acme"
	defaultWorkspace = "default"

	// Window multipliers named so the suite reads as code rather than
	// tripping the magic-number check (this is library code, not _test.go).
	wantTwoKeys = 2
	twoHours    = 2
	threeHours  = 3
	fourHours   = 4
)

// baseTime anchors the denylist windows so expiry math needs no real clock.
//
//nolint:gochecknoglobals // shared immutable test anchor, never mutated
var baseTime = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

// OpenFunc hands the suite a fresh, empty auth store. The implementation
// registers cleanup (t.Cleanup with Close) so each subtest gets an isolated
// database. Returning the interface keeps the suite backend-agnostic.
type OpenFunc func(t *testing.T) auth.Store

// RunConformance runs the full auth.Store behavioral suite against the store
// openFn hands out. Each named subtest opens its own store, so they are
// independent and the caller may run the suite under t.Parallel.
func RunConformance(t *testing.T, openFn OpenFunc) {
	t.Helper()

	cases := []struct {
		name string
		run  func(t *testing.T, openFn OpenFunc)
	}{
		{"KeyCRUD", testKeyCRUD},
		{"KeyWorkspaceRoundTrip", testKeyWorkspaceRoundTrip},
		{"GetKeyNotFound", testGetKeyNotFound},
		{"CreateKeyRejectsDuplicateID", testCreateKeyRejectsDuplicateID},
		{"SetKeyEnabled", testSetKeyEnabled},
		{"IsDeniedWindow", testIsDeniedWindow},
		{"DenyExtendsButNeverShortens", testDenyExtendsButNeverShortens},
		{"RemoveKeyDenialsLiftsOnlySubRows", testRemoveKeyDenialsLiftsOnlySubRows},
		{"PruneDenylistDropsExpiredRows", testPruneDenylistDropsExpiredRows},
		{"IssuedExpRoundTrip", testIssuedExpRoundTrip},
		{"MaxIssuedExp", testMaxIssuedExp},
		{"DenyKeySinceSparesFreshTokens", testDenyKeySinceSparesFreshTokens},
		{"PruneDropsExpiredIssuedRows", testPruneDropsExpiredIssuedRows},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t, openFn)
		})
	}
}

// seedKey mints and stores one enabled key, returning it with its plaintext.
func seedKey(t *testing.T, store auth.Store, scopes []string) (*auth.Key, string) {
	t.Helper()

	key, plaintext, err := auth.NewKey(testLabel, scopes, "", baseTime)
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}

	if err := store.CreateKey(t.Context(), key); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	return key, plaintext
}

func testKeyCRUD(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)

	first, _ := seedKey(t, store, []string{auth.ScopeReadRun})
	second, _ := seedKey(t, store, []string{auth.ScopeAdmin})

	got, err := store.GetKey(t.Context(), first.ID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}

	if got.Label != first.Label || got.Hash != first.Hash || !got.Enabled {
		t.Fatalf("round trip lost fields: %+v", got)
	}

	if !slices.Equal(got.Scopes, first.Scopes) {
		t.Fatalf("scopes = %v, want %v", got.Scopes, first.Scopes)
	}

	keys, err := store.ListKeys(t.Context())
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}

	if len(keys) != wantTwoKeys {
		t.Fatalf("listed %d keys, want 2", len(keys))
	}

	_ = second
}

// testKeyWorkspaceRoundTrip pins the multi-tenancy column on the key store: a
// key created with a workspace reads it back, so login can mint the workspace
// claim, while an empty-workspace key reads back the default workspace
// (.agents/spec/12-ui-and-governance.md, "Multi-tenancy").
func testKeyWorkspaceRoundTrip(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)

	scoped, _, err := auth.NewKey(testLabel, []string{auth.ScopeReadRun}, workspaceAcme, baseTime)
	if err != nil {
		t.Fatalf("NewKey scoped: %v", err)
	}

	if err := store.CreateKey(t.Context(), scoped); err != nil {
		t.Fatalf("CreateKey scoped: %v", err)
	}

	got, err := store.GetKey(t.Context(), scoped.ID)
	if err != nil {
		t.Fatalf("GetKey scoped: %v", err)
	}

	if got.Workspace != workspaceAcme {
		t.Fatalf("workspace = %q, want %q", got.Workspace, workspaceAcme)
	}

	defaulted, _, err := auth.NewKey(testLabel, []string{auth.ScopeReadRun}, "", baseTime)
	if err != nil {
		t.Fatalf("NewKey default: %v", err)
	}

	if err := store.CreateKey(t.Context(), defaulted); err != nil {
		t.Fatalf("CreateKey default: %v", err)
	}

	gotDefault, err := store.GetKey(t.Context(), defaulted.ID)
	if err != nil {
		t.Fatalf("GetKey default: %v", err)
	}

	if gotDefault.Workspace != defaultWorkspace {
		t.Fatalf("default key workspace = %q, want %q", gotDefault.Workspace, defaultWorkspace)
	}
}

func testGetKeyNotFound(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)

	if _, err := store.GetKey(t.Context(), "absent"); !errors.Is(err, auth.ErrKeyNotFound) {
		t.Fatalf("GetKey error = %v, want ErrKeyNotFound", err)
	}
}

func testCreateKeyRejectsDuplicateID(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)
	key, _ := seedKey(t, store, []string{auth.ScopeReadRun})

	if err := store.CreateKey(t.Context(), key); err == nil {
		t.Fatal("duplicate CreateKey succeeded, want an error")
	}
}

func testSetKeyEnabled(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)
	key, _ := seedKey(t, store, []string{auth.ScopeReadRun})

	if err := store.SetKeyEnabled(t.Context(), key.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}

	got, err := store.GetKey(t.Context(), key.ID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}

	if got.Enabled {
		t.Fatal("key still enabled after disable")
	}

	if err := store.SetKeyEnabled(t.Context(), "absent", false); !errors.Is(err, auth.ErrKeyNotFound) {
		t.Fatalf("disable absent key error = %v, want ErrKeyNotFound", err)
	}
}

func testIsDeniedWindow(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)
	until := baseTime.Add(time.Hour)
	revokedAt := baseTime

	if err := store.DenyToken(t.Context(), deniedJTI, until); err != nil {
		t.Fatalf("DenyToken: %v", err)
	}

	if err := store.DenyKeySince(t.Context(), deniedKey, revokedAt, until); err != nil {
		t.Fatalf("DenyKeySince: %v", err)
	}

	// iat at or before the revoked-at instant is denied by a sub row; iat
	// strictly after it (a fresh token) passes (spec M3).
	preRevoke := revokedAt
	postRevoke := revokedAt.Add(time.Second)

	cases := []struct {
		name string
		jti  string
		sub  string
		iat  time.Time
		now  time.Time
		want bool
	}{
		{name: "live jti row denies", jti: deniedJTI, sub: unrelated, iat: postRevoke, now: baseTime, want: true},
		{name: "sub row denies pre-revoke token", jti: unrelated, sub: deniedKey, iat: preRevoke, now: baseTime, want: true},
		{name: "sub row spares fresh token", jti: unrelated, sub: deniedKey, iat: postRevoke, now: baseTime, want: false},
		{name: "unrelated token passes", jti: unrelated, sub: "another", iat: preRevoke, now: baseTime, want: false},
		{name: "expired row stops denying", jti: deniedJTI, sub: deniedKey, iat: preRevoke, now: until.Add(time.Second), want: false},
		{name: "jti checked against jti rows only", jti: deniedKey, sub: deniedJTI, iat: preRevoke, now: baseTime, want: false},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			denied, err := store.IsDenied(t.Context(), tt.jti, tt.sub, tt.iat, tt.now)
			if err != nil {
				t.Fatalf("IsDenied: %v", err)
			}

			if denied != tt.want {
				t.Fatalf("IsDenied(%q, %q) = %t, want %t", tt.jti, tt.sub, denied, tt.want)
			}
		})
	}
}

func testDenyExtendsButNeverShortens(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)
	long := baseTime.Add(twoHours * time.Hour)
	short := baseTime.Add(time.Hour)

	if err := store.DenyToken(t.Context(), "jti-x", long); err != nil {
		t.Fatalf("first DenyToken: %v", err)
	}

	// A second revocation with a shorter window must not shrink the first.
	if err := store.DenyToken(t.Context(), "jti-x", short); err != nil {
		t.Fatalf("second DenyToken: %v", err)
	}

	denied, err := store.IsDenied(t.Context(), "jti-x", "none", baseTime, short.Add(time.Minute))
	if err != nil {
		t.Fatalf("IsDenied: %v", err)
	}

	if !denied {
		t.Fatal("re-deny with a shorter window shortened the denial")
	}
}

func testRemoveKeyDenialsLiftsOnlySubRows(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)
	until := baseTime.Add(time.Hour)

	if err := store.DenyKeySince(t.Context(), deniedKey, baseTime, until); err != nil {
		t.Fatalf("DenyKeySince: %v", err)
	}

	if err := store.DenyToken(t.Context(), deniedJTI, until); err != nil {
		t.Fatalf("DenyToken: %v", err)
	}

	if err := store.RemoveKeyDenials(t.Context(), deniedKey); err != nil {
		t.Fatalf("RemoveKeyDenials: %v", err)
	}

	subDenied, err := store.IsDenied(t.Context(), "none", deniedKey, baseTime, baseTime)
	if err != nil {
		t.Fatalf("IsDenied sub: %v", err)
	}

	if subDenied {
		t.Fatal("sub denial survived RemoveKeyDenials")
	}

	jtiDenied, err := store.IsDenied(t.Context(), deniedJTI, "none", baseTime, baseTime)
	if err != nil {
		t.Fatalf("IsDenied jti: %v", err)
	}

	if !jtiDenied {
		t.Fatal("jti denial was removed by RemoveKeyDenials; it must only lift sub rows")
	}
}

func testPruneDenylistDropsExpiredRows(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)
	ctx := t.Context()

	expired := baseTime.Add(time.Hour)
	live := baseTime.Add(threeHours * time.Hour)

	if err := store.DenyToken(ctx, "jti-expired", expired); err != nil {
		t.Fatalf("DenyToken expired: %v", err)
	}

	if err := store.DenyKeySince(ctx, "key-live", baseTime, live); err != nil {
		t.Fatalf("DenyKeySince live: %v", err)
	}

	// Prune at a moment past the first window but before the second: only the
	// expired row goes.
	pruned, err := store.PruneDenylist(ctx, expired.Add(time.Minute))
	if err != nil {
		t.Fatalf("PruneDenylist: %v", err)
	}

	if pruned != 1 {
		t.Fatalf("pruned %d rows, want 1", pruned)
	}

	stillExpired, err := store.IsDenied(ctx, "jti-expired", "none", baseTime, baseTime)
	if err != nil {
		t.Fatalf("IsDenied expired after prune: %v", err)
	}

	if stillExpired {
		t.Fatal("expired row survived prune")
	}

	stillLive, err := store.IsDenied(ctx, "none", "key-live", baseTime, baseTime)
	if err != nil {
		t.Fatalf("IsDenied live after prune: %v", err)
	}

	if !stillLive {
		t.Fatal("prune dropped a live denylist row")
	}
}

// testIssuedExpRoundTrip records one token's exp and reads it back, and
// confirms an unknown jti reports not-found rather than an error (spec M2).
func testIssuedExpRoundTrip(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)
	ctx := t.Context()
	exp := baseTime.Add(threeHours * time.Hour)

	if err := store.RecordIssued(ctx, deniedJTI, deniedKey, exp); err != nil {
		t.Fatalf("RecordIssued: %v", err)
	}

	got, found, err := store.IssuedExp(ctx, deniedJTI)
	if err != nil {
		t.Fatalf("IssuedExp: %v", err)
	}

	if !found {
		t.Fatal("IssuedExp reported no record for a token just recorded")
	}

	if !got.Equal(exp) {
		t.Fatalf("IssuedExp = %v, want %v", got, exp)
	}

	_, found, err = store.IssuedExp(ctx, "no-such-jti")
	if err != nil {
		t.Fatalf("IssuedExp unknown: %v", err)
	}

	if found {
		t.Fatal("IssuedExp reported a record for an unknown jti")
	}
}

// testMaxIssuedExp confirms MaxIssuedExp returns the latest exp across a
// subject's tokens and reports not-found for a subject with none.
func testMaxIssuedExp(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)
	ctx := t.Context()

	early := baseTime.Add(time.Hour)
	late := baseTime.Add(fourHours * time.Hour)

	if err := store.RecordIssued(ctx, "jti-early", deniedKey, early); err != nil {
		t.Fatalf("RecordIssued early: %v", err)
	}

	if err := store.RecordIssued(ctx, "jti-late", deniedKey, late); err != nil {
		t.Fatalf("RecordIssued late: %v", err)
	}

	got, found, err := store.MaxIssuedExp(ctx, deniedKey)
	if err != nil {
		t.Fatalf("MaxIssuedExp: %v", err)
	}

	if !found {
		t.Fatal("MaxIssuedExp reported no records for a subject with tokens")
	}

	if !got.Equal(late) {
		t.Fatalf("MaxIssuedExp = %v, want the later exp %v", got, late)
	}

	_, found, err = store.MaxIssuedExp(ctx, "subject-with-no-tokens")
	if err != nil {
		t.Fatalf("MaxIssuedExp empty: %v", err)
	}

	if found {
		t.Fatal("MaxIssuedExp reported a record for a subject with no tokens")
	}
}

// testDenyKeySinceSparesFreshTokens is the M3 store invariant: a key
// revoked at an instant denies tokens minted at or before it but spares
// tokens minted after, so a still-enabled key keeps issuing usable tokens.
func testDenyKeySinceSparesFreshTokens(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)
	ctx := t.Context()

	revokedAt := baseTime.Add(twoHours * time.Hour)
	until := baseTime.Add(fourHours * time.Hour)

	if err := store.DenyKeySince(ctx, deniedKey, revokedAt, until); err != nil {
		t.Fatalf("DenyKeySince: %v", err)
	}

	preDenied, err := store.IsDenied(ctx, unrelated, deniedKey, revokedAt.Add(-time.Minute), baseTime)
	if err != nil {
		t.Fatalf("IsDenied pre-revoke: %v", err)
	}

	if !preDenied {
		t.Fatal("a token minted before the revoke instant slipped through")
	}

	freshDenied, err := store.IsDenied(ctx, unrelated, deniedKey, revokedAt.Add(time.Minute), baseTime)
	if err != nil {
		t.Fatalf("IsDenied fresh: %v", err)
	}

	if freshDenied {
		t.Fatal("a token minted after the revoke instant was denied; the key must keep minting")
	}
}

// testPruneDropsExpiredIssuedRows confirms PruneDenylist also clears expired
// issued-token records, so the M2 lookup table does not grow without bound.
func testPruneDropsExpiredIssuedRows(t *testing.T, openFn OpenFunc) {
	t.Helper()

	store := openFn(t)
	ctx := t.Context()

	expired := baseTime.Add(time.Hour)
	live := baseTime.Add(threeHours * time.Hour)

	if err := store.RecordIssued(ctx, "jti-expired", deniedKey, expired); err != nil {
		t.Fatalf("RecordIssued expired: %v", err)
	}

	if err := store.RecordIssued(ctx, "jti-live", deniedKey, live); err != nil {
		t.Fatalf("RecordIssued live: %v", err)
	}

	if _, err := store.PruneDenylist(ctx, expired.Add(time.Minute)); err != nil {
		t.Fatalf("PruneDenylist: %v", err)
	}

	if _, ok, err := store.IssuedExp(ctx, "jti-expired"); err != nil || ok {
		t.Fatalf("expired issued row survived prune (ok=%t, err=%v)", ok, err)
	}

	if _, ok, err := store.IssuedExp(ctx, "jti-live"); err != nil || !ok {
		t.Fatalf("prune dropped a live issued row (ok=%t, err=%v)", ok, err)
	}
}
