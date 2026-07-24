package webhook

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/chadit/CairnOS/internal/store"
)

// webhookSchema is the registrations table with IF NOT EXISTS so reopening the
// shared schedule database file is idempotent. workflow_name is unique so a
// workflow holds at most one registration; both hash columns are unique so a
// token hash maps to exactly one registration and endpoint.
const webhookSchema = `
CREATE TABLE IF NOT EXISTS webhook_registrations (
    id              TEXT PRIMARY KEY,
    workflow_name   TEXT NOT NULL UNIQUE,
    workflow_path   TEXT NOT NULL,
    prod_token_hash TEXT NOT NULL UNIQUE,
    test_token_hash TEXT NOT NULL UNIQUE,
    hmac_secret_ref TEXT NOT NULL DEFAULT '',
    payload_input   TEXT NOT NULL DEFAULT '',
    rate_per_minute INTEGER NOT NULL DEFAULT 0,
    created_at      TIMESTAMP NOT NULL
);
`

// webhookColumns is the SELECT list every registration query shares, in
// scanRegistration order.
const webhookColumns = "id, workflow_name, workflow_path, prod_token_hash, test_token_hash, hmac_secret_ref, payload_input, rate_per_minute, created_at"

// storedTimeLayout is RFC 3339 UTC with fixed-width nanoseconds, matching the
// schedule and run stores so this table's created_at sorts lexicographically
// alongside its siblings in the shared file.
const storedTimeLayout = "2006-01-02T15:04:05.000000000Z"

// SQLiteWebhooks is the single-node registration backend. It shares the
// schedule store's database file with a disjoint table, the arrangement the
// scheduler queue and memory stores already use.
type SQLiteWebhooks struct {
	db *sql.DB
}

// OpenSQLiteWebhooks opens (creating if needed) the registrations database at
// path and applies the schema, with the same WAL and busy_timeout pragmas the
// schedule and queue stores use so the CLI can register while serve holds the
// file open.
func OpenSQLiteWebhooks(path string) (*SQLiteWebhooks, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open webhook database %q: %w", path, err)
	}

	// Schema creation is process-lifetime initialization with no request to
	// inherit a context from; OpenSQLiteWebhooks carries no ctx on purpose.
	setupCtx := context.Background()

	if _, execErr := db.ExecContext(setupCtx, webhookSchema); execErr != nil {
		return nil, fmt.Errorf("create webhook schema: %w", errors.Join(execErr, db.Close()))
	}

	return &SQLiteWebhooks{db: db}, nil
}

// Close releases the underlying database.
func (s *SQLiteWebhooks) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close webhook database: %w", err)
	}

	return nil
}

// Add persists a registration, filling the id and created_at the caller left
// zero. ON CONFLICT on workflow_name reports zero rows affected, which maps to
// ErrDuplicate: one registration per workflow.
func (s *SQLiteWebhooks) Add(ctx context.Context, reg *Registration) error {
	id := reg.ID
	if id == "" {
		id = store.NewID()
	}

	createdAt := reg.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	result, err := s.db.ExecContext(ctx,
		`INSERT INTO webhook_registrations
		 (id, workflow_name, workflow_path, prod_token_hash, test_token_hash, hmac_secret_ref, payload_input, rate_per_minute, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(workflow_name) DO NOTHING`,
		id, reg.WorkflowName, reg.WorkflowPath, reg.ProdTokenHash, reg.TestTokenHash,
		reg.HMACSecretRef, reg.PayloadInput, reg.RatePerMinute, storedTime(createdAt))
	if err != nil {
		return fmt.Errorf("insert webhook registration %s: %w", reg.WorkflowName, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("insert webhook registration %s: rows affected: %w", reg.WorkflowName, err)
	}

	if affected == 0 {
		return fmt.Errorf("%w: %s", ErrDuplicate, reg.WorkflowName)
	}

	reg.ID = id
	reg.CreatedAt = createdAt

	return nil
}

// GetByTokenHash finds the registration whose production or test hash equals
// hash and reports which endpoint matched. The OR spans two unique indexes;
// unknown hash is ErrNotFound.
func (s *SQLiteWebhooks) GetByTokenHash(ctx context.Context, hash string) (*Registration, Endpoint, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+webhookColumns+" FROM webhook_registrations WHERE prod_token_hash = ? OR test_token_hash = ?",
		hash, hash)

	reg, err := scanRegistrationRow(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, EndpointProd, ErrNotFound
		}

		return nil, EndpointProd, err
	}

	endpoint := EndpointProd
	if reg.TestTokenHash == hash {
		endpoint = EndpointTest
	}

	return reg, endpoint, nil
}

