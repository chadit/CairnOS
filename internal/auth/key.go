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

// HashCost is the argon2id work one derivation pays. The zero value is the
// production default above. A key stores the cost it was minted under, so
// verifying never needs it; minting and the unknown-key decoy do.
type HashCost struct {
	MemoryKiB uint32
	Time      uint32
	Threads   uint8
}

// withDefault resolves the zero value to the production cost and lifts a
// zero pass or lane count to one, since argon2 panics on either.
func (c HashCost) withDefault() HashCost {
	if c == (HashCost{}) {
		return HashCost{MemoryKiB: argonMemory, Time: argonTime, Threads: argonThreads}
	}

	c.Time = max(c.Time, 1)
	c.Threads = max(c.Threads, 1)

	return c
}

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
// "Access keys"): id, label, scope set, enabled flag, the workspace the key
// acts in (.agents/spec/12-ui-and-governance.md, "Multi-tenancy"), and the
// argon2id hash of its secret. The plaintext exists only in the one
// `cairnos keys add` printout. An empty Workspace means the default
// workspace, so a key created before tenancy still behaves as one.
type Key struct {
	CreatedAt time.Time
	ID        string
	Label     string
	Workspace string
	Hash      string
	Scopes    []string
	Enabled   bool
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
// empty workspace becomes the default, and a zero cost the production one. It
// returns the key record and the plaintext to print exactly once.
func NewKey(label string, scopes []string, workspace string, now time.Time, cost HashCost) (*Key, string, error) {
	secretRaw := make([]byte, keySecretBytes)
	if _, err := rand.Read(secretRaw); err != nil {
		return nil, "", fmt.Errorf("generate key secret: %w", err)
	}

	id := ulid.Make().String()
	secret := base64.RawURLEncoding.EncodeToString(secretRaw)

	hash, err := hashSecret(secret, cost.withDefault())
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
// SplitN bounds the cut because the base64url secret may itself contain the
// "_" separator; the ULID id never does (Crockford base32).
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

	got := argon2.IDKey([]byte(secret), salt, params.Time, params.MemoryKiB, params.Threads, uint32(len(want))) // #nosec G115 -- hash length is the fixed argonKeyLen

	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrKeyMismatch
	}

	return nil
}

// hashSecret derives the argon2id digest of one key secret and renders it
// as a PHC string: $argon2id$v=19$m=...,t=...,p=...$<salt>$<hash>.
func hashSecret(secret string, cost HashCost) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}

	digest := argon2.IDKey([]byte(secret), salt, cost.Time, cost.MemoryKiB, cost.Threads, argonKeyLen)

	return fmt.Sprintf("%sv=%d$m=%d,t=%d,p=%d$%s$%s",
		phcPrefix, argon2.Version, cost.MemoryKiB, cost.Time, cost.Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest)), nil
}

// parsePHC splits a stored hash back into salt, digest, and parameters.
func parsePHC(encoded string) ([]byte, []byte, HashCost, error) {
	fields := strings.Split(encoded, "$")
	if len(fields) != phcFieldCount || fields[1] != "argon2id" {
		return nil, nil, HashCost{}, fmt.Errorf("%w: %q", ErrHashMalformed, redactHash(encoded))
	}

	var version int
	if _, err := fmt.Sscanf(fields[2], "v=%d", &version); err != nil || version != argon2.Version {
		return nil, nil, HashCost{}, fmt.Errorf("%w: unsupported version", ErrHashMalformed)
	}

	var params HashCost
	if _, err := fmt.Sscanf(fields[3], "m=%d,t=%d,p=%d", &params.MemoryKiB, &params.Time, &params.Threads); err != nil {
		return nil, nil, HashCost{}, fmt.Errorf("%w: parameters: %w", ErrHashMalformed, err)
	}

	salt, err := base64.RawStdEncoding.DecodeString(fields[4])
	if err != nil {
		return nil, nil, HashCost{}, fmt.Errorf("%w: salt: %w", ErrHashMalformed, err)
	}

	digest, err := base64.RawStdEncoding.DecodeString(fields[5])
	if err != nil {
		return nil, nil, HashCost{}, fmt.Errorf("%w: digest: %w", ErrHashMalformed, err)
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

// verifyDecoy burns an argon2id derivation against a fixed salt so the
// unknown-key login path costs the same as a real comparison. Login passes the
// cost its Authenticator mints under, so raising the defaults raises both
// costs and keeps the timing oracle closed (spec L6).
func verifyDecoy(secret string, cost HashCost) {
	salt := make([]byte, argonSaltLen)

	if secret == "" {
		secret = decoySecret
	}

	_ = argon2.IDKey([]byte(secret), salt, cost.Time, cost.MemoryKiB, cost.Threads, argonKeyLen)
}
