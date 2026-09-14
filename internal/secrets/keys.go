package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
)

// Environment variables that carry the store's key material. Secrets and keys
// come only from the environment or a file the operator points at, never the
// config file. These constants name variables, not key values.
const (
	// keyEnv holds the primary key, base64 standard, decoding to exactly 32 bytes.
	keyEnv = "CAIRNOS_SECRETS_KEY"
	// keyFileEnv points at a file holding the base64 primary key (trimmed).
	keyFileEnv = "CAIRNOS_SECRETS_KEY_FILE"
	// prevKeyEnv holds the previous key during a rotation window, same encoding.
	prevKeyEnv = "CAIRNOS_SECRETS_PREV_KEY"
	// prevKeyFileEnv points at a file holding the base64 previous key.
	prevKeyFileEnv = "CAIRNOS_SECRETS_PREV_KEY_FILE"
)

// groupOtherPermBits masks the group and other permission bits; a key file any
// of them can read is a leak risk, so LoadKeys refuses it.
const groupOtherPermBits = 0o077

// LoadKeys resolves the primary and optional previous key material for the
// encrypted store. getenv is injected so tests need no process environment. The
// primary is required (the store cannot seal without it); the previous key is
// present only during a rotation window. Every error names the offending
// variable and, for a wrong size, the length, never the key bytes.
func LoadKeys(getenv func(string) string) ([]byte, [][]byte, error) {
	primary, err := loadKeySlot(getenv, keyEnv, keyFileEnv)
	if err != nil {
		return nil, nil, err
	}

	if primary == nil {
		return nil, nil, fmt.Errorf("%w: set %s or %s", ErrKeyMissing, keyEnv, keyFileEnv)
	}

	prev, err := loadKeySlot(getenv, prevKeyEnv, prevKeyFileEnv)
	if err != nil {
		return nil, nil, err
	}

	if prev == nil {
		return primary, nil, nil
	}

	return primary, [][]byte{prev}, nil
}

// GenerateKey returns a fresh 32-byte AES-256 key from the OS CSPRNG, base64
// standard encoded for CAIRNOS_SECRETS_KEY. cairnos secrets keygen prints this.
// A hand-typed or passphrase-derived key passes LoadKeys' length check yet is
// insecure, since that check cannot see entropy. Key compromise loses the whole
// store; the previous-key window is the only partial mitigation.
func GenerateKey() (string, error) {
	key := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return "", fmt.Errorf("generate secrets key: %w", err)
	}

	return base64.StdEncoding.EncodeToString(key), nil
}

// loadKeySlot resolves one key slot from its value variable or its file variant.
// Setting both is a conflict; setting neither yields a nil key (the slot is
// absent), which LoadKeys treats as missing for the primary and as no-rotation
// for the previous slot.
func loadKeySlot(getenv func(string) string, valueEnv, fileEnv string) ([]byte, error) {
	value := getenv(valueEnv)
	path := getenv(fileEnv)

	switch {
	case value != "" && path != "":
		return nil, fmt.Errorf("%w: %s and %s", ErrKeySourceConflict, valueEnv, fileEnv)
	case value != "":
		return decodeKey(value, valueEnv)
	case path != "":
		return loadKeyFile(path, fileEnv)
	default:
		return nil, nil
	}
}

// decodeKey decodes a base64 key and checks its length. source names the origin
// variable for the error; neither the encoded nor the decoded bytes ride it.
func decodeKey(encoded, source string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrBadKeyEncoding, source)
	}

	if len(key) != keySize {
		return nil, fmt.Errorf("%w: %s decoded to %d bytes", ErrKeyWrongSize, source, len(key))
	}

	return key, nil
}

// loadKeyFile refuses a group- or world-readable file, then reads and decodes
// its base64 contents.
func loadKeyFile(path, source string) ([]byte, error) {
	if err := refuseOpenKeyFile(path); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path) // #nosec G304 -- the key file path is the operator's own environment configuration
	if err != nil {
		return nil, fmt.Errorf("read key file %s: %w", source, err)
	}

	return decodeKey(string(data), source)
}

// refuseOpenKeyFile rejects a key file readable by group or other; a
// world-readable master key undermines every secret sealed under it. The check
// is skipped on platforms whose Stat reports no Unix mode bits, where perm is
// always zero.
func refuseOpenKeyFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat key file: %w", err)
	}

	if info.Mode().Perm()&groupOtherPermBits != 0 {
		return fmt.Errorf("%w: %s (mode %#o); chmod 600 it", ErrKeyFilePermissive, path, info.Mode().Perm())
	}

	return nil
}
