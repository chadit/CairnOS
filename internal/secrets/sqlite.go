package secrets

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/chadit/CairnOS/internal/store"
)

// secretsSchema is the envelope table shared in shape by both backends: rows
// carry the key id, nonce, and ciphertext of an AES-256-GCM seal plus the
// timestamps, never plaintext. The workspace column is read from the request
// context so a later API surface needs no migration; the CLI writes the default
// workspace. IF NOT EXISTS keeps opening an existing database idempotent.
const secretsSchema = `
CREATE TABLE IF NOT EXISTS secrets (
    workspace  TEXT NOT NULL,
    name       TEXT NOT NULL,
    key_id     TEXT NOT NULL,
    nonce      BLOB NOT NULL,
    ciphertext BLOB NOT NULL,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    PRIMARY KEY (workspace, name)
);`

// listColumns is the metadata SELECT list every List query shares, in
// scanMetadata order. It deliberately omits nonce and ciphertext: a list view
// never touches sealed material.
const listColumns = "name, key_id, created_at, updated_at"

// SQLite is the default secret store backend: a single-file envelope table, no
// external services, safe for concurrent use through WAL mode and busy_timeout.
// It holds the sealer so every read opens and every write seals under one place.
type SQLite struct {
	db     *sql.DB
	sealer *Sealer
}

// OpenSQLite opens (creating if needed) the secret database at path, applies the
// schema under ctx, and wraps it in sealer. The same pragma choices as the vault
// and run stores: WAL so readers do not block the writer, busy_timeout so
// concurrent writers wait instead of failing with SQLITE_BUSY.
func OpenSQLite(ctx context.Context, path string, sealer *Sealer) (*SQLite, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open secrets database %q: %w", path, err)
	}

	if _, execErr := db.ExecContext(ctx, secretsSchema); execErr != nil {
		return nil, fmt.Errorf("create secrets schema: %w", errors.Join(execErr, db.Close()))
	}

	return &SQLite{db: db, sealer: sealer}, nil
}

// Close releases the underlying database.
func (s *SQLite) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close secrets database: %w", err)
	}

	return nil
}

