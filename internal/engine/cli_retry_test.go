package engine_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/shellquote"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/pkg/runner"
)

// cliRetryTOML is one claude action with room for retries. The %s slot takes
// an idempotent line or nothing, since the retry gate reads that flag.
const cliRetryTOML = `
[workflow]
name = "cli_retry"

[action.ask]
runner = "claude"
%s
params = { prompt = "summarize the queue" }

[action.ask.retry]
max_attempts = 3
backoff = "constant"
base_delay = "1ms"
`

// writeRefusingClaude writes a claude stand-in that prints refusal on stdout
// and exits 1 on its first launch, then answers on every launch after, the way
// a pick that moved off a capped account would. Call it before t.Parallel: a
// fork racing the open write fd fails with ETXTBSY (Go issue 22315).
func writeRefusingClaude(t *testing.T, refusal string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
marker="$(dirname "$0")/refused"
if [ -e "$marker" ]; then
	echo "queue summary"
	exit 0
fi
: > "$marker"
printf '%s\n' ` + shellquote.Word(refusal) + `
exit 1
`

	// #nosec G306 -- the stand-in must be executable; it lives in t.TempDir.
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write claude stand-in: %v", err)
	}

	return path
}

func TestCLIFailureRetriesByClass(t *testing.T) {
	cases := []struct {
		name        string
		idempotent  string
		binary      string
		wantRun     store.RunStatus
		wantAction  store.ActionStatus
		wantCode    string
		wantAttempt int
	}{
		{
			// A capped account never ran the call, so even an action not marked
			// idempotent goes again, and the retry's pick lands elsewhere.
			name:        "limit refusal on a non-idempotent action retries and succeeds",
			binary:      writeRefusingClaude(t, "You've hit your session limit · resets 3pm (America/Denver)"),
			wantRun:     store.RunSucceeded,
			wantAction:  store.ActionSucceeded,
			wantAttempt: 2,
		},
		{
			name:        "transient overload on a non-idempotent action is not retried",
			binary:      writeRefusingClaude(t, `API Error: 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`),
			wantRun:     store.RunFailed,
			wantAction:  store.ActionFailed,
			wantCode:    engine.CodeRunnerTransient,
			wantAttempt: 1,
		},
		{
			// The second launch would succeed and the action allows retries, so
			// only the classification keeps it at one attempt.
			name:        "unexplained exit on an idempotent action is not retried",
			idempotent:  "idempotent = true",
			binary:      writeRefusingClaude(t, "compilation failed: 3 errors"),
			wantRun:     store.RunFailed,
			wantAction:  store.ActionFailed,
			wantCode:    engine.CodeRunnerPermanent,
			wantAttempt: 1,
		},
	}

	t.Parallel()

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			eng, db := newEngineWithRunners(t, baseConfig(), map[string]runner.Factory{
				builtins.ClaudeName: builtins.CLIFactory(builtins.ClaudeName, builtins.CLITool{Binary: tt.binary}, nil, nil),
			})

			run := mustRun(t, eng, parseFlow(t, fmt.Sprintf(cliRetryTOML, tt.idempotent)), nil)

			wantRunStatus(t, run, tt.wantRun)

			result := wantActionStatus(t, db, run.ID, "ask", tt.wantAction)

			if result.Attempt != tt.wantAttempt {
				t.Fatalf("attempt = %d, want %d", result.Attempt, tt.wantAttempt)
			}

			if tt.wantCode != "" {
				wantErrCode(t, result.Error, tt.wantCode)
			}
		})
	}
}
