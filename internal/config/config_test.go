package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/config"
)

const fileTOML = `
[engine]
max_concurrent_actions = 8
default_timeout = "5m"

[engine.retry]
max_attempts = 5
base_delay = "2s"

[store]
dsn = "file:/tmp/x.db"

[retention]
runs = "30d"

[env]
allowlist = ["HOME", "PATH"]

[api]
enabled = true

[runner.openai]
model = "gpt-4o"

[runner.openai.circuit_breaker]
failure_threshold = 0.5
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return path
}

func TestLoadMissingFileServesDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Engine.MaxConcurrentActions != 4 || cfg.Store.Backend != "sqlite" {
		t.Fatalf("defaults not served: %+v", cfg.Engine)
	}

	if cfg.Engine.MaxCallDepth != 3 {
		t.Fatalf("engine.max_call_depth default = %d, want 3", cfg.Engine.MaxCallDepth)
	}

	if cfg.Retention.Runs != 90*24*time.Hour {
		t.Fatalf("retention.runs default = %v", cfg.Retention.Runs)
	}

	if cfg.API.Enabled || cfg.Auth.Enabled || cfg.Hive.Enabled {
		t.Fatal("network surfaces must default off")
	}
}

func TestLoadOverlaysFileOnDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(writeConfig(t, fileTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Engine.MaxConcurrentActions != 8 || cfg.Engine.DefaultTimeout != 5*time.Minute {
		t.Fatalf("engine overlay missing: %+v", cfg.Engine)
	}

	if cfg.Engine.Retry.MaxAttempts != 5 || cfg.Engine.Retry.BaseDelay != 2*time.Second {
		t.Fatalf("retry overlay missing: %+v", cfg.Engine.Retry)
	}

	// Values the file leaves out keep their defaults.
	if cfg.Engine.Retry.MaxDelay != 30*time.Second || !cfg.Engine.Retry.Jitter {
		t.Fatalf("retry defaults lost: %+v", cfg.Engine.Retry)
	}

	if cfg.Retention.Runs != 30*24*time.Hour {
		t.Fatalf("day-suffix duration = %v, want 720h", cfg.Retention.Runs)
	}

	if len(cfg.Env.Allowlist) != 2 || !cfg.API.Enabled {
		t.Fatalf("allowlist/api overlay missing: %+v %+v", cfg.Env, cfg.API)
	}

	block, exists := cfg.Runners["openai"]
	if !exists || block["model"] != "gpt-4o" {
		t.Fatalf("runner block missing: %+v", cfg.Runners)
	}

	breaker, isTable := block["circuit_breaker"].(map[string]any)
	if !isTable || breaker["failure_threshold"] != 0.5 {
		t.Fatalf("nested breaker block missing: %+v", block)
	}
}

func TestLoadEnvOverridesBeatFile(t *testing.T) {
	t.Setenv("CAIRNOS_ENGINE_MAX_CONCURRENT_ACTIONS", "16")
	t.Setenv("CAIRNOS_ENGINE_MAX_CALL_DEPTH", "5")
	t.Setenv("CAIRNOS_STORE_DSN", "file:/tmp/env.db")
	t.Setenv("CAIRNOS_ENV_ALLOWLIST", "HOME, USER")
	t.Setenv("CAIRNOS_RETENTION_SWEEP", "1d")

	cfg, err := config.Load(writeConfig(t, fileTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Engine.MaxConcurrentActions != 16 {
		t.Fatalf("env did not beat file: %d", cfg.Engine.MaxConcurrentActions)
	}

	if cfg.Engine.MaxCallDepth != 5 {
		t.Fatalf("engine.max_call_depth env override = %d, want 5", cfg.Engine.MaxCallDepth)
	}

	if cfg.Store.DSN != "file:/tmp/env.db" {
		t.Fatalf("dsn = %q", cfg.Store.DSN)
	}

	if len(cfg.Env.Allowlist) != 2 || cfg.Env.Allowlist[1] != "USER" {
		t.Fatalf("allowlist = %v", cfg.Env.Allowlist)
	}

	if cfg.Retention.Sweep != 24*time.Hour {
		t.Fatalf("sweep = %v", cfg.Retention.Sweep)
	}
}

func TestLoadTuningEmbeddingBlockFromFile(t *testing.T) {
	t.Parallel()

	const tuningTOML = `
