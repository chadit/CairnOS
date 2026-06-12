// Package config loads CairnOS settings: defaults, then the TOML file, then
// environment overrides, strongest last
// (.agents/spec/11-deployment-and-config.md, "Configuration").
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/chadit/CairnOS/internal/workflow"
)

// Config is the fully resolved settings tree.
type Config struct {
	Engine    Engine
	Store     Store
	Memory    Memory
	Retention Retention
	Env       Env
	Telemetry Telemetry
	API       API
	Auth      Auth
	OAuth     OAuth
	A2A       A2A
	MCP       MCP
	Hive      Hive
	Autonomic Autonomic
	Tuning    Tuning
	Chat      Chat
	// Runners carries each [runner.<name>] block verbatim for the registry's
	// factories, including nested tables like circuit_breaker.
	Runners map[string]map[string]any
}

// Engine bounds execution.
type Engine struct {
	MaxConcurrentActions int
	DefaultTimeout       time.Duration
	Retry                Retry
}

// Retry is the engine-wide default policy; actions override it per file.
type Retry struct {
	MaxAttempts int
	Backoff     string
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Jitter      bool
}

// Store selects the state backend.
type Store struct {
	Backend string
	DSN     string
}

// Memory selects the memory backend.
type Memory struct {
	Backend         string
	Vector          bool
	EmbeddingRunner string
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
	Enabled      bool
	Prometheus   string
	OTLPEndpoint string
}

// API configures the gRPC and HTTP listeners (off until Phase 1a serves).
type API struct {
	Enabled bool
	GRPC    string
	HTTP    string

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
}

// Auth gates external calls (.agents/spec/08-auth-and-access.md,
// "Configuration"). Signing secrets are never config-file keys: the HS256
// secret and the EdDSA key file paths come from CAIRNOS_AUTH_HS256_SECRET
// and CAIRNOS_AUTH_PRIVATE_KEY_FILE / CAIRNOS_AUTH_PUBLIC_KEY_FILE.
type Auth struct {
	Enabled bool

	// Issuer and Audience fill and check the iss/aud token claims.
	Issuer   string
	Audience string

	// AccessTokenTTL bounds each token; refresh is re-login with the
	// access key (spec Change 2), so the default stays short at 1h.
	AccessTokenTTL time.Duration

	// SigningAlgorithm picks the token tier: HS256 (self-issued, shared
	// secret) or EdDSA (asymmetric, cross-org-ready; spec Change 3).
	SigningAlgorithm string

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

	// MaxConcurrentLogins bounds simultaneous credential derivations, since
	// each unauthenticated argon2id login costs ~64 MiB; excess logins get
	// RESOURCE_EXHAUSTED rather than pinning memory (spec M1).
	MaxConcurrentLogins int

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
}

// OAuth configures the Phase 5b OIDC human-login path
// (.agents/spec/12-ui-and-governance.md, "Multi-tenancy"): browser users sign
// in at an identity provider, and CairnOS mints its own JWT carrying the
// workspace and the role's scopes. It sits alongside the access-key model,
// not in place of it. Off by default; the client secret never lives in the
// config file, only in CAIRNOS_OAUTH_CLIENT_SECRET.
type OAuth struct {
	Enabled bool

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

	// AutoProvision gates first-login auto-provisioning. Default false: an
	// IdP-authenticated subject with no pre-existing membership is refused
	// rather than silently added to DefaultWorkspace, closing the open-door
	// provisioning hole (spec H1). With it true, AllowedEmailDomains and
	// AllowedSubjects still gate which subjects may self-provision when either
	// list is set.
	AutoProvision bool

	// AllowedEmailDomains is the auto-provision domain allowlist, matched
	// against the verified email claim's domain (the part after @). Empty means
	// domain is not checked, so AllowedSubjects (or an empty pair, which only
	// matters when AutoProvision is true) decides.
	AllowedEmailDomains []string

	// AllowedSubjects is the auto-provision subject allowlist, matched against
	// the verified ID token subject. Empty means subject is not checked.
	AllowedSubjects []string
}

// A2A configures the Phase 4b Agent-to-Agent protocol adapter
// (.agents/spec/09-a2a-and-hive.md). Off by default; when enabled, the serve
// process registers the A2A service, serves the signed agent card at the
// well-known path, and optionally fronts the gRPC listener with mTLS.
type A2A struct {
	Enabled bool

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
}

