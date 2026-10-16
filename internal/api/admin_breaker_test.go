package api_test

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/reliability"
)

// TestResetBreakerFlipsOpenBreakerClosed trips the mock runner's breaker
// open, resets it over the admin API, and checks the breaker really moved
// to CLOSED, not just the response text.
func TestResetBreakerFlipsOpenBreakerClosed(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	breaker := fix.breakers.Get("mock")
	breaker.Record(false)

	if state := breaker.State(); state != reliability.StateOpen {
		t.Fatalf("breaker state after one failure = %v, want OPEN", state)
	}

	resp, err := fix.admin.ResetBreaker(t.Context(), &cairnosv1.ResetBreakerRequest{Runner: runnerMock})
	if err != nil {
		t.Fatalf("ResetBreaker: %v", err)
	}

	if resp.GetRunner() != runnerMock || resp.GetState() != "CLOSED" {
		t.Fatalf("ResetBreaker response = %q/%q, want mock/CLOSED", resp.GetRunner(), resp.GetState())
	}

	if state := breaker.State(); state != reliability.StateClosed {
		t.Fatalf("breaker state after reset = %v, want CLOSED", state)
	}
}

// TestResetBreakerUnknownRunnerAnswersNotFound proves a runner that never
// produced a breaker maps to NOT_FOUND rather than a fake success.
func TestResetBreakerUnknownRunnerAnswersNotFound(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	_, err := fix.admin.ResetBreaker(t.Context(), &cairnosv1.ResetBreakerRequest{Runner: unknownName})
	if err == nil {
		t.Fatal("ResetBreaker(ghost) succeeded, want NOT_FOUND")
	}

	if got := status.Code(err); got != codes.NotFound {
		t.Fatalf("ResetBreaker(ghost) code = %v, want %v (err: %v)", got, codes.NotFound, err)
	}
}
