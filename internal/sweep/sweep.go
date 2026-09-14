// Package sweep runs the retention prune on a schedule
// (.agents/spec/04-execution-engine.md, "Retention and pruning"): a
// background loop in the serve process deletes runs past the window,
// truncates old outputs, and logs what each pass removed.
package sweep

import (
	"context"
	"log/slog"
	"time"

	"github.com/chadit/CairnOS/internal/store"
)

// Pruner is the slice of the store the sweeper needs; store.Store
// satisfies it.
type Pruner interface {
	Prune(ctx context.Context, opts store.PruneOptions) (store.PruneStats, error)
}

// DenylistPruner is the auth slice of the sweep: expired revocation rows
// are dead weight once their token's exp has passed
// (.agents/spec/08-auth-and-access.md, Change 1); auth.SQLite satisfies it.
type DenylistPruner interface {
	PruneDenylist(ctx context.Context, now time.Time) (int, error)
}

// Config wires one sweep loop.
type Config struct {
	// Store is pruned on every tick.
	Store Pruner

	// Denylist, when set, also has its expired revocation rows deleted on
	// every pass; nil (auth disabled) skips it.
	Denylist DenylistPruner

	// Logger receives per-pass stats; nil means slog.Default.
	Logger *slog.Logger

	// Ticks overrides the interval ticker so tests drive passes without a
	// clock; each received time anchors that pass's age math. Nil means a
	// real time.Ticker at Interval.
	Ticks <-chan time.Time

	// Interval is the wait between passes (retention.sweep); zero or
	// negative disables the loop unless Ticks supplies its own source.
	Interval time.Duration

	// Runs and Outputs are the retention windows handed to Prune.
	Runs    time.Duration
	Outputs time.Duration
}

// Run prunes on every tick until ctx cancels. It blocks, so callers start
// it on its own goroutine; cancellation is the only exit besides a
// disabled interval. A failed pass logs and waits for the next tick rather
// than stopping the loop, since retention pressure only grows.
func Run(ctx context.Context, cfg *Config) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	ticks := cfg.Ticks
	if ticks == nil {
		if cfg.Interval <= 0 {
			logger.Info("retention sweep disabled: no sweep interval configured")

			return
		}

		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()

		ticks = ticker.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticks:
			prune(ctx, cfg, logger, now)
		}
	}
}

// prune runs one pass, logging stats on success and the error otherwise.
func prune(ctx context.Context, cfg *Config, logger *slog.Logger, now time.Time) {
	stats, err := cfg.Store.Prune(ctx, store.PruneOptions{
		RunsOlderThan:    cfg.Runs,
		OutputsOlderThan: cfg.Outputs,
		Now:              now,
	})
	if err != nil {
		logger.Error("retention sweep failed", "error", err)

		return
	}

	denied := pruneDenylist(ctx, cfg, logger, now)

	logger.Info("retention sweep finished",
		"runs_deleted", stats.RunsDeleted,
		"outputs_truncated", stats.OutputsTruncated,
		"suggestions_deleted", stats.SuggestionsDeleted,
		"denylist_pruned", denied)
}

// pruneDenylist clears expired auth revocation rows when a denylist is
// wired; a failure logs and leaves the rows for the next pass, since an
// unpruned denylist row is only wasted space, never wrong.
func pruneDenylist(ctx context.Context, cfg *Config, logger *slog.Logger, now time.Time) int {
	if cfg.Denylist == nil {
		return 0
	}

	denied, err := cfg.Denylist.PruneDenylist(ctx, now)
	if err != nil {
		logger.Error("denylist prune failed", "error", err)

		return 0
	}

	return denied
}
