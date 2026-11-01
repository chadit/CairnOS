package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"
)

// pgVaultSchema mirrors the sqlite vault tables, down to the links table and
// its target index, so backlink derivation behaves identically. Keyword search
// uses a GIN index over to_tsvector(title||body) because Postgres full-text
// search runs against the base row, not a separate virtual table.
//
// The pgvector path stays a stub (D2): a semantic Query falls back to keyword
// and flags it.
const pgVaultSchema = `
CREATE TABLE IF NOT EXISTS documents (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL DEFAULT 'default',
    title        TEXT NOT NULL,
    body         TEXT NOT NULL,
    tags_json    JSONB NOT NULL DEFAULT '[]',
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_documents_workspace ON documents(workspace_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_documents_fts ON documents
    USING GIN (to_tsvector('simple', title || ' ' || body));

CREATE TABLE IF NOT EXISTS document_links (
    workspace_id TEXT NOT NULL DEFAULT 'default',
    doc_id       TEXT NOT NULL,
    target_id    TEXT NOT NULL,
    PRIMARY KEY (workspace_id, doc_id, target_id)
);

CREATE INDEX IF NOT EXISTS idx_document_links_target ON document_links(workspace_id, target_id);
`

// pgDocColumns is the SELECT list every Postgres document query shares, in
// scanPGDocument order.
const pgDocColumns = "id, title, body, tags_json, created_at, updated_at"

// Postgres is the shared, multi-node Store backend: keyword search via
// to_tsvector/plainto_tsquery, a links table for backlinks, no vectors yet.
// Safe for concurrent use through pgxpool. It shares the run store's database
// (different tables), the same arrangement the sqlite backends use within one
// file.
type Postgres struct {
	pool *pgxpool.Pool
}

// OpenPostgres connects to dsn, verifies the connection, and applies the vault
// schema. The DSN carries credentials from CAIRNOS_STORE_DSN; pool sizing
// rides the DSN (pool_max_conns) like the other stores.
func OpenPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open vault postgres pool: %w", err)
	}

	if pingErr := pool.Ping(ctx); pingErr != nil {
		pool.Close()

		return nil, fmt.Errorf("ping vault postgres: %w", pingErr)
	}

	if _, execErr := pool.Exec(ctx, pgVaultSchema); execErr != nil {
		pool.Close()

		return nil, fmt.Errorf("create vault schema: %w", execErr)
	}

	return &Postgres{pool: pool}, nil
}

// Close releases the connection pool.
func (s *Postgres) Close() error {
	s.pool.Close()

	return nil
}

// Put persists a new document or overwrites an existing one, returning the id.
// It runs in a transaction so the document row and its link rows commit
// together, the same atomicity the sqlite backend gives. CreatedAt is
// preserved across an overwrite so an edit does not reset age.
func (s *Postgres) Put(ctx context.Context, doc *Document) (string, error) {
	id := doc.ID
	if id == "" {
		id = ulid.Make().String()
	}

	workspace := workspaceFromContext(ctx)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin vault put: %w", err)
	}
	defer rollbackPG(ctx, tx)

	createdAt, err := pgPutCreatedAt(ctx, tx, id, workspace, doc.CreatedAt)
	if err != nil {
		return "", err
	}

	if err := pgUpsertDocument(ctx, tx, id, workspace, doc, createdAt); err != nil {
		return "", err
	}

	if err := pgRewriteLinks(ctx, tx, id, workspace, doc.Links); err != nil {
		return "", err
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit vault put %s: %w", id, err)
	}

	return id, nil
}

// pgPutCreatedAt picks the created_at to store: the document's own value when
// set, the existing row's value when overwriting, and now for a fresh
// document.
func pgPutCreatedAt(ctx context.Context, tx pgx.Tx, id, workspace string, declared time.Time) (time.Time, error) {
	if !declared.IsZero() {
		return declared, nil
	}

	var existing time.Time

	row := tx.QueryRow(ctx,
		"SELECT created_at FROM documents WHERE id = $1 AND workspace_id = $2", id, workspace)

	switch err := row.Scan(&existing); {
	case errors.Is(err, pgx.ErrNoRows):
		return time.Now(), nil
	case err != nil:
		return time.Time{}, fmt.Errorf("read existing created_at for %s: %w", id, err)
	default:
		return existing, nil
	}
}