// Set seals value under the primary key and upserts the row. created_at is
// preserved across an overwrite so an edit does not reset age; updated_at
// advances and is the recency stamp folded into the AAD.
func (s *SQLite) Set(ctx context.Context, name, value string) error {
	workspace := store.WorkspaceFromContext(ctx)
	canonical := NormalizeName(name)
	updatedAt := nowStamp()

	keyID, nonce, ciphertext, err := s.sealer.Seal(workspace, canonical, storeVersion(updatedAt), value)
	if err != nil {
		return sealFailed(canonical, err)
	}

	createdAt, err := s.createdAt(ctx, workspace, canonical, updatedAt)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO secrets (workspace, name, key_id, nonce, ciphertext, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(workspace, name) DO UPDATE SET
		     key_id = excluded.key_id, nonce = excluded.nonce,
		     ciphertext = excluded.ciphertext, updated_at = excluded.updated_at`,
		workspace, canonical, keyID, nonce, ciphertext, storedTime(createdAt), storedTime(updatedAt))
	if err != nil {
		return fmt.Errorf("write secret %q: %w", canonical, err)
	}

	return nil
}

// Get opens the sealed row for name within the request workspace. A missing row
// is ErrNotFound; a row that fails authentication is ErrOpenSealed, and no error
// carries plaintext.
func (s *SQLite) Get(ctx context.Context, name string) (string, error) {
	workspace := store.WorkspaceFromContext(ctx)
	canonical := NormalizeName(name)

	var (
		keyID      string
		nonce      []byte
		ciphertext []byte
		updatedAt  sql.NullTime
	)

	row := s.db.QueryRowContext(ctx,
		"SELECT key_id, nonce, ciphertext, updated_at FROM secrets WHERE workspace = ? AND name = ?",
		workspace, canonical)

	switch err := row.Scan(&keyID, &nonce, &ciphertext, &updatedAt); {
	case errors.Is(err, sql.ErrNoRows):
		return "", notFound(name)
	case err != nil:
		return "", fmt.Errorf("read secret %q: %w", canonical, err)
	}

	value, err := s.sealer.Open(workspace, canonical, storeVersion(updatedAt.Time), keyID, nonce, ciphertext)
	if err != nil {
		return "", openFailed(canonical, err)
	}

	return value, nil
}

// Delete removes one row within the request workspace. A missing row is
// ErrNotFound so a caller can tell a miss from a no-op.
func (s *SQLite) Delete(ctx context.Context, name string) error {
	workspace := store.WorkspaceFromContext(ctx)
	canonical := NormalizeName(name)

	result, err := s.db.ExecContext(ctx,
		"DELETE FROM secrets WHERE workspace = ? AND name = ?", workspace, canonical)
	if err != nil {
		return fmt.Errorf("delete secret %q: %w", canonical, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete secret %q: rows affected: %w", canonical, err)
	}

	if affected == 0 {
		return notFound(name)
	}

	return nil
}

// List returns the workspace's secret metadata ordered by name, never values.
func (s *SQLite) List(ctx context.Context) ([]Metadata, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+listColumns+" FROM secrets WHERE workspace = ? ORDER BY name",
		store.WorkspaceFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	defer closeRows(rows)

	var out []Metadata

	for rows.Next() {
		var (
			meta                 Metadata
			createdAt, updatedAt sql.NullTime
		)

		if scanErr := rows.Scan(&meta.Name, &meta.KeyID, &createdAt, &updatedAt); scanErr != nil {
			return nil, fmt.Errorf("scan secret metadata: %w", scanErr)
		}

		meta.CreatedAt = createdAt.Time
		meta.UpdatedAt = updatedAt.Time
		out = append(out, meta)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}

	return out, nil
}

// Rotate re-seals every row whose key id is not the primary's, across all
// workspaces, and returns the count. Rows are read into memory first, then
// re-sealed in one transaction; each needs a fresh nonce and ciphertext, so no
// batched statement can express the writes. A row that opens under no loaded
// key fails naming the secret only.
func (s *SQLite) Rotate(ctx context.Context) (int, error) {
	pending, err := s.rowsToRotate(ctx)
	if err != nil {
		return 0, err
	}

	if len(pending) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin secrets rotate: %w", err)
	}
	defer func() {
		// Rollback after Commit is the happy path. Any other failure is only
		// logged, because Rotate has already chosen its return value.
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			slog.WarnContext(ctx, "roll back secrets rotate", "error", rollbackErr)
		}
	}()

	for idx := range pending {
		if err := s.reseal(ctx, tx, &pending[idx]); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit secrets rotate: %w", err)
	}

	return len(pending), nil
}

// createdAt picks the created_at to store: the existing row's value when
// overwriting so an edit keeps its age, otherwise the write time for a fresh
// row.
func (s *SQLite) createdAt(ctx context.Context, workspace, name string, fallback time.Time) (time.Time, error) {
	var existing sql.NullTime

	row := s.db.QueryRowContext(ctx,
		"SELECT created_at FROM secrets WHERE workspace = ? AND name = ?", workspace, name)

	switch err := row.Scan(&existing); {
	case errors.Is(err, sql.ErrNoRows):
		return fallback, nil
	case err != nil:
		return time.Time{}, fmt.Errorf("read created_at for %q: %w", name, err)
	default:
		return existing.Time, nil
	}
}

// rowsToRotate materializes every row not under the primary key, closing the
// read cursor before the write pass so the transaction never nests against an
// open scan.
func (s *SQLite) rowsToRotate(ctx context.Context) ([]sealedRow, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT workspace, name, key_id, nonce, ciphertext, updated_at FROM secrets WHERE key_id != ?",
		s.sealer.PrimaryID())
	if err != nil {
		return nil, fmt.Errorf("scan rows to rotate: %w", err)
	}
	defer closeRows(rows)

	var pending []sealedRow

	for rows.Next() {
		var (
			one       sealedRow
			updatedAt sql.NullTime
		)

		if scanErr := rows.Scan(&one.workspace, &one.name, &one.keyID, &one.nonce, &one.ciphertext, &updatedAt); scanErr != nil {
			return nil, fmt.Errorf("scan row to rotate: %w", scanErr)
		}

		one.version = storeVersion(updatedAt.Time)
		pending = append(pending, one)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan rows to rotate: %w", err)
	}

	return pending, nil
}

// reseal opens one row under its current key and writes it back sealed under the
// primary, leaving the timestamps (and so the bound version) unchanged.
func (s *SQLite) reseal(ctx context.Context, tx *sql.Tx, row *sealedRow) error {
	keyID, nonce, ciphertext, err := resealRow(s.sealer, row)
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE secrets SET key_id = ?, nonce = ?, ciphertext = ? WHERE workspace = ? AND name = ?",
		keyID, nonce, ciphertext, row.workspace, row.name); err != nil {
		return fmt.Errorf("reseal secret %q: %w", row.name, err)
	}

	return nil
}

// storedTime renders a timestamp in fixed-width RFC 3339 UTC with nanosecond
// places, matching the vault store. The instant is already microsecond-truncated
// upstream, so the version derived from it on read equals the one bound at seal.
func storedTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

// closeRows closes rows where the read path already reports errors via
// rows.Err, making a second error from Close redundant.
func closeRows(rows *sql.Rows) {
	_ = rows.Close()
}
