package webhook_test

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/chadit/CairnOS/internal/webhook"
	"github.com/chadit/CairnOS/internal/webhook/webhooktest"
)

// tokenHexLen is the character length of a minted token: 32 crypto/rand bytes
// hex-encoded. The handler rejects anything but this exact shape before a
// lookup, so the mint output and the accepted input match.
const tokenHexLen = 64

// TestSQLiteWebhooksConformance runs the shared registration-store contract
// against the sqlite backend. The postgres counterpart lives in
// tests/integration so it can run against a real database without an env-gate.
func TestSQLiteWebhooksConformance(t *testing.T) {
	t.Parallel()

	webhooktest.RunConformance(t, func(t *testing.T) webhook.Store {
		t.Helper()

		store, err := webhook.OpenSQLiteWebhooks(filepath.Join(t.TempDir(), "webhooks.db"))
		if err != nil {
			t.Fatalf("open sqlite webhooks: %v", err)
		}

		t.Cleanup(func() { _ = store.Close() })

		return store
	})
}

// TestMintTokenStoresHashNotToken proves MintToken hands back a 64-hex token
// and that its stored hash is the SHA-256 of that token, never the token
// itself. A dumped table therefore holds no working capability.
func TestMintTokenStoresHashNotToken(t *testing.T) {
	t.Parallel()

	token, hash, err := webhook.MintToken()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	if len(token) != tokenHexLen {
		t.Errorf("token length = %d, want %d", len(token), tokenHexLen)
	}

	if _, decodeErr := hex.DecodeString(token); decodeErr != nil {
		t.Errorf("token is not hex: %v", decodeErr)
	}

	if hash == token {
		t.Fatal("stored hash equals the token; the token must never be stored")
	}

	sum := sha256.Sum256([]byte(token))
	if want := hex.EncodeToString(sum[:]); hash != want {
		t.Errorf("hash = %q, want SHA-256 of token %q", hash, want)
	}
}

// TestMintTokenIsUnique proves two mints yield different tokens and hashes, so
// production and test endpoints never collide on one capability.
func TestMintTokenIsUnique(t *testing.T) {
	t.Parallel()

	first, firstHash, err := webhook.MintToken()
	if err != nil {
		t.Fatalf("mint first: %v", err)
	}

	second, secondHash, err := webhook.MintToken()
	if err != nil {
		t.Fatalf("mint second: %v", err)
	}

	if first == second {
		t.Error("two mints produced the same token")
	}

	if firstHash == secondHash {
		t.Error("two mints produced the same hash")
	}
}

// TestHashTokenMatchesMint proves HashToken reproduces the stored hash for a
// minted token, the equality the ingress handler relies on to look up by hash.
func TestHashTokenMatchesMint(t *testing.T) {
	t.Parallel()

	token, hash, err := webhook.MintToken()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	if got := webhook.HashToken(token); got != hash {
		t.Errorf("HashToken(token) = %q, want minted hash %q", got, hash)
	}
}
