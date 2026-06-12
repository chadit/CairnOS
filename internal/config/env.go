package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/workflow"
)

// envPrefix namespaces every override: engine.max_concurrent_actions becomes
// CAIRNOS_ENGINE_MAX_CONCURRENT_ACTIONS
// (.agents/spec/11-deployment-and-config.md, "Environment override convention").
const envPrefix = "CAIRNOS_"

// applyEnv overlays environment overrides onto cfg. The key set is an
// explicit table rather than reflection: every supported variable is
// greppable, and a typo'd variable simply has no effect instead of guessing.
func applyEnv(cfg *Config, getenv func(string) string) error {
	for _, binding := range envBindings(cfg) {
		raw := getenv(envPrefix + binding.suffix)
		if raw == "" {
			continue
		}

		if err := binding.set(raw); err != nil {
			return fmt.Errorf("%w: %s%s: %w", ErrInvalidConfig, envPrefix, binding.suffix, err)
		}
	}

	return nil
}

type envBinding struct {
	suffix string
	set    func(raw string) error
}

func envBindings(cfg *Config) []envBinding {
	bindings := []envBinding{
		{suffix: "ENGINE_MAX_CONCURRENT_ACTIONS", set: intoInt(&cfg.Engine.MaxConcurrentActions)},
		{suffix: "ENGINE_DEFAULT_TIMEOUT", set: intoDuration(&cfg.Engine.DefaultTimeout)},
		{suffix: "ENGINE_RETRY_MAX_ATTEMPTS", set: intoInt(&cfg.Engine.Retry.MaxAttempts)},
		{suffix: "ENGINE_RETRY_BACKOFF", set: intoString(&cfg.Engine.Retry.Backoff)},
		{suffix: "ENGINE_RETRY_BASE_DELAY", set: intoDuration(&cfg.Engine.Retry.BaseDelay)},
		{suffix: "ENGINE_RETRY_MAX_DELAY", set: intoDuration(&cfg.Engine.Retry.MaxDelay)},
		{suffix: "ENGINE_RETRY_JITTER", set: intoBool(&cfg.Engine.Retry.Jitter)},
		{suffix: "STORE_BACKEND", set: intoString(&cfg.Store.Backend)},
		{suffix: "STORE_DSN", set: intoString(&cfg.Store.DSN)},
		{suffix: "MEMORY_BACKEND", set: intoString(&cfg.Memory.Backend)},
		{suffix: "MEMORY_VECTOR", set: intoBool(&cfg.Memory.Vector)},
		{suffix: "MEMORY_EMBEDDING_RUNNER", set: intoString(&cfg.Memory.EmbeddingRunner)},
		{suffix: "RETENTION_RUNS", set: intoDuration(&cfg.Retention.Runs)},
		{suffix: "RETENTION_OUTPUTS", set: intoDuration(&cfg.Retention.Outputs)},
		{suffix: "RETENTION_SWEEP", set: intoDuration(&cfg.Retention.Sweep)},
		{suffix: "ENV_ALLOWLIST", set: intoList(&cfg.Env.Allowlist)},
		{suffix: "TELEMETRY_ENABLED", set: intoBool(&cfg.Telemetry.Enabled)},
		{suffix: "TELEMETRY_PROMETHEUS", set: intoString(&cfg.Telemetry.Prometheus)},
		{suffix: "TELEMETRY_OTLP_ENDPOINT", set: intoString(&cfg.Telemetry.OTLPEndpoint)},
		{suffix: "API_ENABLED", set: intoBool(&cfg.API.Enabled)},
		{suffix: "API_GRPC", set: intoString(&cfg.API.GRPC)},
		{suffix: "API_HTTP", set: intoString(&cfg.API.HTTP)},
		{suffix: "API_CORS_ORIGINS", set: intoList(&cfg.API.CORSOrigins)},
		{suffix: "API_MAX_BODY_BYTES", set: intoInt64(&cfg.API.MaxBodyBytes)},
		{suffix: "API_MAX_RECV_MSG_BYTES", set: intoInt(&cfg.API.MaxRecvMsgBytes)},
		{suffix: "AUTH_ENABLED", set: intoBool(&cfg.Auth.Enabled)},
		{suffix: "AUTH_ISSUER", set: intoString(&cfg.Auth.Issuer)},
		{suffix: "AUTH_AUDIENCE", set: intoString(&cfg.Auth.Audience)},
		{suffix: "AUTH_ACCESS_TOKEN_TTL", set: intoDuration(&cfg.Auth.AccessTokenTTL)},
		{suffix: "AUTH_SIGNING_ALGORITHM", set: intoString(&cfg.Auth.SigningAlgorithm)},
		{suffix: "AUTH_REQUIRE_EXP", set: intoBool(&cfg.Auth.RequireExp)},
		{suffix: "AUTH_REQUIRE_JTI", set: intoBool(&cfg.Auth.RequireJTI)},
		{suffix: "AUTH_SERVE_WITHOUT_AUTH_ON_NETWORK", set: intoBool(&cfg.Auth.ServeWithoutAuthOnNetwork)},
		{suffix: "AUTH_TLS_TERMINATED", set: intoBool(&cfg.Auth.TLSTerminated)},
		{suffix: "AUTH_MAX_CONCURRENT_LOGINS", set: intoInt(&cfg.Auth.MaxConcurrentLogins)},
		{suffix: "AUTH_LOGIN_RATE_PER_MINUTE", set: intoInt(&cfg.Auth.LoginRatePerMinute)},
		{suffix: "AUTH_STREAM_RECHECK", set: intoDuration(&cfg.Auth.StreamRecheck)},
		{suffix: "AUTH_CLOCK_SKEW", set: intoDuration(&cfg.Auth.ClockSkew)},
		{suffix: "OAUTH_ENABLED", set: intoBool(&cfg.OAuth.Enabled)},
		{suffix: "OAUTH_ISSUER", set: intoString(&cfg.OAuth.Issuer)},
		{suffix: "OAUTH_CLIENT_ID", set: intoString(&cfg.OAuth.ClientID)},
		// The client secret is a credential, so it comes only from the
		// environment, never the config file (.agents/spec/08-auth-and-access.md,
		// "Configuration").
		{suffix: "OAUTH_CLIENT_SECRET", set: intoString(&cfg.OAuth.ClientSecret)},
		{suffix: "OAUTH_REDIRECT_URL", set: intoString(&cfg.OAuth.RedirectURL)},
		{suffix: "OAUTH_DEFAULT_WORKSPACE", set: intoString(&cfg.OAuth.DefaultWorkspace)},
		{suffix: "OAUTH_DEFAULT_ROLE", set: intoString(&cfg.OAuth.DefaultRole)},
		{suffix: "OAUTH_AUTO_PROVISION", set: intoBool(&cfg.OAuth.AutoProvision)},
		{suffix: "OAUTH_ALLOWED_EMAIL_DOMAINS", set: intoList(&cfg.OAuth.AllowedEmailDomains)},
		{suffix: "OAUTH_ALLOWED_SUBJECTS", set: intoList(&cfg.OAuth.AllowedSubjects)},
		{suffix: "MCP_ENABLED", set: intoBool(&cfg.MCP.Enabled)},
		{suffix: "MCP_TRANSPORT", set: intoString(&cfg.MCP.Transport)},
		{suffix: "MCP_PATH", set: intoString(&cfg.MCP.Path)},
		{suffix: "MCP_WORKFLOWS_DIR", set: intoString(&cfg.MCP.WorkflowsDir)},
		{suffix: "HIVE_ENABLED", set: intoBool(&cfg.Hive.Enabled)},
		{suffix: "HIVE_ENDPOINT", set: intoString(&cfg.Hive.Endpoint)},
		{suffix: "HIVE_NODE_PSEUDONYM", set: intoString(&cfg.Hive.NodePseudonym)},
		{suffix: "HIVE_PUBLISH_ENABLED", set: intoBool(&cfg.Hive.PublishEnabled)},
		{suffix: "HIVE_CATEGORIES", set: intoList(&cfg.Hive.Categories)},
		{suffix: "HIVE_K_ANONYMITY_FLOOR", set: intoInt(&cfg.Hive.KAnonymityFloor)},
		{suffix: "HIVE_MIN_OBSERVATIONS", set: intoInt(&cfg.Hive.MinObservations)},
		{suffix: "AUTONOMIC_AUTO_APPLY_ENABLED", set: intoBool(&cfg.Autonomic.AutoApplyEnabled)},
		{suffix: "TUNING_MIN_SAVINGS_USD", set: intoFloat(&cfg.Tuning.MinSavingsUSD)},
		{suffix: "TUNING_EMBEDDING_MODEL", set: intoString(&cfg.Tuning.EmbeddingModel)},
		{suffix: "TUNING_EQUIVALENCE_THRESHOLD", set: intoFloat(&cfg.Tuning.EquivalenceThreshold)},
	}

	return bindings
}

func intoString(dst *string) func(string) error {
	return func(raw string) error {
		*dst = raw

		return nil
	}
}

func intoInt(dst *int) func(string) error {
	return func(raw string) error {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("not an integer: %w", err)
		}

		*dst = parsed

		return nil
	}
}

func intoInt64(dst *int64) func(string) error {
	return func(raw string) error {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("not an integer: %w", err)
		}

		*dst = parsed

		return nil
	}
}

func intoFloat(dst *float64) func(string) error {
	return func(raw string) error {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("not a number: %w", err)
		}

		*dst = parsed

		return nil
	}
}

func intoBool(dst *bool) func(string) error {
	return func(raw string) error {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("not a bool: %w", err)
		}

		*dst = parsed

		return nil
	}
}

func intoDuration(dst *time.Duration) func(string) error {
	return func(raw string) error {
		parsed, err := workflow.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("not a duration: %w", err)
		}

		*dst = parsed

		return nil
	}
}

func intoList(dst *[]string) func(string) error {
	return func(raw string) error {
		parts := strings.Split(raw, ",")

		cleaned := make([]string, 0, len(parts))
		for _, part := range parts {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				cleaned = append(cleaned, trimmed)
			}
		}

		*dst = cleaned

		return nil
	}
}
