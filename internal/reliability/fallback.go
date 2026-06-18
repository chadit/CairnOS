package reliability

import (
	"context"
	"errors"
	"fmt"

	"github.com/chadit/CairnOS/pkg/runner"
)

// Fallback tries providers in order and returns the first success. Each provider
// runs through call, which the engine wires to that provider's own breaker,
// retry, and execute, so a provider that fails terminally (a dead service, an
// open breaker, a rejected request, or an expired credential) falls through to
// the next while an alternative remains. This is the mixed-model layer: many
// providers in one install, tried in order, so one provider's outage does not
// fail an action another can serve (.agents/spec/14-open-decisions.md, D12).
//
// When every provider fails, it returns the last attempt's result and a joined
// error of all of them. The engine classifies that joined error as usual: the
// first classified failure in the chain wins, so a primary that needs re-auth
// still holds the job once no fallback could serve it. An empty chain returns
// ErrNoProviders. A ctx cancellation between providers stops the loop with the
// ctx error rather than marching through the rest of the chain.
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