[tuning]
embedding_model = "text-embedding-3-small"
equivalence_threshold = 0.92
`

	cfg, err := config.Load(writeConfig(t, tuningTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Tuning.EmbeddingModel != "text-embedding-3-small" {
		t.Fatalf("embedding_model = %q, want it parsed from the file", cfg.Tuning.EmbeddingModel)
	}

	if cfg.Tuning.EquivalenceThreshold != 0.92 {
		t.Fatalf("equivalence_threshold = %v, want 0.92", cfg.Tuning.EquivalenceThreshold)
	}
}

func TestLoadEngineFallbackChainFromFile(t *testing.T) {
	t.Parallel()

	const fallbackTOML = `
[engine.fallback]
codex = ["qwen", "omp"]
qwen = ["omp"]
`

	cfg, err := config.Load(writeConfig(t, fallbackTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got := cfg.Engine.Fallback["codex"]; !slices.Equal(got, []string{"qwen", "omp"}) {
		t.Fatalf("codex fallback = %v, want [qwen omp]", got)
	}

	if got := cfg.Engine.Fallback["qwen"]; !slices.Equal(got, []string{"omp"}) {
		t.Errorf("qwen fallback = %v, want [omp]", got)
	}
}

func TestLoadEngineFallbackDefaultsEmpty(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(cfg.Engine.Fallback) != 0 {
		t.Errorf("default fallback = %v, want empty", cfg.Engine.Fallback)
	}
}

func TestLoadTuningEmbeddingDefaultsOff(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Empty model is the opt-in default: replay keeps exact-match comparison.
	if cfg.Tuning.EmbeddingModel != "" {
		t.Fatalf("embedding_model = %q, want empty by default", cfg.Tuning.EmbeddingModel)
	}
}

func TestLoadTuningEmbeddingEnvOverrides(t *testing.T) {
	t.Setenv("CAIRNOS_TUNING_EMBEDDING_MODEL", "text-embedding-3-large")
	t.Setenv("CAIRNOS_TUNING_EQUIVALENCE_THRESHOLD", "0.97")

	const tuningTOML = `
[tuning]
embedding_model = "text-embedding-3-small"
equivalence_threshold = 0.90
`

	cfg, err := config.Load(writeConfig(t, tuningTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Tuning.EmbeddingModel != "text-embedding-3-large" {
		t.Fatalf("env did not beat file: %q", cfg.Tuning.EmbeddingModel)
	}

	if cfg.Tuning.EquivalenceThreshold != 0.97 {
		t.Fatalf("threshold env did not beat file: %v", cfg.Tuning.EquivalenceThreshold)
	}
}

func TestLoadAuthDefaultsAreSecure(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Auth.Enabled {
		t.Fatal("auth must default off for local-first setups")
	}

	if !cfg.Auth.RequireExp || !cfg.Auth.RequireJTI {
		t.Fatalf("require_exp/require_jti must default on: %+v", cfg.Auth)
	}

	if cfg.Auth.AccessTokenTTL != time.Hour || cfg.Auth.SigningAlgorithm != "HS256" {
		t.Fatalf("token defaults wrong: %+v", cfg.Auth)
	}

	if cfg.Auth.ServeWithoutAuthOnNetwork || cfg.Auth.TLSTerminated {
		t.Fatalf("network flags must default off: %+v", cfg.Auth)
	}

	if cfg.Auth.MaxConcurrentLogins != 4 || cfg.Auth.LoginRatePerMinute != 60 {
		t.Fatalf("login DoS defaults wrong: %+v", cfg.Auth)
	}

	if cfg.Auth.StreamRecheck != 15*time.Second || cfg.Auth.ClockSkew != 30*time.Second {
		t.Fatalf("recheck/skew defaults wrong: %+v", cfg.Auth)
	}
}

func TestLoadAuthM1M4L2BlockFromFile(t *testing.T) {
	t.Parallel()

	authTOML := `
