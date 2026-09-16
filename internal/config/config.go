// Package config loads CairnOS settings: defaults, then the TOML file, then
// environment overrides, strongest last
// (.agents/spec/11-deployment-and-config.md, "Configuration").
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/chadit/CairnOS/internal/workflow"
)

// Config is the fully resolved settings tree.
type Config struct {
	Engine Engine

	// Runners carries each [runner.<name>] block verbatim for the registry's
	// factories, including nested tables like circuit_breaker.
	Runners map[string]map[string]any

	// Credentials carries each [credential.<name>] block parsed and validated
	// into a typed Credential (unlike Runners, which stays verbatim, because a
	// credential's grant, token_url, and secret refs validate at Load).
	Credentials map[string]Credential

	// Sources carries each [source.<name>] block parsed and validated into a
	// typed Source, so serve can clone a missing tree before the runner registry
	// is built.
	Sources map[string]Source

	// Hosts carries each [host.<name>] block, with the tool blocks nested under
	// it, parsed and validated into a typed Host. Wiring renders the table into
	// the environment every job's child process receives.
	Hosts map[string]Host

	Hive      Hive
	MCP       MCP
	A2A       A2A
	Telemetry Telemetry
	Memory    Memory
	Store     Store
	Secrets   Secrets
	OAuth     OAuth
	Notify    Notify
	Tuning    Tuning
	Env       Env
	Chat      Chat
	API       API
	Auth      Auth
	Bridge    Bridge
	Scheduler Scheduler
	Webhook   Webhook
	Retention Retention
	Autonomic Autonomic
}

// Grant names a [credential.<name>] block's OAuth2 grant. Named so config
// validation, the credentials manager, and the docs agree on one spelling.
const (
	// GrantClientCredentials mints tokens from a client id and secret with no
	// user context, for machine-to-machine SaaS calls.
	GrantClientCredentials = "client_credentials"
	// GrantRefreshToken mints access tokens from a long-lived refresh token.
	GrantRefreshToken = "refresh_token"
)

// Credential is one outbound OAuth2 credential parsed from a [credential.<name>]
// block (.agents/spec/parity/secrets-credentials.md, "outbound OAuth2
// credentials"). The http and mcp_client runners reference it by name to send a
// bearer token. Client secrets and refresh tokens never live in this struct or
// the config file: only the secret-ref name does, resolved through the secret
// chain at wiring time.
type Credential struct {
	// Grant is GrantClientCredentials or GrantRefreshToken.
	Grant string

	// TokenURL is the OAuth2 token endpoint. It must be https unless
	// AllowPrivateTargets is set, which mirrors the runner egress opt-out.
	TokenURL string

	// ClientID identifies this client at the provider. It is plain config, not a
	// secret.
	ClientID string

	// ClientSecretRef is the secret name of the client secret, resolved through
	// the chain. Required for client_credentials, optional for refresh_token.
	ClientSecretRef string

	// RefreshTokenRef is the secret name of the refresh token, resolved through
	// the chain. Required for and unique to the refresh_token grant.
	RefreshTokenRef string

	// Scopes are the requested scopes, meaningful for client_credentials only.
	Scopes []string

	// AllowPrivateTargets drops the token client's dial-time IP guard (keeping
	// the scheme guard) for a provider on an internal or dev endpoint, matching
	// the runner opt-out. It also lets TokenURL use http.
	AllowPrivateTargets bool
}

// Source is one [source.<name>] block: a git working tree CairnOS clones when
// it is absent and keeps current through the source runner, never writing to
// it. The tree is how a host gets the workflows and scripts it runs, so whoever
// can push to Ref runs code as the serve user.
type Source struct {
	// Repo is the clone URL. CairnOS passes no credentials of its own, so a
	// private repo works only when the serve user's own git setup reaches it.
	Repo string

	// Dest is the absolute path of the working tree. It sits outside the CairnOS
	// directory so the state CairnOS prunes and the source it syncs never
	// overlap.
	Dest string

	// Ref is a remote branch to follow, an exact tag to pin, or a tag glob whose
	// newest match by version sort wins. Empty means DefaultSourceRef.
	Ref string

	// DirtyReset lets the sync force the tree back onto Ref when it carries
	// tracked changes, sits elsewhere, or cannot fast-forward. It defaults to
	// true so a host repairs itself instead of failing a scheduled sync every
	// few minutes until someone logs in. False leaves the tree alone.
	DirtyReset bool
}

// Engine bounds execution.
type Engine struct {
	// Fallback is the mixed-model chain, keyed by primary runner name: when an
	// action's runner fails terminally, the engine tries the listed runners in
	// order before the action fails (.agents/spec/14-open-decisions.md, D12). An
	// absent or empty entry means the action's runner has no fallback.
	Fallback map[string][]string

	Retry                Retry
	MaxConcurrentActions int
	DefaultTimeout       time.Duration

	// MaxCallDepth caps workflow-call nesting; nesting past it fails with
	// engine.call_depth_exceeded. Below 1 falls back to the engine default
	// (.agents/spec/parity/workflow-language.md, "Workflow call").
	MaxCallDepth int
}

// Retry is the engine-wide default policy; actions override it per file.
type Retry struct {
	Backoff     string
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Jitter      bool
}

// Store selects the state backend.
type Store struct {
	Backend string
	DSN     string
}

// Secrets selects how a ${{ secrets.name }} reference resolves
// (.agents/spec/parity/secrets-credentials.md, "Config"). "env" reads only
// CAIRNOS_SECRET_<NAME>; "store" chains env ahead of an encrypted store, so
// env wins.
//
// "Encrypted" is narrow: the store defends a read-at-rest attacker, not one
// who injects environment variables. Secrets interpolated into action output
// stay plaintext in run history, and env secrets reach child CLI runners.
type Secrets struct {
	// Bitwarden is the [secrets.bitwarden] block, nil when absent. It renders an
	// environment file at deployment time rather than resolving at runtime,
	// because most credentials here are read by bare os.Getenv.
	Bitwarden *Bitwarden

	// Backend is "env" (default) or "store".
	Backend string
}

// Secret backend names Load accepts. Named constants so the CLI backend guard,
// the wiring source builder, and validation all agree on one spelling.
const (
	// SecretsBackendEnv reads secrets only from CAIRNOS_SECRET_<NAME>; no key
	// material is needed and behavior matches the pre-store engine exactly.
	SecretsBackendEnv = "env"
	// SecretsBackendStore composes the env source with the encrypted store as
	// Chain(env, store), env first.
	SecretsBackendStore = "store"
)

// Memory selects the memory backend.
type Memory struct {
	Backend         string
	EmbeddingRunner string
	Vector          bool
}

// Retention bounds state growth (.agents/spec/04-execution-engine.md).
type Retention struct {
	Runs    time.Duration
	Outputs time.Duration
	Sweep   time.Duration
}

// Env controls which environment variables workflows may read through
// env references. Empty means no env access at all.
type Env struct {
	Allowlist []string
}

// Telemetry configures exporters.
type Telemetry struct {
	Prometheus   string
	OTLPEndpoint string
	Enabled      bool
}

// API configures the gRPC and HTTP listeners (off until Phase 1a serves).
type API struct {
	GRPC string
	HTTP string

	// CORSOrigins is the explicit CORS allowlist for the gateway. Empty (the
	// default) sends no CORS headers, so the browser enforces same-origin. A
	// wildcard with credentials is never emitted; only listed origins echo back
	// (spec M1).
	CORSOrigins []string

	// MaxBodyBytes caps a gateway request body, wrapping each handler in an
	// http.MaxBytesReader so an over-limit POST is rejected with 413 instead of
	// buffering unbounded input (spec M4). Zero falls back to the default.
	MaxBodyBytes int64

	// MaxRecvMsgBytes caps a single gRPC message, set explicitly on the server
	// so the transport rejects an over-limit frame rather than relying on the
	// library default (spec M4). Zero falls back to the default.
	MaxRecvMsgBytes int

	Enabled bool
}

