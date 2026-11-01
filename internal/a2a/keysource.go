package a2a

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
)

// KeyOrigin records where the card signing key came from, so the wiring can
// log it and tests can assert the fallback path taken.
type KeyOrigin string

// Card signing key origins, in resolution order.
const (
	// OriginFile means the key loaded from the configured PEM file.
	OriginFile KeyOrigin = "file"

	// OriginAuthReuse means the key is the auth layer's EdDSA private key,
	// reused so the card and the cross-org tokens share one trust anchor.
	OriginAuthReuse KeyOrigin = "auth-reuse"

	// OriginEphemeral means no key was configured or reusable, so the node
	// generated one at boot. The key dies with the process, so a restart
	// publishes a card under a new key: dev only.
	OriginEphemeral KeyOrigin = "ephemeral"
)

// ResolveSigningKey picks the card signing key in priority order: the configured
// PEM file, the auth layer's EdDSA key when the auth tier is asymmetric, then an
// ephemeral key. Ephemeral warns because a key that dies on restart is dev-only.
// authKey is nil when auth is off or the tier is HS256.
func ResolveSigningKey(keyFile string, authKey ed25519.PrivateKey, logger *slog.Logger) (ed25519.PrivateKey, KeyOrigin, error) {
	if logger == nil {
		logger = slog.Default()
	}

	if keyFile != "" {
		key, err := loadEd25519PEM(keyFile)
		if err != nil {
			return nil, "", err
		}

		return key, OriginFile, nil
	}

	if len(authKey) > 0 {
		return authKey, OriginAuthReuse, nil
	}

	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("generate ephemeral a2a card signing key: %w", err)
	}

	logger.Warn("a2a card signing key is ephemeral: no card_signing_key_file set and the auth tier is not EdDSA; " +
		"the card key changes on every restart, which is fine for local use but wrong for a node other agents trust")

	return key, OriginEphemeral, nil
}

// loadEd25519PEM reads a PKCS#8 PEM file into an ed25519 private key, matching
// the auth package's key file format so an operator can point both at the same
// key when running the asymmetric tier.
func loadEd25519PEM(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the key file path is the operator's own [a2a] config
	if err != nil {
		return nil, fmt.Errorf("read a2a card signing key: %w", err)
	}

	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%w: %s has no PEM block", ErrSigningKeyFileInvalid, path)
	}

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrSigningKeyFileInvalid, path, err)
	}

	key, isEd25519 := parsed.(ed25519.PrivateKey)
	if !isEd25519 {
		return nil, fmt.Errorf("%w: %s holds %T, want ed25519", ErrSigningKeyFileInvalid, path, parsed)
	}

	return key, nil
}