[auth]
enabled = true
max_concurrent_logins = 2
login_rate_per_minute = 10
stream_recheck = "5s"
clock_skew = "45s"
`

	cfg, err := config.Load(writeConfig(t, authTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Auth.MaxConcurrentLogins != 2 || cfg.Auth.LoginRatePerMinute != 10 {
		t.Fatalf("login DoS overlay missing: %+v", cfg.Auth)
	}

	if cfg.Auth.StreamRecheck != 5*time.Second || cfg.Auth.ClockSkew != 45*time.Second {
		t.Fatalf("recheck/skew overlay missing: %+v", cfg.Auth)
	}
}

func TestLoadAuthM1M4L2EnvOverrides(t *testing.T) {
	t.Setenv("CAIRNOS_AUTH_ENABLED", "true")
	t.Setenv("CAIRNOS_AUTH_MAX_CONCURRENT_LOGINS", "8")
	t.Setenv("CAIRNOS_AUTH_LOGIN_RATE_PER_MINUTE", "120")
	t.Setenv("CAIRNOS_AUTH_STREAM_RECHECK", "20s")
	t.Setenv("CAIRNOS_AUTH_CLOCK_SKEW", "10s")

	cfg, err := config.Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Auth.MaxConcurrentLogins != 8 || cfg.Auth.LoginRatePerMinute != 120 {
		t.Fatalf("login DoS env overlay missing: %+v", cfg.Auth)
	}

	if cfg.Auth.StreamRecheck != 20*time.Second || cfg.Auth.ClockSkew != 10*time.Second {
		t.Fatalf("recheck/skew env overlay missing: %+v", cfg.Auth)
	}
}

// TestLoadRejectsInvalidAuthWhenEnabled is the L1 fail-fast: auth.enabled with
// a missing issuer or audience, a non-positive TTL, or an unknown signing
// algorithm is config.invalid at load, before anything tries to mint a token.
func TestLoadRejectsInvalidAuthWhenEnabled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		toml string
	}{
		{name: "empty issuer", toml: "[auth]\nenabled = true\nissuer = \"\""},
		{name: "empty audience", toml: "[auth]\nenabled = true\naudience = \"\""},
		{name: "zero ttl", toml: "[auth]\nenabled = true\naccess_token_ttl = \"0s\""},
		{name: "unknown algorithm", toml: "[auth]\nenabled = true\nsigning_algorithm = \"RS256\""},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Load(writeConfig(t, tt.toml))
			if !errors.Is(err, config.ErrInvalidConfig) {
				t.Fatalf("error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

// TestLoadSkipsAuthValidationWhenDisabled keeps the secure-default off path
// usable: an auth-disabled config with an otherwise invalid auth block still
// loads, since the token path never runs.
func TestLoadSkipsAuthValidationWhenDisabled(t *testing.T) {
	t.Parallel()

	disabled := "[auth]\nenabled = false\nissuer = \"\"\nsigning_algorithm = \"nonsense\""

	if _, err := config.Load(writeConfig(t, disabled)); err != nil {
		t.Fatalf("auth-off config with a bad auth block should load: %v", err)
	}
}

func TestLoadAuthBlockFromFile(t *testing.T) {
	t.Parallel()

	authTOML := `
[auth]
enabled = true
issuer = "node-7"
audience = "fleet"
access_token_ttl = "30m"
signing_algorithm = "EdDSA"
require_jti = false
serve_without_auth_on_network = true
tls_terminated = true
`

	cfg, err := config.Load(writeConfig(t, authTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !cfg.Auth.Enabled || cfg.Auth.Issuer != "node-7" || cfg.Auth.Audience != "fleet" {
		t.Fatalf("auth identity overlay missing: %+v", cfg.Auth)
	}

	if cfg.Auth.AccessTokenTTL != 30*time.Minute || cfg.Auth.SigningAlgorithm != "EdDSA" {
		t.Fatalf("token overlay missing: %+v", cfg.Auth)
	}

	// require_exp was left out, so its secure default survives; require_jti
	// was deliberately overridden.
	if !cfg.Auth.RequireExp || cfg.Auth.RequireJTI {
		t.Fatalf("require flags wrong: %+v", cfg.Auth)
	}

	if !cfg.Auth.ServeWithoutAuthOnNetwork || !cfg.Auth.TLSTerminated {
		t.Fatalf("network flag overlay missing: %+v", cfg.Auth)
	}
}

func TestLoadA2ABlockFromFile(t *testing.T) {
	t.Parallel()

	a2aTOML := `
