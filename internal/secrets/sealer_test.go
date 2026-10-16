package secrets_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/secrets"
)

const (
	testWorkspace = "default"
	testName      = "db_password"
	// testValue is distinctive so the hiding and no-leak checks can look for it
	// verbatim in ciphertext and error strings.
	testValue = "correct-horse-battery-staple"
	// dottedName is testName with a dot and capitals, which normalization must
	// fold to the same secret.
	dottedName = "DB.PASSWORD"
)

// randomKey returns 32 random bytes for an AES-256 sealer.
func randomKey(t *testing.T) []byte {
	t.Helper()

	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		t.Fatalf("generate key: %v", err)
	}

	return key
}

// newSealer builds a single-key sealer over a fresh random key.
func newSealer(t *testing.T) *secrets.Sealer {
	t.Helper()

	sealer, err := secrets.NewSealer(randomKey(t))
	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}

	return sealer
}

func TestSealOpenRoundTrip(t *testing.T) {
	t.Parallel()

	sealer := newSealer(t)

	keyID, nonce, ciphertext, err := sealer.Seal(testWorkspace, testName, 1, testValue)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if keyID != sealer.PrimaryID() {
		t.Fatalf("seal key id = %q, want the primary id %q", keyID, sealer.PrimaryID())
	}

	got, err := sealer.Open(testWorkspace, testName, 1, keyID, nonce, ciphertext)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if got != testValue {
		t.Fatalf("round-trip value = %q, want %q", got, testValue)
	}
}

// TestSealedCiphertextHidesValue proves the sealing hides the plaintext: the
// value never appears verbatim in the bytes the store persists.
func TestSealedCiphertextHidesValue(t *testing.T) {
	t.Parallel()

	_, nonce, ciphertext, err := newSealer(t).Seal(testWorkspace, testName, 1, testValue)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if bytes.Contains(ciphertext, []byte(testValue)) {
		t.Errorf("ciphertext carries the plaintext value verbatim")
	}

	if bytes.Contains(nonce, []byte(testValue)) {
		t.Errorf("nonce carries the plaintext value verbatim")
	}
}

func TestOpenRejectsTamperedCiphertext(t *testing.T) {
	t.Parallel()

	sealer := newSealer(t)

	keyID, nonce, ciphertext, err := sealer.Seal(testWorkspace, testName, 1, testValue)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	tampered := bytes.Clone(ciphertext)
	tampered[0] ^= 0xFF

	if _, err := sealer.Open(testWorkspace, testName, 1, keyID, nonce, tampered); !errors.Is(err, secrets.ErrOpenSealed) {
		t.Errorf("open tampered ciphertext = %v, want ErrOpenSealed", err)
	}
}

func TestOpenRejectsTamperedNonce(t *testing.T) {
	t.Parallel()

	sealer := newSealer(t)

	keyID, nonce, ciphertext, err := sealer.Seal(testWorkspace, testName, 1, testValue)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	tampered := bytes.Clone(nonce)
	tampered[0] ^= 0xFF

	if _, err := sealer.Open(testWorkspace, testName, 1, keyID, tampered, ciphertext); !errors.Is(err, secrets.ErrOpenSealed) {
		t.Errorf("open tampered nonce = %v, want ErrOpenSealed", err)
	}
}

// TestOpenRejectsSwappedWorkspaceOrName proves the AAD binding: a row cannot be
// re-pointed at another workspace or secret without the open failing.
func TestOpenRejectsSwappedWorkspaceOrName(t *testing.T) {
	t.Parallel()

	sealer := newSealer(t)

	keyID, nonce, ciphertext, err := sealer.Seal(testWorkspace, testName, 1, testValue)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if _, err := sealer.Open("other_workspace", testName, 1, keyID, nonce, ciphertext); !errors.Is(err, secrets.ErrOpenSealed) {
		t.Errorf("open under swapped workspace = %v, want ErrOpenSealed", err)
	}

	if _, err := sealer.Open(testWorkspace, "other_name", 1, keyID, nonce, ciphertext); !errors.Is(err, secrets.ErrOpenSealed) {
		t.Errorf("open under swapped name = %v, want ErrOpenSealed", err)
	}
}

// TestOpenRejectsRolledBackVersion is the security-council amendment 6 case: an
// attacker who restores an older ciphertext row for the same workspace and name,
// while the row's recency stamp has moved on, cannot open it. Opening under the
// version it was sealed at still works, proving the binding is version-exact and
// not a blanket rejection.
func TestOpenRejectsRolledBackVersion(t *testing.T) {
	t.Parallel()

	sealer := newSealer(t)

	oldKeyID, oldNonce, oldCiphertext, err := sealer.Seal(testWorkspace, testName, 100, "stale-value")
	if err != nil {
		t.Fatalf("seal old: %v", err)
	}

	// The row has since been re-sealed at version 200. Restoring the version-100
	// ciphertext but presenting the current version 200 must fail.
	if _, openErr := sealer.Open(testWorkspace, testName, 200, oldKeyID, oldNonce, oldCiphertext); !errors.Is(openErr, secrets.ErrOpenSealed) {
		t.Errorf("open rolled-back ciphertext = %v, want ErrOpenSealed", openErr)
	}

	got, err := sealer.Open(testWorkspace, testName, 100, oldKeyID, oldNonce, oldCiphertext)
	if err != nil {
		t.Fatalf("open at sealed version: %v", err)
	}

	if got != "stale-value" {
		t.Fatalf("open at sealed version = %q, want stale-value", got)
	}
}

