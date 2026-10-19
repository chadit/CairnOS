package mattermost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// apiPrefix prefixes every REST path and the WebSocket endpoint.
	apiPrefix = "/api/v4"

	// defaultTimeout bounds one REST call. The WebSocket read has no such
	// bound: it is idle by design between posts.
	defaultTimeout = 30 * time.Second

	// defaultMaxPostSize is the fallback when the server's client config is
	// unreadable. Every released server accepts at least this many runes, so a
	// reply split at this width always lands (FR5, edge case 10).
	defaultMaxPostSize = 4000

	// configKeyMaxPostSize names the field in the old-format client config.
	// The old format is a flat map of string values with PascalCase keys, so
	// this key is read rather than bound to a struct tag.
	configKeyMaxPostSize = "MaxPostSize"

	// userCacheSize bounds the is_bot cache. The gate looks up the author of
	// every automated post, and a channel's worth of webhooks is a handful of
	// ids repeated, so a small cache removes nearly all of those round trips.
	userCacheSize = 256

	// maxResponseBytes caps a decoded response body. The largest answer this
	// client reads is a client config of a few kilobytes, so a megabyte is
	// generous, and without a cap a hostile or broken server streams JSON until
	// the request timeout.
	maxResponseBytes = 1 << 20

	// defaultPingInterval is how often an idle event socket is probed. The probe
	// deadline is a third of it.
	defaultPingInterval = 30 * time.Second

	// maxRateLimitRetries bounds the 429 retry at a single attempt. The park the
	// first 429 bought is spent by that retry's own wait, so parking again on a
	// retry that also failed would hold every identity on this server for a
	// second full window on one server answer. The caller counts the failure and
	// the next call rediscovers the limit.
	maxRateLimitRetries = 1
)

// The schemes a base URL may use. Plain http needs the InsecureHTTP opt-in, and
// the event socket dials the WebSocket twin of whichever one the URL names.
const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

// Each REST method's name as an APIError reports it. It is the Client method's
// own name, so an operator reading a failure sees which call broke without
// seeing the URL.
const (
	methodConnect        = "Connect"
	methodCreatePost     = "CreatePost"
	methodDirectChannel  = "DirectChannel"
	methodUserByUsername = "UserByUsername"
	methodUser           = "User"
	methodChannelByName  = "ChannelByName"
	methodTyping         = "Typing"
	methodAddReaction    = "AddReaction"
	methodListen         = "Listen"
)

// ClientConfig builds a Client. URL and Token are required; everything else has
// a working default.
type ClientConfig struct {
	// Limiter is the bucket shared by every identity on this server. A nil
	// limiter throttles nothing, which is what the unit tests want.
	Limiter *Bucket

	// Log receives the client's own lines: a warning about an unreadable post
	// size and debug lines from the event socket. nil means slog.Default.
	Log *slog.Logger

	URL   string
	Token string

	// Timeout bounds one REST call; zero means defaultTimeout.
	Timeout time.Duration

	// KeepaliveInterval is how often the event socket is probed while idle; zero
	// means defaultPingInterval. Tests shorten it so a half-open socket is
	// detected without waiting out the real interval.
	KeepaliveInterval time.Duration

	// InsecureHTTP permits a plain-http server URL. It exists for a test
	// server and a lab instance; over http the bearer token is on the wire in
	// the clear.
	InsecureHTTP bool
}

// Client talks to one Mattermost server as one bot identity. Safe for
// concurrent use: the send path, the gate's user lookups, and a reconnect that
// re-reads the identity all run on different goroutines.
type Client struct {
	// The embedded cache supplies CachedUser, which the event path calls
	// because its socket has a single consumer: a blocking lookup there stalls
	// the read that would have delivered the next post.
	*userCache

	http    *http.Client
	wsHTTP  *http.Client
	limiter *Bucket
	log     *slog.Logger
	base    *url.URL

	token string

	// me and maxPost are guarded by mu, which sits at the end because the
	// alignment check orders these fields, not their grouping by purpose.
	me User

	timeout time.Duration

	// pingEvery is the idle-socket probe interval; ws.go owns the probe itself.
	pingEvery time.Duration

	maxPost int

	mu sync.RWMutex
}

// NewClient validates the config and builds a client. It performs no I/O; the
// identity and the server's limits are learned by Connect.
func NewClient(cfg ClientConfig) (*Client, error) {
	return NewClientOver(cfg, nil)
}

