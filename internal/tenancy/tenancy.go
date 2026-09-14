// Package tenancy holds the workspace model that isolates users, workflows,
// runs, memory, and budgets (.agents/spec/12-ui-and-governance.md,
// "Multi-tenancy"). A workspace is the tenancy boundary every stored row
// carries; users belong to workspaces with roles, and a role maps to a scope
// set the auth layer enforces. The store filtering itself lives on the
// request context (internal/store.WorkspaceFromContext); this package owns the
// workspace and membership records and the role-to-scope mapping.
package tenancy

import (
	"context"
	"time"
)

// Store is the workspace and membership persistence
// (.agents/spec/12-ui-and-governance.md, "Multi-tenancy"). It is intentionally
// narrow so the OAuth handler, the CLI, and the login path depend on the
// surface they use rather than a broad aggregate. SQLite implements it sharing
// the run store's database file with disjoint tables; Postgres does the same
// in one database.
type Store interface {
	// CreateWorkspace persists a new workspace. A zero ID gets a fresh ULID
	// and a zero CreatedAt the current time. A duplicate name is an error so
	// the unique-name invariant holds.
	CreateWorkspace(ctx context.Context, workspace *Workspace) error

	// GetWorkspace fetches one workspace by id, reporting ErrWorkspaceNotFound
	// when none exists.
	GetWorkspace(ctx context.Context, id string) (*Workspace, error)

	// ListWorkspaces returns every workspace, oldest first.
	ListWorkspaces(ctx context.Context) ([]*Workspace, error)

	// AddMember records (or updates) one user's role in a workspace. Re-adding
	// the same subject overwrites the role rather than failing, so a role
	// change is one call.
	AddMember(ctx context.Context, workspaceID, subject, role string) error

	// GetMemberRole returns one subject's role in a workspace, reporting
	// ErrMemberNotFound when the subject is not a member.
	GetMemberRole(ctx context.Context, workspaceID, subject string) (string, error)

	// ListMembers returns every member of a workspace, oldest membership
	// first.
	ListMembers(ctx context.Context, workspaceID string) ([]*Member, error)
}

// Workspace is one tenancy boundary: an id, a unique name, and a creation
// instant (.agents/spec/12-ui-and-governance.md, "Multi-tenancy").
type Workspace struct {
	CreatedAt time.Time
	ID        string
	Name      string
}

// Member is one user's membership in a workspace, carrying the role that maps
// to a scope set.
type Member struct {
	CreatedAt   time.Time
	WorkspaceID string
	Subject     string
	Role        string
}
