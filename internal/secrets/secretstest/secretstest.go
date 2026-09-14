// Package secretstest is the shared behavioral spec for secrets.Store
// implementations. RunConformance is one suite every backend runs against a
// fresh store: the sqlite backend calls it directly, and the postgres backend
// calls it through the integration suite when a container is available. One
// spec, two backends, so a sealing, normalization, or rotation difference
// between them fails a test instead of going unnoticed.
package secretstest

import (
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/secrets"
	"github.com/chadit/CairnOS/internal/store"
)

// Shared fixture values; goconst applies to this repo's non-test files and this
// package is one, so the repeated names and values live in constants.
const (
	secretName  = "db_password"
	secretValue = "correct-horse-battery-staple"
	otherName   = "api_key"
	otherValue  = "s3cr3t-token-material"
	styledName  = "db-password"
	wsAlpha     = "alpha"
	wsBeta      = "beta"
)

// keyBytes is the AES-256 key length the suite generates for each store.
const keyBytes = 32

// twoSecrets is the row count the list and rotate cases expect; a named
// constant keeps the magic-number check happy.
const twoSecrets = 2

// KeySet is the key material a store opens under. The suite hands different sets
// to the same underlying database so the rotation cases can seal under one key
// and reopen under another.
type KeySet struct {
	// Primary is the key new writes seal under.
	Primary []byte
	// Previous holds keys a rotation window still opens rows under.
	Previous [][]byte
}

// OpenFunc hands the suite a store backed by a database that persists across
// calls within one subtest, opened under keys. Taking keys is what lets a single
// subtest seal under one generation and reopen under another to exercise the
// previous-key window and rotate. The implementation registers cleanup (Close)
// so each subtest stays isolated.
type OpenFunc func(t *testing.T, keys KeySet) secrets.Store

// RunConformance runs the full secrets.Store behavioral suite against the store
// openFn hands out. Each named subtest keys its own database off t.Name, so they
// are independent and run in parallel.
func RunConformance(t *testing.T, openFn OpenFunc) {
	t.Helper()

	cases := []struct {
		run  func(t *testing.T, openFn OpenFunc)
		name string
	}{
		{name: "SetGetRoundTrip", run: testSetGetRoundTrip},
		{name: "OverwriteKeepsCreatedAt", run: testOverwriteKeepsCreatedAt},
		{name: "MissingIsNotFound", run: testMissingIsNotFound},
		{name: "ListIsMetadataOnly", run: testListIsMetadataOnly},
		{name: "WorkspaceIsolation", run: testWorkspaceIsolation},
		{name: "NameNormalization", run: testNameNormalization},
		{name: "RotateReSealsAndCounts", run: testRotateReSealsAndCounts},
		{name: "OpenUnderPreviousKey", run: testOpenUnderPreviousKey},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t, openFn)
		})
	}
}

// newKey returns 32 random bytes for an AES-256 sealer.
func newKey(t *testing.T) []byte {
	t.Helper()

	key := make([]byte, keyBytes)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		t.Fatalf("generate key: %v", err)
	}

	return key
}

// oneKey opens a store under a single fresh primary key, the setup the cases
// that never rotate share.
func oneKey(t *testing.T, openFn OpenFunc) secrets.Store {
	t.Helper()

	return openFn(t, KeySet{Primary: newKey(t)})
}

// set stages one secret, failing the test on error.
func set(t *testing.T, secretStore secrets.Store, name, value string) {
	t.Helper()

	if err := secretStore.Set(t.Context(), name, value); err != nil {
		t.Fatalf("set %q: %v", name, err)
	}
}

// mustGet reads one secret, failing the test on error.
func mustGet(t *testing.T, secretStore secrets.Store, name string) string {
	t.Helper()

	value, err := secretStore.Get(t.Context(), name)
	if err != nil {
		t.Fatalf("get %q: %v", name, err)
	}

	return value
}

func testSetGetRoundTrip(t *testing.T, openFn OpenFunc) {
	t.Helper()

	secretStore := oneKey(t, openFn)

	set(t, secretStore, secretName, secretValue)

	if got := mustGet(t, secretStore, secretName); got != secretValue {
		t.Fatalf("round-trip value = %q, want %q", got, secretValue)
	}

	if _, err := secretStore.Get(t.Context(), "never_set"); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("get missing = %v, want ErrNotFound", err)
	}
}

func testOverwriteKeepsCreatedAt(t *testing.T, openFn OpenFunc) {
	t.Helper()

	secretStore := oneKey(t, openFn)

	set(t, secretStore, secretName, secretValue)
	before := onlyMetadata(t, secretStore)

	set(t, secretStore, secretName, otherValue)
	after := onlyMetadata(t, secretStore)

	if got := mustGet(t, secretStore, secretName); got != otherValue {
		t.Errorf("overwritten value = %q, want %q", got, otherValue)
	}

	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("overwrite moved created_at from %v to %v", before.CreatedAt, after.CreatedAt)
	}

	// updated_at must not regress; on a real clock it advances, but two writes
	// inside one microsecond can tie, so equality is allowed.
	if after.UpdatedAt.Before(before.UpdatedAt) {
		t.Errorf("overwrite moved updated_at backward from %v to %v", before.UpdatedAt, after.UpdatedAt)
	}
}

func testMissingIsNotFound(t *testing.T, openFn OpenFunc) {
	t.Helper()

	secretStore := oneKey(t, openFn)

	if _, err := secretStore.Get(t.Context(), "ghost"); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("get missing = %v, want ErrNotFound", err)
	}

	if err := secretStore.Delete(t.Context(), "ghost"); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("delete missing = %v, want ErrNotFound", err)
	}
}

