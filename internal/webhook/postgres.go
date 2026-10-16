package webhook

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chadit/CairnOS/internal/store"
)

// pgWebhookSchema is the Postgres registrations table: timestamptz created_at,
// a unique workflow_name for one-per-workflow, and unique hash columns so a
// token hash maps to exactly one registration and endpoint.
const pgWebhookSchema = `
CREATE TABLE IF NOT EXISTS webhook_registrations (
    id              TEXT PRIMARY KEY,
    workflow_name   TEXT NOT NULL UNIQUE,
    workflow_path   TEXT NOT NULL,
    prod_token_hash TEXT NOT NULL UNIQUE,
    test_token_hash TEXT NOT NULL UNIQUE,
    hmac_secret_ref TEXT NOT NULL DEFAULT '',
    payload_input   TEXT NOT NULL DEFAULT '',
    rate_per_minute INTEGER NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL
);
`

// pgWebhookColumns is the SELECT list every registration query shares, in
// scanPGRegistration order.
const pgWebhookColumns = "id, workflow_name, workflow_path, prod_token_hash, test_token_hash, hmac_secret_ref, payload_input, rate_per_minute, created_at"

// PostgresWebhooks is the multi-node registration backend. It borrows a shared
// pool and does not own it; the pool's creator closes it, so a node runs one
// pool for the run store, schedules, the queue, and registrations together.
type PostgresWebhooks struct {
	pool *pgxpool.Pool
}

// NewPostgresWebhooks applies the registrations schema on a shared pool and
// returns the backend. It takes the pool rather than a DSN so it joins the
// node's existing pool instead of opening a second one.
func NewPostgresWebhooks(ctx context.Context, pool *pgxpool.Pool) (*PostgresWebhooks, error) {
	if pool == nil {
		return nil, ErrNilPool
	}

	if _, err := pool.Exec(ctx, pgWebhookSchema); err != nil {
		return nil, fmt.Errorf("create webhook schema: %w", err)
	}

	return &PostgresWebhooks{pool: pool}, nil
}

// Add persists a registration, filling the id and created_at the caller left
// zero. ON CONFLICT on workflow_name reports zero rows affected, which maps to
// ErrDuplicate: one registration per workflow.
func (p *PostgresWebhooks) Add(ctx context.Context, reg *Registration) error {
	id := reg.ID
	if id == "" {
		id = store.NewID()
	}

	createdAt := reg.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	tag, err := p.pool.Exec(ctx,
		`INSERT INTO webhook_registrations
		 (id, workflow_name, workflow_path, prod_token_hash, test_token_hash, hmac_secret_ref, payload_input, rate_per_minute, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 ON CONFLICT (workflow_name) DO NOTHING`,
		id, reg.WorkflowName, reg.WorkflowPath, reg.ProdTokenHash, reg.TestTokenHash,
		reg.HMACSecretRef, reg.PayloadInput, reg.RatePerMinute, createdAt.UTC())
	if err != nil {
		return fmt.Errorf("insert webhook registration %s: %w", reg.WorkflowName, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrDuplicate, reg.WorkflowName)
	}

	reg.ID = id
	reg.CreatedAt = createdAt

	return nil
}

// GetByTokenHash finds the registration whose production or test hash equals
// hash and reports which endpoint matched. Unknown hash is ErrNotFound.
func (p *PostgresWebhooks) GetByTokenHash(ctx context.Context, hash string) (*Registration, Endpoint, error) {
	row := p.pool.QueryRow(ctx,
		"SELECT "+pgWebhookColumns+" FROM webhook_registrations WHERE prod_token_hash = $1 OR test_token_hash = $1",
		hash)

	reg, err := scanPGRegistration(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
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
func (p *PostgresWebhooks) GetByWorkflow(ctx context.Context, workflowName string) (*Registration, error) {
	row := p.pool.QueryRow(ctx,
		"SELECT "+pgWebhookColumns+" FROM webhook_registrations WHERE workflow_name = $1", workflowName)

	reg, err := scanPGRegistration(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, workflowName)
		}

		return nil, err
	}

	return reg, nil
}

// List returns every registration, oldest first.
func (p *PostgresWebhooks) List(ctx context.Context) ([]*Registration, error) {
	rows, err := p.pool.Query(ctx,
		"SELECT "+pgWebhookColumns+" FROM webhook_registrations ORDER BY created_at ASC, id ASC")
	if err != nil {
		return nil, fmt.Errorf("list webhook registrations: %w", err)
	}
	defer rows.Close()

	var regs []*Registration

	for rows.Next() {
		reg, scanErr := scanPGRegistration(rows)
		if scanErr != nil {
			return nil, scanErr
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
func (p *PostgresWebhooks) UpdateTokenHash(ctx context.Context, workflowName string, endpoint Endpoint, newHash string) error {
	column, err := columnForEndpoint(endpoint)
	if err != nil {
		return err
	}

	// #nosec G202 -- column is one of two package constants chosen by
	// columnForEndpoint, never caller input; the hash and name bind as parameters.
	tag, err := p.pool.Exec(ctx,
		"UPDATE webhook_registrations SET "+column+" = $1 WHERE workflow_name = $2", newHash, workflowName)
	if err != nil {
		return fmt.Errorf("rotate webhook token for %s: %w", workflowName, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, workflowName)
	}

	return nil
}

// Remove deletes a workflow's registration; zero rows is ErrNotFound.
func (p *PostgresWebhooks) Remove(ctx context.Context, workflowName string) error {
	tag, err := p.pool.Exec(ctx,
		"DELETE FROM webhook_registrations WHERE workflow_name = $1", workflowName)
	if err != nil {
		return fmt.Errorf("remove webhook registration %s: %w", workflowName, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, workflowName)
	}

	return nil
}

// scanPGRegistration reads one pgWebhookColumns row into a Registration.
func scanPGRegistration(row pgx.Row) (*Registration, error) {
	var reg Registration

	err := row.Scan(&reg.ID, &reg.WorkflowName, &reg.WorkflowPath, &reg.ProdTokenHash,
		&reg.TestTokenHash, &reg.HMACSecretRef, &reg.PayloadInput, &reg.RatePerMinute, &reg.CreatedAt)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers add the operation context and map pgx.ErrNoRows
	}

	return &reg, nil
}