// Auth gates external calls (.agents/spec/08-auth-and-access.md,
// "Configuration"). Signing secrets are never config-file keys: the HS256
// secret and the EdDSA key file paths come from CAIRNOS_AUTH_HS256_SECRET
// and CAIRNOS_AUTH_PRIVATE_KEY_FILE / CAIRNOS_AUTH_PUBLIC_KEY_FILE.
type Auth struct {
	// SigningAlgorithm picks the token tier: HS256 (self-issued, shared
	// secret) or EdDSA (asymmetric, cross-org-ready; spec Change 3).
	SigningAlgorithm string

	// Issuer and Audience fill and check the iss/aud token claims.
	Issuer   string
	Audience string

	// MaxConcurrentLogins bounds simultaneous credential derivations, since
	// each unauthenticated argon2id login costs ~64 MiB; excess logins get
	// RESOURCE_EXHAUSTED rather than pinning memory (spec M1).
	MaxConcurrentLogins int

	// AccessTokenTTL bounds each token; refresh is re-login with the
	// access key (spec Change 2), so the default stays short at 1h.
	AccessTokenTTL time.Duration

	// LoginRatePerMinute bounds one peer's logins per minute (also its
	// burst), throttling a single source on top of the global cap (spec M1).
	LoginRatePerMinute int

	// StreamRecheck is how often an open stream re-verifies its token's
	// expiry and denylist state, so a long-lived stream stops after a
	// mid-stream revoke or expiry (spec M4).
	StreamRecheck time.Duration

	// ClockSkew is the leeway the cross-instance EdDSA verifier allows on the
	// time claims, absorbing small clock differences between issuer and
	// verifier (spec L2).
	ClockSkew time.Duration

	// RequireExp and RequireJTI reject tokens missing those claims. Both
	// default on and are overridden only deliberately.
	RequireExp bool
	RequireJTI bool

	// ServeWithoutAuthOnNetwork is the extra guard: with auth disabled, a
	// non-loopback bind is refused unless this is also set, so an open API
	// never happens by accident.
	ServeWithoutAuthOnNetwork bool

	// TLSTerminated asserts that a TLS-terminating proxy fronts every
	// non-loopback listener. /login carries the long-lived access key, so
	// CairnOS refuses plaintext non-loopback binds with auth on unless the
	// operator makes this claim.
	TLSTerminated bool

	Enabled bool
}

// OAuth configures the Phase 5b OIDC human-login path
// (.agents/spec/12-ui-and-governance.md, "Multi-tenancy"): browser users sign
// in at an identity provider, and CairnOS mints its own JWT carrying the
// workspace and the role's scopes. It sits alongside the access-key model,
// not in place of it. Off by default; the client secret never lives in the
// config file, only in CAIRNOS_OAUTH_CLIENT_SECRET.
type OAuth struct {
	// Issuer is the IdP's OIDC issuer URL; the provider's discovery document
	// and JWKS are fetched from it.
	Issuer string

	// ClientID identifies this app at the IdP. ClientSecret comes from
	// CAIRNOS_OAUTH_CLIENT_SECRET, never the config file.
	ClientID     string
	ClientSecret string

	// RedirectURL is the callback the IdP returns the user to; it must match a
	// redirect registered at the IdP and point at this server's
	// /v1/oauth/callback route.
	RedirectURL string

	// DefaultWorkspace and DefaultRole place a first-time human login: a new
	// member is auto-provisioned into DefaultWorkspace with DefaultRole, and
	// the minted token carries that workspace and the role's scopes. DefaultRole
	// should be the lowest-privilege role (viewer) so a fresh sign-in cannot
	// change anything until an admin grants more.
	DefaultWorkspace string
	DefaultRole      string

	// AllowedEmailDomains is the auto-provision domain allowlist, matched
	// against the verified email claim's domain (the part after @). Empty means
	// domain is not checked, so AllowedSubjects (or an empty pair, which only
	// matters when AutoProvision is true) decides.
	AllowedEmailDomains []string

	// AllowedSubjects is the auto-provision subject allowlist, matched against
	// the verified ID token subject. Empty means subject is not checked.
	AllowedSubjects []string

	Enabled bool

	// AutoProvision gates first-login auto-provisioning. Default false: an
	// IdP-authenticated subject with no pre-existing membership is refused
	// rather than silently added to DefaultWorkspace, closing the open-door
	// provisioning hole (spec H1). With it true, AllowedEmailDomains and
	// AllowedSubjects still gate which subjects may self-provision when either
	// list is set.
	AutoProvision bool
}

// A2A configures the Phase 4b Agent-to-Agent protocol adapter
// (.agents/spec/09-a2a-and-hive.md). Off by default; when enabled, the serve
// process registers the A2A service, serves the signed agent card at the
// well-known path, and optionally fronts the gRPC listener with mTLS.
type A2A struct {
	// CardSigningKeyFile names a PEM ed25519 private key that signs the agent
	// card. Empty reuses the auth EdDSA key when the auth tier is asymmetric;
	// with neither, the node generates an ephemeral keypair at boot and logs a
	// warning, which is a dev convenience, not a production posture.
	CardSigningKeyFile string

	// EndpointAdvertise is the address the card advertises for this node, e.g.
	// "grpc://host:7777". Empty falls back to the bound gRPC address, which is
	// right for local use but usually wrong behind NAT or a proxy.
	EndpointAdvertise string

	// CertFile, KeyFile, and ClientCAFile turn on mTLS at the gRPC transport
	// (.agents/spec/09-a2a-and-hive.md, "Transport security"). All three are
	// required together; any subset is a configuration error. mTLS is
	// orthogonal to the bearer token: the connection proves itself with
	// certificates, the caller proves its scopes with the token.
	CertFile     string
	KeyFile      string
	ClientCAFile string

	Enabled bool
}

// MCP configures the Phase 7a MCP server
// (.agents/spec/10-mcp-integration.md, "MCP Server"). Off by default; with the
// http transport the serve process mounts the streamable handler on the API
// mux at Path, and an external client presents the same bearer token, under
// the same scopes. The stdio transport is the `cairnos mcp serve` subcommand,
// where auth is the process boundary.
type MCP struct {
	// Transport selects how the server is reached: "http" mounts the streamable
	// handler on the shared API mux, "stdio" is served only by the mcp serve
	// subcommand and ignored by serve. Empty defaults to http.
	Transport string

	// Path is the mux pattern the http transport mounts on. Empty defaults to
	// /mcp.
	Path string

	// WorkflowsDir overrides where the workflows resource lists files. Empty
	// falls back to the node's workflows directory, the same one run and serve
	// resolve names in.
	WorkflowsDir string

	// RegistryURL is the base URL of the MCP registry this node publishes to and
	// imports from (.agents/spec/10-mcp-integration.md, "MCP Registry"). Empty
	// disables both: mcp publish and mcp import report it must be set. The
	// registry token is a secret read from CAIRNOS_MCP_REGISTRY_TOKEN, never this
	// struct.
	RegistryURL string

	// AdvertiseURL is this node's externally reachable MCP http endpoint, the
	// address mcp publish registers so other nodes can import and call its tools.
	// Empty leaves nothing publishable, so publish requires it.
	AdvertiseURL string

	// Enabled turns the server on. With it off, the serve process mounts no MCP
	// handler and the mcp serve subcommand still works on demand.
	Enabled bool
}

// MCP transport values.
const (
	MCPTransportHTTP  = "http"
	MCPTransportStdio = "stdio"
)

// defaultMCPPath is the mux pattern the http transport mounts on when the
// config sets no path (.agents/spec/10-mcp-integration.md, "MCP Server").
const defaultMCPPath = "/mcp"

// Hive gates network tip sharing (Phase 6,
// .agents/spec/09-a2a-and-hive.md). Subscribing is always safe; publishing is
// gated and off by default, pending the privacy review (R2) and the hosting
// decision (R3) in .agents/spec/14-open-decisions.md.
type Hive struct {
	// Endpoint is the hive API base URL.
	Endpoint string

	// NodePseudonym is the rotating id the node presents to the hive. Empty
	// means the wiring mints a fresh random pseudonym at startup; it is never
	// the hostname or a stable instance name.
	NodePseudonym string

	// KeyRef is the secret name of the shared cluster key, resolved through the
	// SecretSource seam (R2/R3 in .agents/spec/14-open-decisions.md). Empty means
	// no key: the client keeps the plaintext wire format, the no-key mode for
	// SQLite or an unconfigured cluster. When set, published tips are sealed and
	// received envelopes opened under the resolved 32-byte key.
	KeyRef string

	// PrevKeyRef is the secret name of the previous shared key, still accepted
	// during a key rotation so opening keeps working. Empty when no rotation is
	// in flight. A node seals under KeyRef and opens under KeyRef or PrevKeyRef,
	// so a cluster rotates keys without a flag day (R2/R3 in
	// .agents/spec/14-open-decisions.md).
	PrevKeyRef string

	// Categories are the opted-in categories. Sharing is per-category, off
	// until a category is listed here.
	Categories []string

	// KAnonymityFloor is the server-side k the node knows about; informational
	// on the client, which still refuses below MinObservations locally.
	KAnonymityFloor int

	// MinObservations is the local floor: the node refuses to publish a pattern
	// it has seen fewer than this many times, ahead of the server's k.
	MinObservations int

	// Enabled turns the hive client on. With it off, no client is built and
	// neither subscribe nor publish runs.
	Enabled bool

	// PublishEnabled gates publishing. Default false: the node receives tips
	// but sends none until the operator opts in, after the privacy review.
	PublishEnabled bool
}

