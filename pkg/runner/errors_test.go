package runner_test

import (
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
