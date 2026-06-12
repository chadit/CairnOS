package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"google.golang.org/grpc/codes"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
)

// defaultListLimit caps ListRuns when the request does not set one.
const defaultListLimit = 50

// workflowFileSuffix selects workflow definitions in the workflows directory.
const workflowFileSuffix = ".toml"

// cancelAckStatus acknowledges a cancel signal; the run finalizes
// asynchronously and lands in history as canceled.
const cancelAckStatus = "canceling"

// workflowService implements cairnos.v1.WorkflowService over the run
// manager, the durable store, and the workflows directory.
type workflowService struct {
	cairnosv1.UnimplementedWorkflowServiceServer

	store        store.Store
	manager      *Manager
	workflowsDir string
	logger       *slog.Logger
}

// RunWorkflow starts a run by name and streams its events until the run
// finalizes. A dropped HTTP or gRPC client does not cancel the run; only
// CancelRun and server shutdown do (.agents/spec/04-execution-engine.md:
// canceled means an operator stopped the run).
func (s *workflowService) RunWorkflow(req *cairnosv1.RunWorkflowRequest, stream cairnosv1.WorkflowService_RunWorkflowServer) error {
	flow, err := NewWorkflowResolver(s.workflowsDir, s.logger).Resolve(req.GetWorkflowName())
	if err != nil {
		return err
	}

	handle, err := s.manager.StartRun(stream.Context(), flow, req.GetInputs().AsMap(), true, triggerAPI)
	if err != nil {
		return canonicalStatus(err)
	}

	if err := s.forwardEvents(handle, stream); err != nil {
		return err
	}

	return s.sendFinished(handle, stream)
}

// GetRun fetches one run and its action results.
func (s *workflowService) GetRun(ctx context.Context, req *cairnosv1.GetRunRequest) (*cairnosv1.GetRunResponse, error) {
	run, err := s.store.GetRun(ctx, req.GetRunId())
	if err != nil {
		return nil, runLookupStatus(req.GetRunId(), err)
	}

	results, err := s.store.ListActionResults(ctx, run.ID)
	if err != nil {
		return nil, canonicalStatus(err)
	}

	actions := make([]*cairnosv1.ActionResult, 0, len(results))
	for _, result := range results {
		actions = append(actions, protoActionResult(result))
	}

	return &cairnosv1.GetRunResponse{Run: protoRun(run), Actions: actions}, nil
}

// ListRuns lists runs newest-first under the request filter.
func (s *workflowService) ListRuns(ctx context.Context, req *cairnosv1.ListRunsRequest) (*cairnosv1.ListRunsResponse, error) {
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultListLimit
	}

	runs, err := s.store.ListRuns(ctx, store.ListFilter{
		WorkflowName: req.GetWorkflow(),
		Status:       store.RunStatus(req.GetStatus()),
		Limit:        limit,
	})
	if err != nil {
		return nil, canonicalStatus(err)
	}

	out := make([]*cairnosv1.Run, 0, len(runs))
	for _, run := range runs {
		out = append(out, protoRun(run))
	}

	return &cairnosv1.ListRunsResponse{Runs: out}, nil
}

// CancelRun signals an active run to stop. Runs not managed by this server
// (finished, unknown, or started elsewhere) answer with a canonical error
// instead of a silent no-op.
func (s *workflowService) CancelRun(ctx context.Context, req *cairnosv1.CancelRunRequest) (*cairnosv1.CancelRunResponse, error) {
	runID := req.GetRunId()

	if s.manager.Cancel(ctx, runID) {
		return &cairnosv1.CancelRunResponse{RunId: runID, Status: cancelAckStatus}, nil
	}

	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return nil, runLookupStatus(runID, err)
	}

	if run.Status == store.RunSucceeded || run.Status == store.RunFailed || run.Status == store.RunCanceled {
		return nil, errorStatus(codes.FailedPrecondition,
			cairnerr.Newf(codeRunFinished, "run %s already finished as %s", runID, run.Status))
	}

	return nil, errorStatus(codes.FailedPrecondition,
		cairnerr.Newf(codeRunNotManaged, "run %s was not started by this server; cancel it where it was started", runID))
}

// ValidateWorkflow parses and validates a workflow file without running it.
// Validation problems are the successful answer here (valid=false plus the
// canonical codes), not an RPC error: the caller asked whether the file is
// good, and "no, here is why" answers that question.
func (*workflowService) ValidateWorkflow(ctx context.Context, req *cairnosv1.ValidateWorkflowRequest) (*cairnosv1.ValidateWorkflowResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, canonicalStatus(err)
	}

	_, err := workflow.Parse(req.GetToml())
	if err == nil {
		return &cairnosv1.ValidateWorkflowResponse{Valid: true}, nil
	}

	list, isList := errors.AsType[cairnerr.Errors](err)
	if !isList {
		return nil, canonicalStatus(err)
	}

	problems := make([]*cairnosv1.Error, 0, len(list))
	for _, item := range list {
		problems = append(problems, protoError(item))
	}

	return &cairnosv1.ValidateWorkflowResponse{Valid: false, Errors: problems}, nil
}

// forwardEvents relays run_started and action transitions until the manager
// closes the channel. The terminal event is composed by sendFinished from
// the run record instead, so a dropped hook event can never truncate the
// stream's ending.
func (*workflowService) forwardEvents(handle *RunHandle, stream cairnosv1.WorkflowService_RunWorkflowServer) error {
	for evt := range handle.Events {
		if evt.Type == engine.EventRunFinished {
			continue
		}

		if err := stream.Send(protoEvent(&evt)); err != nil {
			// The client went away; the run keeps going and the channel
			// drains via the manager's buffer-and-drop dispatch.
			return fmt.Errorf("send run event: %w", err)
		}
	}

	return nil
}

// sendFinished waits for the terminal run record and sends the closing
// run_finished event carrying the full run snapshot.
func (*workflowService) sendFinished(handle *RunHandle, stream cairnosv1.WorkflowService_RunWorkflowServer) error {
	out := <-handle.Outcome
	if out.Err != nil {
		return canonicalStatus(out.Err)
	}

	final := &cairnosv1.RunEvent{
		RunId: out.Run.ID,
		Type:  cairnosv1.RunEventType_RUN_EVENT_TYPE_RUN_FINISHED,
		Run:   protoRun(out.Run),
		Error: protoError(out.Run.Error),
	}

	if err := stream.Send(final); err != nil {
		return fmt.Errorf("send run finished event: %w", err)
	}

	return nil
}

// runLookupStatus classifies a store GetRun failure.
func runLookupStatus(runID string, err error) error {
	if errors.Is(err, store.ErrRunNotFound) {
		return errorStatus(codes.NotFound, cairnerr.Newf(codeRunNotFound, "no run %s", runID))
	}

	return canonicalStatus(err)
}