[a2a]
enabled = true
card_signing_key_file = "/keys/a2a.pem"
endpoint_advertise = "grpc://node.example:7777"
cert_file = "/tls/cert.pem"
key_file = "/tls/key.pem"
client_ca_file = "/tls/ca.pem"
`

	cfg, err := config.Load(writeConfig(t, a2aTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !cfg.A2A.Enabled {
		t.Fatalf("a2a not enabled: %+v", cfg.A2A)
	}

	if cfg.A2A.CardSigningKeyFile != "/keys/a2a.pem" || cfg.A2A.EndpointAdvertise != "grpc://node.example:7777" {
		t.Fatalf("a2a card overlay missing: %+v", cfg.A2A)
	}

	if cfg.A2A.CertFile != "/tls/cert.pem" || cfg.A2A.KeyFile != "/tls/key.pem" || cfg.A2A.ClientCAFile != "/tls/ca.pem" {
		t.Fatalf("a2a mTLS overlay missing: %+v", cfg.A2A)
	}
}

func TestLoadA2ADefaultsOff(t *testing.T) {
	t.Parallel()

	cfg := config.Default()

	if cfg.A2A.Enabled {
		t.Fatal("a2a should default off")
	}
}

func TestLoadHiveKeyRefFromFile(t *testing.T) {
	t.Parallel()

	hiveTOML := `
[hive]
enabled = true
endpoint = "https://hive.example"
key_ref = "hive_key"
prev_key_ref = "hive_prev_key"
categories = ["job-search"]
`

	cfg, err := config.Load(writeConfig(t, hiveTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Hive.KeyRef != "hive_key" {
		t.Fatalf("key_ref from file = %q, want hive_key", cfg.Hive.KeyRef)
	}

	if cfg.Hive.PrevKeyRef != "hive_prev_key" {
		t.Fatalf("prev_key_ref from file = %q, want hive_prev_key", cfg.Hive.PrevKeyRef)
	}
}

func TestLoadHiveKeyRefEnvOverride(t *testing.T) {
	t.Setenv("CAIRNOS_HIVE_KEY_REF", "rotated_hive_key")

	cfg, err := config.Load(writeConfig(t, "[hive]\nenabled = true\nkey_ref = \"hive_key\""))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Hive.KeyRef != "rotated_hive_key" {
		t.Fatalf("key_ref env override = %q, want rotated_hive_key", cfg.Hive.KeyRef)
	}
}

func TestLoadHiveKeyRefDefaultsEmpty(t *testing.T) {
	t.Parallel()

	if ref := config.Default().Hive.KeyRef; ref != "" {
		t.Fatalf("hive key_ref default = %q, want empty (plaintext, no-key mode)", ref)
	}
}

func TestHiveNeedsPostgres(t *testing.T) {
	t.Parallel()

	const sqliteBackend = "sqlite"

	cases := []struct {
		name    string
		backend string
		enabled bool
		want    bool
	}{
		{name: "enabled on sqlite is refused", enabled: true, backend: sqliteBackend, want: true},
		{name: "enabled on postgres runs", enabled: true, backend: "postgres", want: false},
		{name: "disabled never needs postgres", enabled: false, backend: sqliteBackend, want: false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			cfg := &config.Config{}
			cfg.Hive.Enabled = testCase.enabled
			cfg.Store.Backend = testCase.backend

			if got := cfg.HiveNeedsPostgres(); got != testCase.want {
				t.Errorf("HiveNeedsPostgres() = %t, want %t", got, testCase.want)
			}
		})
	}
}

func TestLoadMCPBlockFromFile(t *testing.T) {
	t.Parallel()

	mcpTOML := `