func TestOpenRejectsUnknownKeyID(t *testing.T) {
	t.Parallel()

	keyID, nonce, ciphertext, err := newSealer(t).Seal(testWorkspace, testName, 1, testValue)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if _, err := newSealer(t).Open(testWorkspace, testName, 1, keyID, nonce, ciphertext); !errors.Is(err, secrets.ErrOpenSealed) {
		t.Errorf("open under a stranger key = %v, want ErrOpenSealed", err)
	}
}

func TestNewSealerRejectsWrongSizeKey(t *testing.T) {
	t.Parallel()

	if _, err := secrets.NewSealer(make([]byte, 16)); !errors.Is(err, secrets.ErrKeyWrongSize) {
		t.Errorf("short primary key = %v, want ErrKeyWrongSize", err)
	}

	if _, err := secrets.NewSealer(randomKey(t), make([]byte, 16)); !errors.Is(err, secrets.ErrKeyWrongSize) {
		t.Errorf("short previous key = %v, want ErrKeyWrongSize", err)
	}
}

// TestSealerOpensUnderPreviousKey proves the rotation window: a sealer holding a
// new primary plus the old key opens a row sealed under the old key, that row's
// key id is not the primary (so rotate knows to re-seal it), and a new-only
// sealer cannot open it.
func TestSealerOpensUnderPreviousKey(t *testing.T) {
	t.Parallel()

	oldKey := randomKey(t)
	newKey := randomKey(t)

	oldSealer, err := secrets.NewSealer(oldKey)
	if err != nil {
		t.Fatalf("old sealer: %v", err)
	}

	keyID, nonce, ciphertext, err := oldSealer.Seal(testWorkspace, testName, 1, testValue)
	if err != nil {
		t.Fatalf("seal under old key: %v", err)
	}

	ring, err := secrets.NewSealer(newKey, oldKey)
	if err != nil {
		t.Fatalf("rotating sealer: %v", err)
	}

	got, err := ring.Open(testWorkspace, testName, 1, keyID, nonce, ciphertext)
	if err != nil {
		t.Fatalf("ring open of a previous-key row: %v", err)
	}

	if got != testValue {
		t.Fatalf("ring open value = %q, want %q", got, testValue)
	}

	if keyID == ring.PrimaryID() {
		t.Fatalf("old row key id equals the primary id; rotate would not re-seal it")
	}

	newOnly, err := secrets.NewSealer(newKey)
	if err != nil {
		t.Fatalf("new-only sealer: %v", err)
	}

	if _, err := newOnly.Open(testWorkspace, testName, 1, keyID, nonce, ciphertext); !errors.Is(err, secrets.ErrOpenSealed) {
		t.Errorf("new-only open of an old-key row = %v, want ErrOpenSealed", err)
	}
}

// TestOpenNormalizesNameForAAD proves the AAD uses the canonical name, so a row
// sealed under one styling opens under any equivalent styling.
func TestOpenNormalizesNameForAAD(t *testing.T) {
	t.Parallel()

	sealer := newSealer(t)

	keyID, nonce, ciphertext, err := sealer.Seal(testWorkspace, "db-password", 1, testValue)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	for _, name := range []string{testName, dottedName, "DB-PASSWORD"} {
		got, err := sealer.Open(testWorkspace, name, 1, keyID, nonce, ciphertext)
		if err != nil || got != testValue {
			t.Errorf("open under %q = %q, %v; want %q", name, got, err, testValue)
		}
	}
}

// TestOpenErrorsCarryNoPlaintext is the no-secret-in-logs guarantee for the
// crypto core: every failure path collapses to ErrOpenSealed and no error string
// ever contains the sealed value.
func TestOpenErrorsCarryNoPlaintext(t *testing.T) {
	t.Parallel()

	sealer := newSealer(t)

	keyID, nonce, ciphertext, err := sealer.Seal(testWorkspace, testName, 1, testValue)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	tamperedCiphertext := bytes.Clone(ciphertext)
	tamperedCiphertext[0] ^= 0xFF

	cases := []struct {
		label      string
		workspace  string
		secret     string
		keyID      string
		nonce      []byte
		ciphertext []byte
		version    int64
	}{
		{label: "tampered ciphertext", workspace: testWorkspace, secret: testName, version: 1, keyID: keyID, nonce: nonce, ciphertext: tamperedCiphertext},
		{label: "swapped workspace", workspace: "other_workspace", secret: testName, version: 1, keyID: keyID, nonce: nonce, ciphertext: ciphertext},
		{label: "rolled-back version", workspace: testWorkspace, secret: testName, version: 999, keyID: keyID, nonce: nonce, ciphertext: ciphertext},
		{label: "unknown key id", workspace: testWorkspace, secret: testName, version: 1, keyID: "0000000000000000", nonce: nonce, ciphertext: ciphertext},
	}

	for _, probe := range cases {
		value, openErr := sealer.Open(probe.workspace, probe.secret, probe.version, probe.keyID, probe.nonce, probe.ciphertext)
		if !errors.Is(openErr, secrets.ErrOpenSealed) {
			t.Fatalf("%s: error = %v (value %q), want ErrOpenSealed", probe.label, openErr, value)
		}

		// Scan the rendered text for the forbidden substring; this asserts value
		// absence, not error identity, so it is not an errors.Is case.
		if message := openErr.Error(); strings.Contains(message, testValue) {
			t.Errorf("%s: error %q leaks the plaintext value", probe.label, message)
		}
	}
}
