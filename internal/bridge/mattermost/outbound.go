package mattermost

import (
	"context"
	"strconv"
	"time"

	"github.com/chadit/CairnOS/internal/bridge"
	"github.com/chadit/CairnOS/internal/chat"
)

const (
	// msgExpired is posted in the proposal's thread once its TTL lapses, so the
	// operator sees the decision settled rather than still open. Mattermost has
	// no inline keyboard to rewrite, so this is a new post rather than an edit.
	msgExpired = "This proposal expired; nothing was run."

	// approveInstruction opens the line that tells a reader how to decide. The
	// shared proposal body ends by asking for a decision without saying how, and
	// a button-less transport has to spell out the two words and the deadline
	// (FR21).
	approveInstruction = "Reply `approve` or `deny` within "
)

// Session is the identity itself: who the token belongs to and what the server
// lets that account post. Connect learns both, and it runs on every reconnect,
// so the two answers always come from the same pass.
type Session interface {
	// Connect learns the identity's own user id, username, and post-size limit.
	Connect(ctx context.Context) error

	// Me is who the token belongs to, as the last Connect read it.
	Me() User

	// MaxPostSize is the server's post-length ceiling in runes.
	MaxPostSize() int
}

// Listener is the event socket.
type Listener interface {
	// Listen holds one event socket and forwards posted events until it fails.
	Listen(ctx context.Context, events chan<- Event) error
}

// Poster is what one reply costs: the post itself and the indicator that says
// it is coming.
type Poster interface {
	// CreatePost posts a message, optionally as a reply on rootID's thread.
	CreatePost(ctx context.Context, channelID, rootID, message string) (Post, error)

	// Typing raises the transient typing indicator; failures are ignored.
	Typing(ctx context.Context, channelID, parentID string) error

	// AddReaction acknowledges a post with an emoji; failures are ignored.
	AddReaction(ctx context.Context, postID, emojiName string) error
}

// Channels resolves the two channel forms a send target can name.
type Channels interface {
	// DirectChannel opens, or finds, the DM channel with another user.
	DirectChannel(ctx context.Context, otherUserID string) (Channel, error)

	// ChannelByName resolves a team-scoped channel name to a channel.
	ChannelByName(ctx context.Context, teamName, name string) (Channel, error)
}

// Users resolves post authors and send targets. CachedUser is separate because
// the event path cannot afford a round trip: the socket has one consumer.
type Users interface {
	// UserByUsername resolves a username to a user.
	UserByUsername(ctx context.Context, name string) (User, error)

	// User resolves a user id to a user, through the client's is_bot cache.
	User(ctx context.Context, id string) (User, error)

	// CachedUser reads the is_bot cache without a round trip.
	CachedUser(id string) (User, bool)
}

// Directory is every lookup that turns a name or an id into a channel or a user,
// the half of Conn that resolves rather than posts.
type Directory interface {
	Channels
	Users
}

// Conn is the whole slice of Client one identity's runtime drives, composed from
// the capabilities above. It exists so the bridge tests run against a scripted
// server without an HTTP listener, the way the Telegram adapter tests run
// against a fake BotAPI.
type Conn interface {
	Session
	Listener
	Poster
	Directory
}

// outbound is the Mattermost side of bridge.Outbound: it turns the core's
// neutral sends into posts on the address the gate resolved. It holds no
// per-conversation state, so one value serves every conversation an identity
// handles.
type outbound struct {
	client Conn

	// confirmTTL is named in the proposal's instruction line, so a reader sees
	// the deadline the core's timer is already counting down.
	confirmTTL time.Duration
}

// Send posts one message and returns the created post id as the core's opaque
// reference.
func (o *outbound) Send(ctx context.Context, addr *bridge.Address, text string) (string, error) {
	post, err := o.client.CreatePost(ctx, addr.Channel, addr.Thread, text)
	if err != nil {
		//nolint:wrapcheck // the client returns a classified *APIError that already names CreatePost and its status
		return "", err
	}

	return post.ID, nil
}

// SendProposal posts the shared proposal body plus the line that names the two
// words and the deadline a reader answers with. Mattermost has no button
// affordance, so the typed approve or deny the core already understands is the
// whole decision path here (design D4).
func (o *outbound) SendProposal(
	ctx context.Context,
	addr *bridge.Address,
	_ string,
	proposal *chat.Proposal,
) (string, error) {
	text := bridge.ProposalText(proposal) + "\n" + approveInstruction + shortDuration(o.confirmTTL)

	post, err := o.client.CreatePost(ctx, addr.Channel, addr.Thread, text)
	if err != nil {
		//nolint:wrapcheck // the client returns a classified *APIError that already names CreatePost and its status
		return "", err
	}

	return post.ID, nil
}

// Expired posts the lapsed notice into the proposal's own conversation. The
// reference the core hands back is the proposal's post id, which is not needed:
// Mattermost threads hang off a root, and addr.Thread already names it, so the
// notice lands where the proposal is being read.
func (o *outbound) Expired(ctx context.Context, addr *bridge.Address, _ string) error {
	_, err := o.client.CreatePost(ctx, addr.Channel, addr.Thread, msgExpired)

	//nolint:wrapcheck // the client returns a classified *APIError that already names CreatePost and its status
	return err
}

// MaxChars is the server's own post ceiling, read at connect, so a reply splits
// at the width this server enforces rather than a compiled-in guess (FR18).
func (o *outbound) MaxChars() int {
	return o.client.MaxPostSize()
}

// shortDuration renders a whole-hour or whole-minute span without the zero tail
// time.Duration prints, so a ten-minute TTL reads as "10m" in the line an
// operator answers rather than "10m0s".
func shortDuration(span time.Duration) string {
	switch {
	case span >= time.Hour && span%time.Hour == 0:
		return strconv.FormatInt(int64(span/time.Hour), 10) + "h"
	case span >= time.Minute && span%time.Minute == 0:
		return strconv.FormatInt(int64(span/time.Minute), 10) + "m"
	default:
		return span.String()
	}
}
