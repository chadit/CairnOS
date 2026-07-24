package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// tokenShift estimates tokens from bytes: divide by 4 via shift. A crude
// heuristic (roughly four bytes of English text per token), good enough for
// Phase 0 budget math until a real tokenizer measures Content.
const tokenShift = 2

// memorySchema mirrors the spec schema, plus IF NOT EXISTS so opening an
// existing database is idempotent. External-content FTS5 does not update
// itself, so the standard INSERT/UPDATE/DELETE triggers keep memory_fts in
// sync with memory_entries.
const memorySchema = `
CREATE TABLE IF NOT EXISTS memory_entries (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL DEFAULT 'default',
    scope        TEXT NOT NULL,
    content      TEXT NOT NULL,
    importance   INTEGER NOT NULL DEFAULT 50,
    tags_json    TEXT NOT NULL DEFAULT '[]',
    token_cost   INTEGER NOT NULL DEFAULT 0,
    created_at   TIMESTAMP NOT NULL,
    updated_at   TIMESTAMP NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_memory_scope ON memory_entries(scope, importance DESC);

CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(
    content, tags, content='memory_entries', content_rowid='rowid'
);

CREATE TRIGGER IF NOT EXISTS memory_entries_fts_insert AFTER INSERT ON memory_entries BEGIN
    INSERT INTO memory_fts(rowid, content, tags) VALUES (new.rowid, new.content, new.tags_json);
END;

CREATE TRIGGER IF NOT EXISTS memory_entries_fts_delete AFTER DELETE ON memory_entries BEGIN
    INSERT INTO memory_fts(memory_fts, rowid, content, tags) VALUES ('delete', old.rowid, old.content, old.tags_json);
END;

CREATE TRIGGER IF NOT EXISTS memory_entries_fts_update AFTER UPDATE ON memory_entries BEGIN
    INSERT INTO memory_fts(memory_fts, rowid, content, tags) VALUES ('delete', old.rowid, old.content, old.tags_json);
    INSERT INTO memory_fts(rowid, content, tags) VALUES (new.rowid, new.content, new.tags_json);
END;
`

// entryColumns is the SELECT list every entry query shares, in scanEntry
// order.
const entryColumns = "id, scope, content, importance, tags_json, token_cost, created_at, updated_at"

// SQLite is the default Store backend: FTS5 keyword search, zero external
// services, no vectors (.agents/spec/05-memory-and-knowledge.md,
// "Backends"). Safe for concurrent use through WAL mode and busy_timeout.
type SQLite struct {
	db *sql.DB
}

// OpenSQLite opens (creating if needed) the memory database at path and
// applies the schema. The same pragma choices as the run store: WAL so
// readers do not block the writer, busy_timeout so concurrent writers wait
// instead of failing with SQLITE_BUSY.
func OpenSQLite(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open memory database %q: %w", path, err)
	}

	// Schema creation is process-lifetime initialization with no request to
	// inherit a context from; OpenSQLite carries no ctx on purpose.
	setupCtx := context.Background()

	if _, execErr := db.ExecContext(setupCtx, memorySchema); execErr != nil {
		return nil, fmt.Errorf("create memory schema: %w", errors.Join(execErr, db.Close()))
	}

	return &SQLite{db: db}, nil
}

// Close releases the underlying database.
func (s *SQLite) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close memory database: %w", err)
	}

	return nil
}

// Add persists a new entry and returns its id. A zero TokenCost is replaced
// with the byte heuristic, zero timestamps with the current time, and an
// empty ID with a fresh ULID, so callers only fill what they measured.
func (s *SQLite) Add(ctx context.Context, entry *Entry) (string, error) {
	id := entry.ID
	if id == "" {
		id = ulid.Make().String()
	}

	now := time.Now()

	createdAt := entry.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}

	updatedAt := entry.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = now
	}

	tags, err := marshalTags(entry.Tags)
	if err != nil {
		return "", err
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO memory_entries (id, workspace_id, scope, content, importance, tags_json, token_cost, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, workspaceFromContext(ctx), entry.Scope, entry.Content, entry.Importance, tags,
		costOrEstimate(entry.TokenCost, entry.Content), storedTime(createdAt), storedTime(updatedAt))
	if err != nil {
		return "", fmt.Errorf("insert memory entry %s: %w", id, err)
	}

	return id, nil
}

// Get fetches one entry by id, scoped to the request workspace so an entry
// in another workspace reads as not-found
// (.agents/spec/12-ui-and-governance.md, "Multi-tenancy").
func (s *SQLite) Get(ctx context.Context, id string) (Entry, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+entryColumns+" FROM memory_entries WHERE id = ? AND workspace_id = ?",
		id, workspaceFromContext(ctx))

	entry, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, fmt.Errorf("%w: %s", ErrEntryNotFound, id)
	}

	if err != nil {
		return Entry{}, fmt.Errorf("get memory entry %s: %w", id, err)
	}

	return entry, nil
}