// MCP configures the Phase 7a MCP server
// (.agents/spec/10-mcp-integration.md, "MCP Server"). Off by default; when
// enabled with the http transport, the serve process mounts the streamable MCP
// handler on the API mux at Path, and an external MCP client presents the same
// bearer token CairnOS enforces the same scopes for. The stdio transport is the
// `cairnos mcp serve` subcommand instead, where auth is the process boundary.
type MCP struct {
	// Enabled turns the server on. With it off, the serve process mounts no MCP
	// handler and the mcp serve subcommand still works on demand.
	Enabled bool

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
	// Enabled turns the hive client on. With it off, no client is built and
	// neither subscribe nor publish runs.
	Enabled bool

	// Endpoint is the hive API base URL.
	Endpoint string

	// NodePseudonym is the rotating id the node presents to the hive. Empty
	// means the wiring mints a fresh random pseudonym at startup; it is never
	// the hostname or a stable instance name.
	NodePseudonym string

	// PublishEnabled gates publishing. Default false: the node receives tips
	// but sends none until the operator opts in, after the privacy review.
	PublishEnabled bool

	// Categories are the opted-in categories. Sharing is per-category, off
	// until a category is listed here.
	Categories []string

	// KAnonymityFloor is the server-side k the node knows about; informational
	// on the client, which still refuses below MinObservations locally.
	KAnonymityFloor int

	// MinObservations is the local floor: the node refuses to publish a pattern
	// it has seen fewer than this many times, ahead of the server's k.
	MinObservations int
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
	// MinSavingsUSD is the savings floor: suggestions whose estimated
	// per-run saving falls below it never appear.
	MinSavingsUSD float64

	// EmbeddingModel opts the replay guardrail into similarity comparison
	// (R1): when set, an output-changing suggestion's replay judges
	// equivalence by the cosine similarity of OpenAI embeddings instead of
	// exact byte match, so harmless rephrasing no longer reads as divergence.
	// Empty keeps the exact-match default, so similarity comparison is opt-in.
	EmbeddingModel string

	// EquivalenceThreshold is the cosine similarity at or above which the
	// embedding comparator calls two outputs equivalent. Zero falls back to a
	// conservative default; only consulted when EmbeddingModel is set.
	EquivalenceThreshold float64
}

// Chat configures the Phase 9 conversational interface
// (.agents/spec/13-roadmap.md, Phase 9). Off by default; when enabled the
// serve process mounts ChatService and the chat agent over the same engine the
// workflows run on.
type Chat struct {
	// Enabled turns the chat surface on. With it off, serve mounts no
	// ChatService and the cairnos chat command reports the node has chat
	// disabled.
	Enabled bool

	// Driver is the LLM runner name the chat agent uses to plan turns. It must
	// be a registered runner.ToolCaller (the openai and anthropic runners),
	// the same capability an agent action's driver needs.
	Driver string

	// DefaultPersonality names the personality a new session gets when the
	// request asks for none. Empty falls back to the assistant built-in.
	DefaultPersonality string

	// Personalities maps personality names to their system prompts, overlaid on
	// the built-ins. A name matching a built-in overrides it.
	Personalities map[string]string

	// TurnBudgetUSD caps one chat turn's estimated driver-and-tool spend, the
	// Phase 9 guardrail against a runaway conversation. Zero leaves turns
	// uncapped.
	TurnBudgetUSD float64
}

// Default returns the settings a fresh `cairnos init` ships: local-only,
// SQLite, auth and API off, so the result runs immediately.
func Default() Config {
	return Config{
		Engine: Engine{
			MaxConcurrentActions: 4,
			DefaultTimeout:       10 * time.Minute,
			Retry: Retry{
				MaxAttempts: 3,
				Backoff:     "exponential",
				BaseDelay:   time.Second,
				MaxDelay:    30 * time.Second,
				Jitter:      true,
			},
		},
		Store:  Store{Backend: "sqlite", DSN: "file:./data/cairnos.db"},
		Memory: Memory{Backend: "sqlite", EmbeddingRunner: "openai"},
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
		Chat: Chat{
			Driver:             defaultChatDriver,
			DefaultPersonality: defaultChatPersonality,
		},
		Runners: map[string]map[string]any{},
	}
}

