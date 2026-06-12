package chat_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/pkg/runner"
)

// workspace ids the cross-tenant chat-confirm test runs under.
const (
	tenantA = "tenant-a"
	tenantB = "tenant-b"
)

// driveCtx runs one streaming chat call under an explicit context, collecting
// its events while the call streams. It mirrors the package's drive helper but
// lets the caller pass a workspace-scoped context, which is what the
// cross-tenant test turns on.
func driveCtx(t *testing.T, call func(out chan<- chat.Event) error) error {
	t.Helper()

	events := make(chan chat.Event, driveEvents)

	var group sync.WaitGroup

	group.Go(func() {
		collectEvents(events)
	})

	callErr := call(events)

	group.Wait()

	return callErr
}

// TestConfirmActionRefusesCrossTenant proves the chat confirm gate is workspace
// scoped: tenant A stages a workflow proposal on its session, and tenant B
// cannot confirm it by guessing the session id. The cross-tenant confirm reads
// as not-found and fires nothing; tenant A's own confirm fires the run.
func TestConfirmActionRefusesCrossTenant(t *testing.T) {
	t.Parallel()

	driver := &scriptedDriver{turns: []runner.Result{
		runWorkflowTurn(helloWorkflow, map[string]any{}),
	}}
	agent, _, trigger := newAgent(t, driver, 0)

	ctxA := store.ContextWithWorkspace(t.Context(), tenantA)
	ctxB := store.ContextWithWorkspace(t.Context(), tenantB)

	// Tenant A opens a session and stages a proposal on it.
	session, err := agent.CreateSession(ctxA, "")
	if err != nil {
		t.Fatalf("tenant A CreateSession: %v", err)
	}

	sessionID := session.ID

	if err := driveCtx(t, func(out chan<- chat.Event) error {
		return agent.SendMessage(ctxA, sessionID, "run the hello workflow", out)
	}); err != nil {
		t.Fatalf("tenant A SendMessage: %v", err)
	}

	// Tenant B confirms tenant A's session by id: the workspace-scoped session
	// load fails first, so confirm reads as not-found and nothing fires.
	confirmErr := driveCtx(t, func(out chan<- chat.Event) error {
		return agent.ConfirmAction(ctxB, sessionID, true, out)
	})
	if !errors.Is(confirmErr, store.ErrChatSessionNotFound) {
		t.Fatalf("tenant B ConfirmAction error = %v, want ErrChatSessionNotFound", confirmErr)
	}

	if trigger.runs() != 0 {
		t.Fatalf("trigger fired %d runs on a cross-tenant confirm, want 0", trigger.runs())
	}

	// Tenant A confirms its own session: the run fires.
	if err := driveCtx(t, func(out chan<- chat.Event) error {
		return agent.ConfirmAction(ctxA, sessionID, true, out)
	}); err != nil {
		t.Fatalf("tenant A ConfirmAction(own session): %v", err)
	}

	if trigger.runs() != 1 {
		t.Fatalf("trigger fired %d runs after tenant A confirmed, want 1", trigger.runs())
	}
}
