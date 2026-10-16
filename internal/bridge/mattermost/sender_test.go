package mattermost_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/bridge/mattermost"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/messaging"
)

// errLookup is what countingConn returns once a test makes a lookup fail.
var errLookup = errors.New("lookup refused")

// countingConn wraps fakeConn to count channel-name lookups and to fail them on
// demand, which is what the cache assertion and the error assertion need beyond
// the recording the base fake already does.
type countingConn struct {
	*fakeConn

	channelLookups    int
	failChannelByName bool
}

// newCountingConn builds a counting connection answering as one bot account.
func newCountingConn() *countingConn {
	return &countingConn{fakeConn: newFakeConn(botUserID, botUsername)}
}

// ChannelByName counts the call and answers from the base fake's channel table.
func (c *countingConn) ChannelByName(ctx context.Context, team, name string) (mattermost.Channel, error) {
	c.channelLookups++

	if c.failChannelByName {
		return mattermost.Channel{}, errLookup
	}

	return c.fakeConn.ChannelByName(ctx, team, name)
}

// bridgeFor builds a bridge over one connection with the target cache on its
// default window. It fails the case unless the ops identity registered a send
// surface, so a case can index Senders()[identityOps] directly.
func bridgeFor(t *testing.T, conn mattermost.Conn) *mattermost.Bridge {
	t.Helper()

	return bridgeWithCacheTTL(t, conn, 0)
}

// bridgeWithCacheTTL is bridgeFor with the resolved-target window spelled out,
// for the case that asserts on re-resolution.
func bridgeWithCacheTTL(t *testing.T, conn mattermost.Conn, cacheTTL time.Duration) *mattermost.Bridge {
	t.Helper()

	built := mattermost.New(&mattermost.Config{
		Core:            nil,
		Metrics:         nil,
		Logger:          quietLogger(),
		Identities:      []mattermost.Identity{opsIdentity(conn)},
		ChannelCacheTTL: cacheTTL,
		Server: config.Mattermost{
			URL: serverURL, Team: teamName, ConfirmTTL: 10 * time.Minute,
		},
	})

	if _, registered := built.Senders()[identityOps]; !registered {
		t.Fatalf("identity %q has no send surface; registered: %+v", identityOps, built.Senders())
	}

	return built
}

// TestSenderDescribesIdentity proves the description a listing and the A2A card
// read carries the account a recipient sees and the server it posts to.
func TestSenderDescribesIdentity(t *testing.T) {
	t.Parallel()

	got := bridgeFor(t, newFakeConn(botUserID, botUsername)).Senders()[identityOps].Describe()

	want := messaging.Identity{
		Name:        identityOps,
		Transport:   "mattermost",
		Username:    botUsername,
		DisplayName: botDisplayName,
		URL:         serverURL,
	}

	if got != want {
		t.Fatalf("Describe() = %+v, want %+v", got, want)
	}
}

// TestSenderResolvesEveryTargetForm proves each of the four accepted target
// forms reaches the right channel: an id posts with no lookup, a name resolves
// against the team, and both user forms post into the DM channel (FR27, FR28).
func TestSenderResolvesEveryTargetForm(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		target      string
		wantChannel string
	}{
		{name: "channel id", target: "channel:" + channelID, wantChannel: channelID},
		{name: "channel name", target: "#" + channelName, wantChannel: channelID},
		{name: "user id", target: "user:" + humanUserID, wantChannel: "dm-" + humanUserID},
		{name: "username", target: "@" + humanName, wantChannel: "dm-id-" + humanName},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			conn := newFakeConn(botUserID, botUsername)
			conn.channels[channelName] = mattermost.Channel{ID: channelID, Name: channelName}

			target, err := messaging.ParseTarget(testCase.target)
			if err != nil {
				t.Fatalf("ParseTarget(%q): %v", testCase.target, err)
			}

			ref, err := bridgeFor(t, conn).Senders()[identityOps].Send(t.Context(), target, "hello", "")
			if err != nil {
				t.Fatalf("Send to %q: %v", testCase.target, err)
			}

			if ref.ChannelID != testCase.wantChannel {
				t.Fatalf("posted to channel %q, want %q", ref.ChannelID, testCase.wantChannel)
			}

			if ref.PostID == "" {
				t.Fatalf("Send returned no post id, so a caller cannot thread a follow-up")
			}
		})
	}
}