// hiveBackend is the only store backend that can run the hive: it is a private
// cluster over shared state, so the elected primary and the sealed multi-node
// model need Postgres (R2/R3 in .agents/spec/14-open-decisions.md).
const hiveBackend = "postgres"

// HiveNeedsPostgres reports a hive that is enabled but configured on a store
// backend that cannot run it. serve uses this to refuse the hive on SQLite
// rather than start a single node that cannot coordinate.
func (c *Config) HiveNeedsPostgres() bool {
	return c.Hive.Enabled && c.Store.Backend != hiveBackend
}

// Autonomic gates the Phase 6 auto-apply path
// (.agents/spec/06-observability-and-tuning.md, "Review flow"). Default off:
// nothing auto-applies until the operator opts in, and even then only the
// output-neutral kinds (caching, batching) ever apply.
type Autonomic struct {
	// AutoApplyEnabled is the master switch. Default false.
	AutoApplyEnabled bool
}

// Tuning bounds the Phase 3 suggestion scan
// (.agents/spec/06-observability-and-tuning.md, "Guardrail").
type Tuning struct {
	// EmbeddingModel opts the replay guardrail into similarity comparison
	// (R1): when set, an output-changing suggestion's replay judges
	// equivalence by the cosine similarity of OpenAI embeddings instead of
	// exact byte match, so harmless rephrasing no longer reads as divergence.
	// Empty keeps the exact-match default, so similarity comparison is opt-in.
	EmbeddingModel string

	// MinSavingsUSD is the savings floor: suggestions whose estimated
	// per-run saving falls below it never appear.
	MinSavingsUSD float64

	// EquivalenceThreshold is the cosine similarity at or above which the
	// embedding comparator calls two outputs equivalent. Zero falls back to a
	// conservative default; only consulted when EmbeddingModel is set.
	EquivalenceThreshold float64
}

// Scheduler tunes the durable work-queue the scheduler enqueues into and the
// worker drains (.agents/spec/14-open-decisions.md, S8). A cron fire enqueues a
// work item; a worker claims and runs it. A fire no worker can take right away
// is not dropped, it waits in the queue for the next free worker.
type Scheduler struct {
	// AckWait is the lease an item holds while a worker runs it. A worker that
	// crashes mid-run leaves the lease to expire, and the sweep reclaims it.
	AckWait time.Duration

	// Backoff is how long a failed item waits before it can be redelivered.
	Backoff time.Duration

	// MaxDeliveries caps redelivery attempts; an item past it dead-letters
	// instead of looping forever.
	MaxDeliveries int

	// MaxAge bounds how long a dead-lettered item is kept for inspection before
	// the sweep purges it. Zero keeps dead items indefinitely.
	MaxAge time.Duration

	// WorkerConcurrency is the most items one node runs at once.
	WorkerConcurrency int

	// Prefetch caps how many items one claim leases at a time.
	Prefetch int

	// SweepInterval is how often the worker reclaims expired leases and purges
	// aged dead items.
	SweepInterval time.Duration
}

// Scheduler work-queue defaults, chosen for a small calm node; an operator tunes
// them per deployment (.agents/spec/14-open-decisions.md, S8).
const (
	defaultSchedulerAckWait       = 5 * time.Minute
	defaultSchedulerBackoff       = 30 * time.Second
	defaultSchedulerMaxDeliveries = 5
	defaultSchedulerConcurrency   = 4
	defaultSchedulerPrefetch      = 8
	defaultSchedulerSweepInterval = time.Minute
)

// Chat configures the Phase 9 conversational interface
// (.agents/spec/13-roadmap.md, Phase 9). Off by default; when enabled the
// serve process mounts ChatService and the chat agent over the same engine the
// workflows run on.
type Chat struct {
	// Personalities maps personality names to their system prompts, overlaid on
	// the built-ins. A name matching a built-in overrides it.
	Personalities map[string]string

	// Driver is the LLM runner name the chat agent uses to plan turns. It must
	// be a registered runner.ToolCaller (the openai and anthropic runners),
	// the same capability an agent action's driver needs.
	Driver string

	// DefaultPersonality names the personality a new session gets when the
	// request asks for none. Empty falls back to the assistant built-in.
	DefaultPersonality string

	// FileAllowlist lists the absolute directory roots an @-file reference may
	// read from (.agents/spec/parity/skills-context.md, Part 2). Empty (the
	// default) leaves every @-token inert. A path is readable only when its
	// symlink-resolved form sits inside a symlink-resolved root, so a symlink
	// cannot escape a root and a sibling sharing a root's name prefix is
	// rejected.
	FileAllowlist []string

	// TurnBudgetUSD caps one chat turn's estimated driver-and-tool spend, the
	// Phase 9 guardrail against a runaway conversation. Zero leaves turns
	// uncapped.
	TurnBudgetUSD float64

	// MaxFileBytes caps how much of one @-file reference inlines into the
	// transcript; a larger file truncates at the cap with the byte counts in the
	// record. Default 64 KiB.
	MaxFileBytes int

	// Enabled turns the chat surface on. With it off, serve mounts no
	// ChatService and the cairnos chat command reports the node has chat
	// disabled.
	Enabled bool

	// OpenAICompat mounts the OpenAI-compatible chat completions adapter over
	// the chat surface, so off-the-shelf OpenAI clients can point at this node
	// (.agents/spec/parity/openai-compat.md). Default false; it requires chat
	// enabled, so serve fails fast when this is on with chat off.
	OpenAICompat bool
}

// Webhook configures the webhook-ingress subsystem
// (.agents/spec/parity/triggers-webhooks.md, "Webhook ingress"). Off by
// default: serve mounts no ingress routes until [webhook] enabled is true and
// an admin has registered at least one workflow with `cairnos webhook add`, so
// the default posture exposes no public webhook surface.
type Webhook struct {
	// Enabled gates the ingress routes. With it off, buildWebhookExtension
	// returns nil and serve mounts nothing, so an unregistered node is
	// unreachable by design.
	Enabled bool

	// RatePerMinute is the default per-registration delivery cap; a registration
	// may override it at add time. Below 1 falls back to the handler default.
	RatePerMinute int

	// TestWait bounds how long the test endpoint holds a connection waiting for
	// the terminal run before it answers 202 and lets the run finish in the
	// background. The shared server carries no WriteTimeout, so this is the only
	// bound on a slow test run pinning a connection.
	TestWait time.Duration

	// SignatureSkew is the freshness window for the native signature: a
	// timestamp further than this from now is rejected, and the nonce cache
	// retains entries for this long, so a captured request cannot replay past it
	// (security amendment 1). Below 1 falls back to the handler default.
	SignatureSkew time.Duration
}

// Bridge configures the Phase 9b messaging bridge
// (.agents/spec/parity/messaging-bridge.md). Off by default; a second transport
// beyond Telegram is additive.
type Bridge struct {
	Telegram   Telegram
	Mattermost Mattermost
}

// Telegram configures the Telegram long-poll bridge. The bot token is a
// credential, so it is read only from CAIRNOS_BRIDGE_TELEGRAM_TOKEN at serve
// wiring, never a config-file key. There is deliberately no base_url key: a
// configurable base URL plus the token-in-path Bot API is a token-exfiltration
// vector, so the client's base URL stays a test-only unexported field
// (.agents/spec/parity/messaging-bridge.md, decision 3).
type Telegram struct {
	// Users is the allowlist keyed by Telegram user ID (as a string), mapping
	// each permitted sender to a workspace and its grants. An enabled bridge with
	// no users is a configuration error. The map has no env form; it is file-only.
	Users map[string]TelegramUser

	// PollTimeout is the long-poll hold time per getUpdates call.
	PollTimeout time.Duration

	// SendTimeout bounds each outbound Bot API call.
	SendTimeout time.Duration

	// ConfirmTTL is how long an unanswered D9 proposal stays pending before it
	// expires as a deny.
	ConfirmTTL time.Duration

	// Enabled turns the bridge on. With it off, serve starts no long-poll loop
	// and logs one line. It is also treated as off when the token env is unset.
	Enabled bool

	// Verbose relays tool-call status lines as italic messages when true;
	// otherwise only assistant text and confirmations reach the chat.
	Verbose bool
}

