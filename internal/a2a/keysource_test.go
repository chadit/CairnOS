package a2a_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/chadit/CairnOS/internal/a2a"
)

// writeEd25519Key mints an ed25519 key, writes its PKCS#8 PEM under dir, and
// returns the path plus the public half so a test can confirm which key
// resolution picked.
func writeEd25519Key(t *testing.T, dir string) (string, ed25519.PublicKey) {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	path := filepath.Join(dir, "card.pem")
	encoded := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	return path, pub
}

func TestResolveSigningKeyFromFile(t *testing.T) {
	t.Parallel()

	path, wantPub := writeEd25519Key(t, t.TempDir())

	key, origin, err := a2a.ResolveSigningKey(path, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("ResolveSigningKey: %v", err)
	}

	if origin != a2a.OriginFile {
		t.Fatalf("origin = %q, want %q", origin, a2a.OriginFile)
	}

	gotPub, ok := key.Public().(ed25519.PublicKey)
	if !ok || !gotPub.Equal(wantPub) {
		t.Fatal("resolved key does not match the file key")
	}
}

func TestResolveSigningKeyReusesAuthKey(t *testing.T) {
	t.Parallel()

	authPub, authPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate auth key: %v", err)
	}

	key, origin, err := a2a.ResolveSigningKey("", authPriv, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("ResolveSigningKey: %v", err)
	}

	if origin != a2a.OriginAuthReuse {
		t.Fatalf("origin = %q, want %q", origin, a2a.OriginAuthReuse)
	}

	gotPub, ok := key.Public().(ed25519.PublicKey)
	if !ok || !gotPub.Equal(authPub) {
		t.Fatal("resolved key is not the reused auth key")
	}
}

func TestResolveSigningKeyEphemeralFallback(t *testing.T) {
	t.Parallel()

	key, origin, err := a2a.ResolveSigningKey("", nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("ResolveSigningKey: %v", err)
	}

	if origin != a2a.OriginEphemeral {
		t.Fatalf("origin = %q, want %q", origin, a2a.OriginEphemeral)
	}

	if len(key) == 0 {
		t.Fatal("ephemeral path returned an empty key")
	}
}

func TestResolveSigningKeyRejectsBadFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(path, []byte("not a key"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}

	if _, _, err := a2a.ResolveSigningKey(path, nil, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("ResolveSigningKey accepted a garbage key file")
	}
}