// NewClientOver is NewClient over a caller-supplied round tripper, which
// carries both the REST calls and the socket upgrade. A nil transport keeps
// the default one; a test hands over an in-memory transport so its suite opens
// no loopback socket.
func NewClientOver(cfg ClientConfig, transport http.RoundTripper) (*Client, error) {
	base, err := parseServerURL(cfg.URL, cfg.InsecureHTTP)
	if err != nil {
		return nil, err
	}

	if cfg.Token == "" {
		return nil, ErrNoToken
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}

	ping := cfg.KeepaliveInterval
	if ping <= 0 {
		ping = defaultPingInterval
	}

	return &Client{
		// A 3xx must not be followed. Go re-issues a 301, 302, or 303 as a GET
		// with the body dropped, so a proxy redirecting POST /posts would turn a
		// reply into a read, answer 200, and report a delivered post that never
		// happened. Handing the response back lets classifyStatus fail it.
		http: &http.Client{
			Timeout:       timeout,
			CheckRedirect: refuseRedirect,
			Transport:     transport,
		},
		// The upgrade client's Timeout bounds the handshake only: coder/websocket
		// turns it into a dial-scoped context deadline on a clone whose own
		// Timeout is zero, so the long-lived connection is unaffected.
		wsHTTP:    &http.Client{Timeout: timeout, Transport: transport},
		limiter:   cfg.Limiter,
		log:       log,
		userCache: newUserCache(userCacheSize),
		base:      base,
		token:     cfg.Token,
		timeout:   timeout,
		pingEvery: ping,
		maxPost:   defaultMaxPostSize,
	}, nil
}

// refuseRedirect hands a 3xx back to the caller instead of following it, so the
// status reaches classifyStatus, which already treats it as a failure.
func refuseRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

// parseServerURL rejects a URL the client must not talk to before any request
// carries the token there (FR3).
func parseServerURL(raw string, insecure bool) (*url.URL, error) {
	trimmed := strings.TrimSuffix(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return nil, ErrNoURL
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("mattermost: parse server url: %w", err)
	}

	if parsed.Host == "" {
		return nil, ErrBadURL
	}

	// A query or a fragment cannot survive having an API path appended: the path
	// would land inside the query and every call would go to "/". Rejecting here
	// is what lets newRequest join the two with a plain concatenation.
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, ErrBadURLQuery
	}

	if !allowedScheme(parsed.Scheme, insecure) {
		return nil, ErrInsecureURL
	}

	return parsed, nil
}

// allowedScheme reports whether the client may carry a bearer token over this
// scheme. Plain http needs the explicit opt-in, because the token would be on
// the wire in the clear.
func allowedScheme(scheme string, insecure bool) bool {
	if scheme == schemeHTTPS {
		return true
	}

	return insecure && scheme == schemeHTTP
}

// Me returns the identity this client authenticates as, zero until Connect.
func (c *Client) Me() User {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.me
}

// MaxChars returns the server's post-length ceiling in runes. The name matches
// bridge.Outbound, so the outbound adapter promotes it rather than wrapping it.
func (c *Client) MaxChars() int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.maxPost
}