// TelegramUser maps one allowlisted Telegram sender to a workspace and its
// grants. The bridge is an in-process trusted surface with no token to carry
// scopes, so approving a D9 workflow is a distinct operator-set grant
// (AllowConfirm) rather than a scope check
// (.agents/spec/parity/messaging-bridge.md, decision 4).
type TelegramUser struct {
	// Workspace is the workspace every turn from this sender is stamped with,
	// exactly as the gRPC interceptor would. Required: an empty workspace is a
	// configuration error.
	Workspace string

	// Personality names the chat personality a new session for this sender gets.
	// Empty falls back to [chat] default_personality.
	Personality string

	// AllowConfirm grants this sender the right to Approve a D9 proposal. Default
	// false: chat access does not imply workflow-approval authority, so a
	// chat-only sender's Approve tap is refused.
	AllowConfirm bool
}

// Notify configures outbound event notifications
// (.agents/spec/parity/messaging-bridge.md, decision 7). The Discord webhook URL
// is a credential, so it stays in CAIRNOS_NOTIFY_DISCORD_WEBHOOK, never a config
// key.
type Notify struct {
	Discord    Discord
	Mattermost NotifyMattermost
}

// Discord selects which engine and queue events fan out to the shipped Discord
// webhook. The default set is exactly ["held"], which keeps today's
// re-authentication hold message byte-identical.
type Discord struct {
	// Events is the selected event set. Every value must be one of held,
	// run_failed, dead_lettered, budget_denied, connectivity, or source_dirty;
	// any other value is a configuration error.
	Events []string
}

// Default returns the settings a fresh `cairnos init` ships: local-only,
// SQLite, auth and API off, so the result runs immediately.
func Default() Config {
	return Config{
		Engine: Engine{
			MaxConcurrentActions: 4,
			DefaultTimeout:       10 * time.Minute,
			MaxCallDepth:         3,
			Retry: Retry{
				MaxAttempts: 3,
				Backoff:     "exponential",
				BaseDelay:   time.Second,
				MaxDelay:    30 * time.Second,
				Jitter:      true,
			},
		},
		Store:   Store{Backend: "sqlite", DSN: "file:./data/cairnos.db"},
		Secrets: Secrets{Backend: SecretsBackendEnv},
		Memory:  Memory{Backend: "sqlite", EmbeddingRunner: "openai"},
		Retention: Retention{
			Runs:    90 * 24 * time.Hour,
			Outputs: 30 * 24 * time.Hour,
			Sweep:   6 * time.Hour,
		},
		Telemetry: Telemetry{Enabled: true, Prometheus: ":9090"},
		API: API{
			GRPC:            ":7777",
			HTTP:            ":8080",
			MaxBodyBytes:    defaultMaxBodyBytes,
			MaxRecvMsgBytes: defaultMaxRecvMsgBytes,
		},
		Auth: Auth{
			Issuer:              "cairnos",
			Audience:            "cairnos",
			AccessTokenTTL:      time.Hour,
			SigningAlgorithm:    "HS256",
			RequireExp:          true,
			RequireJTI:          true,
			MaxConcurrentLogins: defaultMaxConcurrentLogins,
			LoginRatePerMinute:  defaultLoginRatePerMinute,
			StreamRecheck:       defaultStreamRecheck,
			ClockSkew:           defaultClockSkew,
		},
		OAuth: OAuth{
			DefaultWorkspace: defaultOAuthWorkspace,
			DefaultRole:      defaultOAuthRole,
		},
		Hive: Hive{
			KAnonymityFloor: defaultKAnonymityFloor,
			MinObservations: defaultMinObservations,
		},
		MCP:    MCP{Transport: MCPTransportHTTP, Path: defaultMCPPath},
		Tuning: Tuning{MinSavingsUSD: defaultMinSavingsUSD},
		Scheduler: Scheduler{
			AckWait:           defaultSchedulerAckWait,
			Backoff:           defaultSchedulerBackoff,
			MaxDeliveries:     defaultSchedulerMaxDeliveries,
			WorkerConcurrency: defaultSchedulerConcurrency,
			Prefetch:          defaultSchedulerPrefetch,
			SweepInterval:     defaultSchedulerSweepInterval,
		},
		Chat: Chat{
			Driver:             defaultChatDriver,
			DefaultPersonality: defaultChatPersonality,
			MaxFileBytes:       defaultChatMaxFileBytes,
		},
		Webhook: Webhook{
			RatePerMinute: defaultWebhookRatePerMinute,
			TestWait:      defaultWebhookTestWait,
			SignatureSkew: defaultWebhookSignatureSkew,
		},
		Bridge: Bridge{
			Telegram: Telegram{
				PollTimeout: defaultBridgePollTimeout,
				SendTimeout: defaultBridgeSendTimeout,
				ConfirmTTL:  defaultBridgeConfirmTTL,
			},
			Mattermost: Mattermost{
				// Send timeout and confirm TTL mean the same thing on both
				// transports, so they share one constant rather than drifting.
				SendTimeout: defaultBridgeSendTimeout,
				ConfirmTTL:  defaultBridgeConfirmTTL,
				RatePerSec:  defaultMattermostRatePerSec,
				RateBurst:   defaultMattermostRateBurst,
				BotLoop: BotLoop{
					MaxEvents: defaultMattermostBotLoopMaxEvents,
					Window:    defaultMattermostBotLoopWindow,
					Cooldown:  defaultMattermostBotLoopCooldown,
				},
			},
		},
		Notify: Notify{
			Discord: Discord{
				// A fresh slice per call so a mutated result never leaks into
				// the next Default(); ["held"] keeps today's behavior.
				Events: []string{NotifyEventHeld},
			},
			Mattermost: NotifyMattermost{
				// Same default set as Discord, so naming an identity and a
				// channel is the only step to the hold notice. A fresh slice
				// per call, for the same reason as above.
				Events: []string{NotifyEventHeld},
			},
		},
		Runners:     map[string]map[string]any{},
		Credentials: map[string]Credential{},
		Sources:     map[string]Source{},
		Hosts:       map[string]Host{},
	}
}

// Webhook ingress defaults (.agents/spec/parity/triggers-webhooks.md,
// "Config"). The per-registration cap matches the login rate's calm-node
// default; the test endpoint holds a connection at most 30s before it answers
// 202 and lets the run finish in the background. Both only matter once an
// operator sets [webhook] enabled = true.
const (
	defaultWebhookRatePerMinute = 60
	defaultWebhookTestWait      = 30 * time.Second
	// defaultWebhookSignatureSkew is the native-signature freshness window and
	// the nonce-cache retention (security amendment 1). Five minutes tolerates
	// clock drift between a sender and this node while keeping the replay window
	// short.
	defaultWebhookSignatureSkew = 5 * time.Minute
)

// Chat defaults: the anthropic runner drives turns (it implements ToolCaller),
// and the assistant personality frames a session that names none
// (.agents/spec/13-roadmap.md, Phase 9). Chat stays off until [chat] enabled is
// set, so these only matter once an operator turns it on.
const (
	defaultChatDriver      = "anthropic"
	defaultChatPersonality = "assistant"
	// defaultChatMaxFileBytes caps one inlined @-file reference at 64 KiB (the
	// shift is byte-size arithmetic), so an @-ref cannot dominate a prompt even
	// once an operator opts a root into chat.file_allowlist.
	defaultChatMaxFileBytes = 64 << 10
)

// Telegram bridge timing defaults (.agents/spec/parity/messaging-bridge.md,
// "Config"). The long poll holds each getUpdates call for 50s, each outbound
// call gets 10s, and an unanswered proposal expires as deny after 10m. All only
// matter once an operator sets [bridge.telegram] enabled = true.
const (
	defaultBridgePollTimeout = 50 * time.Second
	defaultBridgeSendTimeout = 10 * time.Second
	defaultBridgeConfirmTTL  = 10 * time.Minute
)

// Discord fan-out event names (.agents/spec/parity/messaging-bridge.md,
// decision 7). These are the only values [notify.discord] events accepts; the
// default set is exactly held, which keeps today's re-auth hold behavior.
// Exported so the serve producers (held, run_failed, dead_lettered,
// budget_denied) name the same events config validates against, one source of
// truth rather than magic strings on both sides of the wiring.
const (
	NotifyEventHeld         = "held"
	NotifyEventRunFailed    = "run_failed"
	NotifyEventDeadLettered = "dead_lettered"
	NotifyEventBudgetDenied = "budget_denied"
	// NotifyEventConnectivity carries the reachability monitor's lines: a
	// workflow paused because a host it requires is down, the hourly reminder
	// while it stays down, and the recovery.
	NotifyEventConnectivity = "connectivity"
	// NotifyEventSourceDirty carries a source sync that left a tree alone
	// because dirty_reset is off. The sync itself succeeds, so this is the only
	// line an operator gets, and its producer rate-limits it per source.
	NotifyEventSourceDirty = "source_dirty"
)

