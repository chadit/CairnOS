package api

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/reliability"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/pkg/runner"
)

// healthOK is the Health answer when the store responds.
const healthOK = "ok"

// DefaultDrainTimeout bounds a drain a caller gave no ceiling for. It is long
// enough for an agent run that talks to a slow model and short enough that a
// stuck run cannot hold an update open all afternoon.
const DefaultDrainTimeout = 15 * time.Minute

// adminService implements cairnos.v1.AdminService.
type adminService struct {
	cairnosv1.UnimplementedAdminServiceServer

	store    store.Store
	registry *runner.Registry
	breakers *reliability.BreakerSet
	manager  *Manager
	drain    *Drain
	logger   *slog.Logger
}

// Health reports liveness by proving the store answers a query, matching
// the CLI health command. No auth in any phase.
func (s *adminService) Health(ctx context.Context, _ *cairnosv1.HealthRequest) (*cairnosv1.HealthResponse, error) {
	if _, err := s.store.ListRuns(ctx, store.ListFilter{Limit: 1}); err != nil {
		return nil, canonicalStatus(err)
	}

	return &cairnosv1.HealthResponse{Status: healthOK}, nil
}

// ListRunners lists the registered runner names. The ctx check keeps an
// already-canceled call from doing the work; the listing itself is local.
func (s *adminService) ListRunners(ctx context.Context, _ *cairnosv1.ListRunnersRequest) (*cairnosv1.ListRunnersResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, canonicalStatus(err)
	}

	names := s.registry.List()

	runners := make([]*cairnosv1.RunnerInfo, 0, len(names))
	for _, name := range names {
		runners = append(runners, &cairnosv1.RunnerInfo{Name: name})
	}

	return &cairnosv1.ListRunnersResponse{Runners: runners}, nil
}

// ResetBreaker forces one runner's circuit breaker CLOSED: the operator
// override for a breaker stuck open after an outage the operator knows has
// ended. A runner with no breaker (never called, or unknown) answers
// NOT_FOUND rather than pretending the reset did anything; breakers exist
// only once their runner has seen traffic.
func (s *adminService) ResetBreaker(ctx context.Context, req *cairnosv1.ResetBreakerRequest) (*cairnosv1.ResetBreakerResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, canonicalStatus(err)
	}

	if s.breakers == nil {
		return nil, errorStatus(codes.Unimplemented,
			cairnerr.Newf(codeUnimplemented,
				"breaker reset is unavailable: this server was wired without engine breaker access; runner %q was not touched", req.GetRunner()))
	}

	name := req.GetRunner()
	if !s.breakers.Reset(name) {
		return nil, errorStatus(codes.NotFound,
			cairnerr.Newf(codeBreakerNotFound,
				"runner %q has no circuit breaker: breakers are created on a runner's first call", name))
	}

	return &cairnosv1.ResetBreakerResponse{
		Runner: name,
		State:  reliability.StateClosed.String(),
	}, nil
}

// RequestRestart flips the process into draining so `cairnos update` can swap
// the binary without cutting a run short: the scheduler stops dispatching, the
// runs already going finish, and the process then exits for the supervisor to
// start again. A second request while draining reports the state rather than
// failing, so a retry is harmless.
func (s *adminService) RequestRestart(ctx context.Context, req *cairnosv1.RequestRestartRequest) (*cairnosv1.RequestRestartResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, canonicalStatus(err)
	}

	if s.drain == nil {
		return nil, errorStatus(codes.Unimplemented,
			cairnerr.New(codeUnimplemented,
				"restart is unavailable: this server was wired without a drain, so it cannot exit on request"))
	}

	var inFlight int
	if s.manager != nil {
		inFlight = s.manager.InFlight()
	}

	deadline := s.drain.Request(drainWait(req.GetMaxWaitSeconds()), time.Now())

	s.logger.Info("restart requested; draining",
		"in_flight", inFlight, "deadline", deadline)

	return &cairnosv1.RequestRestartResponse{
		InFlight: int64(inFlight),
		Accepted: true,
	}, nil
}

// drainWait turns the request's ceiling into a duration, mapping zero and any
// negative value onto the server default so a caller with no opinion sends 0.
func drainWait(seconds int64) time.Duration {
	if seconds <= 0 {
		return DefaultDrainTimeout
	}

	return time.Duration(seconds) * time.Second
}