// GetByWorkflow returns the registration for a workflow name, or ErrNotFound.
func (s *SQLiteWebhooks) GetByWorkflow(ctx context.Context, workflowName string) (*Registration, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+webhookColumns+" FROM webhook_registrations WHERE workflow_name = ?", workflowName)

	reg, err := scanRegistrationRow(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, workflowName)
		}

		return nil, err
	}

	return reg, nil
}

// List returns every registration, oldest first; created_at is stored in a
// lexicographically ordered layout, so the walk is chronological.
func (s *SQLiteWebhooks) List(ctx context.Context) ([]*Registration, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+webhookColumns+" FROM webhook_registrations ORDER BY created_at ASC, id ASC")
	if err != nil {
		return nil, fmt.Errorf("list webhook registrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var regs []*Registration

	for rows.Next() {
		reg, scanErr := scanRegistration(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("list webhook registrations: %w", scanErr)
		}

		regs = append(regs, reg)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list webhook registrations: %w", err)
	}

	return regs, nil
}

// UpdateTokenHash rotates one endpoint's hash for a workflow. The column comes
// from columnForEndpoint, so the endpoint value never reaches the SQL string;
// zero rows affected is ErrNotFound.
func (s *SQLiteWebhooks) UpdateTokenHash(ctx context.Context, workflowName string, endpoint Endpoint, newHash string) error {
	column, err := columnForEndpoint(endpoint)
	if err != nil {
		return err
	}

	// #nosec G202 -- column is one of two package constants chosen by
	// columnForEndpoint, never caller input; the hash and name bind as parameters.
	result, err := s.db.ExecContext(ctx,
		"UPDATE webhook_registrations SET "+column+" = ? WHERE workflow_name = ?", newHash, workflowName)
	if err != nil {
		return fmt.Errorf("rotate webhook token for %s: %w", workflowName, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rotate webhook token for %s: rows affected: %w", workflowName, err)
	}

	if affected == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, workflowName)
	}

	return nil
}

// Remove deletes a workflow's registration; zero rows is ErrNotFound.
func (s *SQLiteWebhooks) Remove(ctx context.Context, workflowName string) error {
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM webhook_registrations WHERE workflow_name = ?", workflowName)
	if err != nil {
		return fmt.Errorf("remove webhook registration %s: %w", workflowName, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("remove webhook registration %s: rows affected: %w", workflowName, err)
	}

	if affected == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, workflowName)
	}

	return nil
}

// storedTime renders a timestamp in the shared fixed-width layout.
func storedTime(value time.Time) string {
	return value.UTC().Format(storedTimeLayout)
}

// rowScanner is the shared shape of *sql.Row and *sql.Rows, so one scan helper
// serves both the single-row lookups and the list walk.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanRegistration reads one webhookColumns row into a Registration.
func scanRegistration(scanner rowScanner) (*Registration, error) {
	var (
		reg       Registration
		createdAt sql.NullTime
	)

	err := scanner.Scan(&reg.ID, &reg.WorkflowName, &reg.WorkflowPath, &reg.ProdTokenHash,
		&reg.TestTokenHash, &reg.HMACSecretRef, &reg.PayloadInput, &reg.RatePerMinute, &createdAt)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers add the operation context and map sql.ErrNoRows
	}

	reg.CreatedAt = createdAt.Time

	return &reg, nil
}

// scanRegistrationRow is scanRegistration for a single-row query, kept distinct
// so the sql.ErrNoRows mapping stays in the caller.
func scanRegistrationRow(row *sql.Row) (*Registration, error) {
	return scanRegistration(row)
}