// notifyEvents returns the closed vocabulary in the order the load error lists
// it, so the whitelist and the message an operator reads cannot drift apart. It
// is a function rather than a package var because gochecknoglobals refuses one,
// and its caller reads it once per key rather than once per event.
func notifyEvents() []string {
	return []string{
		NotifyEventHeld,
		NotifyEventRunFailed,
		NotifyEventDeadLettered,
		NotifyEventBudgetDenied,
		NotifyEventConnectivity,
		NotifyEventSourceDirty,
	}
}

// Hive publish floor defaults (.agents/spec/09-a2a-and-hive.md,
// "k-anonymity threshold"). The k-anonymity floor is the server-side k the
// client knows about; the local min-observations floor is the count a node
// insists on seeing before it will publish a pattern, ahead of the server's k.
const (
	defaultKAnonymityFloor = 5
	defaultMinObservations = 5
)

// OAuth login defaults: a first-time human lands in the default workspace as
// a viewer, the least-privilege role, so a new sign-in cannot change anything
// until an admin grants more (.agents/spec/12-ui-and-governance.md,
// "Multi-tenancy").
const (
	defaultOAuthWorkspace = "default"
	defaultOAuthRole      = "viewer"
)

// defaultMinSavingsUSD is the spec's suggestion savings floor: a penny per
// run, low enough to surface real waste and high enough to keep noise out
// of the review queue (.agents/spec/06-observability-and-tuning.md).
const defaultMinSavingsUSD = 0.01

// API request-size defaults (.agents/spec/08-auth-and-access.md, M4). The
// gateway body cap and the gRPC message cap both land at 1 MiB, enough for the
// JSON and proto payloads CairnOS exchanges and small enough that an
// over-limit request is rejected before it buffers. 1 << 20 is shift form for
// 1 MiB per the house power-of-two rule.
const (
	defaultMaxBodyBytes    = 1 << 20
	defaultMaxRecvMsgBytes = 1 << 20
)

// Login DoS and verification defaults (.agents/spec/08-auth-and-access.md,
// M1/M4/L2). The login pool is small because each argon2id derivation costs
// ~64 MiB; the per-peer rate is one login per second on average; the stream
// recheck and the cross-instance clock skew are the spec's stated defaults.
const (
	defaultMaxConcurrentLogins = 4
	defaultLoginRatePerMinute  = 60
	defaultStreamRecheck       = 15 * time.Second
	defaultClockSkew           = 30 * time.Second
)

// Load resolves the configuration: defaults, the TOML file when present, then
// CAIRNOS_* environment overrides. A missing file is not an error (defaults
// serve); an unreadable or invalid file is.
func Load(path string) (Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path) // #nosec G304 -- the config path is the operator's own choice
	if err != nil && !os.IsNotExist(err) {
		return cfg, fmt.Errorf("read config: %w", err)
	}

	if err == nil {
		if applyErr := applyTOML(&cfg, data); applyErr != nil {
			return cfg, applyErr
		}
	}

	if envErr := applyEnv(&cfg, os.Getenv); envErr != nil {
		return cfg, envErr
	}

	if validErr := validateSecrets(&cfg.Secrets); validErr != nil {
		return cfg, validErr
	}

	if validErr := validateAuth(&cfg.Auth); validErr != nil {
		return cfg, validErr
	}

	if validErr := validateOAuth(&cfg.OAuth); validErr != nil {
		return cfg, validErr
	}

	if validErr := validateCredentials(cfg.Credentials); validErr != nil {
		return cfg, validErr
	}

	// The config file's own directory is the CairnOS home a synced tree must stay
	// out of, so a source declared in a scaffold cannot land inside it.
	if validErr := validateSources(cfg.Sources, filepath.Dir(path)); validErr != nil {
		return cfg, validErr
	}

	if validErr := validateHosts(cfg.Hosts); validErr != nil {
		return cfg, validErr
	}

	if validErr := validateRunnerNames(cfg.Runners); validErr != nil {
		return cfg, validErr
	}

	if validErr := validateBridge(&cfg.Bridge.Telegram); validErr != nil {
		return cfg, validErr
	}

	if validErr := validateMattermost(&cfg.Bridge.Mattermost, cfg.Notify.Mattermost.Channel); validErr != nil {
		return cfg, validErr
	}

	if validErr := validateNotify(&cfg.Notify, cfg.Bridge.Mattermost.Identity); validErr != nil {
		return cfg, validErr
	}

	return cfg, nil
}

// Signing algorithms the auth layer accepts (mirrors auth.AlgHS256 and
// auth.AlgEdDSA; named here so config validation does not pull the auth
// package's database dependencies into the config package).
const (
	signingAlgHS256 = "HS256"
	signingAlgEdDSA = "EdDSA"
)

// validateSecrets rejects a [secrets] backend outside the known set at load, so
// a typo fails at startup with config.invalid rather than silently falling back
// to env and hiding the store an operator meant to use.
func validateSecrets(secrets *Secrets) error {
	switch secrets.Backend {
	case SecretsBackendEnv, SecretsBackendStore:
	default:
		return fmt.Errorf("%w: secrets.backend %q is not %s or %s",
			ErrInvalidConfig, secrets.Backend, SecretsBackendEnv, SecretsBackendStore)
	}

	return validateBitwarden(secrets.Bitwarden)
}

// validateAuth fails fast when auth is enabled with an unusable identity:
// an empty issuer or audience, a non-positive TTL, or an unknown signing
// algorithm would all break token issue or verification at runtime, so they
// are rejected at load with config.invalid (spec L1). Auth-off configs skip
// this; the token path never runs.
func validateAuth(auth *Auth) error {
	if !auth.Enabled {
		return nil
	}

	switch {
	case auth.Issuer == "":
		return fmt.Errorf("%w: auth.issuer must be set when auth is enabled", ErrInvalidConfig)
	case auth.Audience == "":
		return fmt.Errorf("%w: auth.audience must be set when auth is enabled", ErrInvalidConfig)
	case auth.AccessTokenTTL <= 0:
		return fmt.Errorf("%w: auth.access_token_ttl must be positive, got %s", ErrInvalidConfig, auth.AccessTokenTTL)
	case auth.SigningAlgorithm != signingAlgHS256 && auth.SigningAlgorithm != signingAlgEdDSA:
		return fmt.Errorf("%w: auth.signing_algorithm %q is not %s or %s",
			ErrInvalidConfig, auth.SigningAlgorithm, signingAlgHS256, signingAlgEdDSA)
	default:
		return nil
	}
}

// OAuth roles a default_role may name (mirrors tenancy.Role*; named here so
// config validation does not pull the tenancy package's database dependencies
// into config). The OAuth tests pin these equal to the tenancy constants.
const (
	roleAdmin  = "admin"
	roleEditor = "editor"
	roleViewer = "viewer"
)

// validateOAuth fails fast when the OIDC login path is enabled but
// misconfigured: a missing issuer, client id, redirect url, or client secret
// breaks login at the first request, and an unknown default role grants no
// scopes. The client secret is checked here so a misconfigured deployment
// fails at startup, not at first login.
func validateOAuth(oauth *OAuth) error {
	if !oauth.Enabled {
		return nil
	}

	switch {
	case oauth.Issuer == "":
		return fmt.Errorf("%w: oauth.issuer must be set when oauth is enabled", ErrInvalidConfig)
	case oauth.ClientID == "":
		return fmt.Errorf("%w: oauth.client_id must be set when oauth is enabled", ErrInvalidConfig)
	case oauth.ClientSecret == "":
		return fmt.Errorf("%w: CAIRNOS_OAUTH_CLIENT_SECRET must be set when oauth is enabled", ErrInvalidConfig)
	case oauth.RedirectURL == "":
		return fmt.Errorf("%w: oauth.redirect_url must be set when oauth is enabled", ErrInvalidConfig)
	case oauth.DefaultWorkspace == "":
		return fmt.Errorf("%w: oauth.default_workspace must be set when oauth is enabled", ErrInvalidConfig)
	case !validOAuthRole(oauth.DefaultRole):
		return fmt.Errorf("%w: oauth.default_role %q is not %s, %s, or %s",
			ErrInvalidConfig, oauth.DefaultRole, roleAdmin, roleEditor, roleViewer)
	default:
		return nil
	}
}

// validOAuthRole reports whether role is one of the defined roles.
func validOAuthRole(role string) bool {
	return role == roleAdmin || role == roleEditor || role == roleViewer
}

