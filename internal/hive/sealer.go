package hive

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// keySize is the AES-256 key length in bytes. The shared hive key is the
// private cluster's privacy mechanism (R2/R3): the hive covers one operator's
// own nodes, so there is no untrusted participant to calibrate k-anonymity
// against, and a key the host and transport cannot read carries privacy
// instead. AES-256 because the key is long-lived and shared.
const keySize = 32

// keyIDLen is how many hex chars of a key's SHA-256 identify it on the wire. A
// digest prefix, never the key, so it reveals nothing; long enough that two
// cluster keys do not collide, short enough to stay compact. The id is what lets
// Open match an envelope to the right key during a rotation without trying each.
const keyIDLen = 16

// Category is safe in cleartext because it is a content-free enum from the fixed
// set, already on the allowlist. It is also bound into the ciphertext as
// authenticated data, so a host cannot re-file a tip under a different category
// without the open failing.

// SealedTip is the encrypted envelope that crosses the wire when a shared key
// is configured. Category stays cleartext so the hive can route a tip to its
// category subscribers without holding the key. Everything else about the tip
// lives inside Ciphertext, sealed under the shared key, so the host at rest and
// the transport see only opaque bytes.
type SealedTip struct {
	// KeyID is the digest id of the key that sealed this envelope, so a node
	// holding several keys during a rotation opens it under the right one.
	KeyID string `json:"kid"`

	// Category is the cleartext routing tag, authenticated against the ciphertext.
	Category Category `json:"category"`

	// Nonce is the per-seal GCM nonce, base64 standard encoding.
	Nonce string `json:"nonce"`

	// Ciphertext is the AES-256-GCM sealing of the tip JSON, base64 standard
	// encoding (GCM appends its auth tag, so this carries both).
	Ciphertext string `json:"ciphertext"`

	// Version is the envelope format version, 1 today.
	Version int `json:"v"`
}

// Sealer seals a content-free tip into an encrypted envelope and opens a
// received envelope back into a tip. It is the private cluster's primary
// privacy mechanism: even though a Tip is already allowlist-stripped, sealing it
// under the shared key keeps the host from reading even the coarse pattern.
type Sealer interface {
	// Seal encrypts one tip under the primary key, binding its category as
	// authenticated data and stamping the key's id.
	Seal(tip Tip) (SealedTip, error)

	// Open decrypts and authenticates one envelope under the key its id names. It
	// returns ErrOpenSealed for an unknown key id, a malformed nonce, or a
	// tampered ciphertext or category, so a failed open drops the tip rather than
	// surfacing ciphertext.
	Open(env SealedTip) (Tip, error)
}

// SecretResolver is the slice of the secret source the sealer needs: resolve one
// named secret to its value. internal/secrets.Env satisfies it structurally, so
// the shared key rides the same SecretSource seam as every other secret and can
// later come from Vault or a cloud manager without a hive change.
type SecretResolver interface {
	Get(ctx context.Context, name string) (string, error)
}

// AESSealer is the Sealer built on AES-256-GCM. It seals under one primary key
// and opens under any key in openers (the primary plus the previous keys still
// accepted during a rotation). randReader is the nonce source, held as a field
// so a test can make sealing reproducible; it defaults to crypto/rand.
type AESSealer struct {
	primary    cipher.AEAD
	randReader io.Reader
	openers    map[string]cipher.AEAD
	encode     func(any) ([]byte, error)
	primaryID  string
}

// keyID is the wire id of a key: the hex prefix of its SHA-256. One-way, so the
// id on an envelope never leaks the key; stable, so the same key always maps to
// the same id across nodes.
func keyID(key []byte) string {
	sum := sha256.Sum256(key)

	return hex.EncodeToString(sum[:])[:keyIDLen]
}

// newAEAD builds the AES-256-GCM AEAD for one key, rejecting any key that is not
// the 32 bytes AES-256 needs so a short or padded key fails loudly.
func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("%w: got %d", ErrKeyWrongSize, len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("hive sealer: new cipher: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("hive sealer: new gcm: %w", err)
	}

	return aead, nil
}

// NewSealer builds a single-key sealer that seals and opens under one key.
func NewSealer(key []byte) (*AESSealer, error) {
	return NewRotatingSealer(key)
}

