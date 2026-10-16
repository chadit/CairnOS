package vault

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// vaultSchema mirrors the run store's sqlite conventions: a workspace_id
// column for multi-tenancy and external-content FTS5 kept in sync by triggers.
// Links live in their own table so backlink derivation is a reverse lookup
// over an indexed target_id rather than a JSON scan.
//
// The sqlite-vec path stays a stub (D2): a semantic Query falls back to
// keyword and flags it.
const vaultSchema = `
CREATE TABLE IF NOT EXISTS documents (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL DEFAULT 'default',
    title        TEXT NOT NULL,
    body         TEXT NOT NULL,
    tags_json    TEXT NOT NULL DEFAULT '[]',
    created_at   TIMESTAMP NOT NULL,
    updated_at   TIMESTAMP NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_documents_workspace ON documents(workspace_id, created_at DESC);

CREATE TABLE IF NOT EXISTS document_links (
    workspace_id TEXT NOT NULL DEFAULT 'default',
    doc_id       TEXT NOT NULL,
    target_id    TEXT NOT NULL,
    PRIMARY KEY (workspace_id, doc_id, target_id)
);

CREATE INDEX IF NOT EXISTS idx_document_links_target ON document_links(workspace_id, target_id);

CREATE VIRTUAL TABLE IF NOT EXISTS documents_fts USING fts5(
    title, body, tags, content='documents', content_rowid='rowid'
);

CREATE TRIGGER IF NOT EXISTS documents_fts_insert AFTER INSERT ON documents BEGIN
    INSERT INTO documents_fts(rowid, title, body, tags) VALUES (new.rowid, new.title, new.body, new.tags_json);
END;

CREATE TRIGGER IF NOT EXISTS documents_fts_delete AFTER DELETE ON documents BEGIN
    INSERT INTO documents_fts(documents_fts, rowid, title, body, tags) VALUES ('delete', old.rowid, old.title, old.body, old.tags_json);
END;

CREATE TRIGGER IF NOT EXISTS documents_fts_update AFTER UPDATE ON documents BEGIN
    INSERT INTO documents_fts(documents_fts, rowid, title, body, tags) VALUES ('delete', old.rowid, old.title, old.body, old.tags_json);
    INSERT INTO documents_fts(rowid, title, body, tags) VALUES (new.rowid, new.title, new.body, new.tags_json);
END;
`

// docColumns is the SELECT list every document query shares, in scanDocument
// order.
const docColumns = "id, title, body, tags_json, created_at, updated_at"

// SQLite is the default Store backend: FTS5 keyword search over title and
// body, a links table for backlink derivation, zero external services, no
// vectors (.agents/spec/05-memory-and-knowledge.md). Safe for concurrent use
// through WAL mode and busy_timeout.
type SQLite struct {
	db *sql.DB
}

// OpenSQLite opens (creating if needed) the vault database at path and applies
// the schema. The same pragma choices as the memory and run stores: WAL so
// readers do not block the writer, busy_timeout so concurrent writers wait
// instead of failing with SQLITE_BUSY.
func OpenSQLite(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open vault database %q: %w", path, err)
	}

	// Schema creation is process-lifetime initialization with no request to
	// inherit a context from; OpenSQLite carries no ctx on purpose.
	setupCtx := context.Background()

	if _, execErr := db.ExecContext(setupCtx, vaultSchema); execErr != nil {
		return nil, fmt.Errorf("create vault schema: %w", errors.Join(execErr, db.Close()))
	}

	return &SQLite{db: db}, nil
}

// Close releases the underlying database.
func (s *SQLite) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close vault database: %w", err)
	}

	return nil
}

// Put persists a new document or overwrites an existing one, returning the id.
// It runs in a transaction so the document row and its link rows commit
// together: a half-written link set would corrupt backlink derivation. An
// empty ID generates a fresh ULID; zero timestamps default to now, with
// CreatedAt preserved across an overwrite of an existing id.
func (s *SQLite) Put(ctx context.Context, doc *Document) (string, error) {
	id := doc.ID
	if id == "" {
		id = ulid.Make().String()
	}

	workspace := workspaceFromContext(ctx)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin vault put: %w", err)
	}
	defer rollback(tx)

	createdAt, err := putCreatedAt(ctx, tx, id, workspace, doc.CreatedAt)
	if err != nil {
		return "", err
	}

	if err := upsertDocument(ctx, tx, id, workspace, doc, createdAt); err != nil {
		return "", err
	}

	if err := rewriteLinks(ctx, tx, id, workspace, doc.Links); err != nil {
		return "", err
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit vault put %s: %w", id, err)
	}

	return id, nil
}

