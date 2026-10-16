package builtins_test

import (
	"io"
	"testing"

	"go.uber.org/goleak"

	"github.com/chadit/CairnOS/internal/environ"
)

// keyEnv answers one API-key variable and nothing else. Handing a runner its
// credential this way is what lets these cases run in parallel: setting one in
// the process would be visible to every other case at once.
func keyEnv(name, value string) environ.Lookup {
	return environ.Map(map[string]string{name: value})
}

// writeBody sends a canned response from a test handler. A failed write goes
// through Errorf, not Fatalf, because handlers on a real loopback listener run
// off the test goroutine, where FailNow is not allowed.
func writeBody(t *testing.T, w io.Writer, body string) {
	t.Helper()

	if _, err := io.WriteString(w, body); err != nil {
		t.Errorf("write response body: %v", err)
	}
}

// TestMain sweeps for goroutines the suite left behind. The source runner holds
// a per-spec mutex across a git call, so a caller that never releases it shows up
// here rather than as a suite that quietly slows down.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
