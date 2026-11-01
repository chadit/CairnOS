package hive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// DefaultRequestTimeout bounds one hive HTTP call when the caller's context
// carries no deadline, so a stalled hive never blocks a subscribe loop forever.
const DefaultRequestTimeout = 30 * time.Second

// defaultMinObservations is the local floor a node insists on before it will
// publish a pattern, independent of the server's k-anonymity floor. The client
// refuses below this even though the server enforces its own k, because a node
// must never publish a pattern it has barely seen, regardless of what the
// server would accept.
const defaultMinObservations = 5

// Client is the node's side of the hive API. Subscribe is always safe: the node
// receives tips and sends nothing. Publish is gated and off by default, because
// publishing is the part that risks a content leak and waits on the privacy
// review (R2) and the hosting decision (R3).
type Client interface {
	// Subscribe pulls tips for the given categories. It is receive-only, so it
	// is safe regardless of the publish gate. A category outside the fixed set
	// is dropped before the request.
	Subscribe(ctx context.Context, categories []Category) ([]Tip, error)

	// Publish sends one content-free tip, after the gate checks. It errors with
	// ErrPublishDisabled when publish_enabled is false and with
	// ErrBelowMinObservations when the node has seen the pattern fewer than its
	// local floor. observations is how many independent times this node saw the
	// pattern, which the client checks against min_observations before the
	// server's k-anonymity floor ever sees the tip.
	Publish(ctx context.Context, tip Tip, observations int) error
}

// Config holds the [hive] settings. The node pseudonym is a rotating id, never
// a stable instance name, so a tip cannot be traced back to one node across
// publishes.
type Config struct {
	// Endpoint is the hive API base URL.
	Endpoint string

	// NodePseudonym is the rotating id the node presents to the hive. It is
	// never the hostname or a stable instance name; the wiring rotates it.
	NodePseudonym string

	// Categories are the opted-in categories. Sharing is per-category, off
	// until a category is listed here.
	Categories []Category

	// KAnonymityFloor is the server-side k the node knows about; it is
	// informational on the client (the server enforces it). The client still
	// refuses below MinObservations locally, so it never relies on the server
	// alone.
	KAnonymityFloor int

	// MinObservations is the local floor: the node refuses to publish a pattern
	// it has seen fewer than this many times, ahead of the server's k. Zero
	// falls back to defaultMinObservations.
	MinObservations int

	// Enabled turns the hive client on. With it off, no client is built and
	// neither subscribe nor publish runs.
	Enabled bool

	// PublishEnabled gates publishing. Default false: the node receives tips
	// but sends none until the operator opts in, after the privacy review.
	PublishEnabled bool
}

// HTTPClient is the slice of *http.Client the hive client uses, so a test can
// inject a transport pointed at an httptest server without a real network.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// HTTPHiveClient talks to a hive API over HTTP. It carries the gate state, so
// Publish refuses before any request leaves the node.
type HTTPHiveClient struct {
	doer HTTPClient

	// sealer, when set, encrypts every outbound tip and decrypts every received
	// one under the shared cluster key (R2/R3). Nil keeps the plaintext wire
	// format, the no-key mode the composition root chooses on SQLite or when no
	// key is configured.
	sealer Sealer

	encode func(any) ([]byte, error)
	cfg    Config
}

// Option configures an HTTPHiveClient at construction.
type Option func(*HTTPHiveClient)

// WithSealer turns on shared-key sealing: outbound tips are encrypted before
// they leave the node and received envelopes are opened on the way in.
func WithSealer(sealer Sealer) Option {
	return func(c *HTTPHiveClient) { c.sealer = sealer }
}

// subscribeResponse is the wire shape Subscribe decodes: a tips array. Keeping
// it a named type means an unexpected response field is ignored rather than
// failing the decode.
type subscribeResponse struct {
	Tips []Tip `json:"tips"`
}