// putCreatedAt picks the created_at to store: the document's own value when
// set, the existing row's value when overwriting (so an edit does not reset
// age), and now for a fresh document.
func putCreatedAt(ctx context.Context, tx *sql.Tx, id, workspace string, declared time.Time) (time.Time, error) {
	if !declared.IsZero() {
		return declared, nil
	}

	var existing sql.NullTime

	row := tx.QueryRowContext(ctx,
		"SELECT created_at FROM documents WHERE id = ? AND workspace_id = ?", id, workspace)

	switch err := row.Scan(&existing); {
	case errors.Is(err, sql.ErrNoRows):
		return time.Now(), nil
	case err != nil:
		return time.Time{}, fmt.Errorf("read existing created_at for %s: %w", id, err)
	default:
		return existing.Time, nil
	}
}

// upsertDocument writes the document row, replacing an existing id in place so
// Put doubles as an update. updated_at is always now.
func upsertDocument(ctx context.Context, tx *sql.Tx, id, workspace string, doc *Document, createdAt time.Time) error {
	tags, err := marshalTags(doc.Tags)
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO documents (id, workspace_id, title, body, tags_json, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		     title = excluded.title, body = excluded.body, tags_json = excluded.tags_json,
		     updated_at = excluded.updated_at`,
		id, workspace, doc.Title, doc.Body, tags, storedTime(createdAt), storedTime(time.Now()))
	if err != nil {
		return fmt.Errorf("upsert document %s: %w", id, err)
	}

	return nil
}

// rewriteLinks replaces a document's outbound link rows wholesale: delete the
// old set, insert the new in one batch statement. Doing it inside Put's
// transaction keeps the links table consistent with the document's Links
// field, which is what backlink derivation reads in reverse. The single
// multi-row INSERT avoids a per-link round trip (the N+1 the linter flags).
func rewriteLinks(ctx context.Context, tx *sql.Tx, id, workspace string, links []string) error {
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM document_links WHERE doc_id = ? AND workspace_id = ?", id, workspace); err != nil {
		return fmt.Errorf("clear links for %s: %w", id, err)
	}

	targets := dedupeLinks(links, id)
	if len(targets) == 0 {
		return nil
	}

	query, args := buildLinkInsert(id, workspace, targets)

	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("insert links for %s: %w", id, err)
	}

	return nil
}

// buildLinkInsert assembles one multi-row INSERT for a document's link
// targets: a (?, ?, ?) tuple per target, flattened into the args slice in
// matching order. ON CONFLICT DO NOTHING tolerates a target already present
// from a partial earlier write.
func buildLinkInsert(id, workspace string, targets []string) (string, []any) {
	const columnsPerRow = 3 // workspace_id, doc_id, target_id

	placeholders := make([]string, 0, len(targets))
	args := make([]any, 0, len(targets)*columnsPerRow)

	for _, target := range targets {
		placeholders = append(placeholders, "(?, ?, ?)")
		args = append(args, workspace, id, target)
	}

	query := "INSERT INTO document_links (workspace_id, doc_id, target_id) VALUES " +
		strings.Join(placeholders, ", ") + " ON CONFLICT DO NOTHING"

	return query, args
}

// Get fetches one document by id within the request workspace and populates
// its derived Backlinks. A document in another workspace reads as not-found.
func (s *SQLite) Get(ctx context.Context, id string) (Document, error) {
	workspace := workspaceFromContext(ctx)

	row := s.db.QueryRowContext(ctx,
		"SELECT "+docColumns+" FROM documents WHERE id = ? AND workspace_id = ?", id, workspace)

	doc, err := scanDocument(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, fmt.Errorf("%w: %s", ErrDocumentNotFound, id)
	}

	if err != nil {
		return Document{}, fmt.Errorf("get document %s: %w", id, err)
	}

	doc.Links, err = s.outboundLinks(ctx, id, workspace)
	if err != nil {
		return Document{}, err
	}

	doc.Backlinks, err = s.inboundLinks(ctx, id, workspace)
	if err != nil {
		return Document{}, err
	}

	return doc, nil
}

// Delete removes one document, its outbound link rows, and every inbound link
// pointing at it, all in one transaction so no dangling backlink survives. A
// document in another workspace is invisible, so the delete reports not-found
// rather than crossing tenants.
func (s *SQLite) Delete(ctx context.Context, id string) error {
	workspace := workspaceFromContext(ctx)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin vault delete: %w", err)
	}
	defer rollback(tx)

	result, err := tx.ExecContext(ctx,
		"DELETE FROM documents WHERE id = ? AND workspace_id = ?", id, workspace)
	if err != nil {
		return fmt.Errorf("delete document %s: %w", id, err)
	}

	if err := requireRow(result, id); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		"DELETE FROM document_links WHERE workspace_id = ? AND (doc_id = ? OR target_id = ?)",
		workspace, id, id); err != nil {
		return fmt.Errorf("delete links for %s: %w", id, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit vault delete %s: %w", id, err)
	}

	return nil
}

// List returns the workspace's documents newest first, optionally capped.
// Bodies are included; backlinks are not, since a list view does not need the
// per-document inbound edges a Get carries.
func (s *SQLite) List(ctx context.Context, limit int) ([]Document, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+docColumns+` FROM documents
		 WHERE workspace_id = ?
		 ORDER BY created_at DESC, id DESC
		 LIMIT ?`,
		workspaceFromContext(ctx), limitOrAll(limit))
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	defer closeRows(rows)

	docs, err := collectDocuments(rows)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}

	return docs, nil
}

// outboundLinks reads a document's outbound link targets in stable order.
func (s *SQLite) outboundLinks(ctx context.Context, id, workspace string) ([]string, error) {
	return s.linkIDs(ctx,
		"SELECT target_id FROM document_links WHERE doc_id = ? AND workspace_id = ? ORDER BY target_id",
		id, workspace)
}

// inboundLinks reads the documents that link to id, the derived backlink set.
func (s *SQLite) inboundLinks(ctx context.Context, id, workspace string) ([]string, error) {
	return s.linkIDs(ctx,
		"SELECT doc_id FROM document_links WHERE target_id = ? AND workspace_id = ? ORDER BY doc_id",
		id, workspace)
}

// linkIDs runs one link query and collects the single id column.
func (s *SQLite) linkIDs(ctx context.Context, query, id, workspace string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, id, workspace)
	if err != nil {
		return nil, fmt.Errorf("read links for %s: %w", id, err)
	}
	defer closeRows(rows)

	return collectIDs(rows)
}

// requireRow turns a zero-row write into ErrDocumentNotFound so callers can
// tell a miss from a no-op.
func requireRow(result sql.Result, id string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("document %s: rows affected: %w", id, err)
	}

	if affected == 0 {
		return fmt.Errorf("%w: %s", ErrDocumentNotFound, id)
	}

	return nil
}

// rollback ends a transaction that was not committed, ignoring the
// already-committed error so the deferred call is a clean no-op on the happy
// path. Any other failure is logged: the deferring caller has already chosen
// its return value.
func rollback(tx *sql.Tx) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		slog.Warn("roll back vault transaction", "error", err)
	}
}

// dedupeLinks removes duplicate and self-referential link targets, so a
// document cannot link to itself and a repeated target inserts once. Sorting
// keeps the stored order stable for reproducible reads.
func dedupeLinks(links []string, self string) []string {
	seen := make(map[string]struct{}, len(links))
	out := make([]string, 0, len(links))

	for _, target := range links {
		trimmed := strings.TrimSpace(target)
		if trimmed == "" || trimmed == self {
			continue
		}

		if _, dup := seen[trimmed]; dup {
			continue
		}

		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}

	slices.Sort(out)

	return out
}

// limitOrAll maps "no limit requested" onto SQLite's LIMIT -1 (unlimited).
func limitOrAll(limit int) int {
	if limit <= 0 {
		return -1
	}

	return limit
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

// storedTime renders a timestamp in fixed-width RFC 3339 UTC. The zero padding
// keeps lexicographic order matching chronological order, which the created_at
// ordering relies on.
func storedTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

// rowScanner abstracts *sql.Row and *sql.Rows so scanDocument serves both Get
// and the listing queries.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanDocument reads one docColumns row back into a Document, leaving Links and
// Backlinks for the caller to populate from the links table.
func scanDocument(row rowScanner) (Document, error) {
	var (
		doc       Document
		tags      string
		createdAt sql.NullTime
		updatedAt sql.NullTime
	)

	err := row.Scan(&doc.ID, &doc.Title, &doc.Body, &tags, &createdAt, &updatedAt)
	if err != nil {
		return Document{}, err //nolint:wrapcheck // callers wrap with their operation and id
	}

	if err := json.Unmarshal([]byte(tags), &doc.Tags); err != nil {
		return Document{}, fmt.Errorf("unmarshal tags: %w", err)
	}

	doc.CreatedAt = createdAt.Time
	doc.UpdatedAt = updatedAt.Time

	return doc, nil
}

// collectDocuments drains rows through scanDocument, surfacing iteration
// errors.
func collectDocuments(rows *sql.Rows) ([]Document, error) {
	var docs []Document

	for rows.Next() {
		doc, err := scanDocument(rows)
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

// collectIDs drains a single-column id query.
func collectIDs(rows *sql.Rows) ([]string, error) {
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

// closeRows closes rows where the read path already reports errors via
// rows.Err, making a second error from Close redundant.
func closeRows(rows *sql.Rows) {
	_ = rows.Close()
}
