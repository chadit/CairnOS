package secrets

import (
	"context"
	"fmt"
	"time"
)

// Metadata is what List returns: the fields that identify a stored secret and
// place it on a key generation, never the secret value. A list view exists so
// an operator can audit what is stored and which key id a row belongs to (the
// only rotation diagnostic), which the store can answer without ever opening a
// row.
type Metadata struct {
	// Name is the canonical secret name (NormalizeName applied), the same
	// identity the env source resolves.
	Name string
	// KeyID is the row's key generation. During a rotation window ls shows which
	// rows still belong to a previous key so an operator can see what rotate has
	// left to re-seal.
	KeyID string
	// CreatedAt is when the secret was first set; preserved across overwrites.
	CreatedAt time.Time
	// UpdatedAt is the last write. It doubles as the recency stamp the sealer
	// binds into the AAD, so a rolled-back ciphertext row fails to open.
	UpdatedAt time.Time
}

// Store is the encrypted-at-rest secret store every backend satisfies; swapping
// sqlite for postgres is a [store] backend config choice, never a caller change,
// the same seam vault and memory have. Backends persist AES-256-GCM envelope
// rows only (key id, nonce, ciphertext) plus timestamps, so a database dump
// yields no plaintext.
type Store interface {
	// Get resolves one secret by name, opening its sealed row. It satisfies the
	// engine SecretSource shape so Chain composes it with Env uniformly. A
	// missing row is ErrNotFound; a row that will not open is ErrOpenSealed, and
	// no error ever carries plaintext.
	Get(ctx context.Context, name string) (string, error)

	// Set seals value under the primary key and upserts the row, preserving
	// created_at across an overwrite and advancing updated_at (the recency stamp).
	Set(ctx context.Context, name, value string) error

	// Delete removes one row; a missing row is ErrNotFound so a caller can tell a
	// miss from a no-op.
	Delete(ctx context.Context, name string) error

	// List returns the workspace's secret metadata, never values.
	List(ctx context.Context) ([]Metadata, error)

	// Rotate re-seals every row not already under the primary key, across all
	// workspaces, and returns the count. A row that opens under no loaded key
	// fails the rotate with an error naming the secret (name only), so a stuck
	// generation is visible rather than silently dropped.
	Rotate(ctx context.Context) (int, error)

	// Close releases the backend's database resources.
	Close() error
}

// storeVersion is the recency stamp bound into the sealer AAD: the row's
// updated_at as Unix nanoseconds. The instant is truncated to microseconds
// first because postgres TIMESTAMPTZ keeps only microsecond resolution; without
// the truncation the two backends would derive different versions from the same
// stored instant and Open would fail on postgres. Truncation is idempotent, so
// deriving the version on read matches the value used at seal time.
func storeVersion(updatedAt time.Time) int64 {
	return updatedAt.UTC().Truncate(time.Microsecond).UnixNano()
}

// nowStamp is the write timestamp: UTC, truncated to the microsecond resolution
// both backends preserve, so the version derived from it round-trips exactly.
func nowStamp() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

// notFound reports a referenced secret with no row in the store. It names the
// secret, never a value.
func notFound(name string) error {
	return fmt.Errorf("%w: %s", ErrNotFound, name)
}

// sealFailed wraps a Seal failure with the secret name for context. Seal only
// fails on a nonce-source read error, so this path is rare, but it must still
// carry no plaintext.
func sealFailed(name string, err error) error {
	return fmt.Errorf("seal secret %q: %w", name, err)
}

// openFailed wraps an Open failure (ErrOpenSealed) with the secret name. The
// wrapped error already collapses every decrypt and auth failure to one
// sentinel carrying no plaintext; adding the name keeps the diagnostic useful
// without leaking material.
func openFailed(name string, err error) error {
	return fmt.Errorf("open secret %q: %w", name, err)
}

// sealedRow is one envelope the rotate pass re-seals: identity, recency stamp,
// and the sealed bytes it opens under the row's current key. Both backends
// materialize their rows-to-rotate into this shape so the re-seal transform is
// written once.
type sealedRow struct {
	workspace  string
	name       string
	version    int64
	keyID      string
	nonce      []byte
	ciphertext []byte
}

// resealRow opens one row under its current key and re-seals it under the
// primary, leaving the bound version (updated_at) unchanged so a later Get still
// opens the row. It returns the new envelope the backend writes back. A row that
// opens under no loaded key fails naming the secret only, never a value.
func resealRow(sealer *Sealer, row *sealedRow) (string, []byte, []byte, error) {
	value, err := sealer.Open(row.workspace, row.name, row.version, row.keyID, row.nonce, row.ciphertext)
	if err != nil {
		return "", nil, nil, openFailed(row.name, err)
	}

	keyID, nonce, ciphertext, err := sealer.Seal(row.workspace, row.name, row.version, value)
	if err != nil {
		return "", nil, nil, sealFailed(row.name, err)
	}

	return keyID, nonce, ciphertext, nil
}
