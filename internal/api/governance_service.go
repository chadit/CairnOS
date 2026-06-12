package api

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/store"
)

// Actor fallbacks when no authenticated subject is available
// (.agents/spec/12-ui-and-governance.md, "Control actions"): a request that
// carried no bearer token is recorded as "local".
const (
	actorLocal = "local"
)

// governanceService implements cairnos.v1.GovernanceService over the store's
// governance slice, the run manager (for terminate), and the workflow
// resolver (for budget state read from the file). Budgets are file-defined,
// so there is no SetBudget here: the service reads spend-versus-limit state
// and runs the admin control actions, each one audit-logged
// (.agents/spec/12-ui-and-governance.md, "Governance").
type governanceService struct {
	cairnosv1.UnimplementedGovernanceServiceServer

	store         store.GovernanceStore
	manager       *Manager
	resolver      *WorkflowResolver
	authenticator Authenticator
	logger        *slog.Logger
}

// GetWorkflowBudgetState reports the workflow's current-period spend, its
// configured limit and period, and whether it is paused. The budget spec
// comes from the workflow file; spend comes from the store; the period start
// is computed the same way the engine computes it, so the figure the UI sees
// is the figure enforcement uses.
func (s *governanceService) GetWorkflowBudgetState(ctx context.Context, req *cairnosv1.GetWorkflowBudgetStateRequest) (*cairnosv1.GetWorkflowBudgetStateResponse, error) {
	name := req.GetWorkflowName()
	if name == "" {
		return nil, errorStatus(codes.InvalidArgument, cairnerr.New(codeInvalidArgument, "workflow_name is required"))
	}

	_, paused, err := s.store.GetWorkflowPause(ctx, name)
	if err != nil {
		return nil, canonicalStatus(err)
	}

	resp := &cairnosv1.GetWorkflowBudgetStateResponse{WorkflowName: name, Paused: paused}

	flow, resolveErr := s.resolver.Resolve(name)
	if resolveErr != nil {
		// A missing or unparsable file still answers the pause question; the
		// budget fields stay zero with has_budget false. The resolve error is
		// deliberately swallowed: budget state is a best-effort read, and a
		// paused-but-unresolvable workflow still has a useful answer.
		//nolint:nilerr // resolve failure degrades to the pause-only answer, not an RPC error
		return resp, nil
	}

	if flow.Budget == nil {
		return resp, nil
	}

	spend, err := s.store.SpendSince(ctx, name, engine.PeriodStart(flow.Budget.Period, time.Now()))
	if err != nil {
		return nil, canonicalStatus(err)
	}

	resp.HasBudget = true
	resp.Period = flow.Budget.Period
	resp.LimitUsd = flow.Budget.LimitUSD
	resp.OnExceed = flow.Budget.OnExceed
	resp.SpendUsd = spend

	return resp, nil
}

// PauseWorkflow blocks new runs of a workflow until it is resumed, writing an
// audit row naming the actor. In-flight runs finish; the engine and scheduler
// honor the flag for new runs.
func (s *governanceService) PauseWorkflow(ctx context.Context, req *cairnosv1.PauseWorkflowRequest) (*cairnosv1.PauseWorkflowResponse, error) {
	name := req.GetWorkflowName()
	if name == "" {
		return nil, errorStatus(codes.InvalidArgument, cairnerr.New(codeInvalidArgument, "workflow_name is required"))
	}

	actor := s.actor(ctx)

	if err := s.store.SetWorkflowPause(ctx, &store.WorkflowPause{WorkflowName: name, Actor: actor, Reason: req.GetReason()}); err != nil {
		return nil, canonicalStatus(err)
	}

	s.audit(ctx, actor, store.AuditActionPause, name, req.GetReason())

	return &cairnosv1.PauseWorkflowResponse{WorkflowName: name, Paused: true}, nil
}

// ResumeWorkflow lifts a workflow's pause flag and audits the lift. The
// resumed flag is false when the workflow was not paused, so a redundant
// resume is a reported no-op rather than an error.
func (s *governanceService) ResumeWorkflow(ctx context.Context, req *cairnosv1.ResumeWorkflowRequest) (*cairnosv1.ResumeWorkflowResponse, error) {
	name := req.GetWorkflowName()
	if name == "" {
		return nil, errorStatus(codes.InvalidArgument, cairnerr.New(codeInvalidArgument, "workflow_name is required"))
	}

	actor := s.actor(ctx)

	resumed, err := s.store.ClearWorkflowPause(ctx, name)
	if err != nil {
		return nil, canonicalStatus(err)
	}

	s.audit(ctx, actor, store.AuditActionResume, name, "")

	return &cairnosv1.ResumeWorkflowResponse{WorkflowName: name, Resumed: resumed}, nil
}

// TerminateRun cancels an active run through the same manager path CancelRun
// uses, then audits the termination. A run not active here (finished or
// started elsewhere) answers FAILED_PRECONDITION, the same as CancelRun.
func (s *governanceService) TerminateRun(ctx context.Context, req *cairnosv1.TerminateRunRequest) (*cairnosv1.TerminateRunResponse, error) {
	runID := req.GetRunId()
	if runID == "" {
		return nil, errorStatus(codes.InvalidArgument, cairnerr.New(codeInvalidArgument, "run_id is required"))
	}

	if !s.manager.Cancel(ctx, runID) {
		return nil, errorStatus(codes.FailedPrecondition,
			cairnerr.Newf(codeRunNotManaged, "run %s is not active on this server; terminate it where it was started", runID))
	}

	s.audit(ctx, s.actor(ctx), store.AuditActionTerminate, runID, "")

	return &cairnosv1.TerminateRunResponse{RunId: runID, Status: cancelAckStatus}, nil
}

// ListAudit lists governance audit rows newest-first under the request limit.
func (s *governanceService) ListAudit(ctx context.Context, req *cairnosv1.ListAuditRequest) (*cairnosv1.ListAuditResponse, error) {
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultListLimit
	}

	entries, err := s.store.ListAudit(ctx, store.AuditFilter{Limit: limit})
	if err != nil {
		return nil, canonicalStatus(err)
	}

	out := make([]*cairnosv1.AuditEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, protoAuditEntry(entry))
	}

	return &cairnosv1.ListAuditResponse{Entries: out}, nil
}

// audit writes one append-only governance row, logging a write failure rather
// than failing the control action: the action already happened, and losing
// the audit row must not undo it or surface as the caller's error.
func (s *governanceService) audit(ctx context.Context, actor, action, target, detail string) {
	entry := &store.AuditEntry{Actor: actor, Action: action, Target: target, Detail: detail}
	if err := s.store.AppendAudit(ctx, entry); err != nil {
		s.logger.Error("append governance audit", "action", action, "target", target, "error", err)
	}
}

// actor derives who performed a control action: the authenticated subject when
// a verifiable bearer token rode the call, else "local". A verify failure here
// never blocks the action (the interceptor already authorized it); it only
// means the audit row records "local".
func (s *governanceService) actor(ctx context.Context) string {
	if s.authenticator == nil {
		return actorLocal
	}

	token, err := bearerToken(ctx)
	if err != nil {
		return actorLocal
	}

	claims, err := s.authenticator.Verify(ctx, token)
	if err != nil || claims.Subject == "" {
		return actorLocal
	}

	return claims.Subject
}
