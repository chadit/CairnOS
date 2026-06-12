package store

import "errors"

// Sentinel errors every Store implementation shares. They live on the
// interface package so callers like the API server can classify failures
// without importing a specific backend.
var (
	// ErrRunNotFound reports an operation against a run id that has no row,
	// either because it was never created or because retention pruned it.
	ErrRunNotFound = errors.New("run not found")

	// ErrSuggestionNotFound reports an operation against a suggestion id
	// that has no row, either never saved or already pruned.
	ErrSuggestionNotFound = errors.New("suggestion not found")

	// ErrWorkflowVersionNotFound reports a GetWorkflowVersion against a hash
	// that has no captured snapshot, so there is nothing to roll back to.
	ErrWorkflowVersionNotFound = errors.New("workflow version not found")

	// ErrChatSessionNotFound reports an operation against a chat session id
	// that has no row in the request workspace, either never created, pruned,
	// or owned by another tenant.
	ErrChatSessionNotFound = errors.New("chat session not found")
)
