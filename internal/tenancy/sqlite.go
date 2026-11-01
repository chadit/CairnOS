package tenancy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/chadit/CairnOS/internal/store"
)

// tenancySchema holds the workspaces and their members, plus IF NOT EXISTS so
// opening an existing database is idempotent. It shares the run store's
// database file with disjoint tables, the same arrangement internal/auth and
// internal/memory use. The unique name index enforces the spec's
// unique-workspace-name invariant.
const tenancySchema = `
CREATE TABLE IF NOT EXISTS workspaces (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS workspace_members (
    workspace_id TEXT NOT NULL,
    subject      TEXT NOT NULL,
    role         TEXT NOT NULL,
    created_at   TIMESTAMP NOT NULL,
    PRIMARY KEY (workspace_id, subject)
);

CREATE INDEX IF NOT EXISTS idx_members_workspace ON workspace_members(workspace_id, created_at);
`

// defaultWorkspaceName names the auto-seeded default workspace. It carries the
// same id as its name so a single-tenant deployment has a stable, readable
// workspace id, and so the store-level default ("default") and this row line
// up: a run stamped with store.DefaultWorkspace points at this row.
const defaultWorkspaceName = store.DefaultWorkspace

// tenancyTimeLayout is RFC 3339 UTC with fixed-width nanoseconds, matching the
// run, memory, and auth stores so lexicographic order tracks chronological
// order across every table in the shared file.
const tenancyTimeLayout = "2006-01-02T15:04:05.000000000Z"

// workspaceColumns is the SELECT list every workspace query shares, in
// scanWorkspace order.
const workspaceColumns = "id, name, created_at"

// memberColumns is the SELECT list every member query shares, in scanMember
// order.
const memberColumns = "workspace_id, subject, role, created_at"

// SQLite is the default tenancy Store backend. Safe for concurrent use
// through WAL mode and busy_timeout, so the CLI manages workspaces while the
// serve daemon holds the file open.
type SQLite struct {
	db *sql.DB
}

// OpenSQLite opens (creating if needed) the tenancy tables at path and seeds
// the default workspace so a fresh database has somewhere for the default
// path to land. Same pragma choices as the run store: WAL and busy_timeout.
func OpenSQLite(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open tenancy database %q: %w", path, err)
	}

	// Schema creation is process-lifetime initialization with no request to
	// inherit a context from; OpenSQLite carries no ctx on purpose.
	setupCtx := context.Background()

	if _, execErr := db.ExecContext(setupCtx, tenancySchema); execErr != nil {
		return nil, fmt.Errorf("create tenancy schema: %w", errors.Join(execErr, db.Close()))
	}

	tenancy := &SQLite{db: db}
	if seedErr := tenancy.seedDefault(setupCtx); seedErr != nil {
		return nil, fmt.Errorf("seed default workspace: %w", errors.Join(seedErr, db.Close()))
	}

	return tenancy, nil
}

// Close releases the underlying database.
func (s *SQLite) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close tenancy database: %w", err)
	}

	return nil
}

