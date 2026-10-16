package auth

import (
	"fmt"
	"slices"
	"strings"
)

// Scopes from the spec's table (.agents/spec/08-auth-and-access.md,
// "Scopes"). Phase 4a wires the first six; the vault pair and a2a:task are
// accepted on keys now so Phase 4b/7 keys need no migration, and admin
// implies all of them.
const (
	ScopeReadMemory    = "read:memory"
	ScopeWriteMemory   = "write:memory"
	ScopeReadWorkflow  = "read:workflow"
	ScopeWriteWorkflow = "write:workflow"
	ScopeReadRun       = "read:run"
	ScopeReadVault     = "read:vault"
	ScopeWriteVault    = "write:vault"
	ScopeA2ATask       = "a2a:task"
	// ScopeChatSession gates the Phase 9 chat surface: creating sessions,
	// sending messages, and reading conversations. Triggering a workflow from
	// chat also needs ScopeWriteWorkflow, checked at confirm time
	// (.agents/spec/13-roadmap.md, Phase 9; .agents/spec/14-open-decisions.md,
	// D9).
	ScopeChatSession = "chat:session"
	// ScopeMessageSend gates the outbound send surface: posting a message as a
	// configured identity and listing the identities a caller may send as.
	// Sending is separable from chat because a workflow or an operator script
	// posts without ever holding a conversation, and a key scoped to send
	// should not also read transcripts (FR30).
	ScopeMessageSend = "message:send"
	ScopeAdmin       = "admin"
)

// ValidScope reports whether name is in the spec's scope table.
func ValidScope(name string) bool {
	switch name {
	case ScopeReadMemory, ScopeWriteMemory,
		ScopeReadWorkflow, ScopeWriteWorkflow,
		ScopeReadRun,
		ScopeReadVault, ScopeWriteVault,
		ScopeA2ATask,
		ScopeChatSession,
		ScopeMessageSend,
		ScopeAdmin:
		return true
	default:
		return false
	}
}

// HasScope reports whether the granted set covers the required scope. The
// admin scope grants everything per the spec's table ("all of the above").
func HasScope(granted []string, required string) bool {
	return slices.Contains(granted, ScopeAdmin) || slices.Contains(granted, required)
}

// ParseScopes splits a comma-separated scope list and validates every
// entry against the spec table, so a typo'd scope fails at key creation
// instead of silently never matching.
func ParseScopes(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")

	scopes := make([]string, 0, len(parts))

	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}

		if !ValidScope(trimmed) {
			return nil, fmt.Errorf("%w: %q", ErrScopeUnknown, trimmed)
		}

		if !slices.Contains(scopes, trimmed) {
			scopes = append(scopes, trimmed)
		}
	}

	return scopes, nil
}