func testListIsMetadataOnly(t *testing.T, openFn OpenFunc) {
	t.Helper()

	secretStore := oneKey(t, openFn)

	set(t, secretStore, secretName, secretValue)
	set(t, secretStore, otherName, otherValue)

	metas, err := secretStore.List(t.Context())
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(metas) != twoSecrets {
		t.Fatalf("list returned %d rows, want %d", len(metas), twoSecrets)
	}

	for idx := range metas {
		meta := metas[idx]
		if meta.KeyID == "" || meta.CreatedAt.IsZero() || meta.UpdatedAt.IsZero() {
			t.Errorf("metadata %+v missing key id or timestamps", meta)
		}

		// The metadata carries no value field, so scan every rendered field for
		// the plaintext to prove a list can never leak a value.
		for _, field := range []string{meta.Name, meta.KeyID} {
			if strings.Contains(field, secretValue) || strings.Contains(field, otherValue) {
				t.Errorf("list metadata %q leaks a secret value", field)
			}
		}
	}
}

func testWorkspaceIsolation(t *testing.T, openFn OpenFunc) {
	t.Helper()

	secretStore := oneKey(t, openFn)
	alpha := store.ContextWithWorkspace(t.Context(), wsAlpha)
	beta := store.ContextWithWorkspace(t.Context(), wsBeta)

	if err := secretStore.Set(alpha, secretName, secretValue); err != nil {
		t.Fatalf("set in alpha: %v", err)
	}

	if _, err := secretStore.Get(beta, secretName); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("cross-workspace get = %v, want ErrNotFound", err)
	}

	metas, err := secretStore.List(beta)
	if err != nil {
		t.Fatalf("list in beta: %v", err)
	}

	if len(metas) != 0 {
		t.Errorf("beta list = %d rows, want 0 (alpha secret leaked across tenants)", len(metas))
	}

	if got, getErr := secretStore.Get(alpha, secretName); getErr != nil || got != secretValue {
		t.Errorf("alpha get = %q, %v; want %q", got, getErr, secretValue)
	}
}

func testNameNormalization(t *testing.T, openFn OpenFunc) {
	t.Helper()

	secretStore := oneKey(t, openFn)

	// db-password and db_password are one secret in both sources.
	set(t, secretStore, styledName, secretValue)

	for _, name := range []string{"db_password", "DB.PASSWORD", "DB-PASSWORD"} {
		if got := mustGet(t, secretStore, name); got != secretValue {
			t.Errorf("get %q = %q, want %q", name, got, secretValue)
		}
	}

	metas, err := secretStore.List(t.Context())
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(metas) != 1 || metas[0].Name != secretName {
		t.Errorf("normalized list = %+v, want one row named %q", metas, secretName)
	}
}

func testRotateReSealsAndCounts(t *testing.T, openFn OpenFunc) {
	t.Helper()

	keyA := newKey(t)
	keyB := newKey(t)

	under := openFn(t, KeySet{Primary: keyA})
	set(t, under, secretName, secretValue)
	set(t, under, otherName, otherValue)

	window := openFn(t, KeySet{Primary: keyB, Previous: [][]byte{keyA}})

	count, err := window.Rotate(t.Context())
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}

	if count != twoSecrets {
		t.Fatalf("rotate re-sealed %d rows, want %d", count, twoSecrets)
	}

	// A second rotate finds nothing under a non-primary key.
	again, err := window.Rotate(t.Context())
	if err != nil || again != 0 {
		t.Errorf("second rotate = %d, %v; want 0 rows re-sealed", again, err)
	}

	// With the previous key retired, both rows still open under the new primary.
	retired := openFn(t, KeySet{Primary: keyB})
	if got := mustGet(t, retired, secretName); got != secretValue {
		t.Errorf("post-rotate get %q = %q, want %q", secretName, got, secretValue)
	}

	if got := mustGet(t, retired, otherName); got != otherValue {
		t.Errorf("post-rotate get %q = %q, want %q", otherName, got, otherValue)
	}
}

func testOpenUnderPreviousKey(t *testing.T, openFn OpenFunc) {
	t.Helper()

	keyA := newKey(t)
	keyB := newKey(t)

	under := openFn(t, KeySet{Primary: keyA})
	set(t, under, secretName, secretValue)

	// A new primary with the old key still loaded opens the old-key row without a
	// rotate having run yet.
	window := openFn(t, KeySet{Primary: keyB, Previous: [][]byte{keyA}})
	if got := mustGet(t, window, secretName); got != secretValue {
		t.Errorf("open under previous key = %q, want %q", got, secretValue)
	}

	// The new primary alone cannot open the old-key row.
	newOnly := openFn(t, KeySet{Primary: keyB})
	if _, err := newOnly.Get(t.Context(), secretName); !errors.Is(err, secrets.ErrOpenSealed) {
		t.Errorf("new-primary-only get = %v, want ErrOpenSealed", err)
	}
}

// onlyMetadata reads the single row the caller just wrote, failing if the store
// holds a different number of rows.
func onlyMetadata(t *testing.T, secretStore secrets.Store) secrets.Metadata {
	t.Helper()

	metas, err := secretStore.List(t.Context())
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(metas) != 1 {
		t.Fatalf("list = %d rows, want exactly 1", len(metas))
	}

	return metas[0]
}
