// Package postgres is the multi-node store.Store backend
// (.agents/spec/04-execution-engine.md, "State persistence"; the schema
// "mirrors this with JSONB columns and the same indexes"). It uses pgx/v5
// with pgxpool so the engine's many goroutines share a bounded connection
// pool, creates the schema on Open so a fresh database is immediately
// usable, and keeps identical semantics to the sqlite backend: same ULID
// ids, same lifecycle stamping, same retention rules. The shared Postgres
// database is what lets several nodes run against one state
// (.agents/spec/11-deployment-and-config.md, "Scaling").
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/pkg/runner"
)

// schemaSQL mirrors the spec schema with JSONB columns and timestamptz, and
// IF NOT EXISTS so Open is idempotent across restarts and across the several
// nodes that may open the same database at once. The indexes match the
// sqlite backend's so "list recent runs" and the status filter stay index
// walks here too.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS runs (
    id            TEXT PRIMARY KEY,
    workflow_name TEXT NOT NULL,
    workflow_hash TEXT NOT NULL,
    workspace_id  TEXT NOT NULL DEFAULT 'default',
    status        TEXT NOT NULL,
    inputs_json   JSONB NOT NULL DEFAULT '{}',
    trigger       TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ,
    error_json    JSONB
);

CREATE TABLE IF NOT EXISTS action_results (
    run_id        TEXT NOT NULL REFERENCES runs(id),
    action_name   TEXT NOT NULL,
    runner        TEXT NOT NULL,
    status        TEXT NOT NULL,
    attempt       INTEGER NOT NULL DEFAULT 0,
    output_json   JSONB,
    usage_json    JSONB,
    meta_json     JSONB,
    error_json    JSONB,
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ,
    PRIMARY KEY (run_id, action_name)
);

CREATE TABLE IF NOT EXISTS agent_steps (
    run_id       TEXT NOT NULL,
    action_name  TEXT NOT NULL,
    step         INTEGER NOT NULL,
    kind         TEXT NOT NULL,
    tool         TEXT,
    input_json   JSONB,
    output_json  JSONB,
    usage_json   JSONB,
    created_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (run_id, action_name, step)
);

