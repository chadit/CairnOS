package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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

// savedFilePerm is the permission a saved workflow file keeps: owner
// read/write only, matching the applier's applyFilePerm precedent.
const savedFilePerm = 0o600

// cancelAckStatus acknowledges a cancel signal; the run finalizes
// asynchronously and lands in history as canceled.
const cancelAckStatus = "canceling"

// resumeAckStatus acknowledges an approval decision; the run resumes
// asynchronously and lands in history when its loop settles.
const resumeAckStatus = "resuming"

// workflowService implements cairnos.v1.WorkflowService over the run
// manager, the durable store, and the workflows directory. authenticator is
// nil when auth is disabled; approval decisions then record "local" as the
// actor, the same fallback governance audit rows use.
type workflowService struct {
	cairnosv1.UnimplementedWorkflowServiceServer

	store         store.Store
	authenticator Authenticator
	manager       *Manager
	logger        *slog.Logger
	workflowsDir  string
}

// RunWorkflow starts a run by name and streams its events until the run
// finalizes. A dropped HTTP or gRPC client does not cancel the run; only
// CancelRun and server shutdown do (.agents/spec/04-execution-engine.md:
// canceled means an operator stopped the run).
func (s *workflowService) RunWorkflow(req *cairnosv1.RunWorkflowRequest, stream cairnosv1.WorkflowService_RunWorkflowServer) error {
	flow, err := s.resolveRunFlow(req)
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

// ReplayRun reruns a prior run from its first failed action and streams the new
// run's events until it finalizes. A foreign run reads as not-found, the
// workflow is resolved fresh by name, and a workflow-hash mismatch is refused
// unless force is set. The replay goes through the manager, so it is watchable
// and cancelable (.agents/spec/parity/workflow-language.md, "Replay from failed
// action").
func (s *workflowService) ReplayRun(req *cairnosv1.ReplayRunRequest, stream cairnosv1.WorkflowService_ReplayRunServer) error {
	ctx := stream.Context()

	prior, err := s.store.GetRun(ctx, req.GetRunId())
	if err != nil {
		return runLookupStatus(req.GetRunId(), err)
	}

	results, err := s.store.ListActionResults(ctx, prior.ID)
	if err != nil {
		return canonicalStatus(err)
	}

	flow, err := NewWorkflowResolver(s.workflowsDir, s.logger).Resolve(prior.WorkflowName)
	if err != nil {
		return err
	}

	if !req.GetForce() && flow.Hash != prior.WorkflowHash {
		return errorStatus(codes.FailedPrecondition, cairnerr.Newf(codeReplayHashMismatch,
			"workflow %q changed since run %s (stored hash %s, current %s); replay with force to override",
			prior.WorkflowName, prior.ID, prior.WorkflowHash, flow.Hash))
	}

	handle, err := s.manager.StartReplay(ctx, prior, results, flow)
	if err != nil {
		return canonicalStatus(err)
	}

	if err := s.forwardEvents(handle, stream); err != nil {
		return err
	}

	return s.sendFinished(handle, stream)
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

// SaveWorkflow validates a definition and writes it into the workflows
// directory. Overwriting an existing workflow captures the prior definition in
// the version store before the write, so cairnos suggestions revert restores
// the pre-save definition with no new rollback machinery. Unlike
// ValidateWorkflow, a bad file here is an RPC error: the save refuses it and
// nothing touches disk (.agents/spec/parity/builder-ops.md, "SaveWorkflow
// handler").
func (s *workflowService) SaveWorkflow(ctx context.Context, req *cairnosv1.SaveWorkflowRequest) (*cairnosv1.SaveWorkflowResponse, error) {
	name := req.GetWorkflowName()

	if _, err := parseNamedDefinition(req.GetToml(), name); err != nil {
		return nil, err
	}

	path, priorHash, created, err := s.captureRollbackPoint(ctx, name)
	if err != nil {
		return nil, err
	}

	if writeFileErr := os.WriteFile(path, req.GetToml(), savedFilePerm); writeFileErr != nil {
		return nil, errorStatus(codes.Internal, cairnerr.Newf(codeInternal, "write workflow %q: %v", name, writeFileErr))
	}

	// Reloading proves the written file loads and returns the stamped hash the
	// response reports.
	saved, err := workflow.Load(path)
	if err != nil {
		return nil, canonicalStatus(err)
	}

	return &cairnosv1.SaveWorkflowResponse{
		WorkflowName: name,
		Hash:         saved.Hash,
		PriorHash:    priorHash,
		Created:      created,
	}, nil
}

// ApproveRun decides an approval gate on a held run, then resumes it through the
// manager. The resume is detached, so the response is only an ack and GetRun
// reports the outcome. Everything fallible resolves before the decision is
// written, so nothing strands a decided gate on a held run. A foreign run is
// not-found, never forbidden (.agents/spec/parity/workflow-language.md,
// "Human approval").
func (s *workflowService) ApproveRun(ctx context.Context, req *cairnosv1.ApproveRunRequest) (*cairnosv1.ApproveRunResponse, error) {
	held, err := s.store.GetRun(ctx, req.GetRunId())
	if err != nil {
		return nil, runLookupStatus(req.GetRunId(), err)
	}

	// A run that already finished can still own a pending gate: when a
	// failure or cancel beat the approval branch, the branch's detached write
	// left the gate behind while the run finalized. Deciding that orphaned
	// gate must not resurrect the terminal run, so refuse before touching it.
	if held.Status != store.RunHeld {
		return nil, errorStatus(codes.FailedPrecondition, cairnerr.Newf(codeRunNotHeld,
			"run %s is not awaiting approval (status %s)", held.ID, held.Status))
	}

	actionName, err := s.resolveApprovalAction(ctx, held.ID, req.GetActionName())
	if err != nil {
		return nil, err
	}

	results, err := s.store.ListActionResults(ctx, held.ID)
	if err != nil {
		return nil, canonicalStatus(err)
	}

	flow, err := NewWorkflowResolver(s.workflowsDir, s.logger).Resolve(held.WorkflowName)
	if err != nil {
		return nil, err
	}

	decision := store.ApprovalApproved
	if !req.GetApprove() {
		decision = store.ApprovalDenied
	}

	actor := actorFromContext(ctx, s.authenticator)
	if err := s.store.DecideApproval(ctx, held.ID, actionName, decision, actor, req.GetComment()); err != nil {
		return nil, canonicalStatus(err)
	}

	if err := s.startResume(ctx, held, results, flow); err != nil {
		return nil, err
	}

	return &cairnosv1.ApproveRunResponse{
		RunId:      held.ID,
		ActionName: actionName,
		Approved:   req.GetApprove(),
		Status:     resumeAckStatus,
	}, nil
}

// resolveApprovalAction picks the gate to decide: the named one, or the
// single pending gate when the name is omitted. Zero or several pending
// gates with no name is refused, the several case listing the pending action
// names so the caller reruns naming one, matching the CLI's semantics.
func (s *workflowService) resolveApprovalAction(ctx context.Context, runID, actionName string) (string, error) {
	if actionName != "" {
		return actionName, nil
	}

	pending, err := s.store.ListPendingApprovals(ctx, runID)
	if err != nil {
		return "", canonicalStatus(err)
	}

	switch len(pending) {
	case 0:
		return "", errorStatus(codes.FailedPrecondition, cairnerr.Newf(codeNoPendingApproval,
			"run %s has no pending approvals", runID))
	case 1:
		return pending[0].ActionName, nil
	default:
		return "", errorStatus(codes.FailedPrecondition, cairnerr.Newf(codeApprovalAmbiguous,
			"run %s has %d pending approvals; rerun naming one of: %s",
			runID, len(pending), strings.Join(approvalActionNames(pending), ", ")))
	}
}

// startResume hands the decided run to the manager and drains its outcome in
// the background so an unattended resume failure still lands in the log. A
// run the manager is already driving is a success here, not an error: the
// decision is recorded and the active loop settles it.
func (s *workflowService) startResume(ctx context.Context, held *store.Run, results []*store.ActionResult, flow *workflow.Workflow) error {
	handle, err := s.manager.StartResume(ctx, held, results, flow)
	if err != nil {
		if errors.Is(err, ErrRunActive) {
			return nil
		}

		return canonicalStatus(err)
	}

	go func() {
		out := <-handle.Outcome
		if out.Err != nil {
			s.logger.Error("resume after approval decision failed",
				"run_id", held.ID, "error", out.Err)
		}
	}()

	return nil
}

// approvalActionNames lists the pending gates' action names for the
// ambiguity message.
func approvalActionNames(pending []*store.Approval) []string {
	names := make([]string, 0, len(pending))
	for _, gate := range pending {
		names = append(names, gate.ActionName)
	}

	return names
}

// resolveRunFlow picks the definition a run executes. An empty toml resolves the
// named file from the workflows directory. A non-empty toml runs inline: parsed,
// name-checked against the path param, and hash-stamped so the run records a
// saved file's identity, all before any stream event. Running unsaved is
// scope-equivalent to save-then-run, so no extra privilege is needed
// (.agents/spec/parity/builder-ops.md, "Inline run").
func (s *workflowService) resolveRunFlow(req *cairnosv1.RunWorkflowRequest) (*workflow.Workflow, error) {
	if len(req.GetToml()) == 0 {
		return NewWorkflowResolver(s.workflowsDir, s.logger).Resolve(req.GetWorkflowName())
	}

	flow, err := parseNamedDefinition(req.GetToml(), req.GetWorkflowName())
	if err != nil {
		return nil, err
	}

	flow.Hash = workflow.HashBytes(req.GetToml())

	return flow, nil
}

// A not-found name is a create targeting name.toml. The join is traversal-safe
// because the caller pinned name to a validated lowercase identifier. A NotFound
// from an unreadable directory also lands there, so the later write fails as
// INTERNAL rather than masking the problem here.

// captureRollbackPoint resolves the save target and, for an existing name,
// records its prior bytes in the version store first. Capturing before the write
// makes the save revertible.
func (s *workflowService) captureRollbackPoint(ctx context.Context, name string) (string, string, bool, error) {
	path, priorFlow, err := NewWorkflowResolver(s.workflowsDir, s.logger).ResolvePath(name)
	if err != nil {
		if status.Code(err) != codes.NotFound {
			return "", "", false, err
		}

		return filepath.Join(s.workflowsDir, name+workflowFileSuffix), "", true, nil
	}

	priorBytes, err := os.ReadFile(path) // #nosec G304 -- path comes from the workflows directory scan, not external input
	if err != nil {
		return "", "", false, errorStatus(codes.Internal,
			cairnerr.Newf(codeInternal, "read prior workflow %q: %v", name, err))
	}

	// Saving the same (hash, workflow) twice is a store-level no-op, so
	// re-saving identical bytes is safe (the applier's captureVersion shape).
	if err := s.store.SaveWorkflowVersion(ctx, &store.WorkflowVersion{
		Hash:           priorFlow.Hash,
		WorkflowName:   name,
		DefinitionTOML: string(priorBytes),
	}); err != nil {
		return "", "", false, canonicalStatus(err)
	}

	return path, priorFlow.Hash, false, nil
}

// parseNamedDefinition parses definition bytes and requires its name to match
// the path param. A parse failure returns the INVALID_ARGUMENT list
// ValidateWorkflow reports, so the client sees every workflow.* problem. A
// mismatch answers api.workflow_name_mismatch: without it a save could strand a
// file under a name the client never used, and an inline run could execute under
// the wrong identity (.agents/spec/parity/builder-ops.md).
func parseNamedDefinition(toml []byte, name string) (*workflow.Workflow, error) {
	flow, err := workflow.Parse(toml)
	if err != nil {
		return nil, canonicalStatus(err)
	}

	if flow.Name != name {
		return nil, errorStatus(codes.InvalidArgument, cairnerr.Newf(codeWorkflowNameMismatch,
			"definition name %q does not match workflow name %q in the request path", flow.Name, name))
	}

	return flow, nil
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
