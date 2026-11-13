package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// authSchema holds the access keys and the revocation denylist, plus IF NOT
// EXISTS so opening an existing database is idempotent. It shares the run
// store's database file with disjoint tables, the same arrangement
// internal/scheduler and internal/memory use.
const authSchema = `
CREATE TABLE IF NOT EXISTS auth_keys (
    id           TEXT PRIMARY KEY,
    label        TEXT NOT NULL,
    scopes_json  TEXT NOT NULL,
    workspace_id TEXT NOT NULL DEFAULT 'default',
    hash         TEXT NOT NULL,
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS auth_denylist (
    kind       TEXT NOT NULL,
    value      TEXT NOT NULL,
    expires_at TIMESTAMP NOT NULL,
    revoked_at TIMESTAMP NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL,
    PRIMARY KEY (kind, value)
);

CREATE INDEX IF NOT EXISTS idx_auth_denylist_expires ON auth_denylist(expires_at);

CREATE TABLE IF NOT EXISTS auth_issued (
    jti        TEXT PRIMARY KEY,
    sub        TEXT NOT NULL DEFAULT '',
    expires_at TIMESTAMP NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_auth_issued_expires ON auth_issued(expires_at);
CREATE INDEX IF NOT EXISTS idx_auth_issued_sub ON auth_issued(sub);
`

// Denylist row kinds: one revoked token id, or one access key whose every
// token is dead (deny-by-sub, the default for disabled keys).
const (
	denyKindJTI = "jti"
	denyKindSub = "sub"
)

// keyColumns is the SELECT list every key query shares, in scanKey order.
const keyColumns = "id, label, scopes_json, workspace_id, hash, enabled, created_at"

// authTimeLayout is RFC 3339 UTC with fixed-width nanoseconds, matching
// the run, memory, and schedule stores so lexicographic order tracks
// chronological order across every table in the shared file.
const authTimeLayout = "2006-01-02T15:04:05.000000000Z"

// SQLite is a Store backend. Safe for concurrent use through
// WAL mode and busy_timeout, which also lets the CLI manage keys and
// revocations while the serve daemon holds the file open.
type SQLite struct {
	db *sql.DB
}

// OpenSQLite opens (creating if needed) the auth tables in the database at
// path. Same pragma choices as the run store: WAL so readers do not block
// the writer, busy_timeout so concurrent writers wait instead of failing
// with SQLITE_BUSY.
func OpenSQLite(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open auth database %q: %w", path, err)
	}

	// Schema creation is process-lifetime initialization with no request to
	// inherit a context from; OpenSQLite carries no ctx on purpose.
	setupCtx := context.Background()

	if _, execErr := db.ExecContext(setupCtx, authSchema); execErr != nil {
		return nil, fmt.Errorf("create auth schema: %w", errors.Join(execErr, db.Close()))
	}

	return &SQLite{db: db}, nil
}

// Close releases the underlying database.
func (s *SQLite) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close auth database: %w", err)
	}

	return nil
}

// CreateKey persists one access key row; the hash column is all this
// store ever holds of the credential.
func (s *SQLite) CreateKey(ctx context.Context, key *Key) error {
	scopes, err := json.Marshal(key.Scopes)
	if err != nil {
		return fmt.Errorf("encode scopes for key %s: %w", key.ID, err)
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO auth_keys (id, label, scopes_json, workspace_id, hash, enabled, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		key.ID, key.Label, string(scopes), keyWorkspace(key), key.Hash, key.Enabled, authStoredTime(key.CreatedAt))
	if err != nil {
		return fmt.Errorf("insert key %s: %w", key.ID, err)
	}

	return nil
}

// GetKey fetches one access key by id.
func (s *SQLite) GetKey(ctx context.Context, id string) (*Key, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+keyColumns+" FROM auth_keys WHERE id = ?", id)

	key, err := scanKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrKeyNotFound, id)
	}

	if err != nil {
		return nil, fmt.Errorf("get key %s: %w", id, err)
	}

	return key, nil
}

