package secrets

import "errors"

// Sentinel errors for the secret source, the encrypted store's crypto core,
// and its key material. Every one of these carries a secret name or an
// environment-variable name at most, never a secret value or key bytes, so a
// failure can be logged without leaking material.
var (
	// ErrNotFound reports a referenced secret with no value in the source. The
	// engine surfaces it as the canonical config.missing_secret error.
	ErrNotFound = errors.New("secret not found")

	// ErrOpenSealed reports any decrypt or authentication failure when opening a
	// sealed row: an unknown key id, a malformed nonce, a tampered ciphertext, or
	// an AAD mismatch (wrong workspace, name, or a rolled-back recency stamp).
	// Every case collapses here so a caller cannot distinguish them, and no
	// partial plaintext ever rides the error.
	ErrOpenSealed = errors.New("sealed secret did not open")

	// ErrKeyMissing reports the store backend selected but no primary key
	// configured. The names of both env vars ride the error; no key material does.
	ErrKeyMissing = errors.New("secrets store needs a primary key")

	// ErrKeySourceConflict reports both the value and the file variant set for one
	// key slot (primary or previous). The error names the two variables so the
	// operator can drop one, never their contents.
	ErrKeySourceConflict = errors.New("both key value and key file are set for one slot")

	// ErrBadKeyEncoding reports a key value that is not valid standard base64. The
	// error names the source variable, never the malformed value.
	ErrBadKeyEncoding = errors.New("secrets key must be base64-encoded 32 bytes")

	// ErrKeyWrongSize reports a decoded key that is not the 32 bytes AES-256 needs.
	// The error reports the wrong length, never the key bytes.
	ErrKeyWrongSize = errors.New("secrets key must decode to 32 bytes (AES-256)")

	// ErrKeyFilePermissive reports a key file readable by group or other. A
	// world-readable master key is a leak risk, so LoadKeys refuses it, mirroring
	// the auth key-file check.
	ErrKeyFilePermissive = errors.New("secrets key file is group- or world-readable")
)
