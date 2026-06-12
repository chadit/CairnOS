package store

import "github.com/oklog/ulid/v2"

// NewID returns a ULID: time-sortable so "list recent runs" is an index
// walk, and coordination-free so concurrent producers never collide. It is
// backend-agnostic, so wiring that does not know which backend is configured
// (the engine's id source) gets the same id shape from sqlite and postgres.
// Both backends still expose their own NewID for callers already holding a
// concrete store.
func NewID() string {
	return ulid.Make().String()
}