[mcp]
enabled = true
transport = "stdio"
path = "/tools"
workflows_dir = "/srv/flows"
registry_url = "https://registry.example/v0"
advertise_url = "https://node.example/mcp"
`

	cfg, err := config.Load(writeConfig(t, mcpTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !cfg.MCP.Enabled {
		t.Fatalf("mcp not enabled: %+v", cfg.MCP)
	}

	if cfg.MCP.Transport != config.MCPTransportStdio {
		t.Errorf("mcp transport = %q, want %q", cfg.MCP.Transport, config.MCPTransportStdio)
	}

	if cfg.MCP.Path != "/tools" || cfg.MCP.WorkflowsDir != "/srv/flows" {
		t.Errorf("mcp overlay missing: %+v", cfg.MCP)
	}

	if cfg.MCP.RegistryURL != "https://registry.example/v0" || cfg.MCP.AdvertiseURL != "https://node.example/mcp" {
		t.Errorf("mcp registry overlay missing: %+v", cfg.MCP)
	}
}

func TestLoadMCPRegistryDefaultsEmpty(t *testing.T) {
	t.Parallel()

	cfg := config.Default()

	if cfg.MCP.RegistryURL != "" || cfg.MCP.AdvertiseURL != "" {
		t.Errorf("registry settings should default empty: %+v", cfg.MCP)
	}
}

func TestLoadSchedulerDefaults(t *testing.T) {
	t.Parallel()

	cfg := config.Default()

	if cfg.Scheduler.AckWait != 5*time.Minute || cfg.Scheduler.Backoff != 30*time.Second {
		t.Errorf("scheduler duration defaults wrong: %+v", cfg.Scheduler)
	}

	if cfg.Scheduler.MaxDeliveries != 5 || cfg.Scheduler.WorkerConcurrency != 4 || cfg.Scheduler.Prefetch != 8 {
		t.Errorf("scheduler count defaults wrong: %+v", cfg.Scheduler)
	}

	if cfg.Scheduler.MaxAge != 0 {
		t.Errorf("max_age should default to 0 (keep dead items): %v", cfg.Scheduler.MaxAge)
	}
}

func TestLoadSchedulerBlockFromFile(t *testing.T) {
	t.Parallel()

	schedTOML := `
[scheduler]
ack_wait = "2m"
backoff = "10s"
max_deliveries = 7
max_age = "24h"
worker_concurrency = 16
prefetch = 32
sweep_interval = "45s"
`

	cfg, err := config.Load(writeConfig(t, schedTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Scheduler.AckWait != 2*time.Minute || cfg.Scheduler.Backoff != 10*time.Second {
		t.Errorf("scheduler durations not applied: %+v", cfg.Scheduler)
	}

	if cfg.Scheduler.MaxDeliveries != 7 || cfg.Scheduler.WorkerConcurrency != 16 || cfg.Scheduler.Prefetch != 32 {
		t.Errorf("scheduler counts not applied: %+v", cfg.Scheduler)
	}

	if cfg.Scheduler.MaxAge != 24*time.Hour || cfg.Scheduler.SweepInterval != 45*time.Second {
		t.Errorf("scheduler max_age/sweep not applied: %+v", cfg.Scheduler)
	}
}

func TestLoadSchedulerEnvOverrides(t *testing.T) {
	t.Setenv("CAIRNOS_SCHEDULER_WORKER_CONCURRENCY", "12")
	t.Setenv("CAIRNOS_SCHEDULER_ACK_WAIT", "90s")

	cfg, err := config.Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Scheduler.WorkerConcurrency != 12 || cfg.Scheduler.AckWait != 90*time.Second {
		t.Fatalf("scheduler env overrides missing: %+v", cfg.Scheduler)
	}
}

func TestLoadMCPDefaults(t *testing.T) {
	t.Parallel()

	cfg := config.Default()

	if cfg.MCP.Enabled {
		t.Error("mcp should default off")
	}

	if cfg.MCP.Transport != config.MCPTransportHTTP {
		t.Errorf("mcp transport default = %q, want %q", cfg.MCP.Transport, config.MCPTransportHTTP)
	}

	if cfg.MCP.Path != "/mcp" {
		t.Errorf("mcp path default = %q, want /mcp", cfg.MCP.Path)
	}
}

func TestLoadMCPEnvOverrides(t *testing.T) {
	t.Setenv("CAIRNOS_MCP_ENABLED", "true")
	t.Setenv("CAIRNOS_MCP_TRANSPORT", "stdio")
	t.Setenv("CAIRNOS_MCP_PATH", "/env-mcp")
	t.Setenv("CAIRNOS_MCP_REGISTRY_URL", "https://env-registry/v0")
	t.Setenv("CAIRNOS_MCP_ADVERTISE_URL", "https://env-node/mcp")

	cfg, err := config.Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !cfg.MCP.Enabled || cfg.MCP.Transport != config.MCPTransportStdio || cfg.MCP.Path != "/env-mcp" {
		t.Fatalf("mcp env overrides missing: %+v", cfg.MCP)
	}

	if cfg.MCP.RegistryURL != "https://env-registry/v0" || cfg.MCP.AdvertiseURL != "https://env-node/mcp" {
		t.Fatalf("mcp registry env overrides missing: %+v", cfg.MCP)
	}
}

func TestLoadChatOpenAICompat(t *testing.T) {
	t.Parallel()

	t.Run("default_off", func(t *testing.T) {
		t.Parallel()

		if config.Default().Chat.OpenAICompat {
			t.Error("chat.openai_compat should default off")
		}
	})

	t.Run("toml_parse", func(t *testing.T) {
		t.Parallel()

		cfg, err := config.Load(writeConfig(t, "[chat]\nenabled = true\nopenai_compat = true"))
		if err != nil {
			t.Fatalf("load: %v", err)
		}

		if !cfg.Chat.OpenAICompat {
			t.Error("chat.openai_compat = false from TOML, want true")
		}
	})
}

func TestLoadChatOpenAICompatEnvOverride(t *testing.T) {
	t.Setenv("CAIRNOS_CHAT_OPENAI_COMPAT", "true")

	cfg, err := config.Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !cfg.Chat.OpenAICompat {
		t.Error("CAIRNOS_CHAT_OPENAI_COMPAT override did not set chat.openai_compat")
	}
}

func TestLoadWebhookDefaults(t *testing.T) {
	t.Parallel()

	cfg := config.Default()

	if cfg.Webhook.Enabled {
		t.Error("webhook should default off so no public ingress route mounts")
	}

	if cfg.Webhook.RatePerMinute != 60 {
		t.Errorf("webhook rate_per_minute default = %d, want 60", cfg.Webhook.RatePerMinute)
	}

	if cfg.Webhook.TestWait != 30*time.Second {
		t.Errorf("webhook test_wait default = %v, want 30s", cfg.Webhook.TestWait)
	}

	if cfg.Webhook.SignatureSkew != 5*time.Minute {
		t.Errorf("webhook signature_skew default = %v, want 5m", cfg.Webhook.SignatureSkew)
	}
}

func TestLoadWebhookBlockFromFile(t *testing.T) {
	t.Parallel()

	webhookTOML := `