// Replace overwrites the entry's mutable fields and refreshes UpdatedAt.
// CreatedAt is untouched so age-based ordering survives edits, and the FTS
// update trigger reindexes the new content and tags.
func (s *SQLite) Replace(ctx context.Context, id string, entry *Entry) error {
	tags, err := marshalTags(entry.Tags)
	if err != nil {
		return err
	}

	result, err := s.db.ExecContext(ctx,
		`UPDATE memory_entries
		 SET scope = ?, content = ?, importance = ?, tags_json = ?, token_cost = ?, updated_at = ?
		 WHERE id = ? AND workspace_id = ?`,
		entry.Scope, entry.Content, entry.Importance, tags,
		costOrEstimate(entry.TokenCost, entry.Content), storedTime(time.Now()), id, workspaceFromContext(ctx))
	if err != nil {
		return fmt.Errorf("replace memory entry %s: %w", id, err)
	}

	return requireRow(result, id)
}

// Delete removes one entry by id within the request workspace; the FTS delete
// trigger drops its index rows with it. An entry in another workspace is
// invisible, so the delete reports not-found rather than crossing tenants.
func (s *SQLite) Delete(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM memory_entries WHERE id = ? AND workspace_id = ?",
		id, workspaceFromContext(ctx))
	if err != nil {
		return fmt.Errorf("delete memory entry %s: %w", id, err)
	}

	return requireRow(result, id)
}

// requireRow turns a zero-row write into ErrEntryNotFound so callers can
// tell a miss from a no-op.
func requireRow(result sql.Result, id string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("memory entry %s: rows affected: %w", id, err)
	}

	if affected == 0 {
		return fmt.Errorf("%w: %s", ErrEntryNotFound, id)
	}

	return nil
}

// Search runs keyword (FTS5/bm25) search over content and tags. Phase 0 has
// no vector backend, so a Semantic query falls back to keyword search with
// SearchResult.Fallback set. A query with no text and no tags lists the
// scope by importance instead.
func (s *SQLite) Search(ctx context.Context, query Query) (SearchResult, error) {
	result := SearchResult{Fallback: query.Semantic}

	match := buildMatch(query.Text, query.Tags)
	if match == "" {
		entries, err := s.listByImportance(ctx, query.Scope, query.Limit)
		if err != nil {
			return SearchResult{}, err
		}

		result.Entries = entries

		return result, nil
	}

	// bm25 ranks best-first ascending; the id tie-break keeps equal-rank
	// hits in a stable order. The doubled scope parameter makes the filter
	// optional without assembling SQL at runtime; the workspace clause scopes
	// every hit to the request tenant.
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.id, m.scope, m.content, m.importance, m.tags_json, m.token_cost, m.created_at, m.updated_at
		 FROM memory_fts
		 JOIN memory_entries m ON m.rowid = memory_fts.rowid
		 WHERE memory_fts MATCH ? AND m.workspace_id = ? AND (? = '' OR m.scope = ?)
		 ORDER BY bm25(memory_fts), m.id
		 LIMIT ?`,
		match, workspaceFromContext(ctx), query.Scope, query.Scope, limitOrAll(query.Limit))
	if err != nil {
		return SearchResult{}, fmt.Errorf("search memory: %w", err)
	}
	defer closeRows(rows)

	result.Entries, err = collectEntries(rows)
	if err != nil {
		return SearchResult{}, fmt.Errorf("search memory: %w", err)
	}

	return result, nil
}

// Load returns the entries to inject into a prompt for scope, highest
// importance first with created_at ASC then id breaking ties the same way
// every run, until tokenBudget is spent. An entry that would cross the
// remaining budget is excluded and scanning continues, so a smaller
// lower-importance entry can still use the leftover room.
func (s *SQLite) Load(ctx context.Context, scope string, tokenBudget int) ([]Entry, error) {
	if tokenBudget <= 0 {
		return nil, nil
	}

	rows, err := s.db.QueryContext(ctx,
		"SELECT "+entryColumns+` FROM memory_entries
		 WHERE workspace_id = ? AND scope = ?
		 ORDER BY importance DESC, created_at ASC, id ASC`,
		workspaceFromContext(ctx), scope)
	if err != nil {
		return nil, fmt.Errorf("load memory scope %q: %w", scope, err)
	}
	defer closeRows(rows)

	entries, err := collectEntries(rows)
	if err != nil {
		return nil, fmt.Errorf("load memory scope %q: %w", scope, err)
	}

	remaining := tokenBudget

	loaded := make([]Entry, 0, len(entries))
	for idx := range entries {
		if entries[idx].TokenCost > remaining {
			continue
		}

		loaded = append(loaded, entries[idx])
		remaining -= entries[idx].TokenCost
	}

	return loaded, nil
}

// List returns every entry in the request workspace, optionally narrowed to
// one scope (an empty scope lists all), ordered by created_at then id so a
// repeated export writes the same files in the same order. Unlike Load it
// applies no importance ordering and no token budget: the caller wants the
// whole set for offline editing, not a prompt-sized slice.
func (s *SQLite) List(ctx context.Context, scope string) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+entryColumns+` FROM memory_entries
		 WHERE workspace_id = ? AND (? = '' OR scope = ?)
		 ORDER BY created_at ASC, id ASC`,
		workspaceFromContext(ctx), scope, scope)
	if err != nil {
		return nil, fmt.Errorf("list memory entries: %w", err)
	}
	defer closeRows(rows)

	entries, err := collectEntries(rows)
	if err != nil {
		return nil, fmt.Errorf("list memory entries: %w", err)
	}

	return entries, nil
}

// listByImportance is the no-keywords Search path: the scope's entries in
// Load order, optionally capped.
func (s *SQLite) listByImportance(ctx context.Context, scope string, limit int) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+entryColumns+` FROM memory_entries
		 WHERE workspace_id = ? AND (? = '' OR scope = ?)
		 ORDER BY importance DESC, created_at ASC, id ASC
		 LIMIT ?`,
		workspaceFromContext(ctx), scope, scope, limitOrAll(limit))
	if err != nil {
		return nil, fmt.Errorf("list memory scope %q: %w", scope, err)
	}
	defer closeRows(rows)

	entries, err := collectEntries(rows)
	if err != nil {
		return nil, fmt.Errorf("list memory scope %q: %w", scope, err)
	}

	return entries, nil
}

// buildMatch assembles the FTS5 MATCH expression: the free text as one
// phrase over all indexed columns, each tag as a phrase pinned to the tags
// column, all ANDed.
func buildMatch(text string, tags []string) string {
	terms := make([]string, 0, len(tags)+1)

	if phrase := ftsPhrase(text); phrase != "" {
		terms = append(terms, phrase)
	}

	for _, tag := range tags {
		if phrase := ftsPhrase(tag); phrase != "" {
			terms = append(terms, "tags:"+phrase)
		}
	}

	return strings.Join(terms, " AND ")
}

// ftsPhrase quotes raw user text as an FTS5 phrase. Stripping embedded
// double quotes first means the input can never terminate the phrase early
// and smuggle FTS5 query syntax in.
func ftsPhrase(raw string) string {
	cleaned := strings.TrimSpace(strings.ReplaceAll(raw, `"`, ""))
	if cleaned == "" {
		return ""
	}

	return `"` + cleaned + `"`
}

