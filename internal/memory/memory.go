// Package memory holds the bounded, curated notes that get loaded into
// prompts at session start (.agents/spec/05-memory-and-knowledge.md,
// "Memory"). Entries are small and high-value; Load pulls the most important
// ones for a scope under a token budget so the system prompt stays small
// without the runner managing memory itself.
package memory

import (
	"context"
	"time"
)

// Store is the memory contract every backend satisfies; swapping backends is
// a config choice, never a workflow or runner change. Two deliberate
// deviations from the spec sketch: Add and Replace take *Entry because the
// struct is large enough that house style passes it by pointer, and Search
// returns a SearchResult so the semantic fallback is visible to callers (the
// spec's "sets a flag in the result so callers know").
type Store interface {
	// Add persists a new entry and returns its id, generating a ULID when
	// the entry does not bring one.
	Add(ctx context.Context, entry *Entry) (string, error)

	// Get fetches one entry by id.
	Get(ctx context.Context, id string) (Entry, error)

	// Replace overwrites the entry's content, scope, importance, tags, and
	// token cost, refreshing UpdatedAt while CreatedAt stays put.
	Replace(ctx context.Context, id string, entry *Entry) error

	// Delete removes one entry by id.
	Delete(ctx context.Context, id string) error

	// Search returns entries relevant to the query: keyword (FTS5) by
	// default, semantic when a vector backend is configured and the query
	// is embedded.
	Search(ctx context.Context, query Query) (SearchResult, error)

	// Load returns the entries to inject into a prompt for a given scope,
	// ordered by importance, capped by a token budget.
	Load(ctx context.Context, scope string, tokenBudget int) ([]Entry, error)
}

// Entry is one curated note.
type Entry struct {
	ID         string
	Scope      string // e.g. "global", "project:finance-app", an agent name
	Content    string
	Importance int // 0..100; drives ordering and eviction
	Tags       []string
	TokenCost  int // measured token length of Content
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Query narrows a Search.
type Query struct {
	Scope    string
	Text     string
	Tags     []string
	Limit    int
	Semantic bool // use vectors if available
}

// SearchResult carries search hits plus the fallback signal the spec calls
// for: when Semantic was requested but no vector backend is configured,
// keyword search answers and Fallback is true so callers know what they got.
type SearchResult struct {
	Entries  []Entry
	Fallback bool
}
