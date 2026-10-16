//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"hash/fnv"
	"io"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chadit/CairnOS/internal/secrets"
	"github.com/chadit/CairnOS/internal/secrets/secretstest"
	"github.com/chadit/CairnOS/tests/integration"
)

const (
	// atRestSecretName and atRestSecretValue are the fixture for the postgres
	// at-rest proof; the value is distinctive so the column scan can look for it
	// verbatim.
	atRestSecretName  = "db_password"
	atRestSecretValue = "correct-horse-battery-staple"
	// aesKeyBytes is the AES-256 key length the integration fixtures generate.
	aesKeyBytes = 32
)

// secretsSchemaFor derives a stable, valid Postgres schema name from a subtest
// name so reopening the same subtest under a new key set (the rotation cases)
// hits the same schema while distinct subtests stay isolated.
func secretsSchemaFor(name string) string {
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte(name))

	return "cairnos_secrets_test_" + strconv.FormatUint(hasher.Sum64(), 16)
}

// integrationKey returns 32 random bytes for an AES-256 sealer.
func integrationKey(t *testing.T) []byte {
	t.Helper()

	key := make([]byte, aesKeyBytes)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		t.Fatalf("generate key: %v", err)
	}

	return key
}

// openSecretsSchema opens a secret store in its own Postgres schema off the
// shared container under keys, dropping the schema on cleanup.
func openSecretsSchema(t *testing.T, schema string, keys secretstest.KeySet) *secrets.Postgres {
	t.Helper()

	ctx := t.Context()

	sealer, err := secrets.NewSealer(keys.Primary, keys.Previous...)
	if err != nil {
		t.Fatalf("build sealer: %v", err)
	}

	pgStore, err := secrets.OpenPostgresSchema(ctx, integration.DSN(t), schema, sealer)
	if err != nil {
		t.Fatalf("open postgres secret store: %v", err)
	}

	t.Cleanup(func() {
		dropCtx := context.WithoutCancel(ctx)

		if dropErr := secrets.DropPostgresSchema(dropCtx, pgStore, schema); dropErr != nil {
			t.Errorf("drop test schema: %v", dropErr)
		}

		if closeErr := pgStore.Close(); closeErr != nil {
			t.Errorf("close postgres secret store: %v", closeErr)
		}
	})

	return pgStore
}

// TestSecretsConformance runs the behavioral spec shared with the sqlite backend
// against the live Postgres container, proving the two backends agree on
// sealing, name normalization, workspace isolation, and rotation.
func TestSecretsConformance(t *testing.T) {
	t.Parallel()

	secretstest.RunConformance(t, func(t *testing.T, keys secretstest.KeySet) secrets.Store {
		t.Helper()

		return openSecretsSchema(t, secretsSchemaFor(t.Name()), keys)
	})
}

// TestSecretsNoPlaintextAtRestPostgres is the postgres half of the at-rest
// proof: after sealing a distinctive value, the ciphertext column holds no copy
// of the plaintext. Only the AES-256-GCM envelope is stored.
func TestSecretsNoPlaintextAtRestPostgres(t *testing.T) {
	t.Parallel()

	schema := secretsSchemaFor(t.Name())
	secretStore := openSecretsSchema(t, schema, secretstest.KeySet{Primary: integrationKey(t)})

	if err := secretStore.Set(t.Context(), atRestSecretName, atRestSecretValue); err != nil {
		t.Fatalf("set: %v", err)
	}

	ciphertext := rawCiphertext(t, schema, atRestSecretName)
	if len(ciphertext) == 0 {
		t.Fatal("ciphertext column was empty; nothing was stored")
	}

	if bytes.Contains(ciphertext, []byte(atRestSecretValue)) {
		t.Error("postgres ciphertext column carries the plaintext value")
	}
}

// rawCiphertext reads the sealed bytes straight from the ciphertext column,
// bypassing the store's Open so the scan sees exactly what a database dump would.
func rawCiphertext(t *testing.T, schema, name string) []byte {
	t.Helper()

	ctx := t.Context()

	cfg, err := pgxpool.ParseConfig(integration.DSN(t))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}

	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}

	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open raw pool: %v", err)
	}
	defer pool.Close()

	var ciphertext []byte

	if scanErr := pool.QueryRow(ctx,
		"SELECT ciphertext FROM secrets WHERE name = $1", secrets.NormalizeName(name)).
		Scan(&ciphertext); scanErr != nil {
		t.Fatalf("read ciphertext column: %v", scanErr)
	}

	return ciphertext
}