[webhook]
enabled         = true
rate_per_minute = 120
test_wait       = "10s"
signature_skew  = "2m"
`

	cfg, err := config.Load(writeConfig(t, webhookTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !cfg.Webhook.Enabled {
		t.Error("webhook.enabled = false from TOML, want true")
	}

	if cfg.Webhook.RatePerMinute != 120 {
		t.Errorf("webhook rate_per_minute = %d, want 120", cfg.Webhook.RatePerMinute)
	}

	if cfg.Webhook.TestWait != 10*time.Second {
		t.Errorf("webhook test_wait = %v, want 10s", cfg.Webhook.TestWait)
	}

	if cfg.Webhook.SignatureSkew != 2*time.Minute {
		t.Errorf("webhook signature_skew = %v, want 2m", cfg.Webhook.SignatureSkew)
	}
}

func TestLoadWebhookEnvOverrides(t *testing.T) {
	t.Setenv("CAIRNOS_WEBHOOK_ENABLED", "true")
	t.Setenv("CAIRNOS_WEBHOOK_RATE_PER_MINUTE", "240")
	t.Setenv("CAIRNOS_WEBHOOK_TEST_WAIT", "45s")
	t.Setenv("CAIRNOS_WEBHOOK_SIGNATURE_SKEW", "90s")

	// The env override must beat a file that sets the same keys, so the file
	// carries different values than the environment.
	fileWithWebhook := "[webhook]\nenabled = false\nrate_per_minute = 60\ntest_wait = \"30s\"\nsignature_skew = \"5m\""

	cfg, err := config.Load(writeConfig(t, fileWithWebhook))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !cfg.Webhook.Enabled {
		t.Error("CAIRNOS_WEBHOOK_ENABLED override did not win over the file")
	}

	if cfg.Webhook.RatePerMinute != 240 {
		t.Errorf("webhook rate_per_minute env override = %d, want 240", cfg.Webhook.RatePerMinute)
	}

	if cfg.Webhook.TestWait != 45*time.Second {
		t.Errorf("webhook test_wait env override = %v, want 45s", cfg.Webhook.TestWait)
	}

	if cfg.Webhook.SignatureSkew != 90*time.Second {
		t.Errorf("webhook signature_skew env override = %v, want 90s", cfg.Webhook.SignatureSkew)
	}
}

func TestLoadHiveAndAutonomicDefaultGated(t *testing.T) {
	t.Parallel()

	cfg := config.Default()

	if cfg.Hive.Enabled {
		t.Error("hive should default off")
	}

	if cfg.Hive.PublishEnabled {
		t.Error("hive publish should default off (gated on R2/R3)")
	}

	if cfg.Autonomic.AutoApplyEnabled {
		t.Error("auto-apply should default off")
	}

	if cfg.Hive.KAnonymityFloor <= 0 || cfg.Hive.MinObservations <= 0 {
		t.Errorf("hive floors should default positive: %+v", cfg.Hive)
	}
}

func TestLoadHiveBlockFromFile(t *testing.T) {
	t.Parallel()

	hiveTOML := `
