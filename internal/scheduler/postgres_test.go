package scheduler_test

import (
	"context"
	"testing"

	"github.com/chadit/CairnOS/internal/scheduler"
)

// TestPostgresCoordinatorRejectsCanceledContext proves Leader honors a canceled
// context instead of borrowing a connection, with no Postgres required. The
// live-Postgres advisory-lock tests live in tests/integration.
func TestPostgresCoordinatorRejectsCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	coord := scheduler.NewPostgresCoordinator(nil)

	if _, err := coord.Leader(ctx); err == nil {
		t.Fatal("Leader with a canceled context returned no error")
	}
}
