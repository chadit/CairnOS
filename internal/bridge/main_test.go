package bridge_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain sweeps for goroutines the suite left behind. This package is where
// the lifecycle lives: a worker per conversation, a detached turn goroutine per
// send, and an expiry timer per proposal. A test that forgets to drain leaves
// one running, and a per-case assertion cannot see that.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
