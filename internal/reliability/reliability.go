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
	Backoff     string // BackoffExponential or BackoffConstant
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Jitter      bool
}

// Retry runs call until it succeeds, the policy is exhausted, or the failure is
// not retryable.
//
// A Transient *runner.Error retries only on an idempotent action, a
// RateLimited one on any action (see retryable), waiting at least its
// RetryAfter hint. A plain or Permanent failure stops at once: the engine
// never repeats a side effect a runner did not declare safe.
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

// retryable applies the spec's safety property: a Transient failure runs
// again only on an idempotent action, since the call may have landed before it
// failed. RateLimited skips that gate: a usage-limit refusal means the account
// would not run the call, and a CLI wrapper picks its account per launch, so
// the retry lands on one with budget left.
func retryable(idempotent bool, err error) bool {
	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified {
		return false
	}

	if classified.Class == runner.ClassRateLimited {
		return true
	}

	return idempotent && classified.Class == runner.ClassTransient
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