// validateBridge fails fast when the Telegram bridge is enabled but unusable:
// no allowlisted users accepts nobody, and a user entry with no workspace has
// nothing to stamp turns with. The chat cross-check (bridge on while [chat] is
// off) lives at serve wiring (.agents/spec/parity/messaging-bridge.md,
// decision 6). Iteration is sorted by ID so the reported entry is reproducible.
func validateBridge(telegram *Telegram) error {
	if !telegram.Enabled {
		return nil
	}

	if len(telegram.Users) == 0 {
		return fmt.Errorf("%w: bridge.telegram.users must list at least one user when the bridge is enabled", ErrInvalidConfig)
	}

	ids := make([]string, 0, len(telegram.Users))
	for id := range telegram.Users {
		ids = append(ids, id)
	}

	slices.Sort(ids)

	for _, id := range ids {
		if telegram.Users[id].Workspace == "" {
			return fmt.Errorf("%w: bridge.telegram.users.%s.workspace must be set", ErrInvalidConfig, id)
		}
	}

	return nil
}

// validateNotify rejects any event value outside the known set, so a typo'd
// event name fails at load rather than silently emitting nothing. It runs
// regardless of the bridge: fan-out is its own subsystem. identities is the
// declared Mattermost identity set, which the Mattermost sink must name one of.
func validateNotify(notify *Notify, identities map[string]MattermostIdentity) error {
	if err := validateNotifyEvents("notify.discord.events", notify.Discord.Events); err != nil {
		return err
	}

	if err := validateNotifyEvents("notify.mattermost.events", notify.Mattermost.Events); err != nil {
		return err
	}

	return validateNotifyMattermost(&notify.Mattermost, identities)
}

// validateNotifyEvents reports the failing value under the key it was read
// from, so an operator with both sinks configured knows which list to fix.
func validateNotifyEvents(key string, events []string) error {
	known := notifyEvents()

	for _, event := range events {
		if !slices.Contains(known, event) {
			return fmt.Errorf("%w: %s %q is not one of %s",
				ErrInvalidConfig, key, event, strings.Join(known, ", "))
		}
	}

	return nil
}

// fileConfig mirrors the TOML layout with string durations, so duration
// parsing (including the day suffix) goes through workflow.ParseDuration.
type fileConfig struct {
	Bridge struct {
		Telegram struct {
			Enabled     *bool                     `toml:"enabled"`
			PollTimeout *string                   `toml:"poll_timeout"`
			SendTimeout *string                   `toml:"send_timeout"`
			ConfirmTTL  *string                   `toml:"confirm_ttl"`
			Verbose     *bool                     `toml:"verbose"`
			Users       map[string]fileBridgeUser `toml:"users"`
		} `toml:"telegram"`
		Mattermost fileMattermost `toml:"mattermost"`
	} `toml:"bridge"`
	Auth struct {
		Enabled                   *bool   `toml:"enabled"`
		Issuer                    *string `toml:"issuer"`
		Audience                  *string `toml:"audience"`
		AccessTokenTTL            *string `toml:"access_token_ttl"`
		SigningAlgorithm          *string `toml:"signing_algorithm"`
		RequireExp                *bool   `toml:"require_exp"`
		RequireJTI                *bool   `toml:"require_jti"`
		ServeWithoutAuthOnNetwork *bool   `toml:"serve_without_auth_on_network"`
		TLSTerminated             *bool   `toml:"tls_terminated"`
		MaxConcurrentLogins       *int    `toml:"max_concurrent_logins"`
		LoginRatePerMinute        *int    `toml:"login_rate_per_minute"`
		StreamRecheck             *string `toml:"stream_recheck"`
		ClockSkew                 *string `toml:"clock_skew"`
	} `toml:"auth"`
	Hive struct {
		Enabled         *bool    `toml:"enabled"`
		Endpoint        *string  `toml:"endpoint"`
		NodePseudonym   *string  `toml:"node_pseudonym"`
		PublishEnabled  *bool    `toml:"publish_enabled"`
		KAnonymityFloor *int     `toml:"k_anonymity_floor"`
		MinObservations *int     `toml:"min_observations"`
		KeyRef          *string  `toml:"key_ref"`
		PrevKeyRef      *string  `toml:"prev_key_ref"`
		Categories      []string `toml:"categories"`
	} `toml:"hive"`
	Chat struct {
		Enabled            *bool             `toml:"enabled"`
		Driver             *string           `toml:"driver"`
		DefaultPersonality *string           `toml:"default_personality"`
		Personalities      map[string]string `toml:"personalities"`
		TurnBudgetUSD      *float64          `toml:"turn_budget_usd"`
		OpenAICompat       *bool             `toml:"openai_compat"`
		MaxFileBytes       *int              `toml:"max_file_bytes"`
		FileAllowlist      []string          `toml:"file_allowlist"`
	} `toml:"chat"`
	API struct {
		Enabled         *bool    `toml:"enabled"`
		GRPC            *string  `toml:"grpc"`
		HTTP            *string  `toml:"http"`
		MaxBodyBytes    *int64   `toml:"max_body_bytes"`
		MaxRecvMsgBytes *int     `toml:"max_recv_msg_bytes"`
		CORSOrigins     []string `toml:"cors_origins"`
	} `toml:"api"`
	Scheduler struct {
		AckWait           *string `toml:"ack_wait"`
		Backoff           *string `toml:"backoff"`
		MaxDeliveries     *int    `toml:"max_deliveries"`
		MaxAge            *string `toml:"max_age"`
		WorkerConcurrency *int    `toml:"worker_concurrency"`
		Prefetch          *int    `toml:"prefetch"`
		SweepInterval     *string `toml:"sweep_interval"`
	} `toml:"scheduler"`
	A2A struct {
		Enabled            *bool   `toml:"enabled"`
		CardSigningKeyFile *string `toml:"card_signing_key_file"`
		EndpointAdvertise  *string `toml:"endpoint_advertise"`
		CertFile           *string `toml:"cert_file"`
		KeyFile            *string `toml:"key_file"`
		ClientCAFile       *string `toml:"client_ca_file"`
	} `toml:"a2a"`
	MCP struct {
		Enabled      *bool   `toml:"enabled"`
		Transport    *string `toml:"transport"`
		Path         *string `toml:"path"`
		WorkflowsDir *string `toml:"workflows_dir"`
		RegistryURL  *string `toml:"registry_url"`
		AdvertiseURL *string `toml:"advertise_url"`
	} `toml:"mcp"`
	Engine struct {
		MaxConcurrentActions *int                `toml:"max_concurrent_actions"`
		DefaultTimeout       *string             `toml:"default_timeout"`
		MaxCallDepth         *int                `toml:"max_call_depth"`
		Retry                *fileRetry          `toml:"retry"`
		Fallback             map[string][]string `toml:"fallback"`
	} `toml:"engine"`
	Webhook struct {
		Enabled       *bool   `toml:"enabled"`
		RatePerMinute *int    `toml:"rate_per_minute"`
		TestWait      *string `toml:"test_wait"`
		SignatureSkew *string `toml:"signature_skew"`
	} `toml:"webhook"`
	Memory struct {
		Backend         *string `toml:"backend"`
		Vector          *bool   `toml:"vector"`
		EmbeddingRunner *string `toml:"embedding_runner"`
	} `toml:"memory"`
	Retention struct {
		Runs    *string `toml:"runs"`
		Outputs *string `toml:"outputs"`
		Sweep   *string `toml:"sweep"`
	} `toml:"retention"`
	Telemetry struct {
		Enabled      *bool   `toml:"enabled"`
		Prometheus   *string `toml:"prometheus"`
		OTLPEndpoint *string `toml:"otlp_endpoint"`
	} `toml:"telemetry"`
	Tuning struct {
		MinSavingsUSD        *float64 `toml:"min_savings_usd"`
		EmbeddingModel       *string  `toml:"embedding_model"`
		EquivalenceThreshold *float64 `toml:"equivalence_threshold"`
	} `toml:"tuning"`
	Store struct {
		Backend *string `toml:"backend"`
		DSN     *string `toml:"dsn"`
	} `toml:"store"`
	Autonomic struct {
		AutoApplyEnabled *bool `toml:"auto_apply_enabled"`
	} `toml:"autonomic"`
	Secrets struct {
		Backend *string `toml:"backend"`

		// Bitwarden holds the [secrets.bitwarden] block verbatim so parsing can
		// reject unknown keys itself, the same trap [credential.<name>] uses.
		//
		// The pointer separates a header written with no keys, which is a working
		// config, from no header at all. Both unmarshal to a nil map.
		Bitwarden *map[string]any `toml:"bitwarden"`
	} `toml:"secrets"`
	Runner map[string]map[string]any `toml:"runner"`

	// Credential holds each [credential.<name>] block verbatim so parsing can
	// reject unknown keys itself: go-toml drops unknown struct fields silently,
	// which would let a plaintext client_secret or a typo'd secret key sit in the
	// file unnoticed (security amendment 4).
	Credential map[string]map[string]any `toml:"credential"`

	// Host holds each [host.<name>] block verbatim, nested tool blocks included:
	// go-toml hands the whole two-level shape back inside this one raw type, and
	// parsing it here is what lets an unknown key at any level fail Load.
	Host map[string]map[string]any `toml:"host"`

	// Source holds each [source.<name>] block verbatim for the same reason as
	// Credential: go-toml drops unknown struct fields silently, which would leave
	// a typo'd dest or ref syncing a tree the operator never meant to name.
	Source map[string]map[string]any `toml:"source"`

	OAuth struct {
		Enabled             *bool    `toml:"enabled"`
		Issuer              *string  `toml:"issuer"`
		ClientID            *string  `toml:"client_id"`
		RedirectURL         *string  `toml:"redirect_url"`
		DefaultWorkspace    *string  `toml:"default_workspace"`
		DefaultRole         *string  `toml:"default_role"`
		AutoProvision       *bool    `toml:"auto_provision"`
		AllowedEmailDomains []string `toml:"allowed_email_domains"`
		AllowedSubjects     []string `toml:"allowed_subjects"`
	} `toml:"oauth"`
	Notify struct {
		Discord struct {
			Events []string `toml:"events"`
		} `toml:"discord"`
		Mattermost fileNotifyMattermost `toml:"mattermost"`
	} `toml:"notify"`
	Env struct {
		Allowlist []string `toml:"allowlist"`
	} `toml:"env"`
}

