package mattermost

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/chadit/CairnOS/internal/messaging"
)

// channelCacheSize bounds the resolved-target cache. A serve posts to a handful
// of channels and people over and over, so a small map removes nearly every
// lookup round trip while keeping the memory a fixed cost.
const channelCacheSize = 128

// defaultChannelCacheTTL bounds how long a resolved #name keeps pointing at one
// channel id. Rename #alerts and create a fresh one under the old name and every
// later post lands in the archive with a 200 and a post id, so held-job alerts
// go somewhere nobody reads until the process restarts.
const defaultChannelCacheTTL = 10 * time.Minute

// Senders pairs each identity name with its send surface, so serve registers the
// whole set in the messaging registry in one pass. Every outbound path (bridge
// reply, send service, CLI, workflow runner, notify sink) then posts through the
// same client and the same shared rate bucket.
func (b *Bridge) Senders() map[string]messaging.Sender {
	senders := make(map[string]messaging.Sender, len(b.identities))
	for _, agent := range b.identities {
		senders[agent.name] = agent
	}

	return senders
}

// Describe reports the identity as a send target: the name a caller addresses it
// by, the account a recipient sees, and the server it posts to, so an operator
// running two Mattermost servers can tell two same-named bots apart.
func (i *identity) Describe() messaging.Identity {
	return messaging.Identity{
		Name:        i.name,
		Transport:   transportName,
		Username:    i.cfg.Username,
		DisplayName: i.cfg.DisplayName,
		URL:         i.serverURL,
	}
}

// Send posts text to a parsed target, as a reply on threadID when one is given.
// Names resolve to ids through the cache, so a workflow posting to the same
// channel every run pays for one lookup rather than one per post.
func (i *identity) Send(ctx context.Context, target messaging.Target, text, threadID string) (messaging.PostRef, error) {
	channelID, err := i.resolveTarget(ctx, target)
	if err != nil {
		return messaging.PostRef{}, err
	}

	post, err := i.client.CreatePost(ctx, channelID, threadID, text)
	if err != nil {
		if threadID != "" && isNotFound(err) {
			err = fmt.Errorf("%w %s: %w", messaging.ErrThreadNotFound, threadID, err)
		}

		return messaging.PostRef{}, fmt.Errorf("mattermost identity %q: post to %s: %w", i.name, target, err)
	}

	return messaging.PostRef{PostID: post.ID, ChannelID: post.ChannelID}, nil
}

// isNotFound reports a post refused because something it names is gone. A reply
// reads that as its root, since a missing channel fails the fresh root the same way.
func isNotFound(err error) bool {
	failure, isAPIError := errors.AsType[*APIError](err)

	return isAPIError && failure.Class == ClassNotFound
}

// resolveTarget turns one parsed target into the channel to post in. An id form
// needs no call at all; a name form needs one lookup, and a user form needs the
// DM channel between that user and this identity.
func (i *identity) resolveTarget(ctx context.Context, target messaging.Target) (string, error) {
	if target.Kind == messaging.KindChannelID {
		return target.Value, nil
	}

	now := time.Now()

	if cached, known := i.channels.get(target.String(), now); known {
		return cached, nil
	}

	channelID, err := i.lookupTarget(ctx, target)
	if err != nil {
		return "", err
	}

	i.channels.put(target.String(), channelID, now)

	return channelID, nil
}

// lookupTarget performs the round trips resolveTarget caches the answer to.
func (i *identity) lookupTarget(ctx context.Context, target messaging.Target) (string, error) {
	switch target.Kind {
	case messaging.KindChannelName:
		return i.lookupChannelName(ctx, target.Value)
	case messaging.KindUserID:
		return i.lookupDirect(ctx, target.Value)
	case messaging.KindUsername:
		return i.lookupUsername(ctx, target.Value)
	case messaging.KindChannelID:
		return target.Value, nil
	default:
		return "", fmt.Errorf("mattermost identity %q: %w: %s", i.name, messaging.ErrTargetUnrecognized, target)
	}
}

// lookupChannelName resolves a #name against the team the server block names.
func (i *identity) lookupChannelName(ctx context.Context, name string) (string, error) {
	channel, err := i.client.ChannelByName(ctx, i.team, name)
	if err != nil {
		return "", fmt.Errorf("mattermost identity %q: resolve channel %q: %w", i.name, name, err)
	}

	return channel.ID, nil
}

// lookupUsername resolves a @username to its DM channel with this identity.
func (i *identity) lookupUsername(ctx context.Context, username string) (string, error) {
	user, err := i.client.UserByUsername(ctx, username)
	if err != nil {
		return "", fmt.Errorf("mattermost identity %q: resolve user %q: %w", i.name, username, err)
	}

	return i.lookupDirect(ctx, user.ID)
}

// lookupDirect opens, or finds, the DM channel between this identity and a user.
func (i *identity) lookupDirect(ctx context.Context, userID string) (string, error) {
	channel, err := i.client.DirectChannel(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("mattermost identity %q: open dm with %q: %w", i.name, userID, err)
	}

	return channel.ID, nil
}

// channelCache maps a send target to the channel id it resolved to. It is a
// bounded map rather than the client's LRU because these entries are equally
// valuable: a serve posts to a small fixed set of destinations, so the whole set
// is dropped on overflow instead of tracking a recency order nothing reads.
type channelCache struct {
	byTarget map[string]cachedChannel
	ttl      time.Duration
	mu       sync.Mutex
}

// cachedChannel is one resolved target and the moment it stops being trusted.
type cachedChannel struct {
	expires   time.Time
	channelID string
}

// newChannelCache builds an empty cache; a non-positive ttl takes the default.
func newChannelCache(ttl time.Duration) *channelCache {
	if ttl <= 0 {
		ttl = defaultChannelCacheTTL
	}

	return &channelCache{byTarget: make(map[string]cachedChannel, channelCacheSize), ttl: ttl}
}

// get reads one resolved target that has not aged out.
func (c *channelCache) get(target string, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, known := c.byTarget[target]
	if !known || now.After(entry.expires) {
		return "", false
	}

	return entry.channelID, true
}

// put records one resolved target, clearing the cache first when it is full.
func (c *channelCache) put(target, channelID string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.byTarget) >= channelCacheSize {
		c.byTarget = make(map[string]cachedChannel, channelCacheSize)
	}

	c.byTarget[target] = cachedChannel{channelID: channelID, expires: now.Add(c.ttl)}
}
