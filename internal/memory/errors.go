package memory

import "errors"

// Sentinel errors for the memory package.
var (
	// ErrEntryNotFound reports a Get, Replace, or Delete against an id that
	// has no entry, either never added or already deleted.
	ErrEntryNotFound = errors.New("memory entry not found")

	// ErrBadFrontMatter reports a memory markdown file whose front matter
	// block is missing, malformed, or carries unknown or duplicate keys.
	ErrBadFrontMatter = errors.New("memory markdown front matter is malformed")

	// ErrBadEntryID reports an entry id that is not a canonical ULID. The id
	// is hand-editable in a markdown file and later composes an export
	// filename, so a non-canonical id is refused before it can traverse out
	// of the export directory.
	ErrBadEntryID = errors.New("memory entry id is not a valid ULID")
)
