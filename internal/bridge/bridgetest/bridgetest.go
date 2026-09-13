// Package bridgetest holds the fakes that exercise the bridge core and every
// transport adapter: a chat agent, a session-binding store, a metrics recorder,
// and a recording Outbound. They live in one importable package because the
// core's own suite and each adapter's suite drive the same core through a
// different wire, so a second copy of these fakes would be a second definition
// of what the core is supposed to do.
package bridgetest

import (
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/chat"
)

const (
	// ReplyText is the canned assistant reply Chat streams for an ordinary turn.
	ReplyText = "acknowledged"

	// DenialText is the assistant line Chat streams on a deny, mirroring
	// chat.Agent's own denial message.
	DenialText = "Workflow trigger denied; nothing was run."

	// RunID is the run Chat reports when a proposal is approved, so an assertion
	// can match the relayed run-started line.
	RunID = "run-1"

	// RunStatus is the terminal status Chat reports for RunID, so an assertion can
	// match the relayed run-finished line.
	RunStatus = "succeeded"

	// WorkflowName is the workflow Proposal names, asserted in rendered text.
	WorkflowName = "deploy"

	// Tool is the tool name Chat reports on a tool-call event.
	Tool = "shell"

	// FirstSessionID is the session Chat mints first, so a proposal raised on an
	// opening turn routes under a known id.
	FirstSessionID = "sess-1"

	// waitTimeout bounds WaitFor. The paths it covers run off real goroutines and
	// real timers, so the wait ticks off a real clock rather than synthetic time.
	waitTimeout = 2 * time.Second

	// waitTick is how often WaitFor re-checks its condition.
	waitTick = time.Millisecond
)

// Proposal builds the D9 proposal a raised turn emits and a confirm test renders.
func Proposal() *chat.Proposal {
	return &chat.Proposal{
		WorkflowName:       WorkflowName,
		Inputs:             map[string]any{"env": "prod"},
		EstimatedBudgetUSD: 1.5,
	}
}

// WaitFor polls condition until it holds or a short deadline passes, then fails
// the test. The bridge runs real goroutines behind an asynchronous Handle, so an
// assertion needs a settling point and the fakes are where one is observable.
func WaitFor(t *testing.T, condition func() bool) {
	t.Helper()

	timeout := time.NewTimer(waitTimeout)
	defer timeout.Stop()

	ticker := time.NewTicker(waitTick)
	defer ticker.Stop()

	for {
		if condition() {
			return
		}

		select {
		case <-timeout.C:
			t.Fatal("condition not met before deadline")
		case <-ticker.C:
		}
	}
}
