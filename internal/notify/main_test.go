package notify_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain sweeps for goroutines the suite left behind. The fan-out drainer runs
// on one, so a Run that never returns shows up here rather than as a slow test.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
