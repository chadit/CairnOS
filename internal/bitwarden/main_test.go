package bitwarden_test

import (
	"os"
	"testing"

	"go.uber.org/goleak"
)

// TestMain has two jobs. Launched under the fake's name, this process is a child
// the client started through the symlink, so it runs the fake and exits before
// the suite would start again. Otherwise it sweeps for leaked goroutines, which
// this package earns by starting and killing a subprocess in every case.
func TestMain(m *testing.M) {
	if isFakeInvocation() {
		os.Exit(runFakeBW(os.Args[1:]))
	}

	goleak.VerifyTestMain(m)
}
