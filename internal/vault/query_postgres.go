package vault

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Query runs the typed graph query against Postgres, behaviorally identical to
// the sqlite backend; the shared conformance suite runs both through one spec
// so a divergence fails a test. Semantic falls back to keyword with
// QueryResult.Fallback set (D2).
//
//nolint:gocritic // Store.Query fixes the by-value signature; helpers take *Query
func (s *Postgres) Query(ctx context.Context, query Query) (QueryResult, error) {
	result := QueryResult{Fallback: query.Semantic}
	workspace := workspaceFromContext(ctx)

	candidates, bounded, err := s.candidateIDs(ctx, &query, workspace)
	if err != nil {
		return QueryResult{}, err
	}

	if bounded && len(candidates) == 0 {
		return result, nil
	}

	docs, err := s.filterDocuments(ctx, &query, workspace, candidates, bounded)
	if err != nil {
		return QueryResult{}, err
	}

	result.Documents = docs

	return result, nil
}

// candidateIDs returns the id set the link traversal restricts the query to,
// and whether a traversal ran. Mirrors the sqlite backend.
func (s *Postgres) candidateIDs(ctx context.Context, query *Query, workspace string) ([]string, bool, error) {
	if query.LinkedFrom == "" {
		return nil, false, nil
	}

	reached, err := s.traverseLinks(ctx, query.LinkedFrom, clampHops(query.Hops), workspace)
	if err != nil {
		return nil, false, err
	}

	return reached, true, nil
}

// traverseLinks runs the same bounded BFS the sqlite backend runs, one query
// per hop over the links table, excluding the source and guarding cycles.
func (s *Postgres) traverseLinks(ctx context.Context, source string, hops int, workspace string) ([]string, error) {
	visited := map[string]struct{}{source: {}}
	frontier := []string{source}

	var reached []string

	for remaining := hops; remaining > 0 && len(frontier) > 0; remaining-- {
		next, err := s.linkTargetsFor(ctx, frontier, workspace)
		if err != nil {
			return nil, err
		}

		frontier = frontier[:0]

		for _, target := range next {
			if _, seen := visited[target]; seen {
				continue
			}

			visited[target] = struct{}{}
			reached = append(reached, target)
			frontier = append(frontier, target)
		}
	}

	slices.Sort(reached)

	return reached, nil
}

// linkTargetsFor reads every outbound target of the given source ids in one
// query, the per-level BFS expansion. The query is assembled from fixed
// fragments and a generated placeholder run; every value binds through args,
// following the store package's clause-builder convention (no fmt.Sprintf on
// SQL).
func (s *Postgres) linkTargetsFor(ctx context.Context, sources []string, workspace string) ([]string, error) {
	clause, args, next := pgInClause(sources, 1)
	wsPlaceholder := "$" + strconv.Itoa(next)

	args = append(args, workspace)

	query := "SELECT DISTINCT target_id FROM document_links WHERE doc_id IN (" + clause +
		") AND workspace_id = " + wsPlaceholder

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("traverse links: %w", err)
	}
	defer rows.Close()

	return collectPGIDs(rows)
}

// filterDocuments applies the tag, project, and text filters over the
// candidate set (or the whole workspace), returning matching documents. A
// running parameter index keeps the numbered placeholders in step with args.
func (s *Postgres) filterDocuments(ctx context.Context, query *Query, workspace string, candidates []string, bounded bool) ([]Document, error) {
	next := 1
	where := []string{"workspace_id = $" + strconv.Itoa(next)}
	args := []any{workspace}
	next++

	if bounded {
		clause, idArgs, after := pgInClause(candidates, next)
		where = append(where, "id IN ("+clause+")")
		args = append(args, idArgs...)
		next = after
	}

	for _, tag := range queryTags(query) {
		where = append(where,
			"EXISTS (SELECT 1 FROM jsonb_array_elements_text(documents.tags_json) AS t(value) WHERE value = $"+
				strconv.Itoa(next)+")")
		args = append(args, tag)
		next++
	}

	if trimmed := strings.TrimSpace(query.Text); trimmed != "" {
		where = append(where,
			"to_tsvector('simple', title || ' ' || body) @@ plainto_tsquery('simple', $"+strconv.Itoa(next)+")")
		args = append(args, trimmed)
		next++
	}

	limitPlaceholder := "$" + strconv.Itoa(next)

	args = append(args, limitOrNull(query.Limit))

	sqlText := "SELECT " + pgDocColumns + " FROM documents WHERE " + strings.Join(where, " AND ") +
		" ORDER BY created_at DESC, id DESC LIMIT " + limitPlaceholder

	return s.runDocumentQuery(ctx, sqlText, args)
}

// runDocumentQuery executes a built document query and collects the rows.
func (s *Postgres) runDocumentQuery(ctx context.Context, sqlText string, args []any) ([]Document, error) {
	rows, err := s.pool.Query(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("query documents: %w", err)
	}
	defer rows.Close()

	docs, err := collectPGDocuments(rows)
	if err != nil {
		return nil, fmt.Errorf("query documents: %w", err)
	}

	return docs, nil
}

// pgInClause renders numbered placeholders starting at start for an IN list,
// returning the placeholder text, the bound args, and the next free parameter
// index. An empty input yields NULL and no args so the caller never builds
// "IN ()", leaving start unchanged.
func pgInClause(values []string, start int) (string, []any, int) {
	if len(values) == 0 {
		return emptyInList, nil, start
	}

	placeholders := make([]string, len(values))
	args := make([]any, len(values))

	for i, value := range values {
		placeholders[i] = "$" + strconv.Itoa(start+i)
		args[i] = value
	}

	return strings.Join(placeholders, ", "), args, start + len(values)
}
