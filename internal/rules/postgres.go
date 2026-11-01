package rules

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgRulesSchema mirrors the sqlite rules table with the same composite primary
// key, plus IF NOT EXISTS so opening an existing database is idempotent.
const pgRulesSchema = `
CREATE TABLE IF NOT EXISTS language_rules (
    workspace_id TEXT NOT NULL DEFAULT 'default',
    language     TEXT NOT NULL,
    project      TEXT NOT NULL DEFAULT '',
    key          TEXT NOT NULL,
    value        TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (workspace_id, language, project, key)
);

CREATE INDEX IF NOT EXISTS idx_language_rules_lookup ON language_rules(workspace_id, language, project);
`

// Postgres is the shared, multi-node rules backend. It shares the run store's
// database (a different table), the same arrangement the sqlite backends use
// within one file, and runs the same conformance suite as the sqlite backend so
// the two stay behaviorally identical.
type Postgres struct {
	pool *pgxpool.Pool
}

// OpenPostgres connects to dsn, verifies the connection, and applies the rules
// schema. The DSN carries credentials from CAIRNOS_STORE_DSN.
func OpenPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open rules postgres pool: %w", err)
	}

	if pingErr := pool.Ping(ctx); pingErr != nil {
		pool.Close()

		return nil, fmt.Errorf("ping rules postgres: %w", pingErr)
	}

	if _, execErr := pool.Exec(ctx, pgRulesSchema); execErr != nil {
		pool.Close()

		return nil, fmt.Errorf("create rules schema: %w", execErr)
	}

	return &Postgres{pool: pool}, nil
}

// Close releases the connection pool.
func (s *Postgres) Close() error {
	s.pool.Close()

	return nil
}

// PutRule upserts one rule by its (workspace, language, project, key) primary
// key, the same contract as the sqlite backend.
func (s *Postgres) PutRule(ctx context.Context, rule *Rule) error {
	if rule.Language == "" || rule.Key == "" {
		return fmt.Errorf("%w: language=%q key=%q", ErrInvalidRule, rule.Language, rule.Key)
	}

	_, err := s.pool.Exec(ctx,
		`INSERT INTO language_rules (workspace_id, language, project, key, value, description)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT(workspace_id, language, project, key) DO UPDATE SET
		     value = excluded.value, description = excluded.description`,
		workspaceFromContext(ctx), rule.Language, rule.Project, rule.Key, rule.Value, rule.Description)
	if err != nil {
		return fmt.Errorf("put rule %s/%s/%s: %w", rule.Language, rule.Project, rule.Key, err)
	}

	return nil
}

// GetRules returns the merged rule set for a language, behaviorally identical
// to the sqlite backend: defaults plus project overrides, ErrUnknownLanguage
// when the language has no rows.
func (s *Postgres) GetRules(ctx context.Context, language, project string) (RuleSet, error) {
	workspace := workspaceFromContext(ctx)

	defaults, err := s.rulesFor(ctx, workspace, language, "")
	if err != nil {
		return RuleSet{}, err
	}

	overrides, err := s.projectOverrides(ctx, workspace, language, project)
	if err != nil {
		return RuleSet{}, err
	}

	if len(defaults) == 0 && len(overrides) == 0 {
		return RuleSet{}, fmt.Errorf("%w: %s", ErrUnknownLanguage, language)
	}

	return Merge(language, project, defaults, overrides), nil
}

// ListLanguages returns the distinct languages with at least one rule in the
// workspace, sorted.
func (s *Postgres) ListLanguages(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT DISTINCT language FROM language_rules WHERE workspace_id = $1 ORDER BY language",
		workspaceFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("list languages: %w", err)
	}
	defer rows.Close()

	return collectPGStrings(rows)
}

// ListRules returns every rule for a language across all projects, unmerged,
// sorted by project then key.
func (s *Postgres) ListRules(ctx context.Context, language string) ([]Rule, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+ruleColumns+` FROM language_rules
		 WHERE workspace_id = $1 AND language = $2
		 ORDER BY project, key`,
		workspaceFromContext(ctx), language)
	if err != nil {
		return nil, fmt.Errorf("list rules for %s: %w", language, err)
	}
	defer rows.Close()

	return collectPGRules(rows)
}

// projectOverrides reads the project's rules, or nothing when no project was
// asked for.
func (s *Postgres) projectOverrides(ctx context.Context, workspace, language, project string) ([]Rule, error) {
	if project == "" {
		return nil, nil
	}

	return s.rulesFor(ctx, workspace, language, project)
}

// rulesFor reads the rules for one (language, project) slice within the
// workspace, ordered by key for a stable read.
func (s *Postgres) rulesFor(ctx context.Context, workspace, language, project string) ([]Rule, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+ruleColumns+` FROM language_rules
		 WHERE workspace_id = $1 AND language = $2 AND project = $3
		 ORDER BY key`,
		workspace, language, project)
	if err != nil {
		return nil, fmt.Errorf("read rules %s/%s: %w", language, project, err)
	}
	defer rows.Close()

	return collectPGRules(rows)
}

// scanPGRule reads one ruleColumns row back into a Rule. It reuses the
// package's rowScanner (declared in sqlite.go) since pgx rows satisfy the same
// one-method Scan interface as the database/sql types.
func scanPGRule(row rowScanner) (Rule, error) {
	var rule Rule

	err := row.Scan(&rule.Language, &rule.Project, &rule.Key, &rule.Value, &rule.Description)
	if err != nil {
		return Rule{}, err //nolint:wrapcheck // callers wrap with their operation
	}

	return rule, nil
}

// collectPGRules drains rows through scanPGRule, surfacing iteration errors.
func collectPGRules(rows pgx.Rows) ([]Rule, error) {
	var out []Rule

	for rows.Next() {
		rule, err := scanPGRule(rows)
		if err != nil {
			return nil, err
		}

		out = append(out, rule)
	}

	if err := rows.Err(); err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with their operation
	}

	return out, nil
}

// collectPGStrings drains a single-column string query.
func collectPGStrings(rows pgx.Rows) ([]string, error) {
	var out []string

	for rows.Next() {
		var value string

		if err := rows.Scan(&value); err != nil {
			return nil, fmt.Errorf("scan value: %w", err)
		}

		out = append(out, value)
	}

	if err := rows.Err(); err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with their operation
	}

	return out, nil
}
