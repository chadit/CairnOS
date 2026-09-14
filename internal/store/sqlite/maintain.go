package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/store"
)

// interruptedCode is the canonical error code for work lost to a restart
// (.agents/spec/04-execution-engine.md, "State persistence").
const interruptedCode = "engine.interrupted"

// ReconcileInterrupted marks running actions of running runs as failed with
// engine.interrupted and finalizes those runs as failed. The engine calls it
// once at startup because CairnOS never silently resumes a runner call it
// cannot prove is still alive. It returns the number of runs reconciled.
func (s *Store) ReconcileInterrupted(ctx context.Context) (int, error) {
	interrupted, err := marshalError(cairnerr.New(interruptedCode, "run interrupted by engine restart"))
	if err != nil {
		return 0, err
	}

	now := formatTime(time.Now())

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("reconcile interrupted: begin: %w", err)
	}
	// Rollback after a successful Commit returns ErrTxDone; ignoring it keeps
	// the error path covered without complicating the happy path.
	defer rollbackQuietly(tx)

	_, err = tx.ExecContext(ctx,
		`UPDATE action_results
		 SET status = ?, error_json = ?, finished_at = ?
		 WHERE status = ? AND run_id IN (SELECT id FROM runs WHERE status = ?)`,
		string(store.ActionFailed), interrupted, now, string(store.ActionRunning), string(store.RunRunning))
	if err != nil {
		return 0, fmt.Errorf("reconcile interrupted: fail actions: %w", err)
	}

	result, err := tx.ExecContext(ctx,
		`UPDATE runs
		 SET status = ?, error_json = ?, finished_at = ?
		 WHERE status = ?`,
		string(store.RunFailed), interrupted, now, string(store.RunRunning))
	if err != nil {
		return 0, fmt.Errorf("reconcile interrupted: finalize runs: %w", err)
	}

	reconciled, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reconcile interrupted: rows affected: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("reconcile interrupted: commit: %w", err)
	}

	return int(reconciled), nil
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("prune runs: begin: %w", err)
	}
	defer rollbackQuietly(tx)

	encodedCutoff := formatTime(cutoff)

	_, err = tx.ExecContext(ctx,
		"DELETE FROM agent_steps WHERE run_id IN (SELECT id FROM runs WHERE created_at < ?)", encodedCutoff)
	if err != nil {
		return 0, 0, fmt.Errorf("prune runs: delete agent steps: %w", err)
	}

	_, err = tx.ExecContext(ctx,
		"DELETE FROM action_results WHERE run_id IN (SELECT id FROM runs WHERE created_at < ?)", encodedCutoff)
	if err != nil {
		return 0, 0, fmt.Errorf("prune runs: delete action results: %w", err)
	}

	suggestionResult, err := tx.ExecContext(ctx,
		"DELETE FROM suggestions WHERE created_at < ?", encodedCutoff)
	if err != nil {
		return 0, 0, fmt.Errorf("prune runs: delete stale suggestions: %w", err)
	}

	staleSuggestions, err := suggestionResult.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("prune runs: suggestion rows affected: %w", err)
	}

	result, err := tx.ExecContext(ctx, "DELETE FROM runs WHERE created_at < ?", encodedCutoff)
	if err != nil {
		return 0, 0, fmt.Errorf("prune runs: delete runs: %w", err)
	}

	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("prune runs: rows affected: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("prune runs: commit: %w", err)
	}

	return int(deleted), int(staleSuggestions), nil
}

// truncateOutputsBefore nulls out output_json for actions and agent steps
// whose run was created before cutoff, keeping the rest of each row so run
// metadata and transcript shape stay reportable after large outputs drop.
func (s *Store) truncateOutputsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	encodedCutoff := formatTime(cutoff)

	actionResult, err := s.db.ExecContext(ctx,
		`UPDATE action_results
		 SET output_json = NULL
		 WHERE output_json IS NOT NULL
		   AND run_id IN (SELECT id FROM runs WHERE created_at < ?)`,
		encodedCutoff)
	if err != nil {
		return 0, fmt.Errorf("prune outputs: %w", err)
	}

	truncated, err := actionResult.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune outputs: rows affected: %w", err)
	}

	stepResult, err := s.db.ExecContext(ctx,
		`UPDATE agent_steps
		 SET output_json = NULL
		 WHERE output_json IS NOT NULL
		   AND run_id IN (SELECT id FROM runs WHERE created_at < ?)`,
		encodedCutoff)
	if err != nil {
		return 0, fmt.Errorf("prune agent step outputs: %w", err)
	}

	stepsTruncated, err := stepResult.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune agent step outputs: rows affected: %w", err)
	}

	return int(truncated + stepsTruncated), nil
}

// rollbackQuietly abandons tx; after a successful Commit this is a no-op
// that returns ErrTxDone, which carries no signal worth surfacing.
func rollbackQuietly(tx *sql.Tx) {
	_ = tx.Rollback()
}
