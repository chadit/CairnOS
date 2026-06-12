package tenancy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"
)

// pgTenancySchema holds the workspaces and members
// (.agents/spec/12-ui-and-governance.md, "Multi-tenancy") with timestamptz and
// IF NOT EXISTS so opening an existing database is idempotent. It shares the
// run store's Postgres database with disjoint tables, the same arrangement the
// sqlite backends use within one file.
const pgTenancySchema = `
CREATE TABLE IF NOT EXISTS workspaces (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS workspace_members (
    workspace_id TEXT NOT NULL,
    subject      TEXT NOT NULL,
    role         TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (workspace_id, subject)
);

CREATE INDEX IF NOT EXISTS idx_members_workspace ON workspace_members(workspace_id, created_at);
`

// Postgres is the multi-node tenancy Store backend: the same workspace and
// membership surface the sqlite backend offers, over pgxpool. It shares the
// run store's database (different tables), so several nodes resolve membership
// against one tenancy state.
type Postgres struct {
	pool *pgxpool.Pool
}

// OpenPostgres connects to dsn, verifies the connection, applies the tenancy
// schema, and seeds the default workspace. The DSN carries credentials from
// CAIRNOS_STORE_DSN.
func OpenPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open tenancy postgres pool: %w", err)
	}

	if pingErr := pool.Ping(ctx); pingErr != nil {
		pool.Close()

		return nil, fmt.Errorf("ping tenancy postgres: %w", pingErr)
	}

	if _, execErr := pool.Exec(ctx, pgTenancySchema); execErr != nil {
		pool.Close()

		return nil, fmt.Errorf("create tenancy schema: %w", execErr)
	}

	tenancy := &Postgres{pool: pool}
	if seedErr := tenancy.seedDefault(ctx); seedErr != nil {
		pool.Close()

		return nil, fmt.Errorf("seed default workspace: %w", seedErr)
	}

	return tenancy, nil
}

// Close releases the connection pool.
func (s *Postgres) Close() error {
	s.pool.Close()

	return nil
}

// CreateWorkspace persists a new workspace, stamping a ULID id and the current
// time when the caller leaves them zero. A duplicate name fails on the unique
// constraint, keeping the unique-name invariant.
func (s *Postgres) CreateWorkspace(ctx context.Context, workspace *Workspace) error {
	id := workspace.ID
	if id == "" {
		id = ulid.Make().String()
	}

	createdAt := workspace.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	_, err := s.pool.Exec(ctx,
		`INSERT INTO workspaces (id, name, created_at) VALUES ($1, $2, $3)`,
		id, workspace.Name, createdAt.UTC())
	if err != nil {
		return fmt.Errorf("insert workspace %s: %w", workspace.Name, err)
	}

	workspace.ID = id
	workspace.CreatedAt = createdAt

	return nil
}

// GetWorkspace fetches one workspace by id.
func (s *Postgres) GetWorkspace(ctx context.Context, id string) (*Workspace, error) {
	row := s.pool.QueryRow(ctx,
		"SELECT "+workspaceColumns+" FROM workspaces WHERE id = $1", id)

	workspace, err := scanPGWorkspace(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrWorkspaceNotFound, id)
	}

	if err != nil {
		return nil, fmt.Errorf("get workspace %s: %w", id, err)
	}

	return workspace, nil
}

// ListWorkspaces returns every workspace, oldest first; created_at is a
// timestamptz, so the order is chronological.
func (s *Postgres) ListWorkspaces(ctx context.Context) ([]*Workspace, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+workspaceColumns+" FROM workspaces ORDER BY created_at ASC, id ASC")
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	defer rows.Close()

	var workspaces []*Workspace

	for rows.Next() {
		workspace, scanErr := scanPGWorkspace(rows)
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

// AddMember records one subject's role in a workspace. The role is validated,
// the workspace must exist, and re-adding a subject overwrites the role so a
// role change is one idempotent call.
func (s *Postgres) AddMember(ctx context.Context, workspaceID, subject, role string) error {
	if !ValidRole(role) {
		return fmt.Errorf("%w: %q", ErrRoleUnknown, role)
	}

	if _, err := s.GetWorkspace(ctx, workspaceID); err != nil {
		return err
	}

	_, err := s.pool.Exec(ctx,
		`INSERT INTO workspace_members (workspace_id, subject, role, created_at)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (workspace_id, subject) DO UPDATE SET role = EXCLUDED.role`,
		workspaceID, subject, role, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("add member %s to %s: %w", subject, workspaceID, err)
	}

	return nil
}

// GetMemberRole returns one subject's role in a workspace.
func (s *Postgres) GetMemberRole(ctx context.Context, workspaceID, subject string) (string, error) {
	var role string

	err := s.pool.QueryRow(ctx,
		"SELECT role FROM workspace_members WHERE workspace_id = $1 AND subject = $2",
		workspaceID, subject).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: %s in %s", ErrMemberNotFound, subject, workspaceID)
	}

	if err != nil {
		return "", fmt.Errorf("get member role for %s in %s: %w", subject, workspaceID, err)
	}

	return role, nil
}

// ListMembers returns every member of a workspace, oldest membership first.
func (s *Postgres) ListMembers(ctx context.Context, workspaceID string) ([]*Member, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+memberColumns+` FROM workspace_members
		 WHERE workspace_id = $1
		 ORDER BY created_at ASC, subject ASC`,
		workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list members of %s: %w", workspaceID, err)
	}
	defer rows.Close()

	var members []*Member

	for rows.Next() {
		member, scanErr := scanPGMember(rows)
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

// seedDefault inserts the default workspace if absent. ON CONFLICT DO NOTHING
// keeps the seed idempotent across restarts and across the several nodes that
// may open the same database at once.
func (s *Postgres) seedDefault(ctx context.Context) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO workspaces (id, name, created_at) VALUES ($1, $2, $3)
		 ON CONFLICT (id) DO NOTHING`,
		defaultWorkspaceName, defaultWorkspaceName, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("insert default workspace: %w", err)
	}

	return nil
}

// scanPGWorkspace reads one workspaceColumns row back into a Workspace. It
// reuses the package's scanner interface (declared in sqlite.go) since pgx.Row
// and pgx.Rows satisfy the same one-method Scan surface.
func scanPGWorkspace(row scanner) (*Workspace, error) {
	var (
		workspace Workspace
		createdAt time.Time
	)

	err := row.Scan(&workspace.ID, &workspace.Name, &createdAt)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with the query's identity, including the pgx.ErrNoRows translation
	}

	workspace.CreatedAt = createdAt

	return &workspace, nil
}

// scanPGMember reads one memberColumns row back into a Member.
func scanPGMember(row scanner) (*Member, error) {
	var (
		member    Member
		createdAt time.Time
	)

	err := row.Scan(&member.WorkspaceID, &member.Subject, &member.Role, &createdAt)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with the query's identity
	}

	member.CreatedAt = createdAt

	return &member, nil
}
