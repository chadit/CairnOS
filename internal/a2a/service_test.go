package a2a_test

import (
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	a2av1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/a2a/v1"
)

// clientTaskID is the caller-chosen task id CreateTask must echo back.
const clientTaskID = "client-123"

func TestCreateTaskStartsRun(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{})

	resp, err := fix.a2a.CreateTask(t.Context(), &a2av1.CreateTaskRequest{
		Skill:        helloSkill,
		ClientTaskId: clientTaskID,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	task := resp.GetTask()
	if task.GetTaskId() == "" {
		t.Fatal("CreateTask returned no task id")
	}

	if task.GetSkill() != helloSkill {
		t.Fatalf("task skill = %q, want %q", task.GetSkill(), helloSkill)
	}

	if resp.GetClientTaskId() != clientTaskID {
		t.Fatalf("client task id = %q, want it echoed back", resp.GetClientTaskId())
	}
}

func TestTaskUpdatesStreamsToCompletion(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{})

	// stream, not hello: its first action holds the run open so this consumer
	// attaches to the live handle. Against hello the run can finish between the
	// two calls, and the terminal-record fallback carries no per-action steps.
	created, err := fix.a2a.CreateTask(t.Context(), &a2av1.CreateTaskRequest{Skill: streamSkill})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	taskID := created.GetTask().GetTaskId()

	stream, err := fix.a2a.TaskUpdates(t.Context(), &a2av1.TaskUpdatesRequest{TaskId: taskID})
	if err != nil {
		t.Fatalf("TaskUpdates: %v", err)
	}

	updates := collectUpdates(t, stream)
	if len(updates) == 0 {
		t.Fatal("TaskUpdates produced no updates")
	}

	last := updates[len(updates)-1]
	if last.GetType() != a2av1.TaskUpdateType_TASK_UPDATE_TYPE_FINISHED {
		t.Fatalf("last update type = %v, want FINISHED", last.GetType())
	}

	if got := last.GetTask().GetState(); got != a2av1.TaskState_TASK_STATE_SUCCEEDED {
		t.Fatalf("final task state = %v, want SUCCEEDED (error: %v)", got, last.GetError())
	}

	if last.GetTask().GetTaskId() != taskID {
		t.Fatalf("finished update task id = %q, want %q", last.GetTask().GetTaskId(), taskID)
	}

	assertActionUpdate(t, updates, "greet")
	assertActionUpdate(t, updates, "reply")
}

// assertActionUpdate checks at least one action update carried the given step
// name, proving per-action progress flowed through the A2A stream.
func assertActionUpdate(t *testing.T, updates []*a2av1.TaskUpdate, action string) {
	t.Helper()

	for _, update := range updates {
		if update.GetStep().GetName() == action {
			return
		}
	}

	t.Fatalf("no action update for %q in %d updates", action, len(updates))
}

func TestGetTaskAndListTasksMap(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{})

	created, err := fix.a2a.CreateTask(t.Context(), &a2av1.CreateTaskRequest{Skill: helloSkill})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	taskID := created.GetTask().GetTaskId()

	// Drain the run to completion so GetTask sees a terminal task with actions.
	stream, err := fix.a2a.TaskUpdates(t.Context(), &a2av1.TaskUpdatesRequest{TaskId: taskID})
	if err != nil {
		t.Fatalf("TaskUpdates: %v", err)
	}

	collectUpdates(t, stream)

	got, err := fix.a2a.GetTask(t.Context(), &a2av1.GetTaskRequest{TaskId: taskID})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}

	if got.GetTask().GetSkill() != helloSkill {
		t.Fatalf("GetTask skill = %q, want %q", got.GetTask().GetSkill(), helloSkill)
	}

	if len(got.GetActions()) != 2 {
		t.Fatalf("GetTask returned %d actions, want 2", len(got.GetActions()))
	}

	list, err := fix.a2a.ListTasks(t.Context(), &a2av1.ListTasksRequest{Skill: helloSkill})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}

	if len(list.GetTasks()) != 1 || list.GetTasks()[0].GetTaskId() != taskID {
		t.Fatalf("ListTasks = %v, want exactly the task %s", list.GetTasks(), taskID)
	}

	_, err = fix.a2a.GetTask(t.Context(), &a2av1.GetTaskRequest{TaskId: "missing"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("GetTask(missing) code = %v, want NotFound", status.Code(err))
	}
}