// publishRequest is the plaintext wire shape Publish encodes when no shared key
// is configured: the node pseudonym, the observation count, and the content-free
// tip. The pseudonym is rotating, so the server cannot link two publishes to one
// node.
type publishRequest struct {
	Tip           Tip    `json:"tip"`
	NodePseudonym string `json:"node_pseudonym"`
	Observations  int    `json:"observations"`
}

// sealedPublishRequest is the wire shape Publish encodes under a shared key: the
// same routing metadata, but the tip replaced by its encrypted envelope. The
// pseudonym and observation count stay cleartext because the hive routes and
// counts on them; the tip itself the host cannot read.
type sealedPublishRequest struct {
	NodePseudonym string    `json:"node_pseudonym"`
	SealedTip     SealedTip `json:"sealed_tip"`
	Observations  int       `json:"observations"`
}

// sealedSubscribeResponse is the sealed counterpart of subscribeResponse: a
// list of encrypted envelopes the node opens with the shared key.
type sealedSubscribeResponse struct {
	Tips []SealedTip `json:"tips"`
}

// NewHTTPClient builds an HTTP hive client from the config and an injected
// HTTP doer. A disabled config or missing endpoint fails here rather than at
// first request. A nil doer falls back to a bounded *http.Client.
//
//nolint:gocritic // hugeParam: Config is copied once per process, so the value pass is deliberate
func NewHTTPClient(cfg Config, doer HTTPClient, opts ...Option) (*HTTPHiveClient, error) {
	if !cfg.Enabled {
		return nil, ErrHiveDisabled
	}

	if cfg.Endpoint == "" {
		return nil, ErrNoEndpoint
	}

	if cfg.MinObservations <= 0 {
		cfg.MinObservations = defaultMinObservations
	}

	if doer == nil {
		doer = &http.Client{Timeout: DefaultRequestTimeout}
	}

	client := &HTTPHiveClient{cfg: cfg, doer: doer, encode: json.Marshal}

	for _, opt := range opts {
		opt(client)
	}

	return client, nil
}

// Subscribe pulls tips for the opted-in categories intersected with the
// requested ones. It sends only category enums (no node identity, no content),
// so it is safe even with publish off. Unrecognized categories are dropped
// before the request.
func (c *HTTPHiveClient) Subscribe(ctx context.Context, categories []Category) ([]Tip, error) {
	wanted := c.allowedCategories(categories)

	endpoint, err := url.JoinPath(c.cfg.Endpoint, "v1", "tips")
	if err != nil {
		return nil, fmt.Errorf("hive subscribe: build url: %w", err)
	}

	query := url.Values{}
	for _, category := range wanted {
		query.Add("category", string(category))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("hive subscribe: build request: %w", err)
	}

	tips, err := c.decodeSubscribed(req)
	if err != nil {
		return nil, err
	}

	return keepShareableTips(tips), nil
}

