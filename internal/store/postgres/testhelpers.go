package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// OpenSchema opens a store whose tables live in their own Postgres schema,
// giving each integration subtest the isolation the sqlite backend gets from
// a per-case temp file. Without it, parallel subtests against one shared
// database would truncate each other's rows mid-test. The pool pins
// search_path to schema (and falls back to public for anything shared), the
// schema is created if absent, and the store DDL runs inside it. It is
// exported package code (not a _test.go file) because the backend's
// external suite package needs it and Go cannot share unexported helpers
// across packages.
//
// DropSchema is the cleanup counterpart.
func OpenSchema(ctx context.Context, dsn, schema string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}

	// search_path takes effect on every pooled connection, so the unqualified
	// table names in the store's queries resolve to this schema. quoteIdent
	// guards the schema name even though tests only pass generated ones.
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}

	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open postgres pool: %w", err)
	}

	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+quoteIdent(schema)); err != nil {
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
	_, err := s.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+quoteIdent(schema)+" CASCADE")
	if err != nil {
		return fmt.Errorf("drop schema %s: %w", schema, err)
	}

	return nil
}

// quoteIdent wraps a SQL identifier in double quotes, doubling any embedded
// quote, so a generated schema name can never break out of the identifier
// context. Test schema names are machine-generated, but quoting keeps the
// DDL injection-safe regardless.
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