// CreateWorkspace persists a new workspace, stamping a ULID id and the current
// time when the caller leaves them zero. A duplicate name fails on the unique
// index, keeping the unique-name invariant.
func (s *SQLite) CreateWorkspace(ctx context.Context, workspace *Workspace) error {
	id := workspace.ID
	if id == "" {
		id = ulid.Make().String()
	}

	createdAt := workspace.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO workspaces (id, name, created_at) VALUES (?, ?, ?)`,
		id, workspace.Name, tenancyStoredTime(createdAt))
	if err != nil {
		return fmt.Errorf("insert workspace %s: %w", workspace.Name, err)
	}

	workspace.ID = id
	workspace.CreatedAt = createdAt

	return nil
}

// GetWorkspace fetches one workspace by id.
func (s *SQLite) GetWorkspace(ctx context.Context, id string) (*Workspace, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+workspaceColumns+" FROM workspaces WHERE id = ?", id)

	workspace, err := scanWorkspace(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrWorkspaceNotFound, id)
	}

	if err != nil {
		return nil, fmt.Errorf("get workspace %s: %w", id, err)
	}

	return workspace, nil
}

// ListWorkspaces returns every workspace, oldest first; created_at is stored
// in a lexicographically ordered layout, so the walk is chronological.
func (s *SQLite) ListWorkspaces(ctx context.Context) ([]*Workspace, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+workspaceColumns+" FROM workspaces ORDER BY created_at ASC, id ASC")
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	defer closeRows(rows)

	var workspaces []*Workspace

	for rows.Next() {
		workspace, scanErr := scanWorkspace(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("list workspaces: %w", scanErr)
		}

		workspaces = append(workspaces, workspace)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}

	return workspaces, nil
}

// AddMember records one subject's role in a workspace. The role is validated
// so a typo never grants an empty scope set. Re-adding the same subject
// overwrites the role, so a role change is one idempotent call. The workspace
// must exist; a membership in a missing workspace would be unreachable.
func (s *SQLite) AddMember(ctx context.Context, workspaceID, subject, role string) error {
	if !ValidRole(role) {
		return fmt.Errorf("%w: %q", ErrRoleUnknown, role)
	}

	if _, err := s.GetWorkspace(ctx, workspaceID); err != nil {
		return err
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO workspace_members (workspace_id, subject, role, created_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (workspace_id, subject) DO UPDATE SET role = excluded.role`,
		workspaceID, subject, role, tenancyStoredTime(time.Now()))
	if err != nil {
		return fmt.Errorf("add member %s to %s: %w", subject, workspaceID, err)
	}

	return nil
}

// GetMemberRole returns one subject's role in a workspace.
func (s *SQLite) GetMemberRole(ctx context.Context, workspaceID, subject string) (string, error) {
	var role string

	err := s.db.QueryRowContext(ctx,
		"SELECT role FROM workspace_members WHERE workspace_id = ? AND subject = ?",
		workspaceID, subject).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s in %s", ErrMemberNotFound, subject, workspaceID)
	}

	if err != nil {
		return "", fmt.Errorf("get member role for %s in %s: %w", subject, workspaceID, err)
	}

	return role, nil
}

// ListMembers returns every member of a workspace, oldest membership first.
func (s *SQLite) ListMembers(ctx context.Context, workspaceID string) ([]*Member, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+memberColumns+` FROM workspace_members
		 WHERE workspace_id = ?
		 ORDER BY created_at ASC, subject ASC`,
		workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list members of %s: %w", workspaceID, err)
	}
	defer closeRows(rows)

	var members []*Member

	for rows.Next() {
		member, scanErr := scanMember(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("list members of %s: %w", workspaceID, scanErr)
		}

		members = append(members, member)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list members of %s: %w", workspaceID, err)
	}

	return members, nil
}

// seedDefault inserts the default workspace if it is absent. INSERT OR IGNORE
// keeps the seed idempotent, so every Open is safe and concurrent Opens across
// nodes race harmlessly on the primary key.
func (s *SQLite) seedDefault(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO workspaces (id, name, created_at) VALUES (?, ?, ?)`,
		defaultWorkspaceName, defaultWorkspaceName, tenancyStoredTime(time.Now()))
	if err != nil {
		return fmt.Errorf("insert default workspace: %w", err)
	}

	return nil
}

// tenancyStoredTime renders a timestamp in the shared fixed-width layout.
func tenancyStoredTime(value time.Time) string {
	return value.UTC().Format(tenancyTimeLayout)
}

// scanner covers sql.Row and sql.Rows for the shared scan helpers.
type scanner interface {
	Scan(dest ...any) error
}

// scanWorkspace reads one workspaceColumns row back into a Workspace.
func scanWorkspace(row scanner) (*Workspace, error) {
	var (
		workspace Workspace
		createdAt sql.NullTime
	)

	err := row.Scan(&workspace.ID, &workspace.Name, &createdAt)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with the query's identity, including the sql.ErrNoRows translation
	}

	workspace.CreatedAt = createdAt.Time

	return &workspace, nil
}

// scanMember reads one memberColumns row back into a Member.
func scanMember(row scanner) (*Member, error) {
	var (
		member    Member
		createdAt sql.NullTime
	)

	err := row.Scan(&member.WorkspaceID, &member.Subject, &member.Role, &createdAt)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with the query's identity
	}

	member.CreatedAt = createdAt.Time

	return &member, nil
}

// closeRows closes rows where the read path already reports errors via
// rows.Err, making a second error from Close redundant.
func closeRows(rows *sql.Rows) {
	_ = rows.Close()
}
