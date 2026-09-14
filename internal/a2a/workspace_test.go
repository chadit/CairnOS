package a2a_test

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	a2av1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/a2a/v1"
	"github.com/chadit/CairnOS/internal/auth"
)

// workspace ids the cross-tenant A2A tests run under.
const (
	tenantA = "tenant-a"
	tenantB = "tenant-b"
)

// TestTaskUpdatesRefusesCrossTenant proves the run manager carries the
// workspace into its watched-handle map: tenant B cannot attach to tenant A's
// live task stream by id. TaskUpdates on a foreign task reads as NotFound (the
// store's own answer once the live handle is refused, no oracle), while tenant
// A's own TaskUpdates streams the task to completion.
func TestTaskUpdatesRefusesCrossTenant(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{authEnabled: true})

	tokenA := fix.tokenInWorkspace(t, tenantA, auth.ScopeA2ATask)
	tokenB := fix.tokenInWorkspace(t, tenantB, auth.ScopeA2ATask)

	// Tenant A creates a long-lived task, so the run is still active in the
	// manager's watched-handle map when tenant B tries to attach.
	created, err := fix.a2a.CreateTask(authedCtx(t, tokenA), &a2av1.CreateTaskRequest{Skill: slowSkill})
	if err != nil {
		t.Fatalf("tenant A CreateTask: %v", err)
	}

	taskID := created.GetTask().GetTaskId()

	// Tenant B cannot attach to tenant A's task stream: the live handle is
	// refused for the foreign workspace, and the store snapshot fallback answers
	// NotFound under tenant B's workspace.
	stream, err := fix.a2a.TaskUpdates(authedCtx(t, tokenB), &a2av1.TaskUpdatesRequest{TaskId: taskID})
	if err != nil {
		t.Fatalf("tenant B TaskUpdates open: %v", err)
	}

	if _, recvErr := stream.Recv(); status.Code(recvErr) != codes.NotFound {
		t.Fatalf("tenant B TaskUpdates(A's task) code = %v, want NotFound", status.Code(recvErr))
	}

	// Tenant A stops its own run, then streams its own task to the terminal
	// update, proving same-tenant attach still works.
	if _, cancelTaskErr := fix.a2a.CancelTask(authedCtx(t, tokenA), &a2av1.CancelTaskRequest{TaskId: taskID}); cancelTaskErr != nil {
		t.Fatalf("tenant A CancelTask(own task): %v", cancelTaskErr)
	}

	ownStream, err := fix.a2a.TaskUpdates(authedCtx(t, tokenA), &a2av1.TaskUpdatesRequest{TaskId: taskID})
	if err != nil {
		t.Fatalf("tenant A TaskUpdates: %v", err)
	}

	updates := collectUpdates(t, ownStream)
	if len(updates) == 0 {
		t.Fatal("tenant A TaskUpdates streamed no updates for its own task")
	}
}
