package vault

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// maxHops caps link traversal depth so a dense graph cannot run the BFS
// unbounded. Five hops covers the spec's "documents linked from this, two hops
// out" intent with room to spare; a request for more is clamped, not rejected,
// since a clamp degrades gracefully where a rejection would surprise a caller.
const maxHops = 5

// Query runs the typed graph query against sqlite (S1): tag and project
// filtering, bounded link traversal from LinkedFrom out to Hops, and FTS5
// keyword match. A traversal, when present, produces the candidate id set the
// other filters narrow. Semantic falls back to keyword (D2).
//
//nolint:gocritic // Store.Query fixes the by-value signature; helpers take *Query
func (s *SQLite) Query(ctx context.Context, query Query) (QueryResult, error) {
	result := QueryResult{Fallback: query.Semantic}
	workspace := workspaceFromContext(ctx)

	candidates, bounded, err := s.candidateIDs(ctx, &query, workspace)
	if err != nil {
		return QueryResult{}, err
	}

	if bounded && len(candidates) == 0 {
		// A link traversal that reached nothing means an empty result, not a
		// fall-through to the whole workspace.
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
// and whether a traversal ran (bounded). With no LinkedFrom the result is
// (nil, false): the filter step then scans the whole workspace.
func (s *SQLite) candidateIDs(ctx context.Context, query *Query, workspace string) ([]string, bool, error) {
	if query.LinkedFrom == "" {
		return nil, false, nil
	}

	reached, err := s.traverseLinks(ctx, query.LinkedFrom, clampHops(query.Hops), workspace)
	if err != nil {
		return nil, false, err
	}

	return reached, true, nil
}

// traverseLinks runs a bounded breadth-first walk over the links table from
// the source document, returning the ids reachable within hops. The source
// itself is excluded: "documents linked from this" means the neighbors, not
// the origin. visited guards against cycles, and the frontier keeps only
// newly-seen ids so a diamond is not re-expanded.
func (s *SQLite) traverseLinks(ctx context.Context, source string, hops int, workspace string) ([]string, error) {
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
// query, the per-level expansion the BFS needs. Building one IN clause per
// level keeps the traversal at one round trip per hop, not one per node.
func (s *SQLite) linkTargetsFor(ctx context.Context, sources []string, workspace string) ([]string, error) {
	placeholders, args := inClause(sources)
	args = append(args, workspace)

	// #nosec G202 -- placeholders is a comma-joined run of "?" generated from
	// len(sources), never request text; every id binds as a positional arg.
	query := "SELECT DISTINCT target_id FROM document_links WHERE doc_id IN (" + placeholders +
		") AND workspace_id = ?"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("traverse links: %w", err)
	}
	defer closeRows(rows)

	return collectIDs(rows)
}

// filterDocuments applies the tag, project, and text filters over the
// candidate set (or the whole workspace when the query had no link traversal),
// returning the matching documents with their bodies. Link fields stay empty:
// a Query result is for reading content, and Get is the call that hydrates a
// single document's link graph.
func (s *SQLite) filterDocuments(ctx context.Context, query *Query, workspace string, candidates []string, bounded bool) ([]Document, error) {
	where := []string{"workspace_id = ?"}
	args := []any{workspace}

	if bounded {
		placeholders, idArgs := inClause(candidates)
		where = append(where, "id IN ("+placeholders+")")
		args = append(args, idArgs...)
	}

	for _, tag := range queryTags(query) {
		where = append(where, "EXISTS (SELECT 1 FROM json_each(documents.tags_json) WHERE value = ?)")
		args = append(args, tag)
	}

	if match := buildTextMatch(query.Text); match != "" {
		where = append(where, "rowid IN (SELECT rowid FROM documents_fts WHERE documents_fts MATCH ?)")
		args = append(args, match)
	}

	args = append(args, limitOrAll(query.Limit))

	// #nosec G202 -- every fragment in where is a fixed string with positional
	// placeholders; all values bind through args, none splice into the SQL.
	sqlText := "SELECT " + docColumns + " FROM documents WHERE " + strings.Join(where, " AND ") +
		" ORDER BY created_at DESC, id DESC LIMIT ?"

	return s.runDocumentQuery(ctx, sqlText, args)
}

// runDocumentQuery executes a built document query and collects the rows.
func (s *SQLite) runDocumentQuery(ctx context.Context, sqlText string, args []any) ([]Document, error) {
	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("query documents: %w", err)
	}
	defer closeRows(rows)

	docs, err := collectDocuments(rows)
	if err != nil {
		return nil, fmt.Errorf("query documents: %w", err)
	}

	return docs, nil
}

// queryTags merges the explicit tags with the project tag, so the project
// filter is one more required tag rather than a separate column. Order is
// stable for reproducible SQL.
func queryTags(query *Query) []string {
	tags := slices.Clone(query.Tags)

	if tag := projectTag(query.Project); tag != "" {
		tags = append(tags, tag)
	}

	slices.Sort(tags)

	return slices.Compact(tags)
}

// clampHops bounds the requested traversal depth to [1, maxHops]. A zero or
// negative Hops alongside a LinkedFrom means the direct neighbors (one hop),
// the natural default for "linked from this".
func clampHops(hops int) int {
	if hops < 1 {
		return 1
	}

	if hops > maxHops {
		return maxHops
	}

	return hops
}

// inClause renders n positional placeholders and the matching args for an SQL
// IN list. An empty input yields a placeholder that matches nothing, so the
// caller never builds "IN ()", which is a syntax error.
func inClause(values []string) (string, []any) {
	if len(values) == 0 {
		return "NULL", nil
	}

	placeholders := make([]string, len(values))
	args := make([]any, len(values))

	for i, value := range values {
		placeholders[i] = "?"
		args[i] = value
	}

	return strings.Join(placeholders, ", "), args
}

// buildTextMatch quotes free text as one FTS5 phrase over title and body.
// Stripping embedded double quotes first means the input can never terminate
// the phrase early and smuggle FTS5 query syntax in, the same guard the memory
// store uses.
func buildTextMatch(text string) string {
	cleaned := strings.TrimSpace(strings.ReplaceAll(text, `"`, ""))
	if cleaned == "" {
		return ""
	}

	return `"` + cleaned + `"`
}
