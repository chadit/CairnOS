package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/messaging"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/telemetry"
	"github.com/chadit/CairnOS/pkg/runner"
)

// messagingRoute is the gateway path the send service answers on, printed at
// startup beside the other listeners so an operator sees it without reading the
// swagger.
const messagingRoute = "/v1/messages"

// attachMessagingServe mounts the send service on the API server, so the CLI,
// external engines, and any other HTTP caller reach the same identities the bridge answers
// on. It mounts unconditionally: the registry is still empty here because the
// bridges connect later, and an empty registry answers ListIdentities with an
// empty list rather than a missing route.
func attachMessagingServe(apiCfg *api.Config, registry *messaging.Registry, tel *telemetry.Telemetry) {
	apiCfg.Extensions = append(apiCfg.Extensions, messaging.Extension(registry, tel.Recorder()))
}

// messageSenderAdapter maps the identity registry onto the narrow seam the
// message runner drives, keeping internal/runner/builtins free of any messaging
// import the way bridgeMetricsAdapter keeps internal/bridge free of telemetry.
type messageSenderAdapter struct {
	registry *messaging.Registry
	logger   *slog.Logger
}

// Send resolves the identity and the target, then posts. Both resolution
// failures are permanent: a retry sends the same unknown identity to the same
// unparseable target, so the engine should fail the action rather than queue it
// again.
func (s *messageSenderAdapter) Send(ctx context.Context, identity, recipient, text, thread string) (string, error) {
	sender, known := s.registry.Get(identity)
	if !known {
		return "", runner.NewError(runner.ClassPermanent, "message.identity_not_found",
			fmt.Sprintf("no messaging identity named %q is configured", identity))
	}

	target, err := messaging.ParseTarget(recipient)
	if err != nil {
		return "", runner.NewError(runner.ClassPermanent, "message.bad_target", err.Error())
	}

	ref, err := sender.Send(ctx, target, text, thread)
	if err != nil {
		s.logger.WarnContext(ctx, "message runner send failed",
			"identity", identity, "target", target.String(), "error", err)

		if errors.Is(err, messaging.ErrThreadNotFound) {
			err = fmt.Errorf("%w: %w", builtins.ErrMessageThreadMissing, err)
		}

		return "", fmt.Errorf("send as %q to %s: %w", identity, target, err)
	}

	return ref.PostID, nil
}