// Chat defaults: the anthropic runner drives turns (it implements ToolCaller),
// and the assistant personality frames a session that names none
// (.agents/spec/13-roadmap.md, Phase 9). Chat stays off until [chat] enabled is
// set, so these only matter once an operator turns it on.
const (
	defaultChatDriver      = "anthropic"
	defaultChatPersonality = "assistant"
)

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

	if validErr := validateAuth(&cfg.Auth); validErr != nil {
		return cfg, validErr
	}

	if validErr := validateOAuth(&cfg.OAuth); validErr != nil {
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
// would break the login or callback at the first request, and an unknown
// default role would auto-provision a member into a role that grants no
// scopes. OAuth-off configs skip this. The client secret is checked here
// rather than at first login so a misconfigured deployment fails at startup.
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

// fileConfig mirrors the TOML layout with string durations, so duration
// parsing (including the day suffix) goes through workflow.ParseDuration.
type fileConfig struct {
	Engine struct {
		MaxConcurrentActions *int       `toml:"max_concurrent_actions"`
		DefaultTimeout       *string    `toml:"default_timeout"`
		Retry                *fileRetry `toml:"retry"`
	} `toml:"engine"`
	Store struct {
		Backend *string `toml:"backend"`
		DSN     *string `toml:"dsn"`
	} `toml:"store"`
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
	Env struct {
		Allowlist []string `toml:"allowlist"`
	} `toml:"env"`
	Telemetry struct {
		Enabled      *bool   `toml:"enabled"`
		Prometheus   *string `toml:"prometheus"`
		OTLPEndpoint *string `toml:"otlp_endpoint"`
	} `toml:"telemetry"`
	API struct {
		Enabled         *bool    `toml:"enabled"`
		GRPC            *string  `toml:"grpc"`
		HTTP            *string  `toml:"http"`
		CORSOrigins     []string `toml:"cors_origins"`
		MaxBodyBytes    *int64   `toml:"max_body_bytes"`
		MaxRecvMsgBytes *int     `toml:"max_recv_msg_bytes"`
	} `toml:"api"`
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
	} `toml:"mcp"`
	Hive struct {
		Enabled         *bool    `toml:"enabled"`
		Endpoint        *string  `toml:"endpoint"`
		NodePseudonym   *string  `toml:"node_pseudonym"`
		PublishEnabled  *bool    `toml:"publish_enabled"`
		Categories      []string `toml:"categories"`
		KAnonymityFloor *int     `toml:"k_anonymity_floor"`
		MinObservations *int     `toml:"min_observations"`
	} `toml:"hive"`
	Autonomic struct {
		AutoApplyEnabled *bool `toml:"auto_apply_enabled"`
	} `toml:"autonomic"`
	Tuning struct {
		MinSavingsUSD        *float64 `toml:"min_savings_usd"`
		EmbeddingModel       *string  `toml:"embedding_model"`
		EquivalenceThreshold *float64 `toml:"equivalence_threshold"`
	} `toml:"tuning"`
	Chat struct {
		Enabled            *bool             `toml:"enabled"`
		Driver             *string           `toml:"driver"`
		DefaultPersonality *string           `toml:"default_personality"`
		Personalities      map[string]string `toml:"personalities"`
		TurnBudgetUSD      *float64          `toml:"turn_budget_usd"`
	} `toml:"chat"`
	Runner map[string]map[string]any `toml:"runner"`
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

	if err := applyDurations(cfg, &file); err != nil {
		return err
	}

	if file.Runner != nil {
		cfg.Runners = file.Runner
	}

	return nil
}

// applyScalars copies every present (non-nil) non-duration value. Pointer
// fields distinguish "absent, keep the default" from "present, even if zero".
func applyScalars(cfg *Config, file *fileConfig) {
	setIf(&cfg.Engine.MaxConcurrentActions, file.Engine.MaxConcurrentActions)
	setIf(&cfg.Store.Backend, file.Store.Backend)
	setIf(&cfg.Store.DSN, file.Store.DSN)
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
	setIf(&cfg.Hive.Enabled, file.Hive.Enabled)
	setIf(&cfg.Hive.Endpoint, file.Hive.Endpoint)
	setIf(&cfg.Hive.NodePseudonym, file.Hive.NodePseudonym)
	setIf(&cfg.Hive.PublishEnabled, file.Hive.PublishEnabled)
	setIf(&cfg.Hive.KAnonymityFloor, file.Hive.KAnonymityFloor)
	setIf(&cfg.Hive.MinObservations, file.Hive.MinObservations)
	setIf(&cfg.Autonomic.AutoApplyEnabled, file.Autonomic.AutoApplyEnabled)
	setIf(&cfg.Tuning.MinSavingsUSD, file.Tuning.MinSavingsUSD)
	setIf(&cfg.Tuning.EmbeddingModel, file.Tuning.EmbeddingModel)
	setIf(&cfg.Tuning.EquivalenceThreshold, file.Tuning.EquivalenceThreshold)
	setIf(&cfg.Chat.Enabled, file.Chat.Enabled)
	setIf(&cfg.Chat.Driver, file.Chat.Driver)
	setIf(&cfg.Chat.DefaultPersonality, file.Chat.DefaultPersonality)
	setIf(&cfg.Chat.TurnBudgetUSD, file.Chat.TurnBudgetUSD)

	if file.Chat.Personalities != nil {
		cfg.Chat.Personalities = file.Chat.Personalities
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
}

// durTarget pairs one optional file value with its destination so duration
// parsing reports the failing key by its TOML name.
type durTarget struct {
	name string
	raw  *string
	dst  *time.Duration
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
