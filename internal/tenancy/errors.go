package tenancy

import "errors"

// Sentinel errors for the tenancy package, on the interface so callers
// classify failures without importing a backend.
var (
	// ErrWorkspaceNotFound reports an operation against a workspace id with no
	// stored row.
	ErrWorkspaceNotFound = errors.New("workspace not found")

	// ErrMemberNotFound reports a membership lookup for a subject that is not a
	// member of the workspace.
	ErrMemberNotFound = errors.New("workspace member not found")

	// ErrRoleUnknown reports a role name outside the defined role set.
	ErrRoleUnknown = errors.New("unknown role")
)
