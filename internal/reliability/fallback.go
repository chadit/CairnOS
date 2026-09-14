package reliability

import (
	"context"
	"errors"
	"fmt"

	"github.com/chadit/CairnOS/pkg/runner"
)

// Fallback tries providers in order and returns the first success, so one
// provider's outage does not fail an action another can serve
// (.agents/spec/14-open-decisions.md, D12). call wires each provider's own
// breaker, retry, and execute.
//
// When every provider fails, the last result comes back with a joined error
// whose first classified failure wins, so a primary needing re-auth still holds
// the job.
func Fallback(ctx context.Context, providers []string, call func(context.Context, string) (runner.Result, error)) (runner.Result, error) {
	if len(providers) == 0 {
		return runner.Result{}, ErrNoProviders
	}

	var (
		lastResult runner.Result
		errs       []error
	)

	for _, provider := range providers {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return lastResult, fmt.Errorf("fallback canceled: %w", ctxErr)
		}

		result, err := call(ctx, provider)
		if err == nil {
			return result, nil
		}

		lastResult = result

		errs = append(errs, fmt.Errorf("provider %q: %w", provider, err))
	}

	return lastResult, fmt.Errorf("all fallback providers failed: %w", errors.Join(errs...))
}
