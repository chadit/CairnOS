package tenancy

import (
	"slices"

	"github.com/chadit/CairnOS/internal/auth"
)

// Roles map a workspace membership onto a scope set the auth layer enforces.
// The mapping is a decision, not a given table: three roles cover the
// governance surface.
const (
	// RoleAdmin holds every scope, including admin, so an admin manages keys,
	// pauses workflows, and reads audit history in their workspace.
	RoleAdmin = "admin"

	// RoleEditor reads and writes workflows, memory, and runs, the working set
	// a human building and running workflows needs, but holds no admin scope,
	// so editors cannot pause workflows or manage keys.
	RoleEditor = "editor"

	// RoleViewer holds only the read scopes, so a viewer inspects runs,
	// workflows, and memory without changing anything.
	RoleViewer = "viewer"
)

// roleScopes is the role-to-scope-set mapping. Admin carries every scope so
// HasScope's admin shortcut covers anything not listed explicitly; editor and
// viewer carry exactly the scopes their work needs.
//
//nolint:gochecknoglobals // an immutable lookup table read at login, never mutated
var roleScopes = map[string][]string{
	RoleAdmin: {
		auth.ScopeReadMemory, auth.ScopeWriteMemory,
		auth.ScopeReadWorkflow, auth.ScopeWriteWorkflow,
		auth.ScopeReadRun,
		auth.ScopeReadVault, auth.ScopeWriteVault,
		auth.ScopeA2ATask,
		auth.ScopeChatSession,
		auth.ScopeAdmin,
	},
	RoleEditor: {
		auth.ScopeReadMemory, auth.ScopeWriteMemory,
		auth.ScopeReadWorkflow, auth.ScopeWriteWorkflow,
		auth.ScopeReadRun,
		// An editor manages vault documents and language rules, the working set
		// alongside memory and workflows.
		auth.ScopeReadVault, auth.ScopeWriteVault,
		// An editor holds chat:session and write:workflow, so an editor can hold
		// a conversation and confirm a workflow trigger from it.
		auth.ScopeChatSession,
	},
	RoleViewer: {
		auth.ScopeReadMemory,
		auth.ScopeReadWorkflow,
		auth.ScopeReadRun,
		// A viewer reads vault documents and rules without changing them.
		auth.ScopeReadVault,
		// A viewer holds chat:session to talk to CairnOS, but without
		// write:workflow a viewer's confirmed trigger is refused at confirm time.
		auth.ScopeChatSession,
	},
}

// ValidRole reports whether name is one of the defined roles, so a typo'd
// role fails at membership creation instead of granting an empty scope set
// that silently never matches.
func ValidRole(name string) bool {
	_, ok := roleScopes[name]

	return ok
}

// ScopesForRole returns the scope set one role grants, reporting false for an
// unknown role. The returned slice is a copy, so a caller cannot mutate the
// shared table.
func ScopesForRole(role string) ([]string, bool) {
	scopes, ok := roleScopes[role]
	if !ok {
		return nil, false
	}

	return slices.Clone(scopes), true
}
