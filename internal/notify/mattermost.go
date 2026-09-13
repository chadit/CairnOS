package notify

import (
	"context"
	"fmt"

	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/messaging"
)

// mattermostPoster posts fan-out lines into one Mattermost channel as a named
// identity, through the same send path the CLI and the workflow runner use
// (FR34). It holds the registry rather than a resolved Sender because the
// bridge registers its identities after serve builds the fan-out, and a
// reconnect replaces the sender in place.
type mattermostPoster struct {
	registry *messaging.Registry
	identity string
	channel  string
}

// MattermostSink builds the Poster half of a Mattermost notify sink: every line
// it receives is posted to channel as identity. The identity is looked up per
// post, so a bridge that connects after serve started, or reconnects and
// re-registers, starts working without rebuilding the fan-out. A post before the
// identity registers fails with the NotFound the send service returns.
func MattermostSink(registry *messaging.Registry, identity, channel string) Poster {
	return &mattermostPoster{registry: registry, identity: identity, channel: channel}
}

// Post sends content as one Mattermost message. The channel is parsed per post
// rather than at build time so the constructor stays total; config validation is
// where a malformed target is meant to be caught, and this error path is the
// backstop.
func (p *mattermostPoster) Post(ctx context.Context, content string) error {
	sender, known := p.registry.Get(p.identity)
	if !known {
		//nolint:wrapcheck // api.WireIdentityNotFound returns a gRPC status error; wrapping it would mask the code
		return api.WireIdentityNotFound(p.identity)
	}

	target, err := messaging.ParseTarget(p.channel)
	if err != nil {
		return fmt.Errorf("notify mattermost sink: %w", err)
	}

	// Thread id is empty: a notification is a new post, never a reply.
	if _, err := sender.Send(ctx, target, content, ""); err != nil {
		return fmt.Errorf("notify mattermost sink: post to %s as %q: %w", target, p.identity, err)
	}

	return nil
}
