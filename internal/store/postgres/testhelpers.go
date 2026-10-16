package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OpenSchema opens a store whose tables live in their own Postgres schema, so
// parallel integration subtests get the isolation the sqlite backend gets from
// a per-case temp file instead of truncating each other's rows. It is exported
// because the backend's external suite package needs it and Go cannot share
// unexported helpers across packages.
//
// DropSchema is the cleanup counterpart.
func OpenSchema(ctx context.Context, dsn, schema string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}

	// search_path takes effect on every pooled connection, so the unqualified
	// table names in the store's queries resolve to this schema. Sanitize
	// guards the schema name even though tests only pass generated ones.
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}

	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open postgres pool: %w", err)
	}

	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{schema}.Sanitize()); err != nil {
		pool.Close()

		return nil, fmt.Errorf("create schema %s: %w", schema, err)
	}

	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		pool.Close()

		return nil, fmt.Errorf("create store schema in %s: %w", schema, err)
	}

	return &Store{pool: pool}, nil
}

// DropSchema removes a test schema and everything in it. Called from a
// subtest's cleanup so a finished run leaves no tables behind.
func DropSchema(ctx context.Context, s *Store, schema string) error {
	_, err := s.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	if err != nil {
		return fmt.Errorf("drop schema %s: %w", schema, err)
	}

	return nil
}
