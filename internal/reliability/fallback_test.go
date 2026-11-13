package reliability_test

import (
	"context"
	"errors"
	"testing"

	"github.com/chadit/CairnOS/internal/reliability"
	"github.com/chadit/CairnOS/pkg/runner"
)

func TestFallbackReturnsFirstSuccess(t *testing.T) {
	t.Parallel()

	var tried []string

	result, err := reliability.Fallback(t.Context(), []string{"a", "b"},
		func(_ context.Context, provider string) (runner.Result, error) {
			tried = append(tried, provider)

			return runner.Result{Output: provider}, nil
		})
	if err != nil {
		t.Fatalf("Fallback: %v", err)
	}

	if result.Output != "a" {
		t.Errorf("output = %v, want a", result.Output)
	}

	if len(tried) != 1 || tried[0] != "a" {
		t.Errorf("tried = %v, want only the first provider", tried)
	}
}

func TestFallbackFallsThroughToNext(t *testing.T) {
	t.Parallel()

	var tried []string

	result, err := reliability.Fallback(t.Context(), []string{"a", "b", "c"},
		func(_ context.Context, provider string) (runner.Result, error) {
			tried = append(tried, provider)

			if provider == "a" {
				return runner.Result{}, errors.New("a is down")
			}

			return runner.Result{Output: provider}, nil
		})
	if err != nil {
		t.Fatalf("Fallback: %v", err)
	}

	if result.Output != "b" {
		t.Errorf("output = %v, want b (the first healthy provider)", result.Output)
	}

	if len(tried) != 2 {
		t.Errorf("tried = %v, want the loop to stop after b", tried)
	}
}

func TestFallbackJoinsErrorsAndPreservesClass(t *testing.T) {
	t.Parallel()

	const reauthProvider = "codex"

	// The primary needs re-auth and the fallback is down. The joined error must
	// still classify as re-auth so the engine holds the job once nothing served
	// it, the mixed-model and hold paths composing correctly.
	reauthErr := runner.NewReauthError("codex.needs_reauth", "run: codex login")

	_, err := reliability.Fallback(t.Context(), []string{reauthProvider, "qwen"},
		func(_ context.Context, provider string) (runner.Result, error) {
			if provider == reauthProvider {
				return runner.Result{}, reauthErr
			}

			return runner.Result{}, errors.New("qwen is down")
		})
	if err == nil {
		t.Fatal("want an error when every provider fails")
	}

	if !runner.IsReauth(err) {
		t.Errorf("joined error should still classify as re-auth, got %v", err)
	}
}

func TestFallbackEmptyChainIsWiringError(t *testing.T) {
	t.Parallel()

	_, err := reliability.Fallback(t.Context(), nil,
		func(context.Context, string) (runner.Result, error) {
			t.Fatal("call must not run on an empty chain")

			return runner.Result{}, nil
		})
	if !errors.Is(err, reliability.ErrNoProviders) {
		t.Fatalf("err = %v, want ErrNoProviders", err)
	}
}

func TestFallbackStopsOnCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var calls int

	_, err := reliability.Fallback(ctx, []string{"a", "b"},
		func(context.Context, string) (runner.Result, error) {
			calls++

			return runner.Result{}, errors.New("unreached")
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	if calls != 0 {
		t.Errorf("calls = %d, want 0 when ctx is already canceled", calls)
	}
}
