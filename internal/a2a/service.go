package a2a

import (
	"context"
	"fmt"

	"github.com/chadit/CairnOS/internal/api"
	a2av1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/a2a/v1"
	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/store"
)

// triggerA2A marks runs started over A2A in run history, the value spec 04
// reserves for this protocol (store.Run.Trigger: cli|schedule|api|a2a|webhook).
const triggerA2A = "a2a"

// defaultListLimit caps ListTasks when the request sets no limit, matching the
// WorkflowService ListRuns default so the two list surfaces behave alike.
const defaultListLimit = 50

// cancelAckState acknowledges a cancel signal; the task finalizes
// asynchronously and lands in history as canceled.
const cancelAckState = a2av1.TaskState_TASK_STATE_RUNNING

// Service implements cairnos.a2a.v1.A2AService by translating the A2A task
// vocabulary onto CairnOS runs and reusing the run manager, the workflow
// resolver, and the durable store (.agents/spec/09-a2a-and-hive.md, "Core
// methods"). It owns no engine of its own.
type Service struct {
	a2av1.UnimplementedA2AServiceServer

	manager  *api.Manager
	resolver *api.WorkflowResolver
	store    store.Store
}

// NewService wires the adapter over the shared run manager, a workflow
// resolver for the node's workflows directory, and the run store.
func NewService(manager *api.Manager, resolver *api.WorkflowResolver, runStore store.Store) *Service {
	return &Service{manager: manager, resolver: resolver, store: runStore}
}

// CreateTask starts a run from an A2A task request and returns the created
// task without waiting for completion; a consumer streams progress with
// TaskUpdates. The run lifetime detaches from the request context the same way
// RunWorkflow's does: a dropped client does not kill the run.
//
// The returned task is composed from the request and the new run id rather
// than read back from the store. StartWatched returns the moment the engine
// accepts the run id, which is a hair before CreateRun finishes persisting the
// record, so a GetRun here would race that write. The task is freshly running
// and its detail is already known: id, skill, and params. A consumer reads the
// authoritative state through GetTask or TaskUpdates.
func (s *Service) CreateTask(ctx context.Context, req *a2av1.CreateTaskRequest) (*a2av1.CreateTaskResponse, error) {
	flow, err := s.resolver.Resolve(req.GetSkill())
	if err != nil {
		return nil, wirePassthrough(err)
	}

	runID, err := s.manager.StartWatched(ctx, flow, req.GetParams().AsMap(), triggerA2A)
	if err != nil {
		return nil, wireStatus(err)
	}

	task := &a2av1.Task{
		TaskId: runID,
		Skill:  req.GetSkill(),
		State:  a2av1.TaskState_TASK_STATE_RUNNING,
		Params: req.GetParams(),
	}

	return &a2av1.CreateTaskResponse{
		Task:         task,
		ClientTaskId: req.GetClientTaskId(),
	}, nil
}

// GetTask reads one task's current state plus its action results, backed by
// the store's GetRun and ListActionResults.
func (s *Service) GetTask(ctx context.Context, req *a2av1.GetTaskRequest) (*a2av1.GetTaskResponse, error) {
	run, err := s.store.GetRun(ctx, req.GetTaskId())
	if err != nil {
		return nil, wireRunLookup(req.GetTaskId(), err)
	}

	results, err := s.store.ListActionResults(ctx, run.ID)
	if err != nil {
		return nil, wireStatus(err)
	}

	actions := make([]*cairnosv1.ActionResult, 0, len(results))
	for _, result := range results {
		actions = append(actions, protoActionResult(result))
	}

	return &a2av1.GetTaskResponse{Task: taskFromRun(run), Actions: actions}, nil
}

// ListTasks lists tasks newest-first under the request filter, backed by
// ListRuns. The skill filter narrows by workflow name and the state filter by
// run status.
func (s *Service) ListTasks(ctx context.Context, req *a2av1.ListTasksRequest) (*a2av1.ListTasksResponse, error) {
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultListLimit
	}

	runs, err := s.store.ListRuns(ctx, store.ListFilter{
		WorkflowName: req.GetSkill(),
		Status:       runStatusFilter(req.GetState()),
		Limit:        limit,
	})
	if err != nil {
		return nil, wireStatus(err)
	}

	tasks := make([]*a2av1.Task, 0, len(runs))
	for _, run := range runs {
		tasks = append(tasks, taskFromRun(run))
	}

	return &a2av1.ListTasksResponse{Tasks: tasks}, nil
}

