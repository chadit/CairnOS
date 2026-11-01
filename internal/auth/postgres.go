package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgAuthSchema holds the access keys and the revocation denylist with JSONB
// scopes and timestamptz, plus IF NOT EXISTS so opening an existing database is
// idempotent. It shares the run store's Postgres database with disjoint tables,
// the same arrangement the sqlite backends use within one file.
const pgAuthSchema = `
CREATE TABLE IF NOT EXISTS auth_keys (
    id           TEXT PRIMARY KEY,
    label        TEXT NOT NULL,
    scopes_json  JSONB NOT NULL,
    workspace_id TEXT NOT NULL DEFAULT 'default',
    hash         TEXT NOT NULL,
    enabled      BOOLEAN NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS auth_denylist (
    kind       TEXT NOT NULL,
    value      TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ NOT NULL DEFAULT 'epoch',
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (kind, value)
);

CREATE INDEX IF NOT EXISTS idx_auth_denylist_expires ON auth_denylist(expires_at);

CREATE TABLE IF NOT EXISTS auth_issued (
    jti        TEXT PRIMARY KEY,
    sub        TEXT NOT NULL DEFAULT '',
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_auth_issued_expires ON auth_issued(expires_at);
CREATE INDEX IF NOT EXISTS idx_auth_issued_sub ON auth_issued(sub);
`

// Postgres is the multi-node Store backend: the same access-key and denylist
// surface the sqlite backend offers, over pgxpool. It shares the run store's
// database (different tables), so several nodes verify and revoke against one
// auth state.
type Postgres struct {
	pool *pgxpool.Pool
}

// OpenPostgres connects to dsn, verifies the connection, and applies the auth
// schema. The DSN carries credentials from CAIRNOS_STORE_DSN.
func OpenPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open auth postgres pool: %w", err)
	}

	if pingErr := pool.Ping(ctx); pingErr != nil {
		pool.Close()

		return nil, fmt.Errorf("ping auth postgres: %w", pingErr)
	}

	if _, execErr := pool.Exec(ctx, pgAuthSchema); execErr != nil {
		pool.Close()

		return nil, fmt.Errorf("create auth schema: %w", execErr)
	}

	return &Postgres{pool: pool}, nil
}

// Close releases the connection pool.
func (s *Postgres) Close() error {
	s.pool.Close()

	return nil
}

// CreateKey persists one access key row; the hash column is all this store
// ever holds of the credential.
func (s *Postgres) CreateKey(ctx context.Context, key *Key) error {
	scopes, err := json.Marshal(key.Scopes)
	if err != nil {
		return fmt.Errorf("encode scopes for key %s: %w", key.ID, err)
	}

	_, err = s.pool.Exec(ctx,
		`INSERT INTO auth_keys (id, label, scopes_json, workspace_id, hash, enabled, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		key.ID, key.Label, scopes, keyWorkspace(key), key.Hash, key.Enabled, key.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("insert key %s: %w", key.ID, err)
	}

	return nil
}

// GetKey fetches one access key by id.
func (s *Postgres) GetKey(ctx context.Context, id string) (*Key, error) {
	row := s.pool.QueryRow(ctx,
		"SELECT "+keyColumns+" FROM auth_keys WHERE id = $1", id)

	key, err := scanPGKey(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrKeyNotFound, id)
	}

	if err != nil {
		return nil, fmt.Errorf("get key %s: %w", id, err)
	}

	return key, nil
}

// ListKeys returns every access key, oldest first; created_at is a
// timestamptz, so the order is chronological.
func (s *Postgres) ListKeys(ctx context.Context) ([]*Key, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+keyColumns+" FROM auth_keys ORDER BY created_at ASC, id ASC")
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	defer rows.Close()

	var keys []*Key

	for rows.Next() {
		key, scanErr := scanPGKey(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("list keys: %w", scanErr)
		}

		keys = append(keys, key)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}

	return keys, nil
}

// SetKeyEnabled flips one key's enabled flag; zero rows is ErrKeyNotFound.
func (s *Postgres) SetKeyEnabled(ctx context.Context, id string, enabled bool) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE auth_keys SET enabled = $1 WHERE id = $2", enabled, id)
	if err != nil {
		return fmt.Errorf("set key %s enabled: %w", id, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrKeyNotFound, id)
	}

	return nil
}

// RecordIssued persists one minted token's jti, sub, and exp so RevokeToken
// can deny it until its real exp and RevokeKey can bound its window. The jti
// is unique, so an unexpected re-mint of the same id just refreshes the row.
func (s *Postgres) RecordIssued(ctx context.Context, jti, sub string, exp time.Time) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO auth_issued (jti, sub, expires_at)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (jti) DO UPDATE SET sub = EXCLUDED.sub, expires_at = EXCLUDED.expires_at`,
		jti, sub, exp.UTC())
	if err != nil {
		return fmt.Errorf("record issued token %s: %w", jti, err)
	}

	return nil
}

// IssuedExp returns the recorded exp for one token id, reporting false when
// no row exists.
func (s *Postgres) IssuedExp(ctx context.Context, jti string) (time.Time, bool, error) {
	var exp time.Time

	err := s.pool.QueryRow(ctx,
		"SELECT expires_at FROM auth_issued WHERE jti = $1", jti).Scan(&exp)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}

	if err != nil {
		return time.Time{}, false, fmt.Errorf("issued exp for %s: %w", jti, err)
	}

	return exp, true, nil
}

