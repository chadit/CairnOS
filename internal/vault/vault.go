// Package vault holds the Obsidian-style document store: documents with a
// title, markdown body, tags, and outbound links to other documents
// (.agents/spec/05-memory-and-knowledge.md, "Vault and Knowledge Graph").
// It is a sibling of internal/memory, a superset store: memory entries are
// small and prompt-bound, vault documents are full documents meant for query
// through the typed graph DSL, not automatic prompt injection. Backlinks are
// derived from the links table, so a document never stores its inbound edges.
package vault

import (
	"context"
	"time"
)

// Store is the vault contract every backend satisfies; swapping backends is a
// config choice, never a runner change, the same seam internal/memory.Store
// has. Two shapes carry over from that store: Put takes *Document because the
// struct is wide, and Query returns a QueryResult so the semantic fallback
// stays visible to callers.
type Store interface {
	DocumentStore
	Retriever
}

// DocumentStore writes, reads, and removes one document at a time by id.
type DocumentStore interface {
	// Put persists a new document or overwrites an existing one when the
	// document carries an id, returning the id. It rewrites the document's
	// outbound links so backlink derivation stays in sync.
	Put(ctx context.Context, doc *Document) (string, error)

	// Get fetches one document by id, with its derived Backlinks populated.
	Get(ctx context.Context, id string) (Document, error)

	// Delete removes one document and its outbound link rows. Inbound links
	// from other documents are dropped too, so no dangling backlink survives.
	Delete(ctx context.Context, id string) error
}

// Retriever reads documents in bulk, by recency or by the typed graph query,
// rather than by a known id.
type Retriever interface {
	// List returns documents in the workspace, newest first, optionally
	// capped. Bodies are included; backlinks are not, since a list view does
	// not need the per-document inbound edges a Get carries.
	List(ctx context.Context, limit int) ([]Document, error)

	// Query runs the typed graph query: tag and project filtering, bounded
	// link traversal, and keyword text match, returning documents with their
	// bodies (.agents/spec/14-open-decisions.md, S1: a typed DSL, not GraphQL).
	Query(ctx context.Context, query Query) (QueryResult, error)
}

// Document is one vault entry. Links holds outbound references to other
// document ids; Backlinks is the derived inbound set, populated on Get and
// never stored, so the link graph has one source of truth (the links table).
type Document struct {
	CreatedAt time.Time
	UpdatedAt time.Time
	ID        string
	Title     string
	Body      string // markdown
	Tags      []string
	Links     []string // outbound document ids this one references
	Backlinks []string // inbound document ids (derived, read-only)
}

// Query is the typed graph query DSL (.agents/spec/14-open-decisions.md, S1).
// Every field is optional and AND-combined: a query with Tags and LinkedFrom
// returns documents that both carry the tags and sit within Hops of the
// source. The zero Query lists the workspace.
type Query struct {
	// Project narrows to one project. Projects are a tag convention:
	// "project:finance-app" is a tag, so a project filter is sugar over a tag
	// filter with the projectTagPrefix prepended, which keeps the data model
	// one table instead of a separate project column.
	Project string

	// LinkedFrom is a document id whose link graph is traversed. With Hops it
	// returns the documents reachable from the source by following outbound
	// links, the "documents linked from this, two hops out" intent.
	LinkedFrom string

	// Text is a keyword match over title and body (FTS5 / to_tsvector).
	Text string

	// Tags narrows to documents carrying every listed tag.
	Tags []string

	// Hops bounds the link traversal depth from LinkedFrom. One hop is the
	// direct outbound links; two hops adds their links; and so on. Capped at
	// maxHops so a dense graph cannot run the traversal unbounded.
	Hops int

	// Limit caps the result count; zero means no cap.
	Limit int

	// Semantic asks for vector search when a backend is configured. No vector
	// backend ships in Phase 7 (.agents/spec/14-open-decisions.md, D2), so a
	// semantic query falls back to keyword and sets Fallback in the result.
	Semantic bool
}

// QueryResult carries the matched documents plus the fallback signal the spec
// calls for: when Semantic was requested but no vector backend is configured,
// keyword search answers and Fallback is true so callers know what they got,
// mirroring memory.SearchResult.
type QueryResult struct {
	Documents []Document
	Fallback  bool
}
