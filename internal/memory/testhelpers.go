package memory

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// OpenPostgresSchema opens a memory store in its own Postgres schema, giving
// each integration subtest the isolation the sqlite backend gets from a
// per-case temp file. It lives in package code rather than a _test.go file
// because the backend's external suite package needs it, and Go cannot share
// unexported helpers across packages. DropPostgresSchema is the counterpart.
func OpenPostgresSchema(ctx context.Context, dsn, schema string) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}

	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}

	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open memory postgres pool: %w", err)
	}

	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+quoteIdent(schema)); err != nil {
		pool.Close()

		return nil, fmt.Errorf("create schema %s: %w", schema, err)
	}

	if _, err := pool.Exec(ctx, pgMemorySchema); err != nil {
		pool.Close()

		return nil, fmt.Errorf("create memory schema in %s: %w", schema, err)
	}

	return &Postgres{pool: pool}, nil
}

// DropPostgresSchema removes a test schema and everything in it.
func DropPostgresSchema(ctx context.Context, s *Postgres, schema string) error {
	_, err := s.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+quoteIdent(schema)+" CASCADE")
	if err != nil {
		return fmt.Errorf("drop schema %s: %w", schema, err)
	}

	return nil
}

// quoteIdent wraps a SQL identifier in double quotes, doubling any embedded
// quote, so a generated schema name can never break out of the identifier
// context.
func quoteIdent(name string) string {
	quoted := make([]byte, 0, len(name)+2)
	quoted = append(quoted, '"')

	for i := range len(name) {
		if name[i] == '"' {
			quoted = append(quoted, '"')
		}

		quoted = append(quoted, name[i])
	}

	quoted = append(quoted, '"')

	return string(quoted)
}
