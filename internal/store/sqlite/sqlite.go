// Package sqlite is the default store.Store backend
// (.agents/spec/04-execution-engine.md, "State persistence"). It uses the
// pure-Go modernc.org/sqlite driver so CairnOS builds without CGo, opens the
// database in WAL mode with a busy timeout so the engine's many goroutines
// can write concurrently, and creates the schema on Open so a fresh path is
// immediately usable.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/pkg/runner"
)

// limitClause is the SQL LIMIT suffix shared by the paginated list queries.
const limitClause = " LIMIT ?"

// timeLayout is RFC 3339 UTC with fixed-width nanoseconds. The zero padding
// matters: trailing-zero truncation (time.RFC3339Nano) breaks the
// lexicographic ordering that the created_at index comparisons and pruning
// cutoffs rely on.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

// schemaSQL mirrors the spec schema verbatim, plus IF NOT EXISTS so Open is
// idempotent across restarts.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS runs (
    id            TEXT PRIMARY KEY,
    workflow_name TEXT NOT NULL,
    workflow_hash TEXT NOT NULL,
    workspace_id  TEXT NOT NULL DEFAULT 'default',
    status        TEXT NOT NULL,
    inputs_json   TEXT NOT NULL DEFAULT '{}',
    trigger       TEXT NOT NULL,
    created_at    TIMESTAMP NOT NULL,
    started_at    TIMESTAMP,
    finished_at   TIMESTAMP,
    error_json    TEXT
);

CREATE TABLE IF NOT EXISTS action_results (
    run_id        TEXT NOT NULL REFERENCES runs(id),
    action_name   TEXT NOT NULL,
    runner        TEXT NOT NULL,
    status        TEXT NOT NULL,
    attempt       INTEGER NOT NULL DEFAULT 0,
    output_json   TEXT,
    usage_json    TEXT,
    meta_json     TEXT,
    error_json    TEXT,
    started_at    TIMESTAMP,
    finished_at   TIMESTAMP,
    PRIMARY KEY (run_id, action_name)
);

CREATE TABLE IF NOT EXISTS agent_steps (
    run_id       TEXT NOT NULL,
    action_name  TEXT NOT NULL,
    step         INTEGER NOT NULL,
    kind         TEXT NOT NULL,
    tool         TEXT,
    input_json   TEXT,
    output_json  TEXT,
    usage_json   TEXT,
    created_at   TIMESTAMP NOT NULL,
    PRIMARY KEY (run_id, action_name, step)
);

