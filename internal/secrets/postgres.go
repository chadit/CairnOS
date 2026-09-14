package secrets

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chadit/CairnOS/internal/store"
)

// pgSecretsSchema mirrors the sqlite envelope table with BYTEA for the nonce and
// ciphertext and TIMESTAMPTZ for the timestamps. IF NOT EXISTS keeps opening an
// existing database idempotent, and the workspace column matches sqlite so the
// two backends isolate tenants identically.
const pgSecretsSchema = `
CREATE TABLE IF NOT EXISTS secrets (
    workspace  TEXT NOT NULL,
    name       TEXT NOT NULL,
    key_id     TEXT NOT NULL,
    nonce      BYTEA NOT NULL,
    ciphertext BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (workspace, name)
);`

// Postgres is the shared, multi-node secret store backend: the same envelope
// table as sqlite, persisted through pgxpool. It shares the run store's database
// (a different table), the arrangement the other postgres backends use. It holds
// the sealer so every read opens and every write seals under one place.
type Postgres struct {
	pool   *pgxpool.Pool
	sealer *Sealer
}

// OpenPostgres connects to dsn, verifies the connection, applies the schema, and
// wraps it in sealer. The DSN carries credentials from CAIRNOS_STORE_DSN; pool
// sizing rides the DSN (pool_max_conns) like the other stores.
func OpenPostgres(ctx context.Context, dsn string, sealer *Sealer) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open secrets postgres pool: %w", err)
	}

	if pingErr := pool.Ping(ctx); pingErr != nil {
		pool.Close()

		return nil, fmt.Errorf("ping secrets postgres: %w", pingErr)
	}

	if _, execErr := pool.Exec(ctx, pgSecretsSchema); execErr != nil {
		pool.Close()

		return nil, fmt.Errorf("create secrets schema: %w", execErr)
	}

	return &Postgres{pool: pool, sealer: sealer}, nil
}

// Close releases the connection pool.
func (s *Postgres) Close() error {
	s.pool.Close()

	return nil
}

// Set seals value under the primary key and upserts the row. created_at is
// preserved across an overwrite; updated_at advances and is the recency stamp
// folded into the AAD.
func (s *Postgres) Set(ctx context.Context, name, value string) error {
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

	_, err = s.pool.Exec(ctx,
		`INSERT INTO secrets (workspace, name, key_id, nonce, ciphertext, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT(workspace, name) DO UPDATE SET
		     key_id = excluded.key_id, nonce = excluded.nonce,
		     ciphertext = excluded.ciphertext, updated_at = excluded.updated_at`,
		workspace, canonical, keyID, nonce, ciphertext, createdAt, updatedAt)
	if err != nil {
		return fmt.Errorf("write secret %q: %w", canonical, err)
	}

	return nil
}

// Get opens the sealed row for name within the request workspace. A missing row
// is ErrNotFound; a row that fails authentication is ErrOpenSealed, and no error
// carries plaintext.
func (s *Postgres) Get(ctx context.Context, name string) (string, error) {
	workspace := store.WorkspaceFromContext(ctx)
	canonical := NormalizeName(name)

	var (
		keyID      string
		nonce      []byte
		ciphertext []byte
		updatedAt  time.Time
	)

	row := s.pool.QueryRow(ctx,
		"SELECT key_id, nonce, ciphertext, updated_at FROM secrets WHERE workspace = $1 AND name = $2",
		workspace, canonical)

	switch err := row.Scan(&keyID, &nonce, &ciphertext, &updatedAt); {
	case errors.Is(err, pgx.ErrNoRows):
		return "", notFound(name)
	case err != nil:
		return "", fmt.Errorf("read secret %q: %w", canonical, err)
	}

	value, err := s.sealer.Open(workspace, canonical, storeVersion(updatedAt), keyID, nonce, ciphertext)
	if err != nil {
		return "", openFailed(canonical, err)
	}

	return value, nil
}

