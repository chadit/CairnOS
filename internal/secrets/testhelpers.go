package secrets

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OpenPostgresSchema opens a secret store whose table lives in its own Postgres
// schema, so parallel integration subtests get the isolation the sqlite backend
// gets from a per-case temp file. It is exported because the backend's external
// conformance suite needs it, and Go cannot share unexported helpers across
// packages.
//
// DropPostgresSchema is the cleanup counterpart.
func OpenPostgresSchema(ctx context.Context, dsn, schema string, sealer *Sealer) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}

	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}

	// search_path applies to every pooled connection, so the unqualified table
	// name in the store's queries resolves to this schema.
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open secrets postgres pool: %w", err)
	}

	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{schema}.Sanitize()); err != nil {
		pool.Close()

		return nil, fmt.Errorf("create schema %s: %w", schema, err)
	}

	if _, err := pool.Exec(ctx, pgSecretsSchema); err != nil {
		pool.Close()

		return nil, fmt.Errorf("create secrets schema in %s: %w", schema, err)
	}

	return &Postgres{pool: pool, sealer: sealer}, nil
}

// DropPostgresSchema removes a test schema and everything in it.
func DropPostgresSchema(ctx context.Context, s *Postgres, schema string) error {
	_, err := s.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	if err != nil {
		return fmt.Errorf("drop schema %s: %w", schema, err)
	}

	return nil
}
