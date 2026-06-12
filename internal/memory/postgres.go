package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"
)

// pgMemorySchema mirrors the spec memory table with JSONB tags and
// timestamptz, plus IF NOT EXISTS so opening an existing database is
// idempotent. Keyword search uses a GIN index over to_tsvector(content),
// the Postgres analog of sqlite's FTS5; there is no separate virtual table
// because Postgres full-text search runs against the base row.
//
// The pgvector path stays a stub for Phase 4c: when semantic search ships,
// this schema gains an `embedding vector(768)` column and an ivfflat/hnsw
// index, populated by embedding content through a configured embedding
// runner (.agents/spec/05-memory-and-knowledge.md, "Backends"). No vector
// backend ships in this phase, so a semantic query falls back to keyword and
// flags it, exactly like the sqlite backend.
const pgMemorySchema = `
CREATE TABLE IF NOT EXISTS memory_entries (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL DEFAULT 'default',
    scope        TEXT NOT NULL,
    content      TEXT NOT NULL,
    importance   INTEGER NOT NULL DEFAULT 50,
    tags_json    JSONB NOT NULL DEFAULT '[]',
    token_cost   INTEGER NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_memory_scope ON memory_entries(scope, importance DESC);

CREATE INDEX IF NOT EXISTS idx_memory_fts ON memory_entries
    USING GIN (to_tsvector('simple', content || ' ' || coalesce(tags_json #>> '{}', '')));
`

// pgEntryColumns is the SELECT list every Postgres entry query shares, in
// scanPGEntry order.
const pgEntryColumns = "id, scope, content, importance, tags_json, token_cost, created_at, updated_at"

// Postgres is the shared, multi-node Store backend
// (.agents/spec/05-memory-and-knowledge.md, "Backends"): keyword search via
// to_tsvector/plainto_tsquery, no vectors yet. Safe for concurrent use
// through pgxpool. It shares the run store's database (different tables), the
// same arrangement the sqlite backends use within one file.
type Postgres struct {
	pool *pgxpool.Pool
}

// OpenPostgres connects to dsn, verifies the connection, and applies the
// memory schema. The DSN carries credentials from CAIRNOS_STORE_DSN; pool
// sizing rides the DSN (pool_max_conns) like the run store.
func OpenPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open memory postgres pool: %w", err)
	}

	if pingErr := pool.Ping(ctx); pingErr != nil {
		pool.Close()

		return nil, fmt.Errorf("ping memory postgres: %w", pingErr)
	}

	if _, execErr := pool.Exec(ctx, pgMemorySchema); execErr != nil {
		pool.Close()

		return nil, fmt.Errorf("create memory schema: %w", execErr)
	}

	return &Postgres{pool: pool}, nil
}

// Close releases the connection pool.
func (s *Postgres) Close() error {
	s.pool.Close()

	return nil
}