// Publish sends one tip after the gate checks. The disabled gate and the local
// observation floor both refuse before any bytes leave the node, so a gated
// node never makes a request.
//
//nolint:gocritic // hugeParam: tip is a value to satisfy the Client interface contract
func (c *HTTPHiveClient) Publish(ctx context.Context, tip Tip, observations int) error {
	if !c.cfg.PublishEnabled {
		return ErrPublishDisabled
	}

	if observations < c.cfg.MinObservations {
		return fmt.Errorf("%w: saw %d, need %d", ErrBelowMinObservations, observations, c.cfg.MinObservations)
	}

	if !ValidCategory(tip.Category) {
		return fmt.Errorf("%w: %q", ErrInvalidCategory, tip.Category)
	}

	encoded, err := c.encodePublishBody(tip, observations)
	if err != nil {
		return err
	}

	endpoint, err := url.JoinPath(c.cfg.Endpoint, "v1", "tips")
	if err != nil {
		return fmt.Errorf("hive publish: build url: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("hive publish: build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	return c.do(req, nil)
}

// encodePublishBody marshals the outbound publish body, sealing the tip first
// when a shared key is configured. Without a sealer it falls back to the
// plaintext wire shape, the no-key mode for SQLite or an unconfigured key.
//
//nolint:gocritic // hugeParam: tip stays a value to keep the content-free tip immutable, matching Publish
func (c *HTTPHiveClient) encodePublishBody(tip Tip, observations int) ([]byte, error) {
	if c.sealer == nil {
		encoded, err := c.encode(publishRequest{
			NodePseudonym: c.cfg.NodePseudonym,
			Observations:  observations,
			Tip:           tip,
		})
		if err != nil {
			return nil, fmt.Errorf("hive publish: encode tip: %w", err)
		}

		return encoded, nil
	}

	sealed, err := c.sealer.Seal(tip)
	if err != nil {
		return nil, fmt.Errorf("hive publish: seal tip: %w", err)
	}

	encoded, err := c.encode(sealedPublishRequest{
		NodePseudonym: c.cfg.NodePseudonym,
		Observations:  observations,
		SealedTip:     sealed,
	})
	if err != nil {
		return nil, fmt.Errorf("hive publish: encode sealed tip: %w", err)
	}

	return encoded, nil
}

// decodeSubscribed reads the subscribe response, opening each sealed envelope
// when a shared key is configured. An envelope that does not open under the key
// is dropped, never surfaced as ciphertext, so a wrong key or a tampered tip
// quietly yields no tip rather than poisoning the queue.
func (c *HTTPHiveClient) decodeSubscribed(req *http.Request) ([]Tip, error) {
	if c.sealer == nil {
		var decoded subscribeResponse
		if err := c.do(req, &decoded); err != nil {
			return nil, err
		}

		return decoded.Tips, nil
	}

	var decoded sealedSubscribeResponse
	if err := c.do(req, &decoded); err != nil {
		return nil, err
	}

	opened := make([]Tip, 0, len(decoded.Tips))

	for _, envelope := range decoded.Tips {
		tip, openErr := c.sealer.Open(envelope)
		if openErr != nil {
			continue
		}

		opened = append(opened, tip)
	}

	return opened, nil
}

// allowedCategories intersects the requested categories with the node's
// opted-in set and the fixed shareable set, so a request never names a
// category the node did not opt into or one outside the enum.
func (c *HTTPHiveClient) allowedCategories(requested []Category) []Category {
	optedIn := make(map[Category]struct{}, len(c.cfg.Categories))
	for _, category := range c.cfg.Categories {
		optedIn[category] = struct{}{}
	}

	wanted := make([]Category, 0, len(requested))

	for _, category := range requested {
		if !ValidCategory(category) {
			continue
		}

		if _, ok := optedIn[category]; !ok {
			continue
		}

		wanted = append(wanted, category)
	}

	return wanted
}

// keepShareableTips drops received tips that fail the same content-free shape
// the node enforces on outbound tips: a tip whose category is outside the fixed
// set, or whose kind is unshareable, is discarded rather than queued. A
// malicious or buggy server cannot smuggle an off-allowlist field into the
// node's queue this way.
func keepShareableTips(tips []Tip) []Tip {
	kept := make([]Tip, 0, len(tips))

	for _, tip := range tips {
		if !ValidCategory(tip.Category) || !shareableKind(tip.Kind) {
			continue
		}

		kept = append(kept, tip)
	}

	return kept
}

// do executes one request and, when out is non-nil, decodes the JSON body into
// it. A non-2xx status is ErrHiveRequest. Close drains the unread remainder,
// so the connection returns to the pool.
func (c *HTTPHiveClient) do(req *http.Request, out any) error {
	resp, err := c.doer.Do(req)
	if err != nil {
		return fmt.Errorf("hive request: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("%w: status %d", ErrHiveRequest, resp.StatusCode)
	}

	if out == nil {
		return nil
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("hive request: decode response: %w", err)
	}

	return nil
}
