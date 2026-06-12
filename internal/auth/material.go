package auth

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"

	"github.com/golang-jwt/jwt/v5"
)

// Signing algorithm names accepted by signing_algorithm
// (.agents/spec/08-auth-and-access.md, Change 3): HS256 for the
// self-issued single instance, EdDSA (ed25519) for the asymmetric tier
// where verifiers must not be able to mint.
const (
	AlgHS256 = "HS256"
	AlgEdDSA = "EdDSA"
)

// Environment variable names for the signing material. Secrets come only
// from the environment or a secret source, never the config file
// (.agents/spec/08-auth-and-access.md, "Configuration"). The constants
// name variables, not credential values.
const (
	hs256SecretEnv    = "CAIRNOS_AUTH_HS256_SECRET" // #nosec G101 -- environment variable name, not a credential
	privateKeyFileEnv = "CAIRNOS_AUTH_PRIVATE_KEY_FILE"
	publicKeyFileEnv  = "CAIRNOS_AUTH_PUBLIC_KEY_FILE"
)

// minHS256SecretBytes is the enforced secret floor: HMAC-SHA256 security
// degrades below the hash's 32-byte output size, and a short shared secret
// undermines every token minted under it.
const minHS256SecretBytes = 32

// Material carries the immutable signing and verification keys for the
// configured algorithm. Exactly one branch is populated.
type Material struct {
	alg        string
	hmacSecret []byte
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
}

// LoadMaterial resolves the signing material for one algorithm from the
// environment: the HS256 secret from CAIRNOS_AUTH_HS256_SECRET (minimum 32
// bytes), or the EdDSA key pair from the PEM files named by
// CAIRNOS_AUTH_PRIVATE_KEY_FILE and CAIRNOS_AUTH_PUBLIC_KEY_FILE. getenv
// is injected so tests need no process environment.
func LoadMaterial(alg string, getenv func(string) string) (*Material, error) {
	switch alg {
	case AlgHS256:
		return loadHS256(getenv)
	case AlgEdDSA:
		return loadEdDSA(getenv)
	default:
		return nil, fmt.Errorf("%w: %q (want %s or %s)", ErrAlgorithmUnknown, alg, AlgHS256, AlgEdDSA)
	}
}

// Alg names the configured algorithm; verification pins to exactly this.
func (m *Material) Alg() string {
	return m.alg
}

// EdDSAKeys returns the ed25519 key pair when the configured tier is EdDSA,
// reporting false otherwise. The A2A card signer reuses these when the auth
// layer already runs asymmetric (.agents/spec/09-a2a-and-hive.md: the agent
// card carries the same EdDSA public key the cross-org tokens verify under),
// so the card and the tokens share one trust anchor.
func (m *Material) EdDSAKeys() (ed25519.PrivateKey, ed25519.PublicKey, bool) {
	if m.alg != AlgEdDSA {
		return nil, nil, false
	}

	return m.privateKey, m.publicKey, true
}

// signingMethod maps the algorithm onto the jwt library's method.
func (m *Material) signingMethod() jwt.SigningMethod {
	if m.alg == AlgEdDSA {
		return jwt.SigningMethodEdDSA
	}

	return jwt.SigningMethodHS256
}

// signingKey is the private half used to mint tokens.
func (m *Material) signingKey() any {
	if m.alg == AlgEdDSA {
		return m.privateKey
	}

	return m.hmacSecret
}

// verifyKey is the half handed to the parser's keyfunc.
func (m *Material) verifyKey() any {
	if m.alg == AlgEdDSA {
		return m.publicKey
	}

	return m.hmacSecret
}

// loadHS256 reads and floor-checks the shared secret.
func loadHS256(getenv func(string) string) (*Material, error) {
	secret := getenv(hs256SecretEnv)
	if secret == "" {
		return nil, fmt.Errorf("%w: set %s", ErrSecretMissing, hs256SecretEnv)
	}

	if len(secret) < minHS256SecretBytes {
		return nil, fmt.Errorf("%w: %s holds %d bytes", ErrSecretTooShort, hs256SecretEnv, len(secret))
	}

	return &Material{alg: AlgHS256, hmacSecret: []byte(secret)}, nil
}