[hive]
enabled = true
endpoint = "https://hive.example/api"
node_pseudonym = "node-fixed"
publish_enabled = true
categories = ["job-search", "code-gen"]
k_anonymity_floor = 7
min_observations = 9

[autonomic]
auto_apply_enabled = true
`

	cfg, err := config.Load(writeConfig(t, hiveTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !cfg.Hive.Enabled || !cfg.Hive.PublishEnabled {
		t.Fatalf("hive flags not overlaid: %+v", cfg.Hive)
	}

	if cfg.Hive.Endpoint != "https://hive.example/api" || cfg.Hive.NodePseudonym != "node-fixed" {
		t.Fatalf("hive endpoint/pseudonym not overlaid: %+v", cfg.Hive)
	}

	if len(cfg.Hive.Categories) != 2 || cfg.Hive.Categories[0] != "job-search" {
		t.Fatalf("hive categories not overlaid: %+v", cfg.Hive.Categories)
	}

	if cfg.Hive.KAnonymityFloor != 7 || cfg.Hive.MinObservations != 9 {
		t.Fatalf("hive floors not overlaid: %+v", cfg.Hive)
	}

	if !cfg.Autonomic.AutoApplyEnabled {
		t.Fatal("autonomic auto_apply_enabled not overlaid")
	}
}

func TestLoadHiveEnvOverrides(t *testing.T) {
	t.Setenv("CAIRNOS_HIVE_ENABLED", "true")
	t.Setenv("CAIRNOS_HIVE_PUBLISH_ENABLED", "true")
	t.Setenv("CAIRNOS_HIVE_CATEGORIES", "research, automation")
	t.Setenv("CAIRNOS_AUTONOMIC_AUTO_APPLY_ENABLED", "true")

	cfg, err := config.Load(writeConfig(t, ""))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !cfg.Hive.Enabled || !cfg.Hive.PublishEnabled || !cfg.Autonomic.AutoApplyEnabled {
		t.Fatalf("env flags not applied: hive=%+v autonomic=%+v", cfg.Hive, cfg.Autonomic)
	}

	if len(cfg.Hive.Categories) != 2 || cfg.Hive.Categories[1] != "automation" {
		t.Fatalf("env categories not applied: %+v", cfg.Hive.Categories)
	}
}

func TestLoadAuthEnvOverrides(t *testing.T) {
	t.Setenv("CAIRNOS_AUTH_ENABLED", "true")
	t.Setenv("CAIRNOS_AUTH_ISSUER", "env-node")
	t.Setenv("CAIRNOS_AUTH_ACCESS_TOKEN_TTL", "2h")
	t.Setenv("CAIRNOS_AUTH_TLS_TERMINATED", "true")

	cfg, err := config.Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !cfg.Auth.Enabled || cfg.Auth.Issuer != "env-node" {
		t.Fatalf("env overlay missing: %+v", cfg.Auth)
	}

	if cfg.Auth.AccessTokenTTL != 2*time.Hour || !cfg.Auth.TLSTerminated {
		t.Fatalf("env overlay missing: %+v", cfg.Auth)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		toml string
	}{
		{name: "bad duration", toml: "[engine]\ndefault_timeout = \"soon\""},
		{name: "not toml", toml: "{json: true}"},
		{name: "wrong type", toml: "[engine]\nmax_concurrent_actions = \"many\""},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Load(writeConfig(t, tt.toml))
			if !errors.Is(err, config.ErrInvalidConfig) {
				t.Fatalf("error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestLoadRejectsBadEnvValue(t *testing.T) {
	t.Setenv("CAIRNOS_API_ENABLED", "definitely")

	_, err := config.Load(filepath.Join(t.TempDir(), "absent.toml"))
	if !errors.Is(err, config.ErrInvalidConfig) {
		t.Fatalf("error = %v, want ErrInvalidConfig", err)
	}
}

func TestLoadChatFileRefsDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(cfg.Chat.FileAllowlist) != 0 {
		t.Fatalf("file_allowlist default = %v, want empty", cfg.Chat.FileAllowlist)
	}

	if cfg.Chat.MaxFileBytes != 65536 {
		t.Fatalf("max_file_bytes default = %d, want 65536", cfg.Chat.MaxFileBytes)
	}
}

func TestLoadChatFileRefsFromFile(t *testing.T) {
	t.Parallel()

	chatTOML := `