// fileBridgeUser mirrors one [bridge.telegram.users.<id>] table. Each entry
// replaces wholesale (there is no per-user default to merge with), so plain
// values stand in for the pointer-optional pattern used elsewhere. There is no
// token field here: the token is env-only (decision 3).
type fileBridgeUser struct {
	Workspace    string `toml:"workspace"`
	Personality  string `toml:"personality"`
	AllowConfirm bool   `toml:"allow_confirm"`
}

type fileRetry struct {
	MaxAttempts *int    `toml:"max_attempts"`
	Backoff     *string `toml:"backoff"`
	BaseDelay   *string `toml:"base_delay"`
	MaxDelay    *string `toml:"max_delay"`
	Jitter      *bool   `toml:"jitter"`
}

func applyTOML(cfg *Config, data []byte) error {
	var file fileConfig

	if err := toml.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}

	applyScalars(cfg, &file)

	if err := rejectMattermostToken(file.Bridge.Mattermost.Identity); err != nil {
		return err
	}

	if err := applyDurations(cfg, &file); err != nil {
		return err
	}

	if file.Runner != nil {
		cfg.Runners = file.Runner
	}

	if file.Credential != nil {
		credentials, err := parseCredentialBlocks(file.Credential)
		if err != nil {
			return err
		}

		cfg.Credentials = credentials
	}

	if file.Source != nil {
		sources, err := parseSourceBlocks(file.Source)
		if err != nil {
			return err
		}

		cfg.Sources = sources
	}

	if file.Secrets.Bitwarden != nil {
		block, err := parseBitwardenBlock(*file.Secrets.Bitwarden)
		if err != nil {
			return err
		}

		cfg.Secrets.Bitwarden = block
	}

	if file.Host != nil {
		hosts, err := parseHostBlocks(file.Host)
		if err != nil {
			return err
		}

		cfg.Hosts = hosts
	}

	return nil
}

// applyScalars copies every present (non-nil) non-duration value. Pointer
// fields distinguish "absent, keep the default" from "present, even if zero".
func applyScalars(cfg *Config, file *fileConfig) {
	setIf(&cfg.Engine.MaxConcurrentActions, file.Engine.MaxConcurrentActions)
	setIf(&cfg.Engine.MaxCallDepth, file.Engine.MaxCallDepth)
	setIf(&cfg.Store.Backend, file.Store.Backend)
	setIf(&cfg.Store.DSN, file.Store.DSN)
	setIf(&cfg.Secrets.Backend, file.Secrets.Backend)
	setIf(&cfg.Memory.Backend, file.Memory.Backend)
	setIf(&cfg.Memory.Vector, file.Memory.Vector)
	setIf(&cfg.Memory.EmbeddingRunner, file.Memory.EmbeddingRunner)
	setIf(&cfg.Telemetry.Enabled, file.Telemetry.Enabled)
	setIf(&cfg.Telemetry.Prometheus, file.Telemetry.Prometheus)
	setIf(&cfg.Telemetry.OTLPEndpoint, file.Telemetry.OTLPEndpoint)
	setIf(&cfg.API.Enabled, file.API.Enabled)
	setIf(&cfg.API.GRPC, file.API.GRPC)
	setIf(&cfg.API.HTTP, file.API.HTTP)
	setIf(&cfg.API.MaxBodyBytes, file.API.MaxBodyBytes)
	setIf(&cfg.API.MaxRecvMsgBytes, file.API.MaxRecvMsgBytes)
	setIf(&cfg.Auth.Enabled, file.Auth.Enabled)
	setIf(&cfg.Auth.Issuer, file.Auth.Issuer)
	setIf(&cfg.Auth.Audience, file.Auth.Audience)
	setIf(&cfg.Auth.SigningAlgorithm, file.Auth.SigningAlgorithm)
	setIf(&cfg.Auth.RequireExp, file.Auth.RequireExp)
	setIf(&cfg.Auth.RequireJTI, file.Auth.RequireJTI)
	setIf(&cfg.Auth.ServeWithoutAuthOnNetwork, file.Auth.ServeWithoutAuthOnNetwork)
	setIf(&cfg.Auth.TLSTerminated, file.Auth.TLSTerminated)
	setIf(&cfg.Auth.MaxConcurrentLogins, file.Auth.MaxConcurrentLogins)
	setIf(&cfg.Auth.LoginRatePerMinute, file.Auth.LoginRatePerMinute)
	setIf(&cfg.OAuth.Enabled, file.OAuth.Enabled)
	setIf(&cfg.OAuth.Issuer, file.OAuth.Issuer)
	setIf(&cfg.OAuth.ClientID, file.OAuth.ClientID)
	setIf(&cfg.OAuth.RedirectURL, file.OAuth.RedirectURL)
	setIf(&cfg.OAuth.DefaultWorkspace, file.OAuth.DefaultWorkspace)
	setIf(&cfg.OAuth.DefaultRole, file.OAuth.DefaultRole)
	setIf(&cfg.OAuth.AutoProvision, file.OAuth.AutoProvision)
	setIf(&cfg.A2A.Enabled, file.A2A.Enabled)
	setIf(&cfg.A2A.CardSigningKeyFile, file.A2A.CardSigningKeyFile)
	setIf(&cfg.A2A.EndpointAdvertise, file.A2A.EndpointAdvertise)
	setIf(&cfg.A2A.CertFile, file.A2A.CertFile)
	setIf(&cfg.A2A.KeyFile, file.A2A.KeyFile)
	setIf(&cfg.A2A.ClientCAFile, file.A2A.ClientCAFile)
	setIf(&cfg.MCP.Enabled, file.MCP.Enabled)
	setIf(&cfg.MCP.Transport, file.MCP.Transport)
	setIf(&cfg.MCP.Path, file.MCP.Path)
	setIf(&cfg.MCP.WorkflowsDir, file.MCP.WorkflowsDir)
	setIf(&cfg.MCP.RegistryURL, file.MCP.RegistryURL)
	setIf(&cfg.MCP.AdvertiseURL, file.MCP.AdvertiseURL)
	setIf(&cfg.Hive.Enabled, file.Hive.Enabled)
	setIf(&cfg.Hive.Endpoint, file.Hive.Endpoint)
	setIf(&cfg.Hive.NodePseudonym, file.Hive.NodePseudonym)
	setIf(&cfg.Hive.PublishEnabled, file.Hive.PublishEnabled)
	setIf(&cfg.Hive.KAnonymityFloor, file.Hive.KAnonymityFloor)
	setIf(&cfg.Hive.MinObservations, file.Hive.MinObservations)
	setIf(&cfg.Hive.KeyRef, file.Hive.KeyRef)
	setIf(&cfg.Hive.PrevKeyRef, file.Hive.PrevKeyRef)
	setIf(&cfg.Autonomic.AutoApplyEnabled, file.Autonomic.AutoApplyEnabled)
	setIf(&cfg.Tuning.MinSavingsUSD, file.Tuning.MinSavingsUSD)
	setIf(&cfg.Tuning.EmbeddingModel, file.Tuning.EmbeddingModel)
	setIf(&cfg.Tuning.EquivalenceThreshold, file.Tuning.EquivalenceThreshold)
	setIf(&cfg.Scheduler.MaxDeliveries, file.Scheduler.MaxDeliveries)
	setIf(&cfg.Scheduler.WorkerConcurrency, file.Scheduler.WorkerConcurrency)
	setIf(&cfg.Scheduler.Prefetch, file.Scheduler.Prefetch)
	setIf(&cfg.Chat.Enabled, file.Chat.Enabled)
	setIf(&cfg.Chat.Driver, file.Chat.Driver)
	setIf(&cfg.Chat.DefaultPersonality, file.Chat.DefaultPersonality)
	setIf(&cfg.Chat.TurnBudgetUSD, file.Chat.TurnBudgetUSD)
	setIf(&cfg.Chat.OpenAICompat, file.Chat.OpenAICompat)
	setIf(&cfg.Chat.MaxFileBytes, file.Chat.MaxFileBytes)
	setIf(&cfg.Webhook.Enabled, file.Webhook.Enabled)
	setIf(&cfg.Webhook.RatePerMinute, file.Webhook.RatePerMinute)
	setIf(&cfg.Bridge.Telegram.Enabled, file.Bridge.Telegram.Enabled)
	setIf(&cfg.Bridge.Telegram.Verbose, file.Bridge.Telegram.Verbose)

	if file.Bridge.Telegram.Users != nil {
		cfg.Bridge.Telegram.Users = convertBridgeUsers(file.Bridge.Telegram.Users)
	}

	setIf(&cfg.Bridge.Mattermost.URL, file.Bridge.Mattermost.URL)
	setIf(&cfg.Bridge.Mattermost.Team, file.Bridge.Mattermost.Team)
	setIf(&cfg.Bridge.Mattermost.InsecureHTTP, file.Bridge.Mattermost.InsecureHTTP)
	setIf(&cfg.Bridge.Mattermost.RatePerSec, file.Bridge.Mattermost.RatePerSec)
	setIf(&cfg.Bridge.Mattermost.RateBurst, file.Bridge.Mattermost.RateBurst)
	setIf(&cfg.Bridge.Mattermost.BotLoop.MaxEvents, file.Bridge.Mattermost.BotLoop.MaxEvents)
	setIf(&cfg.Notify.Mattermost.Identity, file.Notify.Mattermost.Identity)
	setIf(&cfg.Notify.Mattermost.Channel, file.Notify.Mattermost.Channel)

	if file.Bridge.Mattermost.Identity != nil {
		cfg.Bridge.Mattermost.Identity = convertMattermostIdentities(file.Bridge.Mattermost.Identity)
	}

	if file.Notify.Discord.Events != nil {
		cfg.Notify.Discord.Events = file.Notify.Discord.Events
	}

	if file.Notify.Mattermost.Events != nil {
		cfg.Notify.Mattermost.Events = file.Notify.Mattermost.Events
	}

	if file.Chat.Personalities != nil {
		cfg.Chat.Personalities = file.Chat.Personalities
	}

	if file.Chat.FileAllowlist != nil {
		cfg.Chat.FileAllowlist = file.Chat.FileAllowlist
	}

	if file.Hive.Categories != nil {
		cfg.Hive.Categories = file.Hive.Categories
	}

	if file.API.CORSOrigins != nil {
		cfg.API.CORSOrigins = file.API.CORSOrigins
	}

	if file.OAuth.AllowedEmailDomains != nil {
		cfg.OAuth.AllowedEmailDomains = file.OAuth.AllowedEmailDomains
	}

	if file.OAuth.AllowedSubjects != nil {
		cfg.OAuth.AllowedSubjects = file.OAuth.AllowedSubjects
	}

	if file.Env.Allowlist != nil {
		cfg.Env.Allowlist = file.Env.Allowlist
	}

	if file.Engine.Retry != nil {
		setIf(&cfg.Engine.Retry.MaxAttempts, file.Engine.Retry.MaxAttempts)
		setIf(&cfg.Engine.Retry.Backoff, file.Engine.Retry.Backoff)
		setIf(&cfg.Engine.Retry.Jitter, file.Engine.Retry.Jitter)
	}

	if file.Engine.Fallback != nil {
		cfg.Engine.Fallback = file.Engine.Fallback
	}
}

