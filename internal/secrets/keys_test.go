package secrets_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/secrets"
)

const (
	keyEnvName     = "CAIRNOS_SECRETS_KEY"
	keyFileEnvName = "CAIRNOS_SECRETS_KEY_FILE"
	prevKeyEnvName = "CAIRNOS_SECRETS_PREV_KEY"
)

// fakeEnv builds a getenv reading from values, so LoadKeys tests need no process
// environment.
func fakeEnv(values map[string]string) func(string) string {
	return func(name string) string {
		return values[name]
	}
}

// b64Key returns a fresh random 32-byte key, base64 standard encoded.
func b64Key(t *testing.T) string {
	t.Helper()

	return base64.StdEncoding.EncodeToString(randomKey(t))
}

// writeKeyFile writes contents to a temp key file at 0600, then chmods it to the
// mode the test needs. Writing at 0600 keeps the writer off gosec's radar; the
// chmod is what sets up the permission the loader inspects.
func writeKeyFile(t *testing.T, contents string, mode os.FileMode) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "secrets.key")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}

	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod key file: %v", err)
	}

	return path
}

func TestLoadKeysReadsPrimaryFromEnv(t *testing.T) {
	t.Parallel()

	encoded := b64Key(t)

	primary, previous, err := secrets.LoadKeys(fakeEnv(map[string]string{keyEnvName: encoded}))
	if err != nil {
		t.Fatalf("load primary: %v", err)
	}

	if base64.StdEncoding.EncodeToString(primary) != encoded {
		t.Fatalf("primary key does not round-trip its base64 source")
	}

	if previous != nil {
		t.Fatalf("previous = %v, want nil without a rotation key", previous)
	}
}

// TestLoadKeysReadsPreviousDuringRotation proves the rotation window loads both
// generations and that the previous key opens rows sealed under it.
func TestLoadKeysReadsPreviousDuringRotation(t *testing.T) {
	t.Parallel()

	prevEncoded := b64Key(t)

	primary, previous, err := secrets.LoadKeys(fakeEnv(map[string]string{
		keyEnvName:     b64Key(t),
		prevKeyEnvName: prevEncoded,
	}))
	if err != nil {
		t.Fatalf("load rotation keys: %v", err)
	}

	if len(previous) != 1 {
		t.Fatalf("previous count = %d, want 1", len(previous))
	}

	prevBytes, err := base64.StdEncoding.DecodeString(prevEncoded)
	if err != nil {
		t.Fatalf("decode prev: %v", err)
	}

	prevSealer, err := secrets.NewSealer(prevBytes)
	if err != nil {
		t.Fatalf("prev sealer: %v", err)
	}

	keyID, nonce, ciphertext, err := prevSealer.Seal(testWorkspace, testName, 1, testValue)
	if err != nil {
		t.Fatalf("seal under prev: %v", err)
	}

	ring, err := secrets.NewSealer(primary, previous...)
	if err != nil {
		t.Fatalf("rotating sealer: %v", err)
	}

	got, err := ring.Open(testWorkspace, testName, 1, keyID, nonce, ciphertext)
	if err != nil || got != testValue {
		t.Fatalf("rotating open = %q, %v; want %q", got, err, testValue)
	}
}

func TestLoadKeysMissingPrimaryIsErrKeyMissing(t *testing.T) {
	t.Parallel()

	_, _, err := secrets.LoadKeys(fakeEnv(nil))
	if !errors.Is(err, secrets.ErrKeyMissing) {
		t.Fatalf("error = %v, want ErrKeyMissing", err)
	}
}

// TestLoadKeysConflictNamesBothVariables proves a slot with both the value and
// the file variant set fails, and the error names both variables (never a value).
func TestLoadKeysConflictNamesBothVariables(t *testing.T) {
	t.Parallel()

	_, _, err := secrets.LoadKeys(fakeEnv(map[string]string{
		keyEnvName:     b64Key(t),
		keyFileEnvName: filepath.Join(t.TempDir(), "unused.key"),
	}))
	if !errors.Is(err, secrets.ErrKeySourceConflict) {
		t.Fatalf("error = %v, want ErrKeySourceConflict", err)
	}

	message := err.Error()
	if !strings.Contains(message, keyEnvName+" and") || !strings.Contains(message, keyFileEnvName) {
		t.Errorf("conflict error %q does not name both variables", message)
	}
}

