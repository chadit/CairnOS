// Package auth implements the access model: admin-managed access keys
// stored argon2id-hashed, short-lived JWTs minted at login with the key's
// scopes, and a jti/sub denylist checked on every verify so revocation is real
// before exp. Signing is two-tier by configuration: HS256 with a shared
// secret for the self-issued single instance, EdDSA (ed25519) where
// verifiers must not be able to mint.
package auth

import (
	"context"
	"fmt"
	"time"
)

// KeyStore is the access-key CRUD half of the auth persistence.
type KeyStore interface {
	// CreateKey persists one access key (hash, never plaintext).
	CreateKey(ctx context.Context, key *Key) error

	// GetKey fetches one access key by id.
	GetKey(ctx context.Context, id string) (*Key, error)

	// ListKeys returns every access key, oldest first.
	ListKeys(ctx context.Context) ([]*Key, error)

	// SetKeyEnabled flips one key's enabled flag.
	SetKeyEnabled(ctx context.Context, id string, enabled bool) error
}

// TokenLedger records each minted token's exp so revocation can deny a token
// until its real expiry even after access_token_ttl drops between mint and
// revoke.
type TokenLedger interface {
	// RecordIssued persists one freshly minted token's jti, subject, and exp
	// so a later RevokeToken can deny it until its real exp, and RevokeKey can
	// bound its window to the subject's longest-lived token. The row is pruned
	// once exp passes.
	RecordIssued(ctx context.Context, jti, sub string, exp time.Time) error

	// IssuedExp returns the recorded exp for one token id, reporting false
	// when no record exists (a bare jti revoked without a mint record, or
	// one already pruned).
	IssuedExp(ctx context.Context, jti string) (time.Time, bool, error)

	// MaxIssuedExp returns the latest exp across every recorded token of one
	// subject, reporting false when the subject has no live records. RevokeKey
	// uses it to bound the deny window to the key's longest-lived token.
	MaxIssuedExp(ctx context.Context, sub string) (time.Time, bool, error)
}

// DenyRecorder writes revocations: deny one token id, deny a key's tokens
// minted up to an instant, or lift a key's denial on re-enable.
type DenyRecorder interface {
	// DenyToken denylists one token id until the given time.
	DenyToken(ctx context.Context, jti string, until time.Time) error

	// DenyKeySince records a revoked-at instant for one key (deny-by-sub):
	// only tokens minted at or before revokedAt are denied, so the key stays
	// enabled and fresh tokens (iat > revokedAt) pass, matching auth.proto.
	// until bounds when the row may be pruned.
	DenyKeySince(ctx context.Context, keyID string, revokedAt, until time.Time) error

	// RemoveKeyDenials deletes one key's deny-by-sub rows, the re-enable
	// counterpart of DenyKeySince.
	RemoveKeyDenials(ctx context.Context, keyID string) error
}

// DenyChecker answers the per-verify revocation question and prunes the rows
// that can no longer deny anything.
type DenyChecker interface {
	// IsDenied reports whether the token is denylisted at the given instant:
	// a live jti row denies outright, and a live sub row denies only when
	// the token's iat is at or before the recorded revoked-at instant.
	IsDenied(ctx context.Context, jti, sub string, iat, now time.Time) (bool, error)

	// PruneDenylist deletes denylist and issued-token rows whose expiry has
	// passed and reports how many went.
	PruneDenylist(ctx context.Context, now time.Time) (int, error)
}

// Denylist is the revocation half: deny by token id or by key, check on every
// verify, and prune expired rows.
type Denylist interface {
	DenyRecorder
	DenyChecker
}

// Store is the full persistence the authenticator and the CLI key commands
// depend on; SQLite implements it in this package, sharing the run store's
// database file with disjoint tables. It composes the key CRUD, the minted-
// token ledger, and the revocation denylist.
type Store interface {
	KeyStore
	TokenLedger
	Denylist
}

// revokeCeiling caps the deny window for a bare jti with no mint record:
// long enough to outlast any token a default deployment hands out, so a
// shortened TTL never resurrects it, and bounded so the row still prunes.
const revokeCeiling = 24 * time.Hour

// Config tunes token issue and verification.
type Config struct {
	// Now anchors issue and expiry math; nil means time.Now. Tests inject
	// a fake clock here so expiry needs no sleeping.
	Now func() time.Time

	// Issuer and Audience fill and check the iss/aud claims.
	Issuer   string
	Audience string

	// TTL bounds each access token; the default is one hour, with
	// refresh by re-login.
	TTL time.Duration

	// ClockSkew is the leeway the cross-instance EdDSA parser allows on the
	// time claims, so a token minted on a peer whose clock runs slightly
	// ahead still verifies. Zero applies no leeway.
	ClockSkew time.Duration

	// RequireExp and RequireJTI reject tokens missing those claims; both
	// default on and are overridden only deliberately.
	RequireExp bool
	RequireJTI bool

	// HashCost is the argon2id cost the unknown-key decoy pays. It should match
	// the cost this deployment mints keys under, which for every real
	// deployment is the zero value, the production default.
	HashCost HashCost
}

// Authenticator owns the login exchange, token verification, and
// revocation. It is safe for concurrent use: the store serializes writes
// and the signing material is immutable after construction.
type Authenticator struct {
	store    Store
	material *Material
	cfg      Config
}

