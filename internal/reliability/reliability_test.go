package reliability_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/reliability"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Shared fixture values; goconst applies to tests in this repo's lint policy.
const (
	codeTransient   = "test.transient"
	codeRateLimited = "test.rate_limited"
	msgBlip         = "blip"
	outputOK        = "ok"
)

// flakyCall builds a call that fails the first failures invocations with
// failErr and then succeeds, counting invocations through the returned
// pointer. It models a flaky service that recovers.
func flakyCall(failures int, failErr error) (func(context.Context) (runner.Result, error), *int) {
	var calls int

	return func(context.Context) (runner.Result, error) {
		calls++
		if calls <= failures {
			return runner.Result{}, failErr
		}

		return runner.Result{Output: outputOK}, nil
	}, &calls
}

func TestRetryNeverRetriesUnsafeFailures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		failure    error
		name       string
		idempotent bool
	}{
		{
			name:       "permanent error on idempotent action",
			idempotent: true,
			failure:    runner.NewError(runner.ClassPermanent, "test.permanent", "bad params"),
		},
		{
			name:       "transient error on non-idempotent action",
			idempotent: false,
			failure:    runner.NewError(runner.ClassTransient, codeTransient, msgBlip),
		},
		{
			name:       "plain unclassified error on idempotent action",
			idempotent: true,
			failure:    errors.New("unclassified failure"),
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			policy := reliability.Policy{MaxAttempts: 3, Backoff: reliability.BackoffConstant, BaseDelay: time.Millisecond}
			call, calls := flakyCall(99, tt.failure)

			_, attempts, err := reliability.Retry(t.Context(), policy, tt.idempotent, call)

			if !errors.Is(err, tt.failure) {
				t.Fatalf("err = %v, want %v", err, tt.failure)
			}

			if attempts != 1 || *calls != 1 {
				t.Fatalf("attempts = %d, calls = %d, want 1 each", attempts, *calls)
			}
		})
	}
}

func TestRetryTransientRetriesThenSucceeds(t *testing.T) {
	t.Parallel()

	policy := reliability.Policy{MaxAttempts: 5, Backoff: reliability.BackoffConstant, BaseDelay: time.Millisecond}
	call, calls := flakyCall(2, runner.NewError(runner.ClassTransient, codeTransient, msgBlip))

	result, attempts, err := reliability.Retry(t.Context(), policy, true, call)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}

	if result.Output != outputOK {
		t.Fatalf("output = %v, want %q", result.Output, outputOK)
	}

	if attempts != 3 || *calls != 3 {
		t.Fatalf("attempts = %d, calls = %d, want 3 each", attempts, *calls)
	}
}

func TestRetryStopsAtMaxAttempts(t *testing.T) {
	t.Parallel()

	failure := runner.NewError(runner.ClassTransient, codeTransient, "always failing")
	policy := reliability.Policy{MaxAttempts: 3, Backoff: reliability.BackoffConstant, BaseDelay: time.Millisecond}
	call, calls := flakyCall(99, failure)

	_, attempts, err := reliability.Retry(t.Context(), policy, true, call)

	if !errors.Is(err, failure) {
		t.Fatalf("err = %v, want %v", err, failure)
	}

	if attempts != 3 || *calls != 3 {
		t.Fatalf("attempts = %d, calls = %d, want 3 each", attempts, *calls)
	}
}

func TestRetryRateLimitedWaitsAtLeastRetryAfter(t *testing.T) {
	t.Parallel()

	retryAfter := 30 * time.Millisecond
	failure := &runner.Error{
		Class:      runner.ClassRateLimited,
		Code:       codeRateLimited,
		Message:    "slow down",
		RetryAfter: retryAfter,
	}
	policy := reliability.Policy{MaxAttempts: 2, Backoff: reliability.BackoffConstant, BaseDelay: time.Millisecond}
	call, _ := flakyCall(1, failure)

	start := time.Now()

	_, attempts, err := reliability.Retry(t.Context(), policy, true, call)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}

	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}

	if elapsed := time.Since(start); elapsed < retryAfter {
		t.Fatalf("elapsed = %v, want >= %v (RetryAfter must win over backoff)", elapsed, retryAfter)
	}
}

