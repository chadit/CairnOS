package a2a

import (
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	a2av1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/a2a/v1"
	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/store"
)

// Run status literals the state mapping reads. They mirror store.RunStatus
// without importing its string values one by one at each call site.
const (
	runPending   = "pending"
	runRunning   = "running"
	runSucceeded = "succeeded"
	runFailed    = "failed"
	runCanceled  = "canceled"
)

// taskFromRun maps a persisted run onto the A2A task wire shape: the run id is
// the task id, the workflow name is the skill, the run status is the task
// state, and the run inputs are the task params. Timestamps render as RFC 3339
// strings so an HTTP-only consumer needs no protobuf timestamp decoding.
func taskFromRun(run *store.Run) *a2av1.Task {
	if run == nil {
		return nil
	}

	return &a2av1.Task{
		TaskId:     run.ID,
		Skill:      run.WorkflowName,
		State:      taskState(string(run.Status)),
		Params:     protoStruct(run.Inputs),
		Error:      protoError(run.Error),
		CreatedAt:  rfc3339(run.CreatedAt),
		StartedAt:  rfc3339(run.StartedAt),
		FinishedAt: rfc3339(run.FinishedAt),
	}
}

// taskState maps a CairnOS run status onto the A2A task state enum, so a
// consumer that only speaks A2A never sees CairnOS run vocabulary.
func taskState(status string) a2av1.TaskState {
	switch status {
	case runPending:
		return a2av1.TaskState_TASK_STATE_PENDING
	case runRunning:
		return a2av1.TaskState_TASK_STATE_RUNNING
	case runSucceeded:
		return a2av1.TaskState_TASK_STATE_SUCCEEDED
	case runFailed:
		return a2av1.TaskState_TASK_STATE_FAILED
	case runCanceled:
		return a2av1.TaskState_TASK_STATE_CANCELED
	default:
		return a2av1.TaskState_TASK_STATE_UNSPECIFIED
	}
}

// runStatusFilter maps an A2A task state filter string onto a run status
// filter. An empty or unrecognized value means "all states", matching the
// ListRuns contract where an empty status filter does not narrow.
func runStatusFilter(state string) store.RunStatus {
	switch state {
	case runPending, runRunning, runSucceeded, runFailed, runCanceled:
		return store.RunStatus(state)
	default:
		return ""
	}
}

// taskUpdateFromEvent maps one engine run event onto the A2A task update wire
// shape. The finished update is composed by the stream handler from the
// terminal run record instead, so this covers started and action updates.
func taskUpdateFromEvent(evt *engine.Event) *a2av1.TaskUpdate {
	out := &a2av1.TaskUpdate{
		TaskId: evt.RunID,
		Type:   taskUpdateType(evt.Type),
		Error:  protoError(evt.Err),
	}

	if evt.Type == engine.EventActionTransition {
		out.Step = &a2av1.TaskStep{
			Name:    evt.Action,
			Status:  evt.Status,
			Attempt: int32(evt.Attempt), // #nosec G115 -- attempts are small positive retry counts
		}
	}

	return out
}

// taskUpdateType maps the engine event names onto the A2A update enum.
func taskUpdateType(eventType engine.EventType) a2av1.TaskUpdateType {
	switch eventType {
	case engine.EventRunStarted:
		return a2av1.TaskUpdateType_TASK_UPDATE_TYPE_STARTED
	case engine.EventActionTransition:
		return a2av1.TaskUpdateType_TASK_UPDATE_TYPE_ACTION
	case engine.EventRunFinished:
		return a2av1.TaskUpdateType_TASK_UPDATE_TYPE_FINISHED
	default:
		return a2av1.TaskUpdateType_TASK_UPDATE_TYPE_UNSPECIFIED
	}
}

// protoActionResult maps a persisted action row onto the WorkflowService
// ActionResult wire shape, reused verbatim so A2A and the native API report
// the same per-action detail.
func protoActionResult(result *store.ActionResult) *cairnosv1.ActionResult {
	return &cairnosv1.ActionResult{
		Name:    result.ActionName,
		Runner:  result.Runner,
		Status:  string(result.Status),
		Attempt: int32(result.Attempt), // #nosec G115 -- attempts are small positive retry counts
		Output:  protoValue(result.Output),
		Usage: &cairnosv1.Usage{
			InputTokens:  result.Usage.InputTokens,
			OutputTokens: result.Usage.OutputTokens,
			CachedTokens: result.Usage.CachedTokens,
			CostUsd:      result.Usage.CostUSD,
			LatencyMs:    result.Usage.Latency.Milliseconds(),
			Retries:      int32(result.Usage.Retries), // #nosec G115 -- retry counts are small and positive
		},
		Error:      protoError(result.Error),
		StartedAt:  protoTime(result.StartedAt),
		FinishedAt: protoTime(result.FinishedAt),
	}
}

// protoError renders a canonical error onto the shared cairnos.v1.Error wire
// shape, so an A2A consumer sees the same code and message a native client
// does.
func protoError(cerr *cairnerr.Error) *cairnosv1.Error {
	if cerr == nil {
		return nil
	}

	out := &cairnosv1.Error{Code: cerr.Code, Message: cerr.Message}

	if len(cerr.Details) > 0 {
		if details, err := structpb.NewStruct(cerr.Details); err == nil {
			out.Details = details
		}
	}

	return out
}

// protoStruct renders run inputs as a JSON struct, dropping a value that
// cannot become a Struct; run inputs are JSON-shaped by the runner contract,
// so a failure here is a programming error upstream, not a request problem.
func protoStruct(values map[string]any) *structpb.Struct {
	if len(values) == 0 {
		return nil
	}

	converted, err := structpb.NewStruct(values)
	if err != nil {
		return nil
	}

	return converted
}

// protoValue renders one action output, degrading a non-JSON value to nil
// rather than failing the response.
func protoValue(value any) *structpb.Value {
	if value == nil {
		return nil
	}

	converted, err := structpb.NewValue(value)
	if err != nil {
		return nil
	}

	return converted
}

// protoTime renders a timestamp, with nil for the zero time so "not yet"
// fields stay absent.
func protoTime(value time.Time) *timestamppb.Timestamp {
	if value.IsZero() {
		return nil
	}

	return timestamppb.New(value)
}

// rfc3339 renders a timestamp as an RFC 3339 string, empty for the zero time.
func rfc3339(value time.Time) string {
	if value.IsZero() {
		return ""
	}

	return value.Format(time.RFC3339Nano)
}