// CancelTask signals an in-flight task to stop, backed by CancelRun. A task
// not managed by this node (finished, unknown, or started elsewhere) answers
// with a canonical error instead of a silent no-op, matching CancelRun.
func (s *Service) CancelTask(ctx context.Context, req *a2av1.CancelTaskRequest) (*a2av1.CancelTaskResponse, error) {
	taskID := req.GetTaskId()

	if s.manager.Cancel(ctx, taskID) {
		return &a2av1.CancelTaskResponse{TaskId: taskID, State: cancelAckState}, nil
	}

	run, err := s.store.GetRun(ctx, taskID)
	if err != nil {
		return nil, wireRunLookup(taskID, err)
	}

	if isTerminal(run.Status) {
		return nil, wireRunFinished(taskID, string(run.Status))
	}

	return nil, wireRunNotManaged(taskID)
}

// TaskUpdates streams one task's run and action events until it finalizes,
// backed by the run event stream. CreateTask started the run without a
// watcher, so this method attaches to the run's live events through the
// manager and replays nothing; a consumer that wants the full stream calls
// TaskUpdates as part of, or right after, CreateTask.
func (s *Service) TaskUpdates(req *a2av1.TaskUpdatesRequest, stream a2av1.A2AService_TaskUpdatesServer) error {
	handle, watching := s.manager.Watch(stream.Context(), req.GetTaskId())
	if !watching {
		return s.sendTerminalSnapshot(stream.Context(), req.GetTaskId(), stream)
	}

	if err := forwardUpdates(handle, stream); err != nil {
		return err
	}

	return sendFinished(handle, stream)
}

// sendTerminalSnapshot answers TaskUpdates for a task the manager no longer
// watches: the run already finished, so the stream carries one finished update
// built from the stored record instead of live events. A missing run surfaces
// as the canonical NotFound.
func (s *Service) sendTerminalSnapshot(ctx context.Context, taskID string, stream a2av1.A2AService_TaskUpdatesServer) error {
	run, err := s.store.GetRun(ctx, taskID)
	if err != nil {
		return wireRunLookup(taskID, err)
	}

	final := &a2av1.TaskUpdate{
		TaskId: run.ID,
		Type:   a2av1.TaskUpdateType_TASK_UPDATE_TYPE_FINISHED,
		Task:   taskFromRun(run),
		Error:  protoError(run.Error),
	}

	if err := stream.Send(final); err != nil {
		return fmt.Errorf("send task finished update: %w", err)
	}

	return nil
}

// sendFinished waits for the terminal run record and sends the closing
// finished update carrying the full task snapshot, composed from the record
// instead of a hook event so a dropped event can never truncate the ending.
func sendFinished(handle *api.RunHandle, stream a2av1.A2AService_TaskUpdatesServer) error {
	out := <-handle.Outcome
	if out.Err != nil {
		return wireStatus(out.Err)
	}

	final := &a2av1.TaskUpdate{
		TaskId: out.Run.ID,
		Type:   a2av1.TaskUpdateType_TASK_UPDATE_TYPE_FINISHED,
		Task:   taskFromRun(out.Run),
		Error:  protoError(out.Run.Error),
	}

	if err := stream.Send(final); err != nil {
		return fmt.Errorf("send task finished update: %w", err)
	}

	return nil
}

// forwardUpdates relays started and action updates until the manager closes
// the channel. The terminal update is composed by sendFinished from the run
// record instead, so a dropped finished event cannot truncate the stream.
func forwardUpdates(handle *api.RunHandle, stream a2av1.A2AService_TaskUpdatesServer) error {
	for evt := range handle.Events {
		if evt.Type == engine.EventRunFinished {
			continue
		}

		if err := stream.Send(taskUpdateFromEvent(&evt)); err != nil {
			return fmt.Errorf("send task update: %w", err)
		}
	}

	return nil
}

// isTerminal reports whether a run status is one of the three final states, so
// CancelTask can tell "already done" from "not mine to cancel".
func isTerminal(status store.RunStatus) bool {
	return status == store.RunSucceeded || status == store.RunFailed || status == store.RunCanceled
}
