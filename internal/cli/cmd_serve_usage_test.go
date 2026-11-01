//go:build !windows

package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/cli"
)

// usageScriptRun is what the fake report script leaves on the FIFO.
type usageScriptRun struct {
	err  error
	data []byte
}

func TestServeRunsTheUsageReportScriptOnStart(t *testing.T) {
	files := t.TempDir()
	signal := filepath.Join(files, "ran")

	// A FIFO rather than a file: opening it to read blocks until the script
	// opens it to write, so the test waits on the run itself and not a clock.
	if err := syscall.Mkfifo(signal, 0o600); err != nil {
		t.Fatalf("make fifo: %v", err)
	}

	script := filepath.Join(files, "usage-report")

	// #nosec G306 -- the script must be executable; it lives in t.TempDir.
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'ran\\n' > \"$(dirname \"$0\")/ran\"\n"), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}

	block := "\n[runner.usage]\nscript = [\"" + script + "\"]\noutput = \"" + filepath.Join(files, "usage.json") +
		"\"\ninterval = 900\n"
	dir := bridgeServeDir(t, serveConfigTOML+block)

	t.Parallel()

	stdout, _, exit, cancel := serveUntilReady(t, dir, nil)

	ran := make(chan usageScriptRun, oneShotChanSize)

	go func() {
		data, err := os.ReadFile(signal)
		ran <- usageScriptRun{data: data, err: err}
	}()

	select {
	case run := <-ran:
		if run.err != nil || string(run.data) != "ran\n" {
			t.Fatalf("script signal = %q, %v; want ran", run.data, run.err)
		}
	case code := <-exit:
		t.Fatalf("serve exited %d before running the usage script", code)
	case <-time.After(time.Minute):
		cancel()
		t.Fatalf("serve never ran the usage script:\n%s", stdout.String())
	}

	cancel()

	if code := <-exit; code != cli.ExitOK {
		t.Fatalf("serve exit = %d, want %d", code, cli.ExitOK)
	}

	if !strings.Contains(stdout.String(), "usage report script running every 15m0s") {
		t.Fatalf("stdout missing the usage refresh banner:\n%s", stdout.String())
	}
}
