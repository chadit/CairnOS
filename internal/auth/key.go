package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"golang.org/x/crypto/argon2"
)

// Plaintext key shape: ck_<ulid>_<base64url secret>. Carrying the id in
// the plaintext lets login do one indexed lookup instead of an argon2id
// comparison against every stored hash.
const (
	keyPrefix      = "ck"
	keySeparator   = "_"
	keySecretBytes = 32
)

// Argon2id parameters, the x/crypto-documented sensible defaults from RFC
// 9106 (64 MiB, 1 pass, 4 lanes). They are baked into each stored hash's
// PHC string, so raising them later only affects new keys while old hashes
// keep verifying with the parameters they were created under.
const (
	argonTime    = 1
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// phcPrefix opens every stored hash; the version and parameters follow.
const phcPrefix = "$argon2id$"

// phcFieldCount is the field count of a split PHC string:
// "", "argon2id", "v=19", "m=...,t=...,p=...", salt, hash.
const phcFieldCount = 6

// decoySecret feeds verifyDecoy a fixed comparison input so a login with
// an unknown key id burns the same argon2id work as a real mismatch.
const decoySecret = "decoy"

// defaultWorkspace names the tenant an access key falls into when none is
// configured, matching store.DefaultWorkspace. It is a local literal rather
// than an import so the auth package stays free of the store dependency; the
// two constants are pinned equal by TestKeyWorkspaceDefaults.
const defaultWorkspace = "default"

// Key is one admin-managed access key (.agents/spec/08-auth-and-access.md,
// "Access keys"): an id, a label, a scope set, an enabled flag, the workspace
// the key acts in (.agents/spec/12-ui-and-governance.md, "Multi-tenancy"),
// and the argon2id hash of its secret. The plaintext exists only in the one
// `cairnos keys add` printout. An empty Workspace means the default workspace,
// so a key created before tenancy keeps behaving as a default-workspace key.
type Key struct {
	ID        string
	Label     string
	Scopes    []string
	Workspace string
	Hash      string
	Enabled   bool
	CreatedAt time.Time
}

// keyWorkspace resolves the tenant a token minted from key acts in, mapping an
// empty key workspace onto the default so a pre-tenancy key still mints
// default-workspace tokens.
func keyWorkspace(key *Key) string {
	if key.Workspace == "" {
		return defaultWorkspace
	}

	return key.Workspace
}

// NewKey mints an access key in the given workspace: a ULID id, a 32-byte
// random secret, and the argon2id hash that is all the store ever sees. An
// empty workspace becomes the default. It returns the key record and the
// plaintext to print exactly once.
func NewKey(label string, scopes []string, workspace string, now time.Time) (*Key, string, error) {
	secretRaw := make([]byte, keySecretBytes)
	if _, err := rand.Read(secretRaw); err != nil {
		return nil, "", fmt.Errorf("generate key secret: %w", err)
	}

	id := ulid.Make().String()
	secret := base64.RawURLEncoding.EncodeToString(secretRaw)

	hash, err := hashSecret(secret)
	if err != nil {
		return nil, "", err
	}

	if workspace == "" {
		workspace = defaultWorkspace
	}

	key := &Key{
		ID:        id,
		Label:     label,
		Scopes:    scopes,
		Workspace: workspace,
		Hash:      hash,
		Enabled:   true,
		CreatedAt: now,
	}

	return key, strings.Join([]string{keyPrefix, id, secret}, keySeparator), nil
}

// SplitKey parses a plaintext access key into its id and secret parts.
// SplitN bounds the cut because the base64url secret may itself contain
// underscores; the ULID id never does (Crockford base32).
func SplitKey(plaintext string) (string, string, error) {
	parts := strings.SplitN(plaintext, keySeparator, 3)
	if len(parts) != 3 || parts[0] != keyPrefix || parts[1] == "" || parts[2] == "" {
		return "", "", ErrKeyMalformed
	}

	return parts[1], parts[2], nil
}

// VerifyKey compares a presented secret against a stored PHC argon2id
// hash in constant time, deriving with the parameters the hash itself
// carries so parameter bumps never invalidate old keys.
func VerifyKey(secret, encoded string) error {
	salt, want, params, err := parsePHC(encoded)
	if err != nil {
		return err
	}

	got := argon2.IDKey([]byte(secret), salt, params.time, params.memory, params.threads, uint32(len(want))) // #nosec G115 -- hash length is the fixed argonKeyLen

	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrKeyMismatch
	}

	return nil
}

// hashSecret derives the argon2id digest of one key secret and renders it
// as a PHC string: $argon2id$v=19$m=...,t=...,p=...$<salt>$<hash>.
func hashSecret(secret string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}

	digest := argon2.IDKey([]byte(secret), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	return fmt.Sprintf("%sv=%d$m=%d,t=%d,p=%d$%s$%s",
		phcPrefix, argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest)), nil
}

// argonParams carries the derivation parameters parsed from a PHC string.
type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

// parsePHC splits a stored hash back into salt, digest, and parameters.
func parsePHC(encoded string) ([]byte, []byte, argonParams, error) {
	fields := strings.Split(encoded, "$")
	if len(fields) != phcFieldCount || fields[1] != "argon2id" {
		return nil, nil, argonParams{}, fmt.Errorf("%w: %q", ErrHashMalformed, redactHash(encoded))
	}

	var version int
	if _, err := fmt.Sscanf(fields[2], "v=%d", &version); err != nil || version != argon2.Version {
		return nil, nil, argonParams{}, fmt.Errorf("%w: unsupported version", ErrHashMalformed)
	}

	var params argonParams
	if _, err := fmt.Sscanf(fields[3], "m=%d,t=%d,p=%d", &params.memory, &params.time, &params.threads); err != nil {
		return nil, nil, argonParams{}, fmt.Errorf("%w: parameters: %w", ErrHashMalformed, err)
	}

	salt, err := base64.RawStdEncoding.DecodeString(fields[4])
	if err != nil {
		return nil, nil, argonParams{}, fmt.Errorf("%w: salt: %w", ErrHashMalformed, err)
	}

	digest, err := base64.RawStdEncoding.DecodeString(fields[5])
	if err != nil {
		return nil, nil, argonParams{}, fmt.Errorf("%w: digest: %w", ErrHashMalformed, err)
	}

	return salt, digest, params, nil
}

// redactHash keeps a malformed hash out of error messages beyond its
// shape-identifying head; the value is still a credential derivative.
func redactHash(encoded string) string {
	const head = 12

	if len(encoded) <= head {
		return encoded
	}

	return encoded[:head] + "..."
}

// verifyDecoy burns one argon2id derivation against a fixed salt so the
// unknown-key login path costs the same as a real comparison; the result
// is discarded on purpose. It derives with the exact parameters hashSecret
// bakes into a new key (argonTime/argonMemory/argonThreads/argonKeyLen), so
// the decoy cost tracks a fresh key's cost automatically: raising the
// defaults raises both together, keeping the timing oracle closed (spec L6).
// TestDecoyParamsMatchStoredHash pins this equality so a future param bump on
// only one path fails the build.
func verifyDecoy(secret string) {
	salt := make([]byte, argonSaltLen)

	if secret == "" {
		secret = decoySecret
	}

	_ = argon2.IDKey([]byte(secret), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
}
