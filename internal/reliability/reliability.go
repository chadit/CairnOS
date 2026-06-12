// Package reliability implements the retry, backoff, and circuit-breaker
// machinery the execution engine wraps around runner calls
// (.agents/spec/04-execution-engine.md, "Retry and backoff" and "Circuit
// breaker"). It deliberately does not import internal/workflow: the engine
// maps workflow.RetryPolicy onto Policy, keeping this package free of the
// schema types so other callers can produce policies too.
package reliability

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/chadit/CairnOS/pkg/runner"
)

// Backoff strategies a Policy can name.
const (
	// BackoffExponential doubles the delay ceiling on every attempt, capped
	// at MaxDelay.
	BackoffExponential = "exponential"
	// BackoffConstant waits BaseDelay between every attempt.
	BackoffConstant = "constant"
)

// Policy mirrors the per-action retry table from the workflow schema.
// MaxAttempts counts total tries including the first; values below one are
// treated as one, so a zero policy still runs the call once.
type Policy struct {
	MaxAttempts int
	Backoff     string // BackoffExponential or BackoffConstant
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Jitter      bool
}

// Retry runs call until it succeeds, the policy is exhausted, or the failure
// is not retryable. It returns the last result, the attempts used, and the
// final error (nil on success).
//
// Retrying happens only when the action is idempotent and the failure is a
// *runner.Error of class Transient or RateLimited. A plain error or a
// Permanent class stops immediately: the engine must never repeat a side
// effect a runner did not declare safe. RateLimited failures wait at least
// their RetryAfter hint. Canceling ctx during a wait stops the loop with
// the ctx error.
func Retry(ctx context.Context, policy Policy, idempotent bool, call func(context.Context) (runner.Result, error)) (runner.Result, int, error) {
	maxAttempts := max(policy.MaxAttempts, 1)

	for attempt := 1; ; attempt++ {
		result, err := call(ctx)
		if err == nil {
			return result, attempt, nil
		}

		if attempt == maxAttempts || !retryable(idempotent, err) {
			return result, attempt, err
		}

		// delayFor takes the zero-based attempt index so the first wait uses
		// the spec's base_delay * 2^0.
		if waitErr := wait(ctx, delayFor(policy, attempt-1, err)); waitErr != nil {
			return result, attempt, waitErr
		}
	}
}

// retryable applies the spec's safety property: only an idempotent action
// with a classified Transient or RateLimited failure may run again.
func retryable(idempotent bool, err error) bool {
	if !idempotent {
		return false
	}

	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified {
		return false
	}

	return classified.Class == runner.ClassTransient || classified.Class == runner.ClassRateLimited
}

// delayFor picks the wait before the next try, preferring a rate-limited
// service's own RetryAfter hint when it exceeds the computed backoff.
func delayFor(policy Policy, attempt int, err error) time.Duration {
	delay := backoffDelay(policy, attempt)

	classified, isClassified := errors.AsType[*runner.Error](err)
	if isClassified && classified.Class == runner.ClassRateLimited && classified.RetryAfter > delay {
		delay = classified.RetryAfter
	}

	return delay
}

// backoffDelay computes the policy's delay for a zero-based attempt index.
// Jitter applies to both strategies: full jitter draws uniformly from
// [0, ceiling] to spread retry storms across time.
func backoffDelay(policy Policy, attempt int) time.Duration {
	ceiling := policy.BaseDelay
	if policy.Backoff == BackoffExponential {
		ceiling = exponentialCeiling(policy.BaseDelay, policy.MaxDelay, attempt)
	}

	if policy.Jitter {
		return jittered(ceiling)
	}

	return ceiling
}

// exponentialCeiling computes min(maxDelay, base * 2^attempt), saturating at
// maxDelay instead of overflowing on deep attempt counts. A non-positive
// maxDelay means uncapped.
func exponentialCeiling(base, maxDelay time.Duration, attempt int) time.Duration {
	if base <= 0 {
		return 0
	}

	ceiling := base

	for range attempt {
		ceiling <<= 1 // double, per the spec's base * 2^attempt; the shift keeps overflow detectable below
		if ceiling <= 0 || (maxDelay > 0 && ceiling >= maxDelay) {
			return maxDelay
		}
	}

	if maxDelay > 0 && ceiling > maxDelay {
		return maxDelay
	}

	return ceiling
}

// jittered draws a uniform delay in [0, ceiling].
func jittered(ceiling time.Duration) time.Duration {
	if ceiling <= 0 {
		return 0
	}

	// #nosec G404 -- jitter randomness is not security-sensitive
	return time.Duration(rand.Int64N(int64(ceiling) + 1))
}

// wait blocks for delay, returning early with a wrapped ctx error when the
// run is canceled mid-backoff.
func wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("retry wait canceled: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}