// pgUpsertDocument writes the document row, replacing an existing id in place
// so Put doubles as an update. updated_at is always now.
func pgUpsertDocument(ctx context.Context, tx pgx.Tx, id, workspace string, doc *Document, createdAt time.Time) error {
	tags, err := marshalTagsJSON(doc.Tags)
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO documents (id, workspace_id, title, body, tags_json, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT(id) DO UPDATE SET
		     title = excluded.title, body = excluded.body, tags_json = excluded.tags_json,
		     updated_at = excluded.updated_at`,
		id, workspace, doc.Title, doc.Body, tags, createdAt.UTC(), time.Now().UTC())
	if err != nil {
		return fmt.Errorf("upsert document %s: %w", id, err)
	}

	return nil
}

// pgRewriteLinks replaces a document's outbound link rows wholesale in one
// batch insert, matching the sqlite backend's behavior and avoiding a per-link
// round trip.
func pgRewriteLinks(ctx context.Context, tx pgx.Tx, id, workspace string, links []string) error {
	if _, err := tx.Exec(ctx,
		"DELETE FROM document_links WHERE doc_id = $1 AND workspace_id = $2", id, workspace); err != nil {
		return fmt.Errorf("clear links for %s: %w", id, err)
	}

	targets := dedupeLinks(links, id)
	if len(targets) == 0 {
		return nil
	}

	query, args := pgBuildLinkInsert(id, workspace, targets)

	if _, err := tx.Exec(ctx, query, args...); err != nil {
		return fmt.Errorf("insert links for %s: %w", id, err)
	}

	return nil
}

// pgBuildLinkInsert assembles one multi-row INSERT for a document's link
// targets using numbered placeholders ($1, $2, ...), Postgres's parameter
// style. ON CONFLICT DO NOTHING tolerates a target already present.
func pgBuildLinkInsert(id, workspace string, targets []string) (string, []any) {
	const columnsPerRow = 3 // workspace_id, doc_id, target_id

	tuples := make([]string, 0, len(targets))
	args := make([]any, 0, len(targets)*columnsPerRow)

	for i, target := range targets {
		base := i * columnsPerRow
		tuples = append(tuples, "("+placeholderRun(base, columnsPerRow)+")")
		args = append(args, workspace, id, target)
	}

	query := "INSERT INTO document_links (workspace_id, doc_id, target_id) VALUES " +
		strings.Join(tuples, ", ") + " ON CONFLICT DO NOTHING"

	return query, args
}

// placeholderRun renders count comma-joined numbered placeholders starting
// after base, e.g. base 0 count 3 yields "$1, $2, $3". It keeps the link
// insert free of inline parameter arithmetic.
func placeholderRun(base, count int) string {
	parts := make([]string, count)
	for i := range count {
		parts[i] = fmt.Sprintf("$%d", base+i+1)
	}

	return strings.Join(parts, ", ")
}

// Get fetches one document by id within the request workspace and populates
// its derived Backlinks. A document in another workspace reads as not-found.
func (s *Postgres) Get(ctx context.Context, id string) (Document, error) {
	workspace := workspaceFromContext(ctx)

	row := s.pool.QueryRow(ctx,
		"SELECT "+pgDocColumns+" FROM documents WHERE id = $1 AND workspace_id = $2", id, workspace)

	doc, err := scanPGDocument(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, fmt.Errorf("%w: %s", ErrDocumentNotFound, id)
	}

	if err != nil {
		return Document{}, fmt.Errorf("get document %s: %w", id, err)
	}

	doc.Links, err = s.linkIDs(ctx,
		"SELECT target_id FROM document_links WHERE doc_id = $1 AND workspace_id = $2 ORDER BY target_id",
		id, workspace)
	if err != nil {
		return Document{}, err
	}

	doc.Backlinks, err = s.linkIDs(ctx,
		"SELECT doc_id FROM document_links WHERE target_id = $1 AND workspace_id = $2 ORDER BY doc_id",
		id, workspace)
	if err != nil {
		return Document{}, err
	}

	return doc, nil
}

// Delete removes one document, its outbound link rows, and every inbound link
// pointing at it in one transaction, so no dangling backlink survives.
func (s *Postgres) Delete(ctx context.Context, id string) error {
	workspace := workspaceFromContext(ctx)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin vault delete: %w", err)
	}
	defer rollbackPG(ctx, tx)

	tag, err := tx.Exec(ctx,
		"DELETE FROM documents WHERE id = $1 AND workspace_id = $2", id, workspace)
	if err != nil {
		return fmt.Errorf("delete document %s: %w", id, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrDocumentNotFound, id)
	}

	if _, err := tx.Exec(ctx,
		"DELETE FROM document_links WHERE workspace_id = $1 AND (doc_id = $2 OR target_id = $2)",
		workspace, id); err != nil {
		return fmt.Errorf("delete links for %s: %w", id, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit vault delete %s: %w", id, err)
	}

	return nil
}

// List returns the workspace's documents newest first, optionally capped.
func (s *Postgres) List(ctx context.Context, limit int) ([]Document, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+pgDocColumns+` FROM documents
		 WHERE workspace_id = $1
		 ORDER BY created_at DESC, id DESC
		 LIMIT $2`,
		workspaceFromContext(ctx), limitOrNull(limit))
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	defer rows.Close()

	docs, err := collectPGDocuments(rows)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}

	return docs, nil
}

// linkIDs runs one link query and collects the single id column.
func (s *Postgres) linkIDs(ctx context.Context, query, id, workspace string) ([]string, error) {
	rows, err := s.pool.Query(ctx, query, id, workspace)
	if err != nil {
		return nil, fmt.Errorf("read links for %s: %w", id, err)
	}
	defer rows.Close()

	return collectPGIDs(rows)
}

// rollbackPG ends a transaction that was not committed, ignoring the
// already-committed error so the deferred call is a clean no-op on the happy
// path. Any other failure is logged: the deferring caller has already chosen
// its return value.
func rollbackPG(ctx context.Context, tx pgx.Tx) {
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		slog.WarnContext(ctx, "roll back vault transaction", "error", err)
	}
}

// limitOrNull maps "no limit requested" onto a SQL NULL, which Postgres treats
// as "no limit" in LIMIT.
func limitOrNull(limit int) any {
	if limit <= 0 {
		return nil
	}

	return limit
}

// marshalTagsJSON renders tags for the tags_json column, defaulting nil to the
// schema's empty array.
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

// scanPGDocument reads one pgDocColumns row back into a Document. It reuses the
// package's rowScanner (declared in sqlite.go) since pgx.Row and pgx.Rows
// satisfy the same one-method Scan interface as the database/sql types.
func scanPGDocument(row rowScanner) (Document, error) {
	var (
		doc       Document
		tags      []byte
		createdAt time.Time
		updatedAt time.Time
	)

	err := row.Scan(&doc.ID, &doc.Title, &doc.Body, &tags, &createdAt, &updatedAt)
	if err != nil {
		return Document{}, err //nolint:wrapcheck // callers wrap with their operation and id
	}

	if err := json.Unmarshal(tags, &doc.Tags); err != nil {
		return Document{}, fmt.Errorf("unmarshal tags: %w", err)
	}

	doc.CreatedAt = createdAt
	doc.UpdatedAt = updatedAt

	return doc, nil
}

// collectPGDocuments drains rows through scanPGDocument, surfacing iteration
// errors.
func collectPGDocuments(rows pgx.Rows) ([]Document, error) {
	var docs []Document

	for rows.Next() {
		doc, err := scanPGDocument(rows)
		if err != nil {
			return nil, err
		}

		docs = append(docs, doc)
	}

	if err := rows.Err(); err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with their operation
	}

	return docs, nil
}

// collectPGIDs drains a single-column id query.
func collectPGIDs(rows pgx.Rows) ([]string, error) {
	var ids []string

	for rows.Next() {
		var id string

		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan id: %w", err)
		}

		ids = append(ids, id)
	}

	if err := rows.Err(); err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with their operation
	}

	return ids, nil
}