func TestLoadKeysBadBase64IsErrBadKeyEncoding(t *testing.T) {
	t.Parallel()

	badValue := "this is not base64!!!"

	_, _, err := secrets.LoadKeys(fakeEnv(map[string]string{keyEnvName: badValue}))
	if !errors.Is(err, secrets.ErrBadKeyEncoding) {
		t.Fatalf("error = %v, want ErrBadKeyEncoding", err)
	}

	if message := err.Error(); strings.Contains(message, badValue) {
		t.Errorf("bad-encoding error %q echoes the offending value", message)
	}
}

// TestLoadKeysWrongSizeReportsLengthNotBytes proves a decoded key of the wrong
// length is rejected and the error carries the length, never the key material.
func TestLoadKeysWrongSizeReportsLengthNotBytes(t *testing.T) {
	t.Parallel()

	shortKey := base64.StdEncoding.EncodeToString(make([]byte, shortKeySize))

	_, _, err := secrets.LoadKeys(fakeEnv(map[string]string{keyEnvName: shortKey}))
	if !errors.Is(err, secrets.ErrKeyWrongSize) {
		t.Fatalf("error = %v, want ErrKeyWrongSize", err)
	}

	if message := err.Error(); strings.Contains(message, shortKey) {
		t.Errorf("wrong-size error %q leaks the key value", message)
	}
}

// TestLoadKeysReadsPrimaryFromFile proves the file variant loads and that a
// trailing newline is trimmed.
func TestLoadKeysReadsPrimaryFromFile(t *testing.T) {
	t.Parallel()

	encoded := b64Key(t)
	path := writeKeyFile(t, encoded+"\n", 0o600)

	primary, previous, err := secrets.LoadKeys(fakeEnv(map[string]string{keyFileEnvName: path}))
	if err != nil {
		t.Fatalf("load from file: %v", err)
	}

	if base64.StdEncoding.EncodeToString(primary) != encoded {
		t.Fatalf("primary from file does not match; trailing newline not trimmed?")
	}

	if previous != nil {
		t.Fatalf("previous = %v, want nil", previous)
	}
}

func TestLoadKeysRefusesPermissiveFile(t *testing.T) {
	t.Parallel()

	path := writeKeyFile(t, b64Key(t), 0o644)

	_, _, err := secrets.LoadKeys(fakeEnv(map[string]string{keyFileEnvName: path}))
	if !errors.Is(err, secrets.ErrKeyFilePermissive) {
		t.Fatalf("error = %v, want ErrKeyFilePermissive", err)
	}
}

// TestGenerateKeyProducesLoadable32ByteKey proves keygen yields a fresh,
// base64-encoded 32-byte key that loads and builds a sealer.
func TestGenerateKeyProducesLoadable32ByteKey(t *testing.T) {
	t.Parallel()

	encoded, err := secrets.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("generated key is not valid base64: %v", err)
	}

	if len(decoded) != aesKeySize {
		t.Fatalf("generated key = %d bytes, want %d", len(decoded), aesKeySize)
	}

	second, err := secrets.GenerateKey()
	if err != nil {
		t.Fatalf("generate second key: %v", err)
	}

	if encoded == second {
		t.Fatalf("two keygen calls returned the same key")
	}

	primary, _, err := secrets.LoadKeys(fakeEnv(map[string]string{keyEnvName: encoded}))
	if err != nil {
		t.Fatalf("load generated key: %v", err)
	}

	if _, err := secrets.NewSealer(primary); err != nil {
		t.Fatalf("sealer from generated key: %v", err)
	}
}

// TestLoadKeysAcceptsLowEntropyKeyByLength documents a known hazard: LoadKeys
// checks length, never entropy, so a 32-byte
// all-'A' key loads even though it is insecure. cairnos secrets keygen exists so
// an operator never hand-picks a key like this.
func TestLoadKeysAcceptsLowEntropyKeyByLength(t *testing.T) {
	t.Parallel()

	lowEntropy := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("A"), aesKeySize))

	primary, _, err := secrets.LoadKeys(fakeEnv(map[string]string{keyEnvName: lowEntropy}))
	if err != nil {
		t.Fatalf("low-entropy 32-byte key rejected by length check: %v", err)
	}

	if len(primary) != aesKeySize {
		t.Fatalf("primary = %d bytes, want %d", len(primary), aesKeySize)
	}
}
