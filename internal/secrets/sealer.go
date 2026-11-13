package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// keySize is the AES-256 key length in bytes. AES-256 because a secret store key
// is long-lived and its compromise loses the whole store, so the wider margin is
// worth the negligible cost, matching the hive sealer.
const keySize = 32

// keyIDLen is how many hex chars of a key's SHA-256 identify it on a row. A
// digest prefix, never the key, so it reveals nothing; long enough that two
// generations do not collide, short enough to stay compact. The id is what lets
// Open match a row to the right key during a rotation without trying each.
const keyIDLen = 16

// Sealer seals a value into an AES-256-GCM envelope and back. It seals under
// the primary key and opens under any loaded key, so a rotation window has both
// generations available.
//
// Unlike internal/hive/sealer.go, it binds workspace, name, and a recency stamp
// as the GCM additional authenticated data, so a row cannot be re-pointed at
// another secret, workspace, or older value.
type Sealer struct {
	primary    cipher.AEAD
	randReader io.Reader
	openers    map[string]cipher.AEAD
	primaryID  string
}

// deriveKeyID is the row id of a key: the hex prefix of its SHA-256. One-way, so
// the id on a row never leaks the key; stable, so the same key always maps to the
// same id across processes.
func deriveKeyID(key []byte) string {
	sum := sha256.Sum256(key)

	return hex.EncodeToString(sum[:])[:keyIDLen]
}

// newAEAD builds the AES-256-GCM AEAD for one key, rejecting any key that is not
// the 32 bytes AES-256 needs so a short or padded key fails loudly rather than
// silently downgrading.
func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("%w: got %d", ErrKeyWrongSize, len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets sealer: new cipher: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets sealer: new gcm: %w", err)
	}

	return aead, nil
}

// NewSealer builds a sealer that seals under primary and opens under primary or
// any previous key. During a rotation the operator promotes a new primary and
// keeps the old key in previous, so reads that match a row's key id still open
// until the rotation re-seals every row. Keys must be exactly 32 bytes.
func NewSealer(primary []byte, previous ...[]byte) (*Sealer, error) {
	primaryAEAD, err := newAEAD(primary)
	if err != nil {
		return nil, err
	}

	openers := map[string]cipher.AEAD{deriveKeyID(primary): primaryAEAD}

	for _, prev := range previous {
		prevAEAD, prevErr := newAEAD(prev)
		if prevErr != nil {
			return nil, prevErr
		}

		openers[deriveKeyID(prev)] = prevAEAD
	}

	return &Sealer{
		primaryID:  deriveKeyID(primary),
		primary:    primaryAEAD,
		openers:    openers,
		randReader: rand.Reader,
	}, nil
}

// PrimaryID reports the key id rows are re-sealed under during a rotation, so the
// store can tell which rows still belong to a previous key.
func (s *Sealer) PrimaryID() string {
	return s.primaryID
}

// Seal encrypts value under the primary key with a fresh nonce and returns the
// envelope fields the store persists: key id, nonce, and ciphertext (GCM appends
// its auth tag). version is the row's recency stamp, which the store binds as
// updated_at in UnixNano. Folding it into the AAD makes a rolled-back ciphertext
// for the same workspace and name fail Open.
func (s *Sealer) Seal(workspace, name string, version int64, value string) (string, []byte, []byte, error) {
	nonce := make([]byte, s.primary.NonceSize())
	if _, err := io.ReadFull(s.randReader, nonce); err != nil {
		return "", nil, nil, fmt.Errorf("secrets seal: nonce: %w", err)
	}

	ciphertext := s.primary.Seal(nil, nonce, []byte(value), aad(workspace, name, version))

	return s.primaryID, nonce, ciphertext, nil
}

// Open reverses Seal under the key keyID names, authenticating against the same
// workspace, name, and version. Any failure collapses to ErrOpenSealed so a
// caller never distinguishes an unknown key from a tampered row and never sees
// partial plaintext.
func (s *Sealer) Open(workspace, name string, version int64, keyID string, nonce, ciphertext []byte) (string, error) {
	aead, known := s.openers[keyID]
	if !known {
		return "", ErrOpenSealed
	}

	if len(nonce) != aead.NonceSize() {
		return "", ErrOpenSealed
	}

	plaintext, err := aead.Open(nil, nonce, ciphertext, aad(workspace, name, version))
	if err != nil {
		return "", ErrOpenSealed
	}

	return string(plaintext), nil
}

// aad builds the GCM additional authenticated data. Workspace and name are
// NUL-joined so "a"/"bc" cannot collide with "ab"/"c", and the name is
// normalized so a raw and a canonical name build the same AAD. The version tail
// means restoring an older ciphertext row fails the open on the recency
// mismatch.
func aad(workspace, name string, version int64) []byte {
	return []byte(workspace + "\x00" + NormalizeName(name) + "\x00" + strconv.FormatInt(version, 10))
}

// NormalizeName maps a secret reference to its canonical store form: lowercase,
// with each dash and dot replaced by an underscore, so secrets.db-password and
// secrets.db_password name one secret. The env source applies the same mapping
// before uppercasing for the variable name, so both sources agree on identity.
func NormalizeName(name string) string {
	return strings.NewReplacer("-", "_", ".", "_").Replace(strings.ToLower(name))
}
