package rules

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// rulesSchema is the dedicated rules table, keyed (workspace, language,
// project, key) so a PutRule upserts in place and the merge is two filtered
// reads. An empty project is the language default; the composite primary key
// lets one (language, key) pair carry both a default row and per-project
// override rows. IF NOT EXISTS keeps opening an existing database idempotent.
const rulesSchema = `
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

// ruleColumns is the SELECT list every rule query shares, in scanRule order.
const ruleColumns = "language, project, key, value, description"

// SQLite is the default rules backend: a flat table, zero external services.
// Safe for concurrent use through WAL mode and busy_timeout, the same pragma
// choices as the memory, vault, and run stores.
type SQLite struct {
	db *sql.DB
}

// OpenSQLite opens (creating if needed) the rules database at path and applies
// the schema. It carries no ctx on purpose: schema creation is
// process-lifetime initialization, not request work.
func OpenSQLite(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open rules database %q: %w", path, err)
	}

	// Schema creation is process-lifetime initialization with no request to
	// inherit a context from; OpenSQLite carries no ctx on purpose.
	setupCtx := context.Background()

	if _, execErr := db.ExecContext(setupCtx, rulesSchema); execErr != nil {
		return nil, fmt.Errorf("create rules schema: %w", errors.Join(execErr, db.Close()))
	}

	return &SQLite{db: db}, nil
}

// Close releases the underlying database.
func (s *SQLite) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close rules database: %w", err)
	}

	return nil
}

// PutRule upserts one rule by its (workspace, language, project, key) primary
// key, so re-putting a key updates its value and description in place. An empty
// language or key is rejected: both are part of the key.
func (s *SQLite) PutRule(ctx context.Context, rule *Rule) error {
	if rule.Language == "" || rule.Key == "" {
		return fmt.Errorf("%w: language=%q key=%q", ErrInvalidRule, rule.Language, rule.Key)
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO language_rules (workspace_id, language, project, key, value, description)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(workspace_id, language, project, key) DO UPDATE SET
		     value = excluded.value, description = excluded.description`,
		workspaceFromContext(ctx), rule.Language, rule.Project, rule.Key, rule.Value, rule.Description)
	if err != nil {
		return fmt.Errorf("put rule %s/%s/%s: %w", rule.Language, rule.Project, rule.Key, err)
	}

	return nil
}

// GetRules returns the merged rule set for a language, with the project's
// overrides applied over the language defaults (.agents/spec/05-memory-and-knowledge.md).
// A language with no rows at all is ErrUnknownLanguage, matching the
// static-file stub's "no rules file for language" so the runner's error code
// stays stable. A known language with no matching project overlay returns the
// defaults, which is normal: most projects ride the language defaults.
func (s *SQLite) GetRules(ctx context.Context, language, project string) (RuleSet, error) {
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
func (s *SQLite) ListLanguages(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT DISTINCT language FROM language_rules WHERE workspace_id = ? ORDER BY language",
		workspaceFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("list languages: %w", err)
	}
	defer closeRows(rows)

	return collectStrings(rows)
}

// ListRules returns every rule for a language across all projects (and the
// defaults), unmerged, sorted by project then key, so an operator can see
// where each value comes from before the merge collapses them.
func (s *SQLite) ListRules(ctx context.Context, language string) ([]Rule, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+ruleColumns+` FROM language_rules
		 WHERE workspace_id = ? AND language = ?
		 ORDER BY project, key`,
		workspaceFromContext(ctx), language)
	if err != nil {
		return nil, fmt.Errorf("list rules for %s: %w", language, err)
	}
	defer closeRows(rows)

	return collectRules(rows)
}

// projectOverrides reads the project's rules, or nothing when no project was
// asked for, so GetRules can union them over the defaults without branching.
func (s *SQLite) projectOverrides(ctx context.Context, workspace, language, project string) ([]Rule, error) {
	if project == "" {
		return nil, nil
	}

	return s.rulesFor(ctx, workspace, language, project)
}

// rulesFor reads the rules for one (language, project) slice within the
// workspace, ordered by key for a stable read.
func (s *SQLite) rulesFor(ctx context.Context, workspace, language, project string) ([]Rule, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+ruleColumns+` FROM language_rules
		 WHERE workspace_id = ? AND language = ? AND project = ?
		 ORDER BY key`,
		workspace, language, project)
	if err != nil {
		return nil, fmt.Errorf("read rules %s/%s: %w", language, project, err)
	}
	defer closeRows(rows)

	return collectRules(rows)
}

// rowScanner abstracts *sql.Row and *sql.Rows so scanRule serves both.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanRule reads one ruleColumns row back into a Rule.
func scanRule(row rowScanner) (Rule, error) {
	var rule Rule

	err := row.Scan(&rule.Language, &rule.Project, &rule.Key, &rule.Value, &rule.Description)
	if err != nil {
		return Rule{}, err //nolint:wrapcheck // callers wrap with their operation
	}

	return rule, nil
}

// collectRules drains rows through scanRule, surfacing iteration errors.
func collectRules(rows *sql.Rows) ([]Rule, error) {
	var out []Rule

	for rows.Next() {
		rule, err := scanRule(rows)
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

// collectStrings drains a single-column string query.
func collectStrings(rows *sql.Rows) ([]string, error) {
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

// closeRows closes rows where the read path already reports errors via
// rows.Err, making a second error from Close redundant.
func closeRows(rows *sql.Rows) {
	_ = rows.Close()
}