func TestListTasksFiltersByState(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{})

	created, err := fix.a2a.CreateTask(t.Context(), &a2av1.CreateTaskRequest{Skill: helloSkill})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	stream, err := fix.a2a.TaskUpdates(t.Context(), &a2av1.TaskUpdatesRequest{TaskId: created.GetTask().GetTaskId()})
	if err != nil {
		t.Fatalf("TaskUpdates: %v", err)
	}

	collectUpdates(t, stream)

	succeeded, err := fix.a2a.ListTasks(t.Context(), &a2av1.ListTasksRequest{State: statusSucceeded})
	if err != nil {
		t.Fatalf("ListTasks succeeded: %v", err)
	}

	if len(succeeded.GetTasks()) != 1 {
		t.Fatalf("succeeded filter returned %d tasks, want 1", len(succeeded.GetTasks()))
	}

	running, err := fix.a2a.ListTasks(t.Context(), &a2av1.ListTasksRequest{State: statusRunning})
	if err != nil {
		t.Fatalf("ListTasks running: %v", err)
	}

	if len(running.GetTasks()) != 0 {
		t.Fatalf("running filter returned %d tasks, want 0", len(running.GetTasks()))
	}
}

func TestCancelTaskStopsInFlightRun(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{})

	created, err := fix.a2a.CreateTask(t.Context(), &a2av1.CreateTaskRequest{Skill: slowSkill})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	taskID := created.GetTask().GetTaskId()

	stream, err := fix.a2a.TaskUpdates(t.Context(), &a2av1.TaskUpdatesRequest{TaskId: taskID})
	if err != nil {
		t.Fatalf("TaskUpdates: %v", err)
	}

	waitForRunningStep(t, stream)

	ack, err := fix.a2a.CancelTask(t.Context(), &a2av1.CancelTaskRequest{TaskId: taskID})
	if err != nil {
		t.Fatalf("CancelTask: %v", err)
	}

	if ack.GetTaskId() != taskID {
		t.Fatalf("cancel ack task id = %q, want %q", ack.GetTaskId(), taskID)
	}

	updates := drainRemaining(t, stream)
	if len(updates) == 0 {
		t.Fatal("no updates after cancel; want the stream to finish")
	}

	last := updates[len(updates)-1]
	if last.GetTask().GetState() != a2av1.TaskState_TASK_STATE_CANCELED {
		t.Fatalf("final task state = %v, want CANCELED", last.GetTask().GetState())
	}

	got, err := fix.a2a.GetTask(t.Context(), &a2av1.GetTaskRequest{TaskId: taskID})
	if err != nil {
		t.Fatalf("GetTask after cancel: %v", err)
	}

	if got.GetTask().GetState() != a2av1.TaskState_TASK_STATE_CANCELED {
		t.Fatalf("stored task state = %v, want CANCELED", got.GetTask().GetState())
	}
}

// waitForRunningStep reads the stream until an action reports running.
func waitForRunningStep(t *testing.T, stream grpc.ServerStreamingClient[a2av1.TaskUpdate]) {
	t.Helper()

	for {
		update, err := stream.Recv()
		if err != nil {
			t.Fatalf("recv while waiting for running step: %v", err)
		}

		if update.GetStep().GetStatus() == statusRunning {
			return
		}
	}
}

// drainRemaining reads the rest of a stream to EOF.
func drainRemaining(t *testing.T, stream grpc.ServerStreamingClient[a2av1.TaskUpdate]) []*a2av1.TaskUpdate {
	t.Helper()

	return collectUpdates(t, stream)
}

func TestCancelTaskRejectsFinishedTask(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{})

	created, err := fix.a2a.CreateTask(t.Context(), &a2av1.CreateTaskRequest{Skill: helloSkill})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	taskID := created.GetTask().GetTaskId()

	stream, err := fix.a2a.TaskUpdates(t.Context(), &a2av1.TaskUpdatesRequest{TaskId: taskID})
	if err != nil {
		t.Fatalf("TaskUpdates: %v", err)
	}

	collectUpdates(t, stream)

	_, err = fix.a2a.CancelTask(t.Context(), &a2av1.CancelTaskRequest{TaskId: taskID})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("CancelTask on a finished task = %v, want FailedPrecondition", status.Code(err))
	}
}

func TestCreateTaskRejectsUnknownSkill(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{})

	_, err := fix.a2a.CreateTask(t.Context(), &a2av1.CreateTaskRequest{Skill: "ghost"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("CreateTask(ghost) code = %v, want NotFound", status.Code(err))
	}
}

func TestCreateTaskRejectsUndeclaredParams(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{})

	params, err := structpb.NewStruct(map[string]any{"bogus": 1})
	if err != nil {
		t.Fatalf("build params: %v", err)
	}

	_, err = fix.a2a.CreateTask(t.Context(), &a2av1.CreateTaskRequest{Skill: helloSkill, Params: params})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("CreateTask with undeclared param code = %v, want InvalidArgument", status.Code(err))
	}
}
