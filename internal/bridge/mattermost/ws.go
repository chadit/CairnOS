package mattermost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// maxFrameBytes caps one WebSocket frame. The library defaults to 32 KiB, which
// a long post plus its metadata can exceed on a server whose MaxPostSize is
// 16383 runes, and an oversized frame kills the connection rather than the
// message.
const maxFrameBytes = 1 << 20

// pingDeadlineDivisor sets the probe deadline as a fraction of the probe
// interval: the default 30s interval gives one ping 10s to come back, which is
// long enough for a slow link and short enough to notice a dead flow.
const pingDeadlineDivisor = 3

// methodListen is the client method every socket failure is reported under, so
// an operator reading a log line sees which call broke without seeing the URL.
const methodListen = "Listen"

// Listen holds one WebSocket connection and forwards every posted event to
// events until ctx ends or the read fails. It never reconnects: the caller owns
// the backoff, so a server refusing the token cannot spin here (FR7).
func (c *Client) Listen(ctx context.Context, events chan<- Event) error {
	conn, err := c.dialSocket(ctx)
	if err != nil {
		return err
	}

	defer func() { _ = conn.CloseNow() }()

	conn.SetReadLimit(maxFrameBytes)

	probeCtx, stopProbe := context.WithCancel(ctx)
	defer stopProbe()

	probe := &keepalive{conn: conn, every: c.pingEvery}
	go probe.run(probeCtx)

	readErr := c.readFrames(ctx, conn, events)

	// A failed probe closed the socket, which is what ended the read. Reporting
	// the probe rather than the read it caused names the real fault.
	if dead := probe.cause(); dead != nil {
		return dead
	}

	return readErr
}

// readFrames forwards posted frames to events until the read fails or ctx ends.
func (c *Client) readFrames(ctx context.Context, conn *websocket.Conn, events chan<- Event) error {
	for {
		_, frame, err := conn.Read(ctx)
		if err != nil {
			return &APIError{Class: ClassTransient, Method: methodListen, Message: "read: " + err.Error(), Cause: err}
		}

		event, err := parseEvent(frame)
		if err != nil {
			c.logDropped(err)

			continue
		}

		select {
		case events <- event:
		case <-ctx.Done():
			return fmt.Errorf("mattermost Listen: %w", ctx.Err())
		}
	}
}

// keepalive probes an idle event socket. A NAT table or a load balancer that
// drops the flow without a FIN leaves Read blocked forever, so the identity
// stops answering with no error and no log line until the process restarts. The
// probe turns that silence into a transient failure the backoff reconnects from
// (FR7, NFR3).
type keepalive struct {
	conn  *websocket.Conn
	err   error
	every time.Duration
	mu    sync.Mutex
}

// run probes on every tick until ctx ends or a probe fails.
func (k *keepalive) run(ctx context.Context) {
	ticker := time.NewTicker(k.every)
	defer ticker.Stop()

	for {
		if !k.wait(ctx, ticker.C) {
			return
		}
	}
}

// wait blocks for the next tick and reports whether the loop should continue.
func (k *keepalive) wait(ctx context.Context, tick <-chan time.Time) bool {
	select {
	case <-ctx.Done():
		return false
	case <-tick:
		return k.probe(ctx)
	}
}

// probe sends one ping and reports whether the socket answered. A failure closes
// the connection, which is what releases the blocked read, and records the cause
// so Listen can report the probe instead of the read.
func (k *keepalive) probe(ctx context.Context) bool {
	pingCtx, cancel := context.WithTimeout(ctx, k.every/pingDeadlineDivisor)
	defer cancel()

	err := k.conn.Ping(pingCtx)
	if err == nil {
		return true
	}

	k.mu.Lock()
	k.err = &APIError{Class: ClassTransient, Method: methodListen, Message: "keepalive: " + err.Error(), Cause: err}
	k.mu.Unlock()

	_ = k.conn.CloseNow()

	return false
}

// cause returns the probe failure that ended the connection, nil when the read
// ended on its own.
func (k *keepalive) cause() error {
	k.mu.Lock()
	defer k.mu.Unlock()

	return k.err
}

// dialSocket opens the event socket, authenticating on the upgrade request.
// Mattermost also accepts an authentication_challenge frame after connecting;
// the header form fails the handshake outright on a bad token instead of
// leaving a connected socket that never delivers.
func (c *Client) dialSocket(ctx context.Context) (*websocket.Conn, error) {
	endpoint := c.base.Clone()
	endpoint.Scheme = socketScheme(endpoint.Scheme)
	endpoint.Path += apiPrefix + "/websocket"

	conn, resp, err := websocket.Dial(ctx, endpoint.String(), &websocket.DialOptions{
		HTTPClient: c.wsHTTP,
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + c.token}},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	if err != nil {
		return nil, &APIError{
			Class:   ClassTransient,
			Method:  methodListen,
			Message: "dial: " + err.Error(),
			Cause:   fmt.Errorf("%w: %w", ErrSocketDial, err),
		}
	}

	return conn, nil
}

// socketScheme maps the REST scheme onto its WebSocket twin.
func socketScheme(scheme string) string {
	if scheme == "http" {
		return "ws"
	}

	return "wss"
}

// logDropped records a frame the parser could not use. A malformed posted
// payload is a server-shape change worth seeing at debug, so it is logged and
// the connection carries on; everything else is silent.
func (c *Client) logDropped(err error) {
	if errors.Is(err, errNotPosted) {
		return
	}

	c.log.Debug("mattermost dropped an unparseable event", "error", err.Error())
}

// envelope is the frame shape: {event, data, broadcast, seq}. data varies by
// event, so it is decoded only once the event name is known.
type envelope struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
	Seq   int64           `json:"seq"`
}

// postedData is the posted payload. The server serializes the post itself as a
// JSON string inside this JSON object, and encodes mentions as a JSON string
// array, so both need a second unmarshal.
type postedData struct {
	Post        string `json:"post"`
	ChannelType string `json:"channel_type"`
	SenderName  string `json:"sender_name"`
	Mentions    string `json:"mentions"`
}

// parseEvent turns one frame into an Event, answering errNotPosted for the
// frames the bridge ignores.
func parseEvent(frame []byte) (Event, error) {
	var env envelope
	if err := json.Unmarshal(frame, &env); err != nil {
		return Event{}, fmt.Errorf("%w: %w", errBadEnvelope, err)
	}

	if env.Event != EventPosted {
		return Event{}, errNotPosted
	}

	var data postedData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return Event{}, fmt.Errorf("%w: %w", errBadPostedData, err)
	}

	var post Post
	if err := json.Unmarshal([]byte(data.Post), &post); err != nil {
		return Event{}, fmt.Errorf("%w: %w", errBadPost, err)
	}

	return Event{
		Mentions:    parseMentions(data.Mentions),
		Name:        env.Event,
		ChannelType: data.ChannelType,
		SenderName:  data.SenderName,
		Post:        &post,
	}, nil
}

// parseMentions decodes the mention list. The field is absent on most posts and
// a rewritten, single-element list when it is present, so an unreadable value
// is treated as "no mention" rather than failing the post.
func parseMentions(raw string) []string {
	if raw == "" {
		return nil
	}

	var mentions []string
	if err := json.Unmarshal([]byte(raw), &mentions); err != nil {
		return nil
	}

	return mentions
}