// limitOrAll maps "no limit requested" onto SQLite's LIMIT -1 (unlimited).
func limitOrAll(limit int) int {
	if limit <= 0 {
		return -1
	}

	return limit
}

// costOrEstimate keeps a measured token cost, estimating one from content
// bytes when the caller passed zero.
func costOrEstimate(cost int, content string) int {
	if cost > 0 {
		return cost
	}

	return len(content) >> tokenShift // divide by 4: crude bytes-per-token heuristic
}

// marshalTags renders tags for the tags_json column, defaulting nil to the
// schema's empty array.
func marshalTags(tags []string) (string, error) {
	if tags == nil {
		return "[]", nil
	}

	encoded, err := json.Marshal(tags)
	if err != nil {
		return "", fmt.Errorf("marshal tags: %w", err)
	}

	return string(encoded), nil
}

// storedTime renders a timestamp in fixed-width RFC 3339 UTC. The zero
// padding keeps lexicographic order matching chronological order, which the
// created_at tie-break comparisons rely on.
func storedTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

// rowScanner abstracts *sql.Row and *sql.Rows so scanEntry serves both Get
// and the listing queries.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanEntry reads one entryColumns row back into an Entry.
func scanEntry(row rowScanner) (Entry, error) {
	var (
		entry     Entry
		tags      string
		createdAt sql.NullTime
		updatedAt sql.NullTime
	)

	err := row.Scan(&entry.ID, &entry.Scope, &entry.Content, &entry.Importance,
		&tags, &entry.TokenCost, &createdAt, &updatedAt)
	if err != nil {
		return Entry{}, err //nolint:wrapcheck // callers wrap with their operation and id
	}

	if err := json.Unmarshal([]byte(tags), &entry.Tags); err != nil {
		return Entry{}, fmt.Errorf("unmarshal tags: %w", err)
	}

	entry.CreatedAt = createdAt.Time
	entry.UpdatedAt = updatedAt.Time

	return entry, nil
}

// collectEntries drains rows through scanEntry, surfacing iteration errors.
func collectEntries(rows *sql.Rows) ([]Entry, error) {
	var entries []Entry

	for rows.Next() {
		entry, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}

		entries = append(entries, entry)
	}

	if err := rows.Err(); err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with their operation
	}

	return entries, nil
}

// closeRows closes rows where the read path already reports errors via
// rows.Err, making a second error from Close redundant.
func closeRows(rows *sql.Rows) {
	_ = rows.Close()
}