// durTarget pairs one optional file value with its destination so duration
// parsing reports the failing key by its TOML name.
type durTarget struct {
	raw  *string
	dst  *time.Duration
	name string
}

func applyDurations(cfg *Config, file *fileConfig) error {
	targets := []durTarget{
		{name: "engine.default_timeout", raw: file.Engine.DefaultTimeout, dst: &cfg.Engine.DefaultTimeout},
		{name: "retention.runs", raw: file.Retention.Runs, dst: &cfg.Retention.Runs},
		{name: "retention.outputs", raw: file.Retention.Outputs, dst: &cfg.Retention.Outputs},
		{name: "retention.sweep", raw: file.Retention.Sweep, dst: &cfg.Retention.Sweep},
		{name: "auth.access_token_ttl", raw: file.Auth.AccessTokenTTL, dst: &cfg.Auth.AccessTokenTTL},
		{name: "auth.stream_recheck", raw: file.Auth.StreamRecheck, dst: &cfg.Auth.StreamRecheck},
		{name: "auth.clock_skew", raw: file.Auth.ClockSkew, dst: &cfg.Auth.ClockSkew},
		{name: "scheduler.ack_wait", raw: file.Scheduler.AckWait, dst: &cfg.Scheduler.AckWait},
		{name: "scheduler.backoff", raw: file.Scheduler.Backoff, dst: &cfg.Scheduler.Backoff},
		{name: "scheduler.max_age", raw: file.Scheduler.MaxAge, dst: &cfg.Scheduler.MaxAge},
		{name: "scheduler.sweep_interval", raw: file.Scheduler.SweepInterval, dst: &cfg.Scheduler.SweepInterval},
		{name: "webhook.test_wait", raw: file.Webhook.TestWait, dst: &cfg.Webhook.TestWait},
		{name: "webhook.signature_skew", raw: file.Webhook.SignatureSkew, dst: &cfg.Webhook.SignatureSkew},
		{name: "bridge.telegram.poll_timeout", raw: file.Bridge.Telegram.PollTimeout, dst: &cfg.Bridge.Telegram.PollTimeout},
		{name: "bridge.telegram.send_timeout", raw: file.Bridge.Telegram.SendTimeout, dst: &cfg.Bridge.Telegram.SendTimeout},
		{name: "bridge.telegram.confirm_ttl", raw: file.Bridge.Telegram.ConfirmTTL, dst: &cfg.Bridge.Telegram.ConfirmTTL},
		{name: "bridge.mattermost.send_timeout", raw: file.Bridge.Mattermost.SendTimeout, dst: &cfg.Bridge.Mattermost.SendTimeout},
		{name: "bridge.mattermost.confirm_ttl", raw: file.Bridge.Mattermost.ConfirmTTL, dst: &cfg.Bridge.Mattermost.ConfirmTTL},
		{name: "bridge.mattermost.bot_loop.window", raw: file.Bridge.Mattermost.BotLoop.Window, dst: &cfg.Bridge.Mattermost.BotLoop.Window},
		{name: "bridge.mattermost.bot_loop.cooldown", raw: file.Bridge.Mattermost.BotLoop.Cooldown, dst: &cfg.Bridge.Mattermost.BotLoop.Cooldown},
	}

	if file.Engine.Retry != nil {
		targets = append(
			targets,
			durTarget{name: "engine.retry.base_delay", raw: file.Engine.Retry.BaseDelay, dst: &cfg.Engine.Retry.BaseDelay},
			durTarget{name: "engine.retry.max_delay", raw: file.Engine.Retry.MaxDelay, dst: &cfg.Engine.Retry.MaxDelay},
		)
	}

	for _, target := range targets {
		if target.raw == nil {
			continue
		}

		parsed, err := workflow.ParseDuration(*target.raw)
		if err != nil {
			return fmt.Errorf("%w: %s: %w", ErrInvalidConfig, target.name, err)
		}

		*target.dst = parsed
	}

	return nil
}

func setIf[T any](dst, src *T) {
	if src != nil {
		*dst = *src
	}
}

// convertBridgeUsers turns the file layout's user tables into the resolved
// TelegramUser map keyed by Telegram user ID.
func convertBridgeUsers(users map[string]fileBridgeUser) map[string]TelegramUser {
	resolved := make(map[string]TelegramUser, len(users))
	for id, user := range users {
		// Identical field set, so a struct conversion carries the tags off.
		resolved[id] = TelegramUser(user)
	}

	return resolved
}