[chat]
enabled = true
file_allowlist = ["/data/docs", "/data/runbooks"]
max_file_bytes = 4096
`

	cfg, err := config.Load(writeConfig(t, chatTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !slices.Equal(cfg.Chat.FileAllowlist, []string{"/data/docs", "/data/runbooks"}) {
		t.Fatalf("file_allowlist from file = %v", cfg.Chat.FileAllowlist)
	}

	if cfg.Chat.MaxFileBytes != 4096 {
		t.Fatalf("max_file_bytes from file = %d, want 4096", cfg.Chat.MaxFileBytes)
	}
}

func TestLoadChatFileRefsEnvOverrides(t *testing.T) {
	t.Setenv("CAIRNOS_CHAT_FILE_ALLOWLIST", "/srv/one, /srv/two")
	t.Setenv("CAIRNOS_CHAT_MAX_FILE_BYTES", "128")

	chatTOML := `
[chat]
enabled = true
file_allowlist = ["/data/docs"]
max_file_bytes = 4096
`

	cfg, err := config.Load(writeConfig(t, chatTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !slices.Equal(cfg.Chat.FileAllowlist, []string{"/srv/one", "/srv/two"}) {
		t.Fatalf("file_allowlist env override = %v", cfg.Chat.FileAllowlist)
	}

	if cfg.Chat.MaxFileBytes != 128 {
		t.Fatalf("max_file_bytes env override = %d, want 128", cfg.Chat.MaxFileBytes)
	}
}

func TestLoadSecretsBackendDefaultsToEnv(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Secrets.Backend != config.SecretsBackendEnv {
		t.Fatalf("secrets.backend default = %q, want %q", cfg.Secrets.Backend, config.SecretsBackendEnv)
	}
}

func TestLoadSecretsBackendFromFile(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(writeConfig(t, "[secrets]\nbackend = \"store\"\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Secrets.Backend != config.SecretsBackendStore {
		t.Fatalf("secrets.backend from file = %q, want %q", cfg.Secrets.Backend, config.SecretsBackendStore)
	}
}

func TestLoadSecretsBackendEnvOverridesFile(t *testing.T) {
	t.Setenv("CAIRNOS_SECRETS_BACKEND", "store")

	// The file asks for env; the environment wins, so the store backend takes.
	cfg, err := config.Load(writeConfig(t, "[secrets]\nbackend = \"env\"\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Secrets.Backend != config.SecretsBackendStore {
		t.Fatalf("secrets.backend env override = %q, want %q", cfg.Secrets.Backend, config.SecretsBackendStore)
	}
}

func TestLoadSecretsBackendRejectsUnknown(t *testing.T) {
	t.Parallel()

	_, err := config.Load(writeConfig(t, "[secrets]\nbackend = \"vault\"\n"))
	if !errors.Is(err, config.ErrInvalidConfig) {
		t.Fatalf("unknown secrets.backend error = %v, want ErrInvalidConfig", err)
	}
}
