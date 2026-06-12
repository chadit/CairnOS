package vault

import "errors"

// Sentinel errors for the vault package.
var (
	// ErrDocumentNotFound reports a Get or Delete against an id that has no
	// document, either never put or already deleted.
	ErrDocumentNotFound = errors.New("vault document not found")
)
