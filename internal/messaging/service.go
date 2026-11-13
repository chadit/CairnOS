package messaging

import (
	"context"
	"strings"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"

	"github.com/chadit/CairnOS/internal/api"
	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
)

// Send outcome labels for cairnos_messaging_sends_total. Only attempted sends
// are counted: a request rejected for a bad target never reached a transport,
// so counting it would read as a delivery failure on the dashboard.
const (
	outcomeOK    = "ok"
	outcomeError = "error"
)

// Metrics is the telemetry slice the service feeds; *telemetry.Recorder
// satisfies it. Declared here rather than imported so this package does not
// depend on internal/telemetry, and so a test can substitute a recorder that
// captures the labels.
type Metrics interface {
	MessagingSend(ctx context.Context, identity, outcome string)
}

// Service implements cairnos.v1.MessagingService over the identity registry.
type Service struct {
	cairnosv1.UnimplementedMessagingServiceServer

	registry *Registry
	metrics  Metrics
}

// NewService builds the service over a registry. metrics may be nil when serve
// runs without telemetry configured.
func NewService(registry *Registry, metrics Metrics) *Service {
	return &Service{registry: registry, metrics: metrics}
}

// SendMessage posts one message as the named identity. The text goes to the
// transport unmodified so Mattermost renders any mentions it carries.
func (s *Service) SendMessage(
	ctx context.Context,
	req *cairnosv1.MessagingServiceSendMessageRequest,
) (*cairnosv1.MessagingServiceSendMessageResponse, error) {
	identity, target, err := parseSendRequest(req)
	if err != nil {
		return nil, err
	}

	sender, known := s.registry.Get(identity)
	if !known {
		return nil, wireIdentityNotFound(identity)
	}

	ref, err := sender.Send(ctx, target, req.GetText(), req.GetThreadId())
	if err != nil {
		s.record(ctx, identity, outcomeError)

		return nil, wireStatus(err)
	}

	s.record(ctx, identity, outcomeOK)

	return &cairnosv1.MessagingServiceSendMessageResponse{
		PostId:    ref.PostID,
		ChannelId: ref.ChannelID,
	}, nil
}

// ListIdentities lists the identities a caller may send as. The ctx check
// keeps an already-canceled call from doing the work, matching ListRunners;
// the listing itself is local.
func (s *Service) ListIdentities(
	ctx context.Context,
	_ *cairnosv1.ListIdentitiesRequest,
) (*cairnosv1.ListIdentitiesResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, wireStatus(err)
	}

	known := s.registry.List()

	identities := make([]*cairnosv1.Identity, 0, len(known))
	for _, identity := range known {
		identities = append(identities, &cairnosv1.Identity{
			Name:        identity.Name,
			Transport:   identity.Transport,
			Username:    identity.Username,
			DisplayName: identity.DisplayName,
			Url:         identity.URL,
		})
	}

	return &cairnosv1.ListIdentitiesResponse{Identities: identities}, nil
}

// parseSendRequest validates one send request and returns the trimmed
// identity name and the parsed target. It runs before the registry lookup, so
// a malformed request is rejected as invalid whether or not its identity exists.
func parseSendRequest(req *cairnosv1.MessagingServiceSendMessageRequest) (string, Target, error) {
	identity := strings.TrimSpace(req.GetIdentity())
	if identity == "" {
		return "", Target{}, wireInvalidArgument("identity is required")
	}

	if strings.TrimSpace(req.GetText()) == "" {
		return "", Target{}, wireInvalidArgument("text is required")
	}

	target, err := ParseTarget(req.GetTo())
	if err != nil {
		return "", Target{}, wireInvalidArgument("%s", err.Error())
	}

	return identity, target, nil
}

// record feeds one attempted send to telemetry when a recorder is wired.
func (s *Service) record(ctx context.Context, identity, outcome string) {
	if s.metrics == nil {
		return
	}

	s.metrics.MessagingSend(ctx, identity, outcome)
}

// Extension builds the api.Extension serve wiring mounts. The extension shape
// keeps internal/api free of any dependency on this package, matching how A2A
// arrives (internal/a2a/bundle.go).
func Extension(registry *Registry, metrics Metrics) *api.Extension {
	service := NewService(registry, metrics)

	return &api.Extension{
		Name: "messaging",
		RegisterGRPC: func(server *grpc.Server) {
			cairnosv1.RegisterMessagingServiceServer(server, service)
		},
		RegisterGateway: registerGateway,
	}
}

// registerGateway adapts the generated gateway registrar to the Extension
// hook, so the server's gateway wiring reports which handler failed.
//
//nolint:wrapcheck // the generated registrar returns a plain error; the api server wraps it with the handler context
func registerGateway(ctx context.Context, mux *runtime.ServeMux, conn *grpc.ClientConn) error {
	return cairnosv1.RegisterMessagingServiceHandler(ctx, mux, conn)
}