// ListKeys returns every access key, oldest first; created_at is stored
// in a lexicographically ordered layout, so the walk is chronological.
func (s *SQLite) ListKeys(ctx context.Context) ([]*Key, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+keyColumns+" FROM auth_keys ORDER BY created_at ASC, id ASC")
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	defer closeRows(rows)

	var keys []*Key

	for rows.Next() {
		key, scanErr := scanKey(rows)
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
func (s *SQLite) SetKeyEnabled(ctx context.Context, id string, enabled bool) error {
	result, err := s.db.ExecContext(ctx,
		"UPDATE auth_keys SET enabled = ? WHERE id = ?", enabled, id)
	if err != nil {
		return fmt.Errorf("set key %s enabled: %w", id, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("set key %s enabled: rows affected: %w", id, err)
	}

	if affected == 0 {
		return fmt.Errorf("%w: %s", ErrKeyNotFound, id)
	}

	return nil
}

// RecordIssued persists one minted token's jti, sub, and exp so RevokeToken
// can deny it until its real exp and RevokeKey can bound its window. The jti
// is unique, so an unexpected re-mint of the same id just refreshes the row.
func (s *SQLite) RecordIssued(ctx context.Context, jti, sub string, exp time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO auth_issued (jti, sub, expires_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT (jti) DO UPDATE SET sub = excluded.sub, expires_at = excluded.expires_at`,
		jti, sub, authStoredTime(exp))
	if err != nil {
		return fmt.Errorf("record issued token %s: %w", jti, err)
	}

	return nil
}

// IssuedExp returns the recorded exp for one token id, reporting false when
// no row exists.
func (s *SQLite) IssuedExp(ctx context.Context, jti string) (time.Time, bool, error) {
	var exp sql.NullTime

	err := s.db.QueryRowContext(ctx,
		"SELECT expires_at FROM auth_issued WHERE jti = ?", jti).Scan(&exp)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}

	if err != nil {
		return time.Time{}, false, fmt.Errorf("issued exp for %s: %w", jti, err)
	}

	return exp.Time.UTC(), exp.Valid, nil
}

// MaxIssuedExp returns the latest exp across one subject's recorded tokens,
// reporting false when the subject has none. MAX over the stored text is
// correct because the fixed-width layout orders lexicographically with time;
// the aggregate strips column type affinity, so the result scans as a string
// and parses back through the shared layout.
func (s *SQLite) MaxIssuedExp(ctx context.Context, sub string) (time.Time, bool, error) {
	var stored sql.NullString

	err := s.db.QueryRowContext(ctx,
		"SELECT MAX(expires_at) FROM auth_issued WHERE sub = ?", sub).Scan(&stored)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("max issued exp for %s: %w", sub, err)
	}

	if !stored.Valid || stored.String == "" {
		return time.Time{}, false, nil
	}

	parsed, err := time.Parse(authTimeLayout, stored.String)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse max issued exp for %s: %w", sub, err)
	}

	return parsed.UTC(), true, nil
}

// DenyToken denylists one token id until the given time. Re-denying
// extends the window rather than failing, so revoking twice is harmless.
func (s *SQLite) DenyToken(ctx context.Context, jti string, until time.Time) error {
	return s.deny(ctx, denyKindJTI, jti, until, time.Time{})
}

// DenyKeySince records the revoked-at instant for one key's deny-by-sub
// row; IsDenied then denies only the key's tokens minted at or before it.
// until bounds when the row may be pruned. A re-revocation keeps
// the later expiry and the later revoked-at, so it never narrows the deny.
func (s *SQLite) DenyKeySince(ctx context.Context, keyID string, revokedAt, until time.Time) error {
	return s.deny(ctx, denyKindSub, keyID, until, revokedAt)
}

// RemoveKeyDenials deletes one key's deny-by-sub rows. Re-enabling a key
// calls this, which also restores any of its still-unexpired tokens; an
// admin who wants those dead too revokes by key id afterwards.
func (s *SQLite) RemoveKeyDenials(ctx context.Context, keyID string) error {
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM auth_denylist WHERE kind = ? AND value = ?", denyKindSub, keyID)
	if err != nil {
		return fmt.Errorf("remove denials for key %s: %w", keyID, err)
	}

	return nil
}

// IsDenied reports whether the token is denied at the given instant. A live
// jti row denies outright; a live sub row denies only the key's tokens
// minted at or before its revoked-at instant, so fresh tokens of a
// key-revoked subject still pass. Both lookups hit the primary
// key index.
func (s *SQLite) IsDenied(ctx context.Context, jti, sub string, iat, now time.Time) (bool, error) {
	var count int

	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM auth_denylist
		 WHERE expires_at > ?
		   AND ( (kind = ? AND value = ?)
		      OR (kind = ? AND value = ? AND revoked_at >= ?) )`,
		authStoredTime(now),
		denyKindJTI, jti,
		denyKindSub, sub, authStoredTime(iat)).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check denylist: %w", err)
	}

	return count > 0, nil
}

// PruneDenylist deletes denylist and issued-token rows whose expiry has passed:
// a token past its exp is dead with or without the rows.
func (s *SQLite) PruneDenylist(ctx context.Context, now time.Time) (int, error) {
	stored := authStoredTime(now)

	denyResult, err := s.db.ExecContext(ctx,
		"DELETE FROM auth_denylist WHERE expires_at <= ?", stored)
	if err != nil {
		return 0, fmt.Errorf("prune denylist: %w", err)
	}

	denied, err := denyResult.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune denylist: rows affected: %w", err)
	}

	if _, err := s.db.ExecContext(ctx,
		"DELETE FROM auth_issued WHERE expires_at <= ?", stored); err != nil {
		return 0, fmt.Errorf("prune issued tokens: %w", err)
	}

	return int(denied), nil
}