CREATE TABLE IF NOT EXISTS suggestions (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL DEFAULT 'default',
    kind          TEXT NOT NULL,
    target        TEXT NOT NULL,
    status        TEXT NOT NULL,
    rationale     TEXT NOT NULL,
    evidence_json TEXT,
    proposed_json TEXT,
    savings_json  TEXT,
    created_at    TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_log (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL DEFAULT 'default',
    actor        TEXT NOT NULL,
    action       TEXT NOT NULL,
    target       TEXT NOT NULL,
    detail       TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS workflow_pauses (
    workspace_id  TEXT NOT NULL DEFAULT 'default',
    workflow_name TEXT NOT NULL,
    actor         TEXT NOT NULL,
    reason        TEXT NOT NULL DEFAULT '',
    paused_at     TIMESTAMP NOT NULL,
    PRIMARY KEY (workspace_id, workflow_name)
);

CREATE TABLE IF NOT EXISTS workflow_versions (
    hash            TEXT PRIMARY KEY,
    workflow_name   TEXT NOT NULL,
    definition_toml TEXT NOT NULL,
    created_at      TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS chat_sessions (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL DEFAULT 'default',
    personality  TEXT NOT NULL,
    created_at   TIMESTAMP NOT NULL,
    updated_at   TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS chat_messages (
    session_id TEXT NOT NULL REFERENCES chat_sessions(id),
    seq        INTEGER NOT NULL,
    role       TEXT NOT NULL,
    content    TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL,
    PRIMARY KEY (session_id, seq)
);

CREATE TABLE IF NOT EXISTS run_approvals (
    run_id       TEXT NOT NULL,
    action_name  TEXT NOT NULL,
    workspace_id TEXT NOT NULL DEFAULT 'default',
    status       TEXT NOT NULL,
    message      TEXT NOT NULL DEFAULT '',
    actor        TEXT NOT NULL DEFAULT '',
    comment      TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMP NOT NULL,
    decided_at   TIMESTAMP,
    PRIMARY KEY (run_id, action_name)
);

CREATE TABLE IF NOT EXISTS bridge_sessions (
    workspace_id TEXT NOT NULL DEFAULT 'default',
    transport    TEXT NOT NULL,
    chat_key     TEXT NOT NULL,
    session_id   TEXT NOT NULL,
    updated_at   TIMESTAMP NOT NULL,
    PRIMARY KEY (workspace_id, transport, chat_key)
);

CREATE TABLE IF NOT EXISTS message_threads (
    workspace_id TEXT NOT NULL DEFAULT 'default',
    identity     TEXT NOT NULL,
    target       TEXT NOT NULL,
    thread_key   TEXT NOT NULL,
    root_id      TEXT NOT NULL,
    updated_at   TIMESTAMP NOT NULL,
    PRIMARY KEY (workspace_id, identity, target, thread_key)
);

CREATE VIRTUAL TABLE IF NOT EXISTS chat_messages_fts USING fts5(
    content, content='chat_messages', content_rowid='rowid'
);

CREATE TRIGGER IF NOT EXISTS chat_messages_fts_insert AFTER INSERT ON chat_messages BEGIN
    INSERT INTO chat_messages_fts(rowid, content) VALUES (new.rowid, new.content);
END;

CREATE TRIGGER IF NOT EXISTS chat_messages_fts_delete AFTER DELETE ON chat_messages BEGIN
    INSERT INTO chat_messages_fts(chat_messages_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
END;

CREATE TRIGGER IF NOT EXISTS chat_messages_fts_update AFTER UPDATE ON chat_messages BEGIN
    INSERT INTO chat_messages_fts(chat_messages_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
    INSERT INTO chat_messages_fts(rowid, content) VALUES (new.rowid, new.content);
END;

CREATE INDEX IF NOT EXISTS idx_runs_workflow ON runs(workflow_name, created_at);
CREATE INDEX IF NOT EXISTS idx_runs_status   ON runs(status);
CREATE INDEX IF NOT EXISTS idx_suggestions_status ON suggestions(workspace_id, status, created_at);
CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_log(workspace_id, created_at);
CREATE INDEX IF NOT EXISTS idx_versions_workflow ON workflow_versions(workflow_name, created_at);
CREATE INDEX IF NOT EXISTS idx_chat_sessions_workspace ON chat_sessions(workspace_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_run_approvals_pending ON run_approvals(workspace_id, run_id, status);
`

// Store is the SQLite-backed implementation of store.Store. It is safe for
// concurrent use: database/sql pools connections, every connection carries
// the WAL and busy_timeout pragmas from the DSN, and SQLite serializes the
// single writer while readers proceed.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and applies the
// schema. The DSN turns on WAL so readers do not block the writer,
// busy_timeout so a second writer waits instead of failing with
// SQLITE_BUSY, and foreign_keys for the action_results reference.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite database %q: %w", path, err)
	}

	// Schema creation is process-lifetime initialization with no request to
	// inherit a context from; Open's contract carries no ctx on purpose.
	setupCtx := context.Background()

	// Databases created before transcript search hold chat_messages rows the
	// FTS triggers never saw, so probe for the index table before the schema
	// runs: absent-then-created means this open must backfill it once.
	ftsPresent, probeErr := chatSearchIndexPresent(setupCtx, db)
	if probeErr != nil {
		return nil, fmt.Errorf("probe chat search index: %w", errors.Join(probeErr, db.Close()))
	}

	if _, execErr := db.ExecContext(setupCtx, schemaSQL); execErr != nil {
		return nil, fmt.Errorf("create schema: %w", errors.Join(execErr, db.Close()))
	}

	// runs.parent_run_id rides a guarded ALTER because the schema has no
	// migration framework: CREATE TABLE IF NOT EXISTS never adds a column to a
	// database an older binary already created, and sqlite lacks ADD COLUMN IF
	// NOT EXISTS, so this probes first (.agents/spec/parity/workflow-language.md).
	if alterErr := addRunsParentColumn(setupCtx, db); alterErr != nil {
		return nil, fmt.Errorf("upgrade runs schema: %w", errors.Join(alterErr, db.Close()))
	}

	if !ftsPresent {
		// FTS5's 'rebuild' reindexes the whole table, so it only runs on the
		// one open that created the index; a reopen finds the table present
		// and skips it, keeping startup cheap.
		if _, rebuildErr := db.ExecContext(setupCtx,
			"INSERT INTO chat_messages_fts(chat_messages_fts) VALUES ('rebuild')"); rebuildErr != nil {
			return nil, fmt.Errorf("backfill chat search index: %w", errors.Join(rebuildErr, db.Close()))
		}
	}

	return &Store{db: db}, nil
}

// chatSearchIndexPresent reports whether the chat_messages_fts table already
// exists, the guard that decides if Open owes the one-time search backfill.
func chatSearchIndexPresent(ctx context.Context, db *sql.DB) (bool, error) {
	var count int

	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'chat_messages_fts'").Scan(&count)
	if err != nil {
		return false, fmt.Errorf("query sqlite_master: %w", err)
	}

	return count > 0, nil
}

// addRunsParentColumn adds runs.parent_run_id when a pre-upgrade database
// lacks it. sqlite has no ADD COLUMN IF NOT EXISTS, so it probes
// pragma table_info first and only ALTERs when the column is missing, which
// keeps a reopen of an already-upgraded database a no-op.
func addRunsParentColumn(ctx context.Context, db *sql.DB) error {
	present, err := runsHasParentColumn(ctx, db)
	if err != nil {
		return err
	}

	if present {
		return nil
	}

	if _, err := db.ExecContext(ctx, "ALTER TABLE runs ADD COLUMN parent_run_id TEXT"); err != nil {
		return fmt.Errorf("add runs.parent_run_id: %w", err)
	}

	return nil
}

// runsHasParentColumn reports whether the runs table already carries the
// parent_run_id column, the guard that decides if Open owes the one-time ALTER.
func runsHasParentColumn(ctx context.Context, db *sql.DB) (bool, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info(runs)")
	if err != nil {
		return false, fmt.Errorf("probe runs columns: %w", err)
	}
	defer closeQuietly(rows)

	for rows.Next() {
		var (
			cid       int
			name      string
			colType   string
			notNull   int
			dflt      sql.NullString
			primaryPK int
		)

		if scanErr := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &primaryPK); scanErr != nil {
			return false, fmt.Errorf("scan runs column: %w", scanErr)
		}

		if name == "parent_run_id" {
			return true, nil
		}
	}

	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("probe runs columns: %w", err)
	}

	return false, nil
}

// NewID returns a ULID: time-sortable so "list recent runs" is an index
// walk, and coordination-free so concurrent producers never collide.
func NewID() string {
	return ulid.Make().String()
}

// Close releases the underlying database.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close sqlite database: %w", err)
	}

	return nil
}

// formatTime renders a timestamp for storage, or NULL for the zero time so
// "not yet started/finished" stays distinguishable in SQL.
func formatTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}

	return value.UTC().Format(timeLayout)
}

// parseTime reads a nullable stored timestamp back into a time.Time, mapping
// NULL to the zero time. The driver already parses TIMESTAMP-declared
// columns into time.Time, so scanning goes through sql.NullTime.
func parseTime(value sql.NullTime) time.Time {
	if !value.Valid {
		return time.Time{}
	}

	return value.Time
}

// collectRows drains rows into a *T slice with scan, wrapping any failure under
// operation and closing rows itself. List queries sharing the
// next-scan-append-Err shape call it rather than repeat that loop, which also
// keeps two such lists from reading as a dupl false positive when only their
// columns and scan func differ.
func collectRows[T any](rows *sql.Rows, operation string, scan func(rowScanner) (*T, error)) ([]*T, error) {
	defer closeQuietly(rows)

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

// marshalJSON renders value as a nullable JSON column; absent selects NULL.
func marshalJSON(value any, absent bool) (sql.NullString, error) {
	if absent {
		return sql.NullString{}, nil
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("marshal json column: %w", err)
	}

	return sql.NullString{String: string(encoded), Valid: true}, nil
}

// unmarshalJSON decodes a nullable JSON column into target, leaving target
// untouched on NULL.
func unmarshalJSON(column sql.NullString, target any) error {
	if !column.Valid {
		return nil
	}

	if err := json.Unmarshal([]byte(column.String), target); err != nil {
		return fmt.Errorf("unmarshal json column: %w", err)
	}

	return nil
}

// marshalError renders a run or action error for its error_json column.
func marshalError(failure *cairnerr.Error) (sql.NullString, error) {
	return marshalJSON(failure, failure == nil)
}

// unmarshalError reads a nullable error_json column into target, leaving a
// NULL column as the nil error the domain types use for "no failure".
func unmarshalError(column sql.NullString, target **cairnerr.Error) error {
	if !column.Valid {
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
// a stable contract independent of that package's field names.
type usageRecord struct {
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CachedTokens int64   `json:"cached_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	LatencyNS    int64   `json:"latency_ns"`
	Retries      int     `json:"retries"`
}

// marshalUsage renders usage for its usage_json column.
func marshalUsage(usage runner.Usage) (sql.NullString, error) {
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
func unmarshalUsage(column sql.NullString) (runner.Usage, error) {
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