// loadEdDSA reads the ed25519 key pair from the configured PEM files. Both
// halves are required: the Phase 4a serve process is issuer and verifier
// in one.
func loadEdDSA(getenv func(string) string) (*Material, error) {
	privatePath := getenv(privateKeyFileEnv)
	if privatePath == "" {
		return nil, fmt.Errorf("%w: set %s", ErrKeyFileMissing, privateKeyFileEnv)
	}

	publicPath := getenv(publicKeyFileEnv)
	if publicPath == "" {
		return nil, fmt.Errorf("%w: set %s", ErrKeyFileMissing, publicKeyFileEnv)
	}

	privateKey, err := readPrivateKeyPEM(privatePath)
	if err != nil {
		return nil, err
	}

	publicKey, err := readPublicKeyPEM(publicPath)
	if err != nil {
		return nil, err
	}

	// The configured public key must be the mate of the private key, or this
	// node would sign tokens its own verifier rejects (a silent
	// misconfiguration that only shows up at the first verify). Compare the
	// derived public key against the loaded one (spec L5).
	derived, ok := privateKey.Public().(ed25519.PublicKey)
	if !ok || !derived.Equal(publicKey) {
		return nil, fmt.Errorf("%w: %s does not match the private key in %s", ErrKeyPairMismatch, publicPath, privatePath)
	}

	return &Material{alg: AlgEdDSA, privateKey: privateKey, publicKey: publicKey}, nil
}

// groupOtherPermBits masks the group and other permission bits; a private key
// file that any of them can read is a leak risk, so loadEdDSA refuses it.
const groupOtherPermBits = 0o077

// readPrivateKeyPEM parses a PKCS#8 PEM file into an ed25519 private key,
// first refusing a world- or group-readable file (spec L5).
func readPrivateKeyPEM(path string) (ed25519.PrivateKey, error) {
	if err := refuseOpenPerms(path); err != nil {
		return nil, err
	}

	block, err := readPEMBlock(path)
	if err != nil {
		return nil, err
	}

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrKeyFileInvalid, path, err)
	}

	key, isEd25519 := parsed.(ed25519.PrivateKey)
	if !isEd25519 {
		return nil, fmt.Errorf("%w: %s holds %T, want ed25519", ErrKeyFileInvalid, path, parsed)
	}

	return key, nil
}

// readPublicKeyPEM parses a PKIX PEM file into an ed25519 public key.
func readPublicKeyPEM(path string) (ed25519.PublicKey, error) {
	block, err := readPEMBlock(path)
	if err != nil {
		return nil, err
	}

	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrKeyFileInvalid, path, err)
	}

	key, isEd25519 := parsed.(ed25519.PublicKey)
	if !isEd25519 {
		return nil, fmt.Errorf("%w: %s holds %T, want ed25519", ErrKeyFileInvalid, path, parsed)
	}

	return key, nil
}

// refuseOpenPerms rejects a private key file readable by group or other; a
// world-readable signing key undermines every token minted under it (spec
// L5). The check is skipped on platforms whose Stat reports no Unix mode
// bits, where perm would always be zero.
func refuseOpenPerms(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat key file: %w", err)
	}

	if info.Mode().Perm()&groupOtherPermBits != 0 {
		return fmt.Errorf("%w: %s is group- or world-readable (mode %#o); chmod 600 it",
			ErrKeyFilePermissive, path, info.Mode().Perm())
	}

	return nil
}

// readPEMBlock reads one PEM block from a key file the operator named in
// the environment.
func readPEMBlock(path string) (*pem.Block, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the key file path is the operator's own environment configuration
	if err != nil {
		return nil, fmt.Errorf("read key file: %w", err)
	}

	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%w: %s has no PEM block", ErrKeyFileInvalid, path)
	}

	return block, nil
}
