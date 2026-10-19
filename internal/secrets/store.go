package secrets

import (
	"context"
	"fmt"
	"io"
	"time"
)

// Metadata is what List returns: the fields that identify a stored secret and
// place it on a key generation, never the secret value. A list view exists so
// an operator can audit what is stored and which key id a row belongs to (the
// only rotation diagnostic), which the store can answer without ever opening a
// row.
type Metadata struct {
	// CreatedAt is when the secret was first set; preserved across overwrites.
	CreatedAt time.Time

	// UpdatedAt is the last write. It doubles as the recency stamp the sealer
	// binds into the AAD, so a rolled-back ciphertext row fails to open.
	UpdatedAt time.Time

	// Name is the canonical secret name (NormalizeName applied), the same
	// identity the env source resolves.
	Name string

	// KeyID is the row's key generation. During a rotation window ls shows which
	// rows still belong to a previous key so an operator can see what rotate has
	// left to re-seal.
	KeyID string
}

// Store is the encrypted-at-rest secret store every backend satisfies; swapping
// sqlite for postgres is a [store] backend config choice, never a caller change,
// the same seam vault and memory have. Backends persist AES-256-GCM envelope
// rows only (key id, nonce, ciphertext) plus timestamps, so a database dump
// yields no plaintext.
type Store interface {
	Reader
	Writer

	// Close releases the backend's database resources.
	io.Closer
}

// Reader is the read side of a Store. Neither method changes a row.
type Reader interface {
	// Get resolves one secret by name, opening its sealed row. It satisfies the
	// engine SecretSource shape so Chain composes it with Env uniformly. A
	// missing row is ErrNotFound; a row that will not open is ErrOpenSealed, and
	// no error ever carries plaintext.
	Get(ctx context.Context, name string) (string, error)

	// List returns the workspace's secret metadata, never values.
	List(ctx context.Context) ([]Metadata, error)
}

// Writer is the write side of a Store: every method seals, re-seals, or
// removes rows.
type Writer interface {
	// Set seals value under the primary key and upserts the row, preserving
	// created_at across an overwrite and advancing updated_at (the recency stamp).
	Set(ctx context.Context, name, value string) error

	// Delete removes one row; a missing row is ErrNotFound so a caller can tell a
	// miss from a no-op.
	Delete(ctx context.Context, name string) error

	// Rotate re-seals every row not already under the primary key, across all
	// workspaces, and returns the count. A row that opens under no loaded key
	// fails the rotate with an error naming the secret (name only), so a stuck
	// generation is visible rather than silently dropped.
	Rotate(ctx context.Context) (int, error)
}

// storeVersion is the recency stamp bound into the sealer AAD: the row's
// updated_at as Unix nanoseconds, truncated to microseconds because postgres
// TIMESTAMPTZ keeps only that much resolution. Without the truncation the two
// backends would derive different versions from one stored instant and Open
// would fail on postgres. Truncation is idempotent, so the read-side version
// matches seal time.
func storeVersion(updatedAt time.Time) int64 {
	return updatedAt.UTC().Truncate(time.Microsecond).UnixNano()
}

// nowStamp is the write timestamp: UTC, truncated to the microsecond resolution
// both backends preserve, so the version derived from it round-trips exactly.
func nowStamp() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

// notFoundError reports a referenced secret with no row in the store. It names
// the secret, never a value.
type notFoundError struct {
	name string
}

func (e *notFoundError) Error() string {
	return fmt.Sprintf("%v: %s", ErrNotFound, e.name)
}

// Unwrap keeps errors.Is(err, ErrNotFound) true for a missing secret.
func (*notFoundError) Unwrap() error {
	return ErrNotFound
}

// The two crypto steps an envelopeError names. Seal only fails on a nonce-source
// read error; Open collapses every decrypt and auth failure to ErrOpenSealed.
const (
	stepSeal = "seal"
	stepOpen = "open"
)

// envelopeError wraps a Seal or Open failure with the secret name, which keeps the
// diagnostic useful while the wrapped error carries no plaintext.
type envelopeError struct {
	err  error
	step string
	name string
}

func (e *envelopeError) Error() string {
	return fmt.Sprintf("%s secret %q: %v", e.step, e.name, e.err)
}

func (e *envelopeError) Unwrap() error {
	return e.err
}

// sealedRow is one envelope the rotate pass re-seals: identity, recency stamp,
// and the sealed bytes it opens under the row's current key. Both backends
// materialize their rows-to-rotate into this shape so the re-seal transform is
// written once.
type sealedRow struct {
	workspace  string
	name       string
	keyID      string
	nonce      []byte
	ciphertext []byte
	version    int64
}

// resealRow opens one row under its current key and re-seals it under the
// primary, leaving the bound version (updated_at) unchanged so a later Get still
// opens the row. It returns the new envelope the backend writes back. A row that
// opens under no loaded key fails naming the secret only, never a value.
func resealRow(sealer *Sealer, row *sealedRow) (string, []byte, []byte, error) {
	value, err := sealer.Open(row.workspace, row.name, row.version, row.keyID, row.nonce, row.ciphertext)
	if err != nil {
		return "", nil, nil, &envelopeError{step: stepOpen, name: row.name, err: err}
	}

	keyID, nonce, ciphertext, err := sealer.Seal(row.workspace, row.name, row.version, value)
	if err != nil {
		return "", nil, nil, &envelopeError{step: stepSeal, name: row.name, err: err}
	}

	return keyID, nonce, ciphertext, nil
}
