package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/chadit/CairnOS/internal/store"
)

// suggestionColumns is the SELECT list every suggestion query shares, in
// scanSuggestion order.
const suggestionColumns = "id, kind, target, status, rationale, evidence_json, proposed_json, savings_json, created_at"

// SaveSuggestion persists one tuning suggestion. A zero CreatedAt is stamped
// with the current time and an empty Status becomes open, matching the sqlite
// backend, so the tuning engine fills only what it computed. The workspace
// stamp makes the suggestion the generating tenant's; cross-tenant reads then
// return not-found.
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

	_, err = s.pool.Exec(ctx,
		`INSERT INTO suggestions (id, workspace_id, kind, target, status, rationale, evidence_json, proposed_json, savings_json, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		suggestion.ID, store.WorkspaceFromContext(ctx), string(suggestion.Kind), suggestion.Target, string(status), suggestion.Rationale,
		evidence, proposed, savings, createdAt.UTC())
	if err != nil {
		return fmt.Errorf("insert suggestion %s: %w", suggestion.ID, err)
	}

	return nil
}

// GetSuggestion fetches one suggestion by id, scoped to the request workspace
// so a suggestion owned by another tenant reads as not-found rather than
// leaking its evidence and proposal across the boundary.
func (s *Store) GetSuggestion(ctx context.Context, id string) (*store.Suggestion, error) {
	row := s.pool.QueryRow(ctx,
		"SELECT "+suggestionColumns+" FROM suggestions WHERE id = $1 AND workspace_id = $2",
		id, store.WorkspaceFromContext(ctx))

	suggestion, err := scanSuggestion(row)
	if isNoRows(err) {
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
	builder := newClauses()
	builder.add("workspace_id = ", store.WorkspaceFromContext(ctx))

	if filter.Status != "" {
		builder.add("status = ", string(filter.Status))
	}

	if filter.Kind != "" {
		builder.add("kind = ", string(filter.Kind))
	}

	query := "SELECT " + suggestionColumns + " FROM suggestions" + builder.where() + " ORDER BY created_at DESC, id DESC"
	if filter.Limit > 0 {
		query += " LIMIT " + builder.add("", filter.Limit)
	}

	rows, err := s.pool.Query(ctx, query, builder.args()...)
	if err != nil {
		return nil, fmt.Errorf("list suggestions: %w", err)
	}
	defer rows.Close()

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
	tag, err := s.pool.Exec(ctx,
		"UPDATE suggestions SET status = $1 WHERE id = $2 AND workspace_id = $3",
		string(status), id, store.WorkspaceFromContext(ctx))
	if err != nil {
		return fmt.Errorf("update suggestion %s status: %w", id, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", store.ErrSuggestionNotFound, id)
	}

	return nil
}

// savingsRecord mirrors store.Savings with JSON tags so the persisted shape
// is a stable contract independent of that package's field names, matching
// the sqlite backend byte for byte.
type savingsRecord struct {
	Confidence    string  `json:"confidence"`
	CostUSDPerRun float64 `json:"cost_usd_per_run"`
	LatencyNS     int64   `json:"latency_ns"`
}

// marshalSavings renders estimated savings for the savings_json column.
func marshalSavings(savings *store.Savings) (any, error) {
	record := savingsRecord{
		CostUSDPerRun: savings.CostUSDPerRun,
		LatencyNS:     int64(savings.LatencyDelta),
		Confidence:    savings.Confidence,
	}

	return marshalJSON(record, false)
}

// unmarshalSavings reads a nullable savings_json column.
func unmarshalSavings(column []byte) (store.Savings, error) {
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
		evidence   []byte
		proposed   []byte
		savings    []byte
		createdAt  time.Time
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

	suggestion.CreatedAt = createdAt

	return &suggestion, nil
}