// Connect learns who this token is and how long a post may be. It runs on every
// reconnect, not only at startup, so an identity renamed in Mattermost picks up
// its new username without a restart (FR5).
func (c *Client) Connect(ctx context.Context) error {
	var identity User

	call := &apiRequest{Method: methodConnect, Verb: http.MethodGet, Path: "/users/me"}
	if err := c.do(ctx, call, &identity); err != nil {
		return err
	}

	size := c.readMaxPostSize(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.me = identity
	c.maxPost = size

	return nil
}

// createPostRequest is the POST /posts body. root_id is omitted for a root post
// because the server treats an empty string and an absent field alike, and
// omitting it keeps the recorded request readable.
type createPostRequest struct {
	ChannelID string `json:"channel_id"`
	Message   string `json:"message"`
	RootID    string `json:"root_id,omitempty"`
}

// CreatePost posts message into channelID. A non-empty rootID makes it a reply
// on that thread; Mattermost threads are one level deep, so rootID is always a
// thread root, never another reply.
func (c *Client) CreatePost(ctx context.Context, channelID, rootID, message string) (Post, error) {
	call := &apiRequest{
		Method: methodCreatePost,
		Verb:   http.MethodPost,
		Path:   "/posts",
		Body:   createPostRequest{ChannelID: channelID, RootID: rootID, Message: message},
	}

	var post Post
	if err := c.do(ctx, call, &post); err != nil {
		return Post{}, err
	}

	return post, nil
}

// DirectChannel opens, or finds, the DM channel between this identity and
// otherUserID. The body is a bare two-element array of user ids rather than an
// object, which is the one endpoint in this client that does not take a struct.
func (c *Client) DirectChannel(ctx context.Context, otherUserID string) (Channel, error) {
	identity := c.Me()
	if identity.ID == "" {
		return Channel{}, &APIError{Class: ClassPermanent, Method: methodDirectChannel, Message: ErrNotConnected.Error(), Cause: ErrNotConnected}
	}

	call := &apiRequest{
		Method: methodDirectChannel,
		Verb:   http.MethodPost,
		Path:   "/channels/direct",
		Body:   []string{identity.ID, otherUserID},
	}

	var channel Channel
	if err := c.do(ctx, call, &channel); err != nil {
		return Channel{}, err
	}

	return channel, nil
}

// UserByUsername resolves a username to a user. It is uncached: a send target
// named by hand is rare, and a stale id after a rename would be worse than a
// round trip.
func (c *Client) UserByUsername(ctx context.Context, name string) (User, error) {
	call := &apiRequest{
		Method: methodUserByUsername,
		Verb:   http.MethodGet,
		Path:   "/users/username/" + url.PathEscape(name),
	}

	var user User
	if err := c.do(ctx, call, &user); err != nil {
		return User{}, err
	}

	return user, nil
}

// User resolves a user id, caching the answer. The gate calls this for the
// author of every post carrying no from_bot prop, so the same webhook id
// repeats all day; is_bot does not change over a process lifetime.
func (c *Client) User(ctx context.Context, id string) (User, error) {
	if cached, ok := c.CachedUser(id); ok {
		return cached, nil
	}

	call := &apiRequest{Method: methodUser, Verb: http.MethodGet, Path: "/users/" + url.PathEscape(id)}

	var user User
	if err := c.do(ctx, call, &user); err != nil {
		return User{}, err
	}

	c.put(id, user)

	return user, nil
}

// ChannelByName resolves a channel by team name and channel name, which is how
// an operator writes a send target ("#alerts" on a named team).
func (c *Client) ChannelByName(ctx context.Context, teamName, name string) (Channel, error) {
	call := &apiRequest{
		Method: methodChannelByName,
		Verb:   http.MethodGet,
		Path:   "/teams/name/" + url.PathEscape(teamName) + "/channels/name/" + url.PathEscape(name),
	}

	var channel Channel
	if err := c.do(ctx, call, &channel); err != nil {
		return Channel{}, err
	}

	return channel, nil
}

// typingRequest is the POST /users/{id}/typing body. parent_id scopes the
// indicator to a thread.
type typingRequest struct {
	ChannelID string `json:"channel_id"`
	ParentID  string `json:"parent_id"`
}

// Typing shows the bot as typing. The server drops the request while it is
// marked busy, so callers log the error at debug and never fail the turn: a
// missing indicator is cosmetic (FR17).
func (c *Client) Typing(ctx context.Context, channelID, parentID string) error {
	identity := c.Me()
	if identity.ID == "" {
		return &APIError{Class: ClassPermanent, Method: methodTyping, Message: ErrNotConnected.Error(), Cause: ErrNotConnected}
	}

	call := &apiRequest{
		Method: methodTyping,
		Verb:   http.MethodPost,
		Path:   "/users/" + url.PathEscape(identity.ID) + "/typing",
		Body:   typingRequest{ChannelID: channelID, ParentID: parentID},
	}

	return c.do(ctx, call, nil)
}

// reactionRequest is the POST /reactions body. emoji_name is the short name
// without colons, and user_id must be the caller's own id or the server refuses it.
type reactionRequest struct {
	UserID    string `json:"user_id"`
	PostID    string `json:"post_id"`
	EmojiName string `json:"emoji_name"`
}

// AddReaction puts emojiName on postID as this identity. The bridge uses it to
// acknowledge a post it picked up, so like Typing its failure is cosmetic.
func (c *Client) AddReaction(ctx context.Context, postID, emojiName string) error {
	identity := c.Me()
	if identity.ID == "" {
		return &APIError{Class: ClassPermanent, Method: methodAddReaction, Message: ErrNotConnected.Error(), Cause: ErrNotConnected}
	}

	call := &apiRequest{
		Method: methodAddReaction,
		Verb:   http.MethodPost,
		Path:   "/reactions",
		Body:   reactionRequest{UserID: identity.ID, PostID: postID, EmojiName: emojiName},
	}

	return c.do(ctx, call, nil)
}

// readMaxPostSize asks the server for its own ceiling rather than hardcoding
// one: the value arrives as a decimal string, it differs between releases, and
// an admin can raise it. An unreadable answer is a warning and a safe default,
// not a failed connect, because a bot that cannot measure posts can still send
// short ones.
func (c *Client) readMaxPostSize(ctx context.Context) int {
	var clientCfg map[string]string

	call := &apiRequest{Method: methodConnect, Verb: http.MethodGet, Path: "/config/client", Query: "format=old"}
	if err := c.do(ctx, call, &clientCfg); err != nil {
		c.log.Warn("mattermost client config unreadable, using the default post size",
			"error", err.Error(), "max_post_size", defaultMaxPostSize)

		return defaultMaxPostSize
	}

	reported := clientCfg[configKeyMaxPostSize]

	size, err := strconv.Atoi(reported)
	if err != nil || size <= 0 {
		c.log.Warn("mattermost reported an unreadable max post size, using the default",
			"reported", reported, "max_post_size", defaultMaxPostSize)

		return defaultMaxPostSize
	}

	return size
}

// apiRequest names one call. Method is the exported client method, carried so
// an error can say which call failed without naming the URL or the headers.
type apiRequest struct {
	Body   any
	Method string
	Verb   string
	Path   string
	Query  string
}

// do runs one REST call, retrying once after a 429. Parking the shared bucket is
// the wait: the retry blocks in the limiter for the window the server named, so
// a burst that clears inside it still delivers its reply (design section 5,
// edge case 11). A 429 on the retry hands back the rate-limited failure.
func (c *Client) do(ctx context.Context, call *apiRequest, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var retries int

	for {
		err := c.attempt(ctx, call, out)
		if retries == maxRateLimitRetries || !c.parkOnRateLimit(err) {
			return err
		}

		retries++
	}
}

// attempt runs one REST round trip: it waits on the shared bucket, attaches the
// bearer token, classifies the response, and decodes into out (nil discards the
// body).
func (c *Client) attempt(ctx context.Context, call *apiRequest, out any) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return &APIError{Class: ClassTransient, Method: call.Method, Message: "rate limiter: " + err.Error(), Cause: err}
	}

	req, err := c.newRequest(ctx, call)
	if err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// The cause names the URL but never the token, which rides the
		// Authorization header (FR43).
		return &APIError{Class: ClassTransient, Method: call.Method, Message: err.Error(), Cause: err}
	}

	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus(call.Method, resp); err != nil {
		return err
	}

	return decodeBody(call.Method, resp.Body, out)
}

