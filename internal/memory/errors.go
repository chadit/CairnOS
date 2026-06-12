package memory

import "errors"

// Sentinel errors for the memory package.
var (
	// ErrEntryNotFound reports a Get, Replace, or Delete against an id that
	// has no entry, either never added or already deleted.
	ErrEntryNotFound = errors.New("memory entry not found")
)
