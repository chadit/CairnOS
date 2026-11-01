package engine

import (
	"context"
	"sync"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// detailItemIndex names which element failed a foreach, stamped on the
// action's error so history points at the offending item.
const detailItemIndex = "item_index"

// foreachAttempt fans a task action over a list, running one attempt per
// element and collapsing the results into a single action outcome. The whole
// fan-out rides the action's one slot of the run semaphore, so a local
// semaphore of size max_parallel is what bounds the elements.
func (e *Engine) foreachAttempt(ctx context.Context, runID string, action *workflow.Action, inputs, outputs map[string]any) attemptOutcome {
	items, cerr := e.resolveForeachItems(ctx, action, inputs, outputs)
	if cerr != nil {
		return attemptOutcome{cerr: cerr}
	}

	if len(items) == 0 {
		// An empty list is a clean success with an empty output; no runner runs.
		return attemptOutcome{result: runner.Result{Output: []any{}}, attempts: 1}
	}

	return e.runForeachItems(ctx, runID, action, inputs, outputs, items)
}

// resolveForeachItems resolves the items source to a list before any element
// runs. A source that resolves to a non-list fails the action as a
// precondition, so no runner is called for a misshapen list.
func (e *Engine) resolveForeachItems(ctx context.Context, action *workflow.Action, inputs, outputs map[string]any) ([]any, *cairnerr.Error) {
	raw, err := resolveValue(action.Foreach.Items, e.resolver(ctx, inputs, outputs))
	if err != nil {
		return nil, asPrecondition(err)
	}

	list, isList := raw.([]any)
	if !isList {
		return nil, errf(CodePreconditionFailed,
			"action %q foreach items resolved to %T, want a list", action.Name, raw)
	}

	return list, nil
}

// runForeachItems runs every element under a max_parallel bound, collecting
// ordered outputs and stopping at the first failure. The launch loop takes a
// semaphore slot in index order before spawning, so elements start in order and
// at most max_parallel run at once. The first failure cancels itemCtx, which
// unblocks the launch loop and skips whatever has not started.
func (e *Engine) runForeachItems(ctx context.Context, runID string, action *workflow.Action, inputs, outputs map[string]any, items []any) attemptOutcome {
	itemCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	maxParallel := max(action.Foreach.MaxParallel, 1)

	state := &foreachState{outputs: make([]any, len(items))}
	sem := make(chan struct{}, maxParallel)

	var wg sync.WaitGroup

launch:
	for idx := range items {
		select {
		case sem <- struct{}{}:
		case <-itemCtx.Done():
			break launch
		}

		wg.Go(func() {
			defer func() { <-sem }()

			if itemCtx.Err() != nil {
				return
			}

			outcome := e.attemptItem(itemCtx, runID, action, inputs, outputs, items[idx], idx)
			state.record(idx, &outcome, cancel)
		})
	}

	wg.Wait()

	return state.outcome()
}

// attemptItem runs one foreach element through the task pipeline with item and
// index bound over the resolver, so per-element retry, breaker, and fallback
// all come from the shared runner path.
func (e *Engine) attemptItem(ctx context.Context, runID string, action *workflow.Action, inputs, outputs map[string]any, item any, index int) attemptOutcome {
	params, err := resolveParamsWith(action.Params, e.foreachResolver(ctx, inputs, outputs, item, index))
	if err != nil {
		return attemptOutcome{cerr: asPrecondition(err)}
	}

	return e.runResolvedParams(ctx, runID, action, inputs, params)
}

// foreachState gathers the per-element outputs and the first failure across
// the fan-out goroutines. The mutex guards every field because elements run
// concurrently.
type foreachState struct {
	firstErr *cairnerr.Error
	outputs  []any
	usage    runner.Usage
	failAt   int
	attempts int
	mu       sync.Mutex
}

// record folds one element's outcome into the shared state. The first failure
// wins, stamps its element index, and cancels the rest of the fan-out so the
// remaining elements stop instead of running work that will be discarded.
func (s *foreachState) record(idx int, outcome *attemptOutcome, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if outcome.cerr != nil {
		if s.firstErr == nil {
			s.firstErr = outcome.cerr
			s.failAt = idx
			s.attempts = outcome.attempts

			cancel()
		}

		return
	}

	s.outputs[idx] = outcome.result.Output
	addUsage(&s.usage, outcome.result.Usage)
}

// outcome collapses the fan-out into one action outcome: the ordered output
// list on success, or the first failure carrying its element index. Called
// after every goroutine has settled, so it reads the fields without the lock.
func (s *foreachState) outcome() attemptOutcome {
	if s.firstErr != nil {
		return attemptOutcome{
			result:   runner.Result{Usage: s.usage},
			attempts: s.attempts,
			cerr:     s.firstErr.WithDetail(detailItemIndex, s.failAt),
		}
	}

	return attemptOutcome{result: runner.Result{Output: s.outputs, Usage: s.usage}, attempts: 1}
}

// addUsage sums one element's usage into the running total so the single
// action row reports the whole fan-out's spend.
func addUsage(total *runner.Usage, add runner.Usage) {
	total.InputTokens += add.InputTokens
	total.OutputTokens += add.OutputTokens
	total.CachedTokens += add.CachedTokens
	total.CostUSD += add.CostUSD
	total.Latency += add.Latency
	total.Retries += add.Retries
}