// New wires an authenticator over its store and signing material.
func New(store Store, material *Material, cfg Config) *Authenticator {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	cfg.HashCost = cfg.HashCost.withDefault()

	return &Authenticator{store: store, material: material, cfg: cfg}
}

// Login exchanges a plaintext access key for a signed token carrying the
// key's scopes, returning the token and its expiry. Every failure path
// surfaces as a sentinel so the API layer can answer with one
// indistinguishable message; the decoy hash comparison keeps an unknown
// key id from answering measurably faster than a wrong secret.
func (a *Authenticator) Login(ctx context.Context, plaintext string) (string, time.Time, error) {
	keyID, secret, err := SplitKey(plaintext)
	if err != nil {
		return "", time.Time{}, err
	}

	key, err := a.store.GetKey(ctx, keyID)
	if err != nil {
		// Burn the same argon2id work a real comparison would, so a miss
		// and a mismatch take comparable time.
		verifyDecoy(secret, a.cfg.HashCost)

		return "", time.Time{}, fmt.Errorf("login: %w", err)
	}

	if verifyErr := VerifyKey(secret, key.Hash); verifyErr != nil {
		return "", time.Time{}, verifyErr
	}

	// The enabled check runs after the hash comparison on purpose: a
	// disabled key answers in the same time as a live one.
	if !key.Enabled {
		return "", time.Time{}, fmt.Errorf("%w: %s", ErrKeyDisabled, key.ID)
	}

	return a.issue(ctx, key)
}

// Verify checks a presented token end to end: signature under the
// configured algorithm only, issuer, audience, expiry, required claims,
// then the denylist. It returns the verified claims for scope checks.
func (a *Authenticator) Verify(ctx context.Context, token string) (*Claims, error) {
	claims, err := a.parse(token)
	if err != nil {
		return nil, err
	}

	denied, err := a.store.IsDenied(ctx, claims.ID, claims.Subject, claimIssuedAt(claims), a.cfg.Now())
	if err != nil {
		return nil, fmt.Errorf("check denylist: %w", err)
	}

	if denied {
		return nil, ErrTokenRevoked
	}

	return claims, nil
}

// RevokeToken denylists one token id until its real exp. The mint-time
// record (RecordIssued) carries that exp, so lowering access_token_ttl
// between mint and revoke cannot shorten the deny window below the token's
// actual life. A bare jti with no record falls back to a ceiling
// that outlasts any default-deployment token.
func (a *Authenticator) RevokeToken(ctx context.Context, jti string) error {
	until, err := a.revokeWindow(ctx, jti)
	if err != nil {
		return err
	}

	if err := a.store.DenyToken(ctx, jti, until); err != nil {
		return fmt.Errorf("revoke token %s: %w", jti, err)
	}

	return nil
}

// RevokeKey denies the access key's tokens minted at or before now
// (deny-by-sub keyed on a revoked-at instant). The key stays enabled and
// fresh tokens minted afterward pass, matching auth.proto. The
// deny row is held until the key's longest-lived recorded token could
// expire, falling back to the TTL/ceiling bound when no record survives.
func (a *Authenticator) RevokeKey(ctx context.Context, keyID string) error {
	revokedAt := a.cfg.Now()

	until, err := a.keyRevokeWindow(ctx, keyID, revokedAt)
	if err != nil {
		return err
	}

	if err := a.store.DenyKeySince(ctx, keyID, revokedAt, until); err != nil {
		return fmt.Errorf("revoke key %s: %w", keyID, err)
	}

	return nil
}

// revokeWindow picks the deny ceiling for one jti: its recorded exp when a
// mint record survives, otherwise now plus the larger of TTL and the
// 24-hour ceiling, so no live token outlives its denial.
func (a *Authenticator) revokeWindow(ctx context.Context, jti string) (time.Time, error) {
	exp, ok, err := a.store.IssuedExp(ctx, jti)
	if err != nil {
		return time.Time{}, fmt.Errorf("look up token %s exp: %w", jti, err)
	}

	if ok {
		return exp, nil
	}

	return a.cfg.Now().Add(max(a.cfg.TTL, revokeCeiling)), nil
}

// keyRevokeWindow bounds how long the key's deny-by-sub row lives: the
// latest exp among the key's recorded tokens, or the TTL/ceiling fallback
// when none survive.
func (a *Authenticator) keyRevokeWindow(ctx context.Context, keyID string, now time.Time) (time.Time, error) {
	exp, ok, err := a.store.MaxIssuedExp(ctx, keyID)
	if err != nil {
		return time.Time{}, fmt.Errorf("look up key %s max exp: %w", keyID, err)
	}

	if ok {
		return exp, nil
	}

	return now.Add(max(a.cfg.TTL, revokeCeiling)), nil
}

// claimIssuedAt reads the iat instant the sub-denial compares against; a
// token with no iat (RequireExp/RequireJTI gate the others, iat is
// optional) yields the zero time, which a revoked-at instant always denies.
func claimIssuedAt(claims *Claims) time.Time {
	if claims.IssuedAt == nil {
		return time.Time{}
	}

	return claims.IssuedAt.Time
}
