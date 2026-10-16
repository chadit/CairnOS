package auth_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/store"
)

// testLabel names every key minted by these tests.
const testLabel = "test-key"

// Table case names shared across the key and token tests.
const (
	caseEmpty         = "empty"
	caseMissingSecret = "missing secret"
)

// baseTime anchors created_at stamps; the value is arbitrary but fixed so
// assertions never depend on the wall clock.
var baseTime = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) //nolint:gochecknoglobals // shared immutable test anchor, never mutated

func TestNewKeyRoundTrip(t *testing.T) {
	t.Parallel()

	key, plaintext, err := auth.NewKey(testLabel, []string{auth.ScopeReadRun}, "", baseTime)
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}

	if key.ID == "" || key.Label != testLabel || !key.Enabled {
		t.Fatalf("key fields wrong: %+v", key)
	}

	if strings.Contains(key.Hash, plaintext) {
		t.Fatal("stored hash contains the plaintext")
	}

	keyID, secret, err := auth.SplitKey(plaintext)
	if err != nil {
		t.Fatalf("SplitKey(%q): %v", plaintext, err)
	}

	if keyID != key.ID {
		t.Fatalf("plaintext key id = %q, want %q", keyID, key.ID)
	}

	if err := auth.VerifyKey(secret, key.Hash); err != nil {
		t.Fatalf("VerifyKey with the right secret: %v", err)
	}
}

func TestVerifyKeyRejectsWrongSecret(t *testing.T) {
	t.Parallel()

	key, _, err := auth.NewKey(testLabel, []string{auth.ScopeReadRun}, "", baseTime)
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}

	if err := auth.VerifyKey("not-the-secret", key.Hash); !errors.Is(err, auth.ErrKeyMismatch) {
		t.Fatalf("VerifyKey error = %v, want ErrKeyMismatch", err)
	}
}

// TestNewKeyWorkspace pins the multi-tenancy field: a named workspace is kept,
// and an empty one defaults to store.DefaultWorkspace, so an access-key login
// always carries a workspace and the store-level default matches the auth
// default (.agents/spec/12-ui-and-governance.md, "Multi-tenancy").
func TestNewKeyWorkspace(t *testing.T) {
	t.Parallel()

	scoped, _, err := auth.NewKey(testLabel, []string{auth.ScopeReadRun}, "acme", baseTime)
	if err != nil {
		t.Fatalf("NewKey scoped: %v", err)
	}

	if scoped.Workspace != "acme" {
		t.Fatalf("named workspace = %q, want %q", scoped.Workspace, "acme")
	}

	defaulted, _, err := auth.NewKey(testLabel, []string{auth.ScopeReadRun}, "", baseTime)
	if err != nil {
		t.Fatalf("NewKey default: %v", err)
	}

	if defaulted.Workspace != store.DefaultWorkspace {
		t.Fatalf("empty workspace defaulted to %q, want %q", defaulted.Workspace, store.DefaultWorkspace)
	}
}

func TestVerifyKeyRejectsMalformedHash(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		hash string
	}{
		{name: caseEmpty, hash: ""},
		{name: "not phc", hash: "plainhash"},
		{name: "wrong algorithm", hash: "$argon2i$v=19$m=65536,t=1,p=4$c2FsdA$aGFzaA"},
		{name: "bad salt encoding", hash: "$argon2id$v=19$m=65536,t=1,p=4$!!!$aGFzaA"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := auth.VerifyKey("secret", tt.hash); !errors.Is(err, auth.ErrHashMalformed) {
				t.Fatalf("VerifyKey error = %v, want ErrHashMalformed", err)
			}
		})
	}
}

// TestSplitKeyKeepsUnderscoredSecrets pins the regression where a base64url
// secret containing the "_" separator was split apart and rejected.
func TestSplitKeyKeepsUnderscoredSecrets(t *testing.T) {
	t.Parallel()

	keyID, secret, err := auth.SplitKey("ck_01HWID_se_cr_et")
	if err != nil {
		t.Fatalf("SplitKey: %v", err)
	}

	if keyID != "01HWID" || secret != "se_cr_et" {
		t.Fatalf("SplitKey = (%q, %q), want (01HWID, se_cr_et)", keyID, secret)
	}
}

func TestSplitKeyRejectsMalformed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		plaintext string
	}{
		{name: caseEmpty, plaintext: ""},
		{name: "no separators", plaintext: "justonechunk"},
		{name: "wrong prefix", plaintext: "xx_id_secret"},
		{name: caseMissingSecret, plaintext: "ck_id_"},
		{name: "missing id", plaintext: "ck__secret"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, _, err := auth.SplitKey(tt.plaintext); !errors.Is(err, auth.ErrKeyMalformed) {
				t.Fatalf("SplitKey(%q) error = %v, want ErrKeyMalformed", tt.plaintext, err)
			}
		})
	}
}

func TestParseScopes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{name: "single", raw: "read:run", want: []string{auth.ScopeReadRun}},
		{
			name: "list with spaces and duplicate",
			raw:  " read:memory, write:workflow ,read:memory",
			want: []string{auth.ScopeReadMemory, auth.ScopeWriteWorkflow},
		},
		{name: auth.ScopeAdmin, raw: "admin", want: []string{auth.ScopeAdmin}},
		// message:send has to clear ValidScope, or `cairnos keys add
		// --scopes message:send` refuses the only key the send path accepts.
		{name: "message send", raw: "message:send", want: []string{auth.ScopeMessageSend}},
		{name: "unknown scope", raw: "read:everything", wantErr: true},
		{name: "typo in valid list", raw: "read:run,write:wrkflow", wantErr: true},
		{name: "empty entries only", raw: " , ,", want: nil},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := auth.ParseScopes(tt.raw)

			if tt.wantErr {
				if !errors.Is(err, auth.ErrScopeUnknown) {
					t.Fatalf("ParseScopes(%q) error = %v, want ErrScopeUnknown", tt.raw, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("ParseScopes(%q): %v", tt.raw, err)
			}

			if !slices.Equal(got, tt.want) {
				t.Fatalf("ParseScopes(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestHasScope(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		required string
		granted  []string
		want     bool
	}{
		{name: "exact match", granted: []string{auth.ScopeReadRun}, required: auth.ScopeReadRun, want: true},
		{name: "missing", granted: []string{auth.ScopeReadRun}, required: auth.ScopeWriteWorkflow, want: false},
		{name: "admin implies all", granted: []string{auth.ScopeAdmin}, required: auth.ScopeWriteMemory, want: true},
		{name: "empty grant", granted: nil, required: auth.ScopeReadRun, want: false},
		{
			name:     "read does not imply write",
			granted:  []string{auth.ScopeReadMemory},
			required: auth.ScopeWriteMemory,
			want:     false,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := auth.HasScope(tt.granted, tt.required); got != tt.want {
				t.Fatalf("HasScope(%v, %q) = %t, want %t", tt.granted, tt.required, got, tt.want)
			}
		})
	}
}
