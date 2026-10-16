package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/store"
)

// interruptedCode is the canonical error code for work lost to a restart
// (.agents/spec/04-execution-engine.md, "State persistence").
const interruptedCode = "engine.interrupted"

// ReconcileInterrupted fails running actions and their running runs with
// engine.interrupted and returns the number of runs reconciled. The engine
// calls it once at startup because CairnOS never silently resumes a runner call
// it cannot prove is still alive.
//
// Several nodes may each run this concurrently, but it is idempotent: a second
// pass finds no running runs to reconcile.
func (s *Store) ReconcileInterrupted(ctx context.Context) (int, error) {
	interrupted, err := marshalError(cairnerr.New(interruptedCode, "run interrupted by engine restart"))
	if err != nil {
		return 0, err
	}

	now := time.Now().UTC()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("reconcile interrupted: begin: %w", err)
	}
	// One deferred rollback covers every error path; after Commit it is the
	// ErrTxClosed no-op rollbackUncommitted ignores.
	defer rollbackUncommitted(ctx, tx)

	_, err = tx.Exec(ctx,
		`UPDATE action_results
		 SET status = $1, error_json = $2, finished_at = $3
		 WHERE status = $4 AND run_id IN (SELECT id FROM runs WHERE status = $5)`,
		string(store.ActionFailed), interrupted, now, string(store.ActionRunning), string(store.RunRunning))
	if err != nil {
		return 0, fmt.Errorf("reconcile interrupted: fail actions: %w", err)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE runs
		 SET status = $1, error_json = $2, finished_at = $3
		 WHERE status = $4`,
		string(store.RunFailed), interrupted, now, string(store.RunRunning))
	if err != nil {
		return 0, fmt.Errorf("reconcile interrupted: finalize runs: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("reconcile interrupted: commit: %w", err)
	}

	return int(tag.RowsAffected()), nil
}

// Prune enforces retention (.agents/spec/04-execution-engine.md, "Retention
// and pruning"): runs older than RunsOlderThan are deleted, and output_json
// older than OutputsOlderThan is nulled while the row stays. Agent transcripts
// age under the outputs policy because they are the fastest-growing data
// (.agents/spec/15-agent-loop.md), and their truncated rows count toward
// OutputsTruncated. Age comes from the run's created_at so a run's rows age
// together.
func (s *Store) Prune(ctx context.Context, opts store.PruneOptions) (store.PruneStats, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	var stats store.PruneStats

	if opts.RunsOlderThan > 0 {
		deleted, staleSuggestions, err := s.deleteRunsBefore(ctx, now.Add(-opts.RunsOlderThan))
		if err != nil {
			return store.PruneStats{}, err
		}

		stats.RunsDeleted = deleted
		stats.SuggestionsDeleted = staleSuggestions
	}

	if opts.OutputsOlderThan > 0 {
		truncated, err := s.truncateOutputsBefore(ctx, now.Add(-opts.OutputsOlderThan))
		if err != nil {
			return store.PruneStats{}, err
		}

		stats.OutputsTruncated = truncated
	}

	return stats, nil
}

// deleteRunsBefore removes runs created before cutoff together with their
// action results and agent steps, in one transaction so a crash cannot
// orphan child rows. Suggestions created before the same cutoff go with
// them: their evidence cites runs that no longer exist, so an aged
// suggestion is stale regardless of its review status.
func (s *Store) deleteRunsBefore(ctx context.Context, cutoff time.Time) (int, int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("prune runs: begin: %w", err)
	}
	defer rollbackUncommitted(ctx, tx)

	utcCutoff := cutoff.UTC()

	_, err = tx.Exec(ctx,
		"DELETE FROM agent_steps WHERE run_id IN (SELECT id FROM runs WHERE created_at < $1)", utcCutoff)
	if err != nil {
		return 0, 0, fmt.Errorf("prune runs: delete agent steps: %w", err)
	}

	_, err = tx.Exec(ctx,
		"DELETE FROM action_results WHERE run_id IN (SELECT id FROM runs WHERE created_at < $1)", utcCutoff)
	if err != nil {
		return 0, 0, fmt.Errorf("prune runs: delete action results: %w", err)
	}

	suggestionTag, err := tx.Exec(ctx,
		"DELETE FROM suggestions WHERE created_at < $1", utcCutoff)
	if err != nil {
		return 0, 0, fmt.Errorf("prune runs: delete stale suggestions: %w", err)
	}

	runTag, err := tx.Exec(ctx, "DELETE FROM runs WHERE created_at < $1", utcCutoff)
	if err != nil {
		return 0, 0, fmt.Errorf("prune runs: delete runs: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("prune runs: commit: %w", err)
	}

	return int(runTag.RowsAffected()), int(suggestionTag.RowsAffected()), nil
}

// truncateOutputsBefore nulls out output_json for actions and agent steps
// whose run was created before cutoff, keeping the rest of each row so run
// metadata and transcript shape stay reportable after large outputs drop.
func (s *Store) truncateOutputsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	utcCutoff := cutoff.UTC()

	actionTag, err := s.pool.Exec(ctx,
		`UPDATE action_results
		 SET output_json = NULL
		 WHERE output_json IS NOT NULL
		   AND run_id IN (SELECT id FROM runs WHERE created_at < $1)`,
		utcCutoff)
	if err != nil {
		return 0, fmt.Errorf("prune outputs: %w", err)
	}

	stepTag, err := s.pool.Exec(ctx,
		`UPDATE agent_steps
		 SET output_json = NULL
		 WHERE output_json IS NOT NULL
		   AND run_id IN (SELECT id FROM runs WHERE created_at < $1)`,
		utcCutoff)
	if err != nil {
		return 0, fmt.Errorf("prune agent step outputs: %w", err)
	}

	return int(actionTag.RowsAffected() + stepTag.RowsAffected()), nil
}

// rollbackUncommitted abandons tx. After a successful Commit pgx reports
// ErrTxClosed, the expected happy-path answer; any other failure is logged
// because the deferring caller has already chosen its own return value.
func rollbackUncommitted(ctx context.Context, tx pgx.Tx) {
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		slog.WarnContext(ctx, "roll back store transaction", "error", err)
	}
}
