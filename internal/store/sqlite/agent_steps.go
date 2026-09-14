package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/chadit/CairnOS/internal/store"
)

// agentStepColumns is the SELECT list agent-step queries share, in
// scanAgentStep order. The ast. prefix lets the workspace-scoped list join
// runs without ambiguous column names.
const agentStepColumns = "ast.run_id, ast.action_name, ast.step, ast.kind, ast.tool, ast.input_json, ast.output_json, ast.usage_json, ast.created_at"

// AppendAgentStep persists one agent-loop transcript step
// (.agents/spec/15-agent-loop.md, "Transcript persistence"). Steps are
// insert-only: the loop never rewrites history, so there is no upsert.
func (s *Store) AppendAgentStep(ctx context.Context, step *store.AgentStep) error {
	input, err := marshalJSON(step.Input, step.Input == nil)
	if err != nil {
		return err
	}

	output, err := marshalJSON(step.Output, step.Output == nil)
	if err != nil {
		return err
	}

	usage, err := marshalUsage(step.Usage)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO agent_steps (run_id, action_name, step, kind, tool, input_json, output_json, usage_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		step.RunID, step.ActionName, step.Step, step.Kind, nullableText(step.Tool),
		input, output, usage, formatTime(step.CreatedAt))
	if err != nil {
		return fmt.Errorf("append agent step %s/%s/%d: %w", step.RunID, step.ActionName, step.Step, err)
	}

	return nil
}

// ListAgentSteps returns one agent action's transcript ordered by step. The
// join to runs scopes the result to the request workspace, so a transcript
// whose run belongs to another workspace is invisible, matching
// ListActionResults (.agents/spec/12-ui-and-governance.md, "Multi-tenancy").
func (s *Store) ListAgentSteps(ctx context.Context, runID, actionName string) ([]*store.AgentStep, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+agentStepColumns+` FROM agent_steps ast
		 JOIN runs r ON r.id = ast.run_id
		 WHERE ast.run_id = ? AND ast.action_name = ? AND r.workspace_id = ?
		 ORDER BY ast.step`,
		runID, actionName, store.WorkspaceFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("list agent steps for %s/%s: %w", runID, actionName, err)
	}
	defer closeQuietly(rows)

	var steps []*store.AgentStep

	for rows.Next() {
		step, scanErr := scanAgentStep(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("list agent steps for %s/%s: %w", runID, actionName, scanErr)
		}

		steps = append(steps, step)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list agent steps for %s/%s: %w", runID, actionName, err)
	}

	return steps, nil
}

// scanAgentStep reads one agentStepColumns row back into a store.AgentStep.
func scanAgentStep(row rowScanner) (*store.AgentStep, error) {
	var (
		step      store.AgentStep
		tool      sql.NullString
		input     sql.NullString
		output    sql.NullString
		usage     sql.NullString
		createdAt sql.NullTime
	)

	err := row.Scan(&step.RunID, &step.ActionName, &step.Step, &step.Kind, &tool,
		&input, &output, &usage, &createdAt)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with their operation and run id
	}

	step.Tool = tool.String

	if unmarshalJSONErr := unmarshalJSON(input, &step.Input); unmarshalJSONErr != nil {
		return nil, unmarshalJSONErr
	}

	if unmarshalJSONErr := unmarshalJSON(output, &step.Output); unmarshalJSONErr != nil {
		return nil, unmarshalJSONErr
	}

	step.Usage, err = unmarshalUsage(usage)
	if err != nil {
		return nil, err
	}

	step.CreatedAt = parseTime(createdAt)

	return &step, nil
}

// nullableText renders an optional text column, mapping "" to NULL so the
// tool column stays queryable with IS NULL.
func nullableText(value string) any {
	if value == "" {
		return nil
	}

	return value
}