// MaxIssuedExp returns the latest exp across one subject's recorded tokens,
// reporting false when the subject has none.
func (s *Postgres) MaxIssuedExp(ctx context.Context, sub string) (time.Time, bool, error) {
	var exp *time.Time

	err := s.pool.QueryRow(ctx,
		"SELECT MAX(expires_at) FROM auth_issued WHERE sub = $1", sub).Scan(&exp)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("max issued exp for %s: %w", sub, err)
	}

	if exp == nil {
		return time.Time{}, false, nil
	}

	return *exp, true, nil
}

// DenyToken denylists one token id until the given time. Re-denying extends
// the window rather than failing, so revoking twice is harmless.
func (s *Postgres) DenyToken(ctx context.Context, jti string, until time.Time) error {
	return s.deny(ctx, denyKindJTI, jti, until, time.Time{})
}

// DenyKeySince records the revoked-at instant for one key's deny-by-sub
// row; IsDenied then denies only the key's tokens minted at or before it
// (spec M3). until bounds when the row may be pruned. A re-revocation keeps
// the later expiry and the later revoked-at, so it never narrows the deny.
func (s *Postgres) DenyKeySince(ctx context.Context, keyID string, revokedAt, until time.Time) error {
	return s.deny(ctx, denyKindSub, keyID, until, revokedAt)
}

// RemoveKeyDenials deletes one key's deny-by-sub rows. Re-enabling a key
// calls this, which also restores any of its still-unexpired tokens; an admin
// who wants those dead too revokes by key id afterwards.
func (s *Postgres) RemoveKeyDenials(ctx context.Context, keyID string) error {
	_, err := s.pool.Exec(ctx,
		"DELETE FROM auth_denylist WHERE kind = $1 AND value = $2", denyKindSub, keyID)
	if err != nil {
		return fmt.Errorf("remove denials for key %s: %w", keyID, err)
	}

	return nil
}

// IsDenied reports whether the token is denied at the given instant. A live
// jti row denies outright; a live sub row denies only the key's tokens
// minted at or before its revoked-at instant, so fresh tokens of a
// key-revoked subject still pass (spec M3). Both lookups hit the primary key
// index.
func (s *Postgres) IsDenied(ctx context.Context, jti, sub string, iat, now time.Time) (bool, error) {
	var count int

	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM auth_denylist
		 WHERE expires_at > $1
		   AND ( (kind = $2 AND value = $3)
		      OR (kind = $4 AND value = $5 AND revoked_at >= $6) )`,
		now.UTC(),
		denyKindJTI, jti,
		denyKindSub, sub, iat.UTC()).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check denylist: %w", err)
	}

	return count > 0, nil
}

// PruneDenylist deletes denylist and issued-token rows whose expiry has passed:
// a token past its exp is dead with or without the rows.
func (s *Postgres) PruneDenylist(ctx context.Context, now time.Time) (int, error) {
	stored := now.UTC()

	tag, err := s.pool.Exec(ctx,
		"DELETE FROM auth_denylist WHERE expires_at <= $1", stored)
	if err != nil {
		return 0, fmt.Errorf("prune denylist: %w", err)
	}

	if _, err := s.pool.Exec(ctx,
		"DELETE FROM auth_issued WHERE expires_at <= $1", stored); err != nil {
		return 0, fmt.Errorf("prune issued tokens: %w", err)
	}

	return int(tag.RowsAffected()), nil
}

// deny upserts one denylist row, keeping the later expiry and the later
// revoked-at when the row already exists so a re-revocation never narrows
// the window. GREATEST over the two timestamptz values is the Postgres
// analog of the sqlite backend's MAX. created_at records when the revocation
// landed, for audit only; no logic reads it.
func (s *Postgres) deny(ctx context.Context, kind, value string, until, revokedAt time.Time) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO auth_denylist (kind, value, expires_at, revoked_at, created_at)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (kind, value) DO UPDATE SET
		   expires_at = GREATEST(auth_denylist.expires_at, EXCLUDED.expires_at),
		   revoked_at = GREATEST(auth_denylist.revoked_at, EXCLUDED.revoked_at)`,
		kind, value, until.UTC(), revokedAt.UTC(), time.Now().UTC())
	if err != nil {
		return fmt.Errorf("deny %s %s: %w", kind, value, err)
	}

	return nil
}

// scanPGKey reads one keyColumns row back into a Key. It reuses the package's
// scanner interface (declared in sqlite.go) since pgx.Row and pgx.Rows
// satisfy the same one-method Scan surface.
func scanPGKey(row scanner) (*Key, error) {
	var (
		key       Key
		scopes    []byte
		createdAt time.Time
	)

	err := row.Scan(&key.ID, &key.Label, &scopes, &key.Workspace, &key.Hash, &key.Enabled, &createdAt)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with the query's identity, including the pgx.ErrNoRows translation
	}

	if err := json.Unmarshal(scopes, &key.Scopes); err != nil {
		return nil, fmt.Errorf("decode scopes for key %s: %w", key.ID, err)
	}

	key.CreatedAt = createdAt

	return &key, nil
}