// TestSenderPassesThreadThrough proves a thread id reaches the post as its root,
// which is how a workflow answers in the conversation it was told to.
func TestSenderPassesThreadThrough(t *testing.T) {
	t.Parallel()

	conn := newFakeConn(botUserID, botUsername)
	target := messaging.Target{Kind: messaging.KindChannelID, Value: channelID}

	if _, err := bridgeFor(t, conn).Senders()[identityOps].Send(t.Context(), target, "in thread", "root-9"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	posts := conn.Posts()
	if len(posts) != 1 || posts[0].RootID != "root-9" {
		t.Fatalf("posts = %+v, want one reply rooted on root-9", posts)
	}
}

// TestSenderCachesNameLookups proves a repeated destination costs one lookup, so
// a workflow posting to the same channel every run does not re-resolve it.
func TestSenderCachesNameLookups(t *testing.T) {
	t.Parallel()

	conn := newCountingConn()
	conn.channels[channelName] = mattermost.Channel{ID: channelID, Name: channelName}

	sender := bridgeFor(t, conn).Senders()[identityOps]
	target := messaging.Target{Kind: messaging.KindChannelName, Value: channelName}

	for range 3 {
		if _, err := sender.Send(t.Context(), target, "hello", ""); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	if conn.channelLookups != 1 {
		t.Fatalf("channel name resolved %d times, want 1", conn.channelLookups)
	}

	if got := len(conn.Posts()); got != 3 {
		t.Fatalf("posted %d times, want 3", got)
	}
}

// TestSenderReportsLookupFailure proves a destination that will not resolve
// surfaces as an error naming the identity and the target, rather than a post
// silently going nowhere.
func TestSenderReportsLookupFailure(t *testing.T) {
	t.Parallel()

	conn := newCountingConn()
	conn.failChannelByName = true

	target := messaging.Target{Kind: messaging.KindChannelName, Value: "missing"}

	_, err := bridgeFor(t, conn).Senders()[identityOps].Send(t.Context(), target, "hello", "")
	if !errors.Is(err, errLookup) {
		t.Fatalf("Send error = %v, want it to wrap the lookup failure", err)
	}

	if got := len(conn.Posts()); got != 0 {
		t.Fatalf("a failed lookup still posted %+v", got)
	}
}

// TestSenderReresolvesAStaleNameEntry proves a cached #name stops being trusted.
// The entry used to live for the life of the process, so renaming #alerts and
// creating a fresh one under the old name sent every later post into the archive
// with a 200 and a post id, and held-job alerts landed where nobody looks.
func TestSenderReresolvesAStaleNameEntry(t *testing.T) {
	t.Parallel()

	// A one-nanosecond window makes every entry stale by the time the next call
	// reads it, so the case proves re-resolution without waiting on a clock.
	const (
		cacheTTL = time.Nanosecond

		// reusedNameChannelID is the fresh channel that takes over the old name.
		reusedNameChannelID = "chan-2"
	)

	conn := newCountingConn()
	conn.channels[channelName] = mattermost.Channel{ID: channelID, Name: channelName}

	sender := bridgeWithCacheTTL(t, conn, cacheTTL).Senders()[identityOps]
	target := messaging.Target{Kind: messaging.KindChannelName, Value: channelName}

	if _, err := sender.Send(t.Context(), target, "hello", ""); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// The channel is renamed and a different one takes the name, which is exactly
	// what the cached id cannot see.
	conn.channels[channelName] = mattermost.Channel{ID: reusedNameChannelID, Name: channelName}

	if _, err := sender.Send(t.Context(), target, "hello again", ""); err != nil {
		t.Fatalf("Send after the window: %v", err)
	}

	if conn.channelLookups != 2 {
		t.Fatalf("channel name resolved %d times, want a second lookup once the entry aged out", conn.channelLookups)
	}

	posts := conn.Posts()
	if len(posts) != 2 || posts[1].ChannelID != reusedNameChannelID {
		t.Fatalf("posts = %+v, want the second one in the re-resolved channel", posts)
	}
}

// goneRootConn fails every post with the not-found class the client gives a
// missing post, which is what a reply to a deleted root meets.
type goneRootConn struct {
	*fakeConn
}

// CreatePost refuses the post the way the server refuses a reply to a gone root.
func (*goneRootConn) CreatePost(_ context.Context, _, _, _ string) (mattermost.Post, error) {
	return mattermost.Post{}, &mattermost.APIError{Class: mattermost.ClassNotFound, Method: createPostMethod, Status: 404}
}

func TestSenderReportsAMissingThreadRoot(t *testing.T) {
	t.Parallel()

	sender := bridgeFor(t, &goneRootConn{fakeConn: newFakeConn(botUserID, botUsername)}).Senders()[identityOps]
	target := messaging.Target{Kind: messaging.KindChannelID, Value: channelID}

	if _, err := sender.Send(t.Context(), target, "in thread", "root-9"); !errors.Is(err, messaging.ErrThreadNotFound) {
		t.Fatalf("reply error = %v, want ErrThreadNotFound", err)
	}

	if _, err := sender.Send(t.Context(), target, "no thread", ""); errors.Is(err, messaging.ErrThreadNotFound) {
		t.Fatalf("root post error = %v, want a plain not-found since no thread was named", err)
	}
}
