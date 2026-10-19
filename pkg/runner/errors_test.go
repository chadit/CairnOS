package runner_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/chadit/CairnOS/pkg/runner"
)

func TestNewReauthErrorClassifiesAsNeedsReauth(t *testing.T) {
	t.Parallel()

	err := runner.NewReauthError("codex.needs_reauth", "run: codex login")

	if err.Class != runner.ClassNeedsReauth {
		t.Errorf("class = %v, want ClassNeedsReauth", err.Class)
	}

	if !runner.IsReauth(err) {
		t.Error("IsReauth should report true for a NeedsReauth error")
	}
}

func TestIsReauthSeesThroughWrappingAndRejectsOthers(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("action %q: %w", "plan", runner.NewReauthError("qwen.needs_reauth", "run: qwen login"))
	if !runner.IsReauth(wrapped) {
		t.Error("IsReauth should unwrap to find a wrapped NeedsReauth error")
	}

	if runner.IsReauth(runner.NewError(runner.ClassPermanent, "p", "bad params")) {
		t.Error("a permanent error must not read as needs-reauth")
	}

	if runner.IsReauth(errors.New("plain")) {
		t.Error("a plain error must not read as needs-reauth")
	}
}

func TestWrapfBuildsAClassifiedErrorWithItsCause(t *testing.T) {
	t.Parallel()

	cause := fmt.Errorf("dial: %w", context.DeadlineExceeded)

	cases := []struct {
		err       *runner.Error
		wantCause error
		name      string
		wantText  string
		wantClass runner.ErrorClass
	}{
		{
			name:      "nil cause leaves nothing to unwrap",
			err:       runner.Wrapf(nil, runner.ClassPermanent, "demo.bad_limit", "limit %d is over %d", 9, 5),
			wantText:  "limit 9 is over 5",
			wantClass: runner.ClassPermanent,
		},
		{
			name:      "cause stays out of the message and in the chain",
			err:       runner.Wrapf(cause, runner.ClassTransient, "demo.timeout", "fetch %s timed out", "report"),
			wantCause: context.DeadlineExceeded,
			wantText:  "fetch report timed out",
			wantClass: runner.ClassTransient,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if testCase.err.Error() != testCase.wantText || testCase.err.Class != testCase.wantClass {
				t.Fatalf("got %q class %v, want %q class %v",
					testCase.err.Error(), testCase.err.Class, testCase.wantText, testCase.wantClass)
			}

			if testCase.wantCause == nil && testCase.err.Unwrap() != nil {
				t.Fatalf("cause = %v, want none", testCase.err.Unwrap())
			}

			if testCase.wantCause != nil && !errors.Is(testCase.err, testCase.wantCause) {
				t.Fatalf("errors.Is(%v, %v) = false", testCase.err, testCase.wantCause)
			}

			// Wrapping the runner error again must still expose its code.
			outer := fmt.Errorf("action report: %w", testCase.err)
			if found, ok := errors.AsType[*runner.Error](outer); !ok || found.Code != testCase.err.Code {
				t.Fatalf("errors.As lost the runner error through a wrap")
			}
		})
	}
}