func TestRetryRateLimitedRetriesNonIdempotentAction(t *testing.T) {
	t.Parallel()

	retryAfter := 20 * time.Millisecond
	failure := &runner.Error{
		Class:      runner.ClassRateLimited,
		Code:       codeRateLimited,
		Message:    "usage limit reached",
		RetryAfter: retryAfter,
	}

	cases := []struct {
		name         string
		failures     int
		wantAttempts int
		wantFailure  bool
	}{
		{name: "recovers on the second attempt", failures: 1, wantAttempts: 2},
		{name: "stops at max attempts", failures: 99, wantAttempts: 3, wantFailure: true},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			policy := reliability.Policy{MaxAttempts: 3, Backoff: reliability.BackoffConstant, BaseDelay: time.Millisecond}
			call, calls := flakyCall(tt.failures, failure)

			start := time.Now()

			_, attempts, err := reliability.Retry(t.Context(), policy, false, call)

			if tt.wantFailure != errors.Is(err, failure) || (!tt.wantFailure && err != nil) {
				t.Fatalf("err = %v, want failure %v", err, tt.wantFailure)
			}

			if attempts != tt.wantAttempts || *calls != tt.wantAttempts {
				t.Fatalf("attempts = %d, calls = %d, want %d each", attempts, *calls, tt.wantAttempts)
			}

			if minWait := retryAfter * time.Duration(tt.wantAttempts-1); time.Since(start) < minWait {
				t.Fatalf("elapsed = %v, want >= %v (RetryAfter still applies)", time.Since(start), minWait)
			}
		})
	}
}

func TestRetryExponentialBackoffWaitsFullDelayWithoutJitter(t *testing.T) {
	t.Parallel()

	failure := runner.NewError(runner.ClassTransient, codeTransient, msgBlip)
	policy := reliability.Policy{
		MaxAttempts: 4,
		Backoff:     reliability.BackoffExponential,
		BaseDelay:   4 * time.Millisecond,
		MaxDelay:    10 * time.Millisecond,
	}
	call, _ := flakyCall(3, failure)

	start := time.Now()

	_, _, err := reliability.Retry(t.Context(), policy, true, call)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}

	// Without jitter the delays are fixed: 4ms, 8ms, then capped at 10ms.
	if elapsed := time.Since(start); elapsed < 22*time.Millisecond {
		t.Fatalf("elapsed = %v, want >= 22ms of accumulated backoff", elapsed)
	}
}

func TestRetryJitterKeepsDelaysWithinCeiling(t *testing.T) {
	t.Parallel()

	failure := runner.NewError(runner.ClassTransient, codeTransient, msgBlip)
	policy := reliability.Policy{
		MaxAttempts: 4,
		Backoff:     reliability.BackoffExponential,
		BaseDelay:   4 * time.Millisecond,
		MaxDelay:    10 * time.Millisecond,
		Jitter:      true,
	}
	call, _ := flakyCall(3, failure)

	start := time.Now()

	_, _, err := reliability.Retry(t.Context(), policy, true, call)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}

	// Full jitter draws each delay from [0, ceiling], so the three waits sum
	// to at most 4+8+10 = 22ms; the budget allows for timer overshoot.
	if elapsed := time.Since(start); elapsed > 120*time.Millisecond {
		t.Fatalf("elapsed = %v, want well under the 120ms jitter-ceiling budget", elapsed)
	}
}

func TestRetryCancellationDuringWaitReturnsPromptly(t *testing.T) {
	t.Parallel()

	failure := runner.NewError(runner.ClassTransient, codeTransient, msgBlip)
	policy := reliability.Policy{MaxAttempts: 3, Backoff: reliability.BackoffConstant, BaseDelay: 5 * time.Second}
	call, calls := flakyCall(99, failure)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()

	_, attempts, err := reliability.Retry(ctx, policy, true, call)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancellation took %v, want prompt return", elapsed)
	}

	if attempts != 1 || *calls != 1 {
		t.Fatalf("attempts = %d, calls = %d, want 1 each", attempts, *calls)
	}
}