// Add persists a new entry and returns its id. Defaults match the sqlite
// backend: a zero TokenCost is the byte heuristic, zero timestamps the
// current time, an empty ID a fresh ULID.
func (s *Postgres) Add(ctx context.Context, entry *Entry) (string, error) {
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

	tags, err := marshalTagsJSON(entry.Tags)
	if err != nil {
		return "", err
	}

	_, err = s.pool.Exec(ctx,
		`INSERT INTO memory_entries (id, workspace_id, scope, content, importance, tags_json, token_cost, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		id, workspaceFromContext(ctx), entry.Scope, entry.Content, entry.Importance, tags,
		costOrEstimatePG(entry.TokenCost, entry.Content), createdAt.UTC(), updatedAt.UTC())
	if err != nil {
		return "", fmt.Errorf("insert memory entry %s: %w", id, err)
	}

	return id, nil
}

// Get fetches one entry by id, scoped to the request workspace so an entry in
// another workspace reads as not-found (.agents/spec/12-ui-and-governance.md,
// "Multi-tenancy").
func (s *Postgres) Get(ctx context.Context, id string) (Entry, error) {
	row := s.pool.QueryRow(ctx,
		"SELECT "+pgEntryColumns+" FROM memory_entries WHERE id = $1 AND workspace_id = $2",
		id, workspaceFromContext(ctx))

	entry, err := scanPGEntry(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, fmt.Errorf("%w: %s", ErrEntryNotFound, id)
	}

	if err != nil {
		return Entry{}, fmt.Errorf("get memory entry %s: %w", id, err)
	}

	return entry, nil
}

// Replace overwrites the entry's mutable fields and refreshes UpdatedAt.
// CreatedAt is untouched so age-based ordering survives edits, and the
// GIN-indexed tsvector reindexes the new content automatically because it is
// computed from the base row.
func (s *Postgres) Replace(ctx context.Context, id string, entry *Entry) error {
	tags, err := marshalTagsJSON(entry.Tags)
	if err != nil {
		return err
	}

	tag, err := s.pool.Exec(ctx,
		`UPDATE memory_entries
		 SET scope = $1, content = $2, importance = $3, tags_json = $4, token_cost = $5, updated_at = $6
		 WHERE id = $7 AND workspace_id = $8`,
		entry.Scope, entry.Content, entry.Importance, tags,
		costOrEstimatePG(entry.TokenCost, entry.Content), time.Now().UTC(), id, workspaceFromContext(ctx))
	if err != nil {
		return fmt.Errorf("replace memory entry %s: %w", id, err)
	}

	return requirePGRow(tag.RowsAffected(), id)
}

// Delete removes one entry by id within the request workspace; an entry in
// another workspace is invisible, so the delete reports not-found rather than
// crossing tenants.
func (s *Postgres) Delete(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx,
		"DELETE FROM memory_entries WHERE id = $1 AND workspace_id = $2",
		id, workspaceFromContext(ctx))
	if err != nil {
		return fmt.Errorf("delete memory entry %s: %w", id, err)
	}

	return requirePGRow(tag.RowsAffected(), id)
}

// Search runs keyword search over content and tags with to_tsvector /
// plainto_tsquery, the Postgres analog of sqlite's FTS5. Phase 4c has no
// vector backend, so a Semantic query falls back to keyword search with
// SearchResult.Fallback set, the same contract the sqlite backend honors. A
// query with no text and no tags lists the scope by importance instead.
func (s *Postgres) Search(ctx context.Context, query Query) (SearchResult, error) {
	result := SearchResult{Fallback: query.Semantic}

	terms := searchTerms(query.Text, query.Tags)
	if terms == "" {
		entries, err := s.listByImportance(ctx, query.Scope, query.Limit)
		if err != nil {
			return SearchResult{}, err
		}

		result.Entries = entries

		return result, nil
	}

	// ts_rank ranks best-first descending; the id tie-break keeps equal-rank
	// hits in a stable order. The doubled scope parameter makes the filter
	// optional without assembling SQL at runtime, and NULL LIMIT means "all".
	// The workspace clause scopes every hit to the request tenant.
	rows, err := s.pool.Query(ctx,
		`SELECT id, scope, content, importance, tags_json, token_cost, created_at, updated_at
		 FROM memory_entries
		 WHERE to_tsvector('simple', content || ' ' || coalesce(tags_json #>> '{}', '')) @@ plainto_tsquery('simple', $1)
		   AND workspace_id = $2
		   AND ($3 = '' OR scope = $3)
		 ORDER BY ts_rank(to_tsvector('simple', content || ' ' || coalesce(tags_json #>> '{}', '')), plainto_tsquery('simple', $1)) DESC, id
		 LIMIT $4`,
		terms, workspaceFromContext(ctx), query.Scope, limitOrNull(query.Limit))
	if err != nil {
		return SearchResult{}, fmt.Errorf("search memory: %w", err)
	}
	defer rows.Close()

	result.Entries, err = collectPGEntries(rows)
	if err != nil {
		return SearchResult{}, fmt.Errorf("search memory: %w", err)
	}

	return result, nil
}

// Load returns the entries to inject into a prompt for scope, highest
// importance first with created_at ASC then id breaking ties the same way
// every run, until tokenBudget is spent. An entry that would cross the
// remaining budget is excluded and scanning continues, so a smaller
// lower-importance entry can still use the leftover room. Behavior matches
// the sqlite backend exactly.
func (s *Postgres) Load(ctx context.Context, scope string, tokenBudget int) ([]Entry, error) {
	if tokenBudget <= 0 {
		return nil, nil
	}

	rows, err := s.pool.Query(ctx,
		"SELECT "+pgEntryColumns+` FROM memory_entries
		 WHERE workspace_id = $1 AND scope = $2
		 ORDER BY importance DESC, created_at ASC, id ASC`,
		workspaceFromContext(ctx), scope)
	if err != nil {
		return nil, fmt.Errorf("load memory scope %q: %w", scope, err)
	}
	defer rows.Close()

	entries, err := collectPGEntries(rows)
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

// listByImportance is the no-keywords Search path: the scope's entries in
// Load order, optionally capped.
func (s *Postgres) listByImportance(ctx context.Context, scope string, limit int) ([]Entry, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+pgEntryColumns+` FROM memory_entries
		 WHERE workspace_id = $1 AND ($2 = '' OR scope = $2)
		 ORDER BY importance DESC, created_at ASC, id ASC
		 LIMIT $3`,
		workspaceFromContext(ctx), scope, limitOrNull(limit))
	if err != nil {
		return nil, fmt.Errorf("list memory scope %q: %w", scope, err)
	}
	defer rows.Close()

	entries, err := collectPGEntries(rows)
	if err != nil {
		return nil, fmt.Errorf("list memory scope %q: %w", scope, err)
	}

	return entries, nil
}

// searchTerms joins the free text and the tag values into one
// plainto_tsquery input. plainto_tsquery ANDs the lexemes itself, so passing
// the words together matches content or tags that contain all of them, which
// keeps the "content and tag combine" behavior the sqlite backend has.
func searchTerms(text string, tags []string) string {
	parts := make([]string, 0, len(tags)+1)

	if trimmed := strings.TrimSpace(text); trimmed != "" {
		parts = append(parts, trimmed)
	}

	for _, tag := range tags {
		if trimmed := strings.TrimSpace(tag); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}

	return strings.Join(parts, " ")
}

// limitOrNull maps "no limit requested" onto a SQL NULL, which Postgres
// treats as "no limit" in LIMIT.
func limitOrNull(limit int) any {
	if limit <= 0 {
		return nil
	}

	return limit
}

// costOrEstimatePG keeps a measured token cost, estimating one from content
// bytes when the caller passed zero, the same heuristic as the sqlite
// backend.
func costOrEstimatePG(cost int, content string) int {
	if cost > 0 {
		return cost
	}

	return len(content) >> tokenShift // divide by 4: crude bytes-per-token heuristic
}

// marshalTagsJSON renders tags for the tags_json column, defaulting nil to
// the schema's empty array.
func marshalTagsJSON(tags []string) (any, error) {
	if tags == nil {
		return []byte("[]"), nil
	}

	encoded, err := json.Marshal(tags)
	if err != nil {
		return nil, fmt.Errorf("marshal tags: %w", err)
	}

	return encoded, nil
}

// requirePGRow turns a zero-row write into ErrEntryNotFound so callers can
// tell a miss from a no-op.
func requirePGRow(affected int64, id string) error {
	if affected == 0 {
		return fmt.Errorf("%w: %s", ErrEntryNotFound, id)
	}

	return nil
}

// scanPGEntry reads one pgEntryColumns row back into an Entry. It reuses the
// package's rowScanner (declared in sqlite.go) since pgx.Row and pgx.Rows
// satisfy the same one-method Scan interface as the database/sql types.
func scanPGEntry(row rowScanner) (Entry, error) {
	var (
		entry     Entry
		tags      []byte
		createdAt time.Time
		updatedAt time.Time
	)

	err := row.Scan(&entry.ID, &entry.Scope, &entry.Content, &entry.Importance,
		&tags, &entry.TokenCost, &createdAt, &updatedAt)
	if err != nil {
		return Entry{}, err //nolint:wrapcheck // callers wrap with their operation and id
	}

	if err := json.Unmarshal(tags, &entry.Tags); err != nil {
		return Entry{}, fmt.Errorf("unmarshal tags: %w", err)
	}

	entry.CreatedAt = createdAt
	entry.UpdatedAt = updatedAt

	return entry, nil
}

// collectPGEntries drains rows through scanPGEntry, surfacing iteration
// errors.
func collectPGEntries(rows pgx.Rows) ([]Entry, error) {
	var entries []Entry

	for rows.Next() {
		entry, err := scanPGEntry(rows)
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