CREATE TABLE IF NOT EXISTS suggestions (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL DEFAULT 'default',
    kind          TEXT NOT NULL,
    target        TEXT NOT NULL,
    status        TEXT NOT NULL,
    rationale     TEXT NOT NULL,
    evidence_json JSONB,
    proposed_json JSONB,
    savings_json  JSONB,
    created_at    TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_log (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL DEFAULT 'default',
    actor        TEXT NOT NULL,
    action       TEXT NOT NULL,
    target       TEXT NOT NULL,
    detail       TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS workflow_pauses (
    workspace_id  TEXT NOT NULL DEFAULT 'default',
    workflow_name TEXT NOT NULL,
    actor         TEXT NOT NULL,
    reason        TEXT NOT NULL DEFAULT '',
    paused_at     TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (workspace_id, workflow_name)
);

CREATE TABLE IF NOT EXISTS workflow_versions (
    hash            TEXT PRIMARY KEY,
    workflow_name   TEXT NOT NULL,
    definition_toml TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS chat_sessions (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL DEFAULT 'default',
    personality  TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS chat_messages (
    session_id TEXT NOT NULL REFERENCES chat_sessions(id),
    seq        INTEGER NOT NULL,
    role       TEXT NOT NULL,
    content    TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (session_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_runs_workflow ON runs(workflow_name, created_at);
CREATE INDEX IF NOT EXISTS idx_runs_status   ON runs(status);
CREATE INDEX IF NOT EXISTS idx_suggestions_status ON suggestions(workspace_id, status, created_at);
CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_log(workspace_id, created_at);
CREATE INDEX IF NOT EXISTS idx_versions_workflow ON workflow_versions(workflow_name, created_at);
CREATE INDEX IF NOT EXISTS idx_chat_sessions_workspace ON chat_sessions(workspace_id, updated_at);
`

// Store is the Postgres-backed implementation of store.Store. It is safe for
// concurrent use: pgxpool hands out connections from a bounded pool, every
// statement carries its own ctx, and Postgres serializes conflicting writes
// at the row level.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to the database named by dsn (a libpq/pgx URL or keyword
// string, expected to carry credentials from CAIRNOS_STORE_DSN), verifies
// the connection, and applies the schema. The pool is created with pgx's
// defaults; a caller wanting a different pool size sets it in the DSN
// (pool_max_conns).
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres pool: %w", err)
	}

	if pingErr := pool.Ping(ctx); pingErr != nil {
		pool.Close()

		return nil, fmt.Errorf("ping postgres: %w", pingErr)
	}

	if _, execErr := pool.Exec(ctx, schemaSQL); execErr != nil {
		pool.Close()

		return nil, fmt.Errorf("create schema: %w", execErr)
	}

	return &Store{pool: pool}, nil
}

// NewID returns a ULID, identical to the sqlite backend so run ids are the
// same time-sortable, coordination-free shape across backends.
func NewID() string {
	return ulid.Make().String()
}

// Pool exposes the underlying connection pool so the scheduler's
// PostgresCoordinator can take its advisory lock on the same database the
// store uses, instead of opening a second pool. The store still owns the
// pool's lifetime; the coordinator only borrows a connection from it.
func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

// Close releases the connection pool. pgxpool.Close blocks until in-flight
// queries finish, so a returning Close means every connection is back.
func (s *Store) Close() error {
	s.pool.Close()

	return nil
}

// nullableTime renders a timestamp for storage, or nil for the zero time so
// "not yet started/finished" stays a SQL NULL the way it does in sqlite.
func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}

	return value.UTC()
}

// timeFromNull reads a nullable timestamptz back into a time.Time, mapping a
// NULL (nil pointer) to the zero time.
func timeFromNull(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}

	return *value
}

// collectRows drains rows, scanning each into a *T with scan and wrapping any
// failure under operation. It owns closing rows, so list queries that share the
// same next-scan-append-Err shape call it instead of repeating that loop, which
// also keeps two such lists from reading as a dupl false positive when their
// only real difference is the column set and scan func. It mirrors the sqlite
// backend's helper of the same name.
func collectRows[T any](rows pgx.Rows, operation string, scan func(rowScanner) (*T, error)) ([]*T, error) {
	defer rows.Close()

	var items []*T

	for rows.Next() {
		item, scanErr := scan(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("%s: %w", operation, scanErr)
		}

		items = append(items, item)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", operation, err)
	}

	return items, nil
}

// marshalJSON renders value as a JSONB parameter, or nil (SQL NULL) when
// absent. Returning the bytes rather than a string lets pgx send them as
// jsonb without a second parse.
func marshalJSON(value any, absent bool) (any, error) {
	if absent {
		return nil, nil //nolint:nilnil // a nil JSONB parameter is the deliberate SQL NULL, not an error
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal json column: %w", err)
	}

	return encoded, nil
}

// unmarshalJSON decodes a nullable JSONB column into target, leaving target
// untouched on NULL. pgx scans a jsonb column into []byte.
func unmarshalJSON(column []byte, target any) error {
	if column == nil {
		return nil
	}

	if err := json.Unmarshal(column, target); err != nil {
		return fmt.Errorf("unmarshal json column: %w", err)
	}

	return nil
}

// marshalError renders a run or action error for its error_json column.
func marshalError(failure *cairnerr.Error) (any, error) {
	return marshalJSON(failure, failure == nil)
}

// unmarshalError reads a nullable error_json column into target, leaving a
// NULL column as the nil error the domain types use for "no failure".
func unmarshalError(column []byte, target **cairnerr.Error) error {
	if column == nil {
		return nil
	}

	failure := &cairnerr.Error{}
	if err := unmarshalJSON(column, failure); err != nil {
		return err
	}

	*target = failure

	return nil
}

// usageRecord mirrors runner.Usage with JSON tags so the persisted shape is
// a stable contract independent of that package's field names, and matches
// the sqlite backend's encoding byte for byte.
type usageRecord struct {
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CachedTokens int64   `json:"cached_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	LatencyNS    int64   `json:"latency_ns"`
	Retries      int     `json:"retries"`
}

// marshalUsage renders usage for its usage_json column.
func marshalUsage(usage runner.Usage) (any, error) {
	record := usageRecord{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		CachedTokens: usage.CachedTokens,
		CostUSD:      usage.CostUSD,
		LatencyNS:    int64(usage.Latency),
		Retries:      usage.Retries,
	}

	return marshalJSON(record, false)
}

// unmarshalUsage reads a nullable usage_json column.
func unmarshalUsage(column []byte) (runner.Usage, error) {
	var record usageRecord
	if err := unmarshalJSON(column, &record); err != nil {
		return runner.Usage{}, err
	}

	return runner.Usage{
		InputTokens:  record.InputTokens,
		OutputTokens: record.OutputTokens,
		CachedTokens: record.CachedTokens,
		CostUSD:      record.CostUSD,
		Latency:      time.Duration(record.LatencyNS),
		Retries:      record.Retries,
	}, nil
}

// isNoRows reports whether err is pgx's "query returned no rows" so GetRun
// and GetSuggestion can translate it into their not-found sentinels.
func isNoRows(err error) bool {
	return errors.Is(err, errNoRows)
}
