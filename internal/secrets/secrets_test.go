package secrets_test

import (
	"context"
	"errors"
	"testing"

	"github.com/chadit/CairnOS/internal/secrets"
)

func TestEnvGetReadsPrefixedVariable(t *testing.T) {
	t.Setenv(secrets.EnvPrefix+"DB_PASSWORD", "hunter2")

	source := secrets.NewEnv()

	value, err := source.Get(t.Context(), "db_password")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if value != "hunter2" {
		t.Fatalf("value = %q, want hunter2", value)
	}
}

func TestEnvGetNormalizesNameStyles(t *testing.T) {
	t.Setenv(secrets.EnvPrefix+"API_KEY", "k")

	source := secrets.NewEnv()

	for _, name := range []string{"api_key", "api-key", "API.KEY"} {
		value, err := source.Get(t.Context(), name)
		if err != nil || value != "k" {
			t.Fatalf("get %q = %q, %v; want k", name, value, err)
		}
	}
}

func TestEnvGetMissingReturnsNotFound(t *testing.T) {
	t.Parallel()

	source := secrets.NewEnv()

	_, err := source.Get(t.Context(), "ghost_secret_never_set")
	if !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestEnvGetHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	source := secrets.NewEnv()

	if _, err := source.Get(ctx, "anything"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}
