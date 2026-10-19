//go:build unix

package workflow_test

import (
	"path/filepath"
	"syscall"
	"testing"

	"github.com/chadit/CairnOS/internal/workflow"
)

// A FIFO has no writer at load time, so a blocking open would hang the load
// forever. The include has to be refused as not a regular file instead.
func TestLoadStdinFileRefusesFIFOWithoutBlocking(t *testing.T) {
	t.Parallel()

	path := stageWorkflow(t, includeFlow(promptParam), nil)

	if err := syscall.Mkfifo(filepath.Join(filepath.Dir(path), promptFile), 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	_, err := workflow.Load(path)
	if err == nil {
		t.Fatal("load succeeded, want a bad_include error")
	}

	if got, want := badIncludeMessage(t, err), promptParam+" is not a regular file"; got != want {
		t.Fatalf("problem = %q\nwant      %q", got, want)
	}
}