// deny upserts one denylist row, keeping the later expiry and the later
// revoked-at when the row already exists so a re-revocation never narrows
// the window. MAX over the stored text values is correct because the
// fixed-width layout orders lexicographically with time. created_at records
// when the revocation landed, for audit only; no logic reads it.
func (s *SQLite) deny(ctx context.Context, kind, value string, until, revokedAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO auth_denylist (kind, value, expires_at, revoked_at, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (kind, value) DO UPDATE SET
		   expires_at = MAX(expires_at, excluded.expires_at),
		   revoked_at = MAX(revoked_at, excluded.revoked_at)`,
		kind, value, authStoredTime(until), authStoredTime(revokedAt), authStoredTime(time.Now()))
	if err != nil {
		return fmt.Errorf("deny %s %s: %w", kind, value, err)
	}

	return nil
}

// authStoredTime renders a timestamp in the shared fixed-width layout.
func authStoredTime(value time.Time) string {
	return value.UTC().Format(authTimeLayout)
}

// scanner covers sql.Row and sql.Rows for the shared scan helper.
type scanner interface {
	Scan(dest ...any) error
}

// scanKey reads one keyColumns row back into a Key.
func scanKey(row scanner) (*Key, error) {
	var (
		key       Key
		scopes    string
		createdAt sql.NullTime
	)

	err := row.Scan(&key.ID, &key.Label, &scopes, &key.Workspace, &key.Hash, &key.Enabled, &createdAt)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with the query's identity, including the sql.ErrNoRows translation
	}

	if err := json.Unmarshal([]byte(scopes), &key.Scopes); err != nil {
		return nil, fmt.Errorf("decode scopes for key %s: %w", key.ID, err)
	}

	key.CreatedAt = createdAt.Time

	return &key, nil
}

// closeRows closes rows where the read path already reports errors via
// rows.Err, making a second error from Close redundant.
func closeRows(rows *sql.Rows) {
	_ = rows.Close()
}