// newRequest builds the HTTP request, including the one header that must never
// reach a log or an error string.
func (c *Client) newRequest(ctx context.Context, call *apiRequest) (*http.Request, error) {
	var body io.Reader

	if call.Body != nil {
		payload, err := json.Marshal(call.Body)
		if err != nil {
			return nil, &APIError{Class: ClassPermanent, Method: call.Method, Message: "encode request: " + err.Error(), Cause: err}
		}

		body = bytes.NewReader(payload)
	}

	// parseServerURL rejected a query and a fragment, so the base is scheme, host,
	// and path only and joining the API path on the end cannot misroute the call.
	endpoint := c.base.String() + apiPrefix + call.Path
	if call.Query != "" {
		endpoint += "?" + call.Query
	}

	req, err := http.NewRequestWithContext(ctx, call.Verb, endpoint, body)
	if err != nil {
		return nil, &APIError{Class: ClassPermanent, Method: call.Method, Message: "build request: " + err.Error(), Cause: err}
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	if call.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return req, nil
}

// parkOnRateLimit holds the shared bucket for the window the server named, so
// the other identities on this server stop adding to a budget already spent. It
// reports whether err was a 429, which is what makes the call worth retrying.
func (c *Client) parkOnRateLimit(err error) bool {
	failure, ok := errors.AsType[*APIError](err)
	if !ok || failure.Class != ClassRateLimited {
		return false
	}

	c.limiter.ParkFor(failure.RetryAfter)

	return true
}

// decodeBody reads the response into out, draining it when out is nil so the
// connection returns to the pool.
func decodeBody(method string, body io.Reader, out any) error {
	limited := io.LimitReader(body, maxResponseBytes)

	if out == nil {
		if _, err := io.Copy(io.Discard, limited); err != nil {
			return &APIError{Class: ClassTransient, Method: method, Message: "drain response: " + err.Error(), Cause: err}
		}

		return nil
	}

	if err := json.NewDecoder(limited).Decode(out); err != nil {
		return &APIError{Class: ClassTransient, Method: method, Message: "decode response: " + err.Error(), Cause: err}
	}

	return nil
}
