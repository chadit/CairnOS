package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/store"
)

// suggestionColumns is the SELECT list every suggestion query shares, in
// scanSuggestion order.
const suggestionColumns = "id, kind, target, status, rationale, evidence_json, proposed_json, savings_json, created_at"

// SaveSuggestion persists one tuning suggestion. A zero CreatedAt is stamped
// with the current time and an empty Status becomes open, so the tuning engine
// only fills what it computed. The row is stamped with the request workspace so
// a suggestion is owned by the tenant that generated it; cross-tenant reads
// then return not-found.
func (s *Store) SaveSuggestion(ctx context.Context, suggestion *store.Suggestion) error {
	status := suggestion.Status
	if status == "" {
		status = store.SuggestionOpen
	}

	createdAt := suggestion.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	evidence, err := marshalJSON(suggestion.Evidence, suggestion.Evidence == nil)
	if err != nil {
		return err
	}

	proposed, err := marshalJSON(suggestion.Proposed, suggestion.Proposed == nil)
	if err != nil {
		return err
	}

	savings, err := marshalSavings(&suggestion.EstSavings)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO suggestions (id, workspace_id, kind, target, status, rationale, evidence_json, proposed_json, savings_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		suggestion.ID, store.WorkspaceFromContext(ctx), string(suggestion.Kind), suggestion.Target, string(status), suggestion.Rationale,
		evidence, proposed, savings, formatTime(createdAt))
	if err != nil {
		return fmt.Errorf("insert suggestion %s: %w", suggestion.ID, err)
	}

	return nil
}

// GetSuggestion fetches one suggestion by id, scoped to the request workspace
// so a suggestion owned by another tenant reads as not-found rather than
// leaking its evidence and proposal across the boundary.
func (s *Store) GetSuggestion(ctx context.Context, id string) (*store.Suggestion, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+suggestionColumns+" FROM suggestions WHERE id = ? AND workspace_id = ?",
		id, store.WorkspaceFromContext(ctx))

	suggestion, err := scanSuggestion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", store.ErrSuggestionNotFound, id)
	}

	if err != nil {
		return nil, fmt.Errorf("get suggestion %s: %w", id, err)
	}

	return suggestion, nil
}

// ListSuggestions returns suggestions newest-first, narrowed by the filter.
// Newest-first uses created_at with the ULID id as tie-break, matching the
// run listing's stable ordering. Every list is scoped to the request workspace
// first, so a token bound to one tenant never sees another's suggestions.
func (s *Store) ListSuggestions(ctx context.Context, filter store.SuggestionFilter) ([]*store.Suggestion, error) {
	clauses := []string{workspaceClause}
	args := []any{store.WorkspaceFromContext(ctx)}

	if filter.Status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, string(filter.Status))
	}

	if filter.Kind != "" {
		clauses = append(clauses, "kind = ?")
		args = append(args, string(filter.Kind))
	}

	// The workspace clause is always present, so WHERE is unconditional.
	// #nosec G202 -- every clause is a constant string; values bind as parameters.
	query := "SELECT " + suggestionColumns + " FROM suggestions WHERE " + strings.Join(clauses, " AND ")

	query += newestFirst

	if filter.Limit > 0 {
		query += limitClause

		args = append(args, filter.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list suggestions: %w", err)
	}
	defer closeQuietly(rows)

	var suggestions []*store.Suggestion

	for rows.Next() {
		suggestion, scanErr := scanSuggestion(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("list suggestions: %w", scanErr)
		}

		suggestions = append(suggestions, suggestion)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list suggestions: %w", err)
	}

	return suggestions, nil
}

// UpdateSuggestionStatus moves a suggestion through its review states, scoped
// to the request workspace so one tenant cannot flip the status of another's
// suggestion by id; a cross-tenant id reads as not-found. The Phase 3 flow only
// ever sets approved or rejected; nothing is auto-applied.
func (s *Store) UpdateSuggestionStatus(ctx context.Context, id string, status store.SuggestionStatus) error {
	result, err := s.db.ExecContext(ctx,
		"UPDATE suggestions SET status = ? WHERE id = ? AND workspace_id = ?",
		string(status), id, store.WorkspaceFromContext(ctx))
	if err != nil {
		return fmt.Errorf("update suggestion %s status: %w", id, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update suggestion %s status: rows affected: %w", id, err)
	}

	if affected == 0 {
		return fmt.Errorf("%w: %s", store.ErrSuggestionNotFound, id)
	}

	return nil
}

// savingsRecord mirrors store.Savings with JSON tags so the persisted shape
// is a stable contract independent of that package's field names.
type savingsRecord struct {
	Confidence    string  `json:"confidence"`
	CostUSDPerRun float64 `json:"cost_usd_per_run"`
	LatencyNS     int64   `json:"latency_ns"`
}

// marshalSavings renders estimated savings for the savings_json column.
func marshalSavings(savings *store.Savings) (sql.NullString, error) {
	record := savingsRecord{
		CostUSDPerRun: savings.CostUSDPerRun,
		LatencyNS:     int64(savings.LatencyDelta),
		Confidence:    savings.Confidence,
	}

	return marshalJSON(record, false)
}

// unmarshalSavings reads a nullable savings_json column.
func unmarshalSavings(column sql.NullString) (store.Savings, error) {
	var record savingsRecord
	if err := unmarshalJSON(column, &record); err != nil {
		return store.Savings{}, err
	}

	return store.Savings{
		CostUSDPerRun: record.CostUSDPerRun,
		LatencyDelta:  time.Duration(record.LatencyNS),
		Confidence:    record.Confidence,
	}, nil
}

// scanSuggestion reads one suggestionColumns row back into a
// store.Suggestion.
func scanSuggestion(row rowScanner) (*store.Suggestion, error) {
	var (
		suggestion store.Suggestion
		kind       string
		status     string
		evidence   sql.NullString
		proposed   sql.NullString
		savings    sql.NullString
		createdAt  sql.NullTime
	)

	err := row.Scan(&suggestion.ID, &kind, &suggestion.Target, &status, &suggestion.Rationale,
		&evidence, &proposed, &savings, &createdAt)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with their operation and suggestion id
	}

	suggestion.Kind = store.SuggestionKind(kind)
	suggestion.Status = store.SuggestionStatus(status)

	if unmarshalJSONErr := unmarshalJSON(evidence, &suggestion.Evidence); unmarshalJSONErr != nil {
		return nil, unmarshalJSONErr
	}

	if unmarshalJSONErr := unmarshalJSON(proposed, &suggestion.Proposed); unmarshalJSONErr != nil {
		return nil, unmarshalJSONErr
	}

	suggestion.EstSavings, err = unmarshalSavings(savings)
	if err != nil {
		return nil, err
	}

	suggestion.CreatedAt = parseTime(createdAt)

	return &suggestion, nil
}