// NewRotatingSealer builds a sealer that seals under primaryKey and opens under
// primaryKey or any previousKeys. Keeping the old key in previousKeys during a
// rotation lets a node open tips other nodes sealed under it until the rotation
// window closes. Each key carries a digest id on the wire, so Open matches an
// envelope to its key without trying every key.
func NewRotatingSealer(primaryKey []byte, previousKeys ...[]byte) (*AESSealer, error) {
	primary, err := newAEAD(primaryKey)
	if err != nil {
		return nil, err
	}

	openers := map[string]cipher.AEAD{keyID(primaryKey): primary}

	for _, prev := range previousKeys {
		prevAEAD, prevErr := newAEAD(prev)
		if prevErr != nil {
			return nil, prevErr
		}

		openers[keyID(prev)] = prevAEAD
	}

	return &AESSealer{
		primaryID:  keyID(primaryKey),
		primary:    primary,
		openers:    openers,
		randReader: rand.Reader,
		encode:     json.Marshal,
	}, nil
}

// keyFromSecret resolves one base64-encoded 32-byte key from the secret source.
// A malformed value is ErrBadKeyEncoding; the length check happens when the key
// becomes an AEAD.
func keyFromSecret(ctx context.Context, source SecretResolver, ref string) ([]byte, error) {
	encoded, err := source.Get(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("hive key lookup: %w", err)
	}

	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadKeyEncoding, err)
	}

	return key, nil
}

// SealerFromSecret resolves the shared key from the secret source by reference
// and builds a single-key sealer. The operator distributes 32 random bytes,
// base64-encoded, through the secret source.
func SealerFromSecret(ctx context.Context, source SecretResolver, ref string) (*AESSealer, error) {
	key, err := keyFromSecret(ctx, source, ref)
	if err != nil {
		return nil, err
	}

	return NewSealer(key)
}

// RotatingSealerFromSecret resolves the primary key and, when prevRef is set, a
// previous key from the secret source and builds a rotating sealer. An empty
// prevRef yields a single-key sealer, so the no-rotation case stays simple.
func RotatingSealerFromSecret(ctx context.Context, source SecretResolver, primaryRef, prevRef string) (*AESSealer, error) {
	primary, err := keyFromSecret(ctx, source, primaryRef)
	if err != nil {
		return nil, err
	}

	if prevRef == "" {
		return NewSealer(primary)
	}

	prev, err := keyFromSecret(ctx, source, prevRef)
	if err != nil {
		return nil, err
	}

	return NewRotatingSealer(primary, prev)
}

// Seal marshals the tip, generates a fresh nonce, and seals the JSON under the
// primary key with the category as authenticated data, stamping the key id.
//
//nolint:gocritic // hugeParam: tip is passed by value to keep the content-free tip immutable through the seal, matching the Client contract
func (s *AESSealer) Seal(tip Tip) (SealedTip, error) {
	plaintext, err := s.encode(tip)
	if err != nil {
		return SealedTip{}, fmt.Errorf("hive seal: encode tip: %w", err)
	}

	nonce := make([]byte, s.primary.NonceSize())
	if _, err := io.ReadFull(s.randReader, nonce); err != nil {
		return SealedTip{}, fmt.Errorf("hive seal: nonce: %w", err)
	}

	ciphertext := s.primary.Seal(nil, nonce, plaintext, []byte(tip.Category))

	return SealedTip{
		Version:    1,
		KeyID:      s.primaryID,
		Category:   tip.Category,
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
	}, nil
}

// Open reverses Seal: pick the key the envelope id names, decode the nonce and
// ciphertext, authenticate against the envelope category, and unmarshal. Any
// failure collapses to ErrOpenSealed so a caller never distinguishes an unknown
// key from a tampered tip and never sees partial plaintext.
func (s *AESSealer) Open(env SealedTip) (Tip, error) {
	aead, known := s.openers[env.KeyID]
	if !known {
		return Tip{}, ErrOpenSealed
	}

	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil || len(nonce) != aead.NonceSize() {
		return Tip{}, ErrOpenSealed
	}

	ciphertext, err := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		return Tip{}, ErrOpenSealed
	}

	plaintext, err := aead.Open(nil, nonce, ciphertext, []byte(env.Category))
	if err != nil {
		return Tip{}, ErrOpenSealed
	}

	var tip Tip
	if err := json.Unmarshal(plaintext, &tip); err != nil {
		return Tip{}, ErrOpenSealed
	}

	return tip, nil
}