// Delete removes one row within the request workspace. A missing row is
// ErrNotFound so a caller can tell a miss from a no-op.
func (s *Postgres) Delete(ctx context.Context, name string) error {
	workspace := store.WorkspaceFromContext(ctx)
	canonical := NormalizeName(name)

	tag, err := s.pool.Exec(ctx,
		"DELETE FROM secrets WHERE workspace = $1 AND name = $2", workspace, canonical)
	if err != nil {
		return fmt.Errorf("delete secret %q: %w", canonical, err)
	}

	if tag.RowsAffected() == 0 {
		return notFound(name)
	}

	return nil
}

// List returns the workspace's secret metadata ordered by name, never values.
func (s *Postgres) List(ctx context.Context) ([]Metadata, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+listColumns+" FROM secrets WHERE workspace = $1 ORDER BY name",
		store.WorkspaceFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	defer rows.Close()

	var out []Metadata

	for rows.Next() {
		var meta Metadata

		if scanErr := rows.Scan(&meta.Name, &meta.KeyID, &meta.CreatedAt, &meta.UpdatedAt); scanErr != nil {
			return nil, fmt.Errorf("scan secret metadata: %w", scanErr)
		}

		out = append(out, meta)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}

	return out, nil
}

// Rotate re-seals every row whose key id is not the primary's, across all
// workspaces, and returns the count. It reads the pending rows into memory
// first, then re-seals them in one transaction; the per-row writes are
// unavoidable because each row gets a fresh nonce. A row no loaded key opens
// fails the rotate, naming the secret only.
func (s *Postgres) Rotate(ctx context.Context) (int, error) {
	pending, err := s.rowsToRotate(ctx)
	if err != nil {
		return 0, err
	}

	if len(pending) == 0 {
		return 0, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin secrets rotate: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for idx := range pending {
		if err := s.reseal(ctx, tx, &pending[idx]); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit secrets rotate: %w", err)
	}

	return len(pending), nil
}

// createdAt picks the created_at to store: the existing row's value when
// overwriting so an edit keeps its age, otherwise the write time for a fresh
// row.
func (s *Postgres) createdAt(ctx context.Context, workspace, name string, fallback time.Time) (time.Time, error) {
	var existing time.Time

	row := s.pool.QueryRow(ctx,
		"SELECT created_at FROM secrets WHERE workspace = $1 AND name = $2", workspace, name)

	switch err := row.Scan(&existing); {
	case errors.Is(err, pgx.ErrNoRows):
		return fallback, nil
	case err != nil:
		return time.Time{}, fmt.Errorf("read created_at for %q: %w", name, err)
	default:
		return existing, nil
	}
}

// rowsToRotate materializes every row not under the primary key, closing the
// read cursor before the write pass so the transaction never nests against an
// open scan.
func (s *Postgres) rowsToRotate(ctx context.Context) ([]sealedRow, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT workspace, name, key_id, nonce, ciphertext, updated_at FROM secrets WHERE key_id != $1",
		s.sealer.PrimaryID())
	if err != nil {
		return nil, fmt.Errorf("scan rows to rotate: %w", err)
	}
	defer rows.Close()

	var pending []sealedRow

	for rows.Next() {
		var (
			one       sealedRow
			updatedAt time.Time
		)

		if scanErr := rows.Scan(&one.workspace, &one.name, &one.keyID, &one.nonce, &one.ciphertext, &updatedAt); scanErr != nil {
			return nil, fmt.Errorf("scan row to rotate: %w", scanErr)
		}

		one.version = storeVersion(updatedAt)
		pending = append(pending, one)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan rows to rotate: %w", err)
	}

	return pending, nil
}

// reseal opens one row under its current key and writes it back sealed under the
// primary, leaving the timestamps (and so the bound version) unchanged.
func (s *Postgres) reseal(ctx context.Context, tx pgx.Tx, row *sealedRow) error {
	keyID, nonce, ciphertext, err := resealRow(s.sealer, row)
	if err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		"UPDATE secrets SET key_id = $1, nonce = $2, ciphertext = $3 WHERE workspace = $4 AND name = $5",
		keyID, nonce, ciphertext, row.workspace, row.name); err != nil {
		return fmt.Errorf("reseal secret %q: %w", row.name, err)
	}

	return nil
}
