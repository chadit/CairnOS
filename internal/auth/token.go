package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/oklog/ulid/v2"
)

// Claims is the CairnOS token payload (.agents/spec/08-auth-and-access.md,
// "Tokens (JWT)"): sub is the access key id, and scopes ride alongside the
// registered claims. Workspace names the tenant the token acts in
// (.agents/spec/12-ui-and-governance.md, "Multi-tenancy"); the interceptor
// lifts it onto the request context so every store query filters by it. Empty
// means the default workspace, so tokens minted before tenancy still work.
type Claims struct {
	jwt.RegisteredClaims

	Workspace string   `json:"workspace,omitempty"`
	Scopes    []string `json:"scopes"`
}

// issue mints one token for a verified, enabled key: sub is the key id,
// scopes are the key's grant, the workspace is the key's. The jti and exp are
// recorded before the token leaves so a later RevokeToken denies it until its
// real exp even after access_token_ttl drops (spec M2).
func (a *Authenticator) issue(ctx context.Context, key *Key) (string, time.Time, error) {
	return a.mint(ctx, key.ID, keyWorkspace(key), key.Scopes)
}

// MintForSubject issues a CairnOS token for a subject the OAuth login resolved
// (.agents/spec/12-ui-and-governance.md, "Multi-tenancy"): the IdP subject
// becomes sub, and workspace and scopes come from the resolved membership role.
// The token has the same shape as an access-key token, so the interceptor
// verifies it the same way and the denylist can revoke it. Callers must resolve
// membership first.
func (a *Authenticator) MintForSubject(ctx context.Context, subject, workspace string, scopes []string) (string, time.Time, error) {
	return a.mint(ctx, subject, workspace, scopes)
}

// mint signs one token for the given subject, workspace, and scopes: jti is a
// fresh ULID so the denylist can name this exact token, exp is now+TTL, and
// the jti/sub/exp are recorded before the token leaves so revocation tracks
// its real life (spec M2).
func (a *Authenticator) mint(ctx context.Context, subject, workspace string, scopes []string) (string, time.Time, error) {
	now := a.cfg.Now()
	expiresAt := now.Add(a.cfg.TTL)
	jti := ulid.Make().String()

	claims := &Claims{
		Scopes:    scopes,
		Workspace: workspace,
		Subject:   subject,
		ID:        jti,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(expiresAt),
		Issuer:    a.cfg.Issuer,
		Audience:  jwt.ClaimStrings{a.cfg.Audience},
	}

	signed, err := jwt.NewWithClaims(a.material.signingMethod(), claims).SignedString(a.material.signingKey())
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign token: %w", err)
	}

	if err := a.store.RecordIssued(ctx, jti, subject, expiresAt); err != nil {
		return "", time.Time{}, fmt.Errorf("record issued token: %w", err)
	}

	return signed, expiresAt, nil
}

// parse verifies a token's signature and claims without touching the
// denylist. The algorithm is pinned to the configured one twice, through
// WithValidMethods and again inside the keyfunc, so a token can never
// select its own verification path (the classic JWT algorithm-confusion
// defense): an HS256 token signed with the public key bytes is rejected
// under EdDSA, and vice versa.
func (a *Authenticator) parse(token string) (*Claims, error) {
	options := []jwt.ParserOption{
		jwt.WithValidMethods([]string{a.material.Alg()}),
		jwt.WithIssuer(a.cfg.Issuer),
		jwt.WithAudience(a.cfg.Audience),
		jwt.WithIssuedAt(),
		jwt.WithTimeFunc(a.cfg.Now),
	}

	// Leeway absorbs small clock differences between a cross-instance issuer
	// and this verifier, so a token whose iat or nbf sits a few seconds in
	// the future still passes (spec L2). Zero skew keeps the strict behavior.
	if a.cfg.ClockSkew > 0 {
		options = append(options, jwt.WithLeeway(a.cfg.ClockSkew))
	}

	if a.cfg.RequireExp {
		options = append(options, jwt.WithExpirationRequired())
	}

	claims := &Claims{}

	if _, err := jwt.ParseWithClaims(token, claims, a.keyfunc, options...); err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, fmt.Errorf("%w: %w", ErrTokenExpired, err)
		}

		return nil, fmt.Errorf("%w: %w", ErrTokenInvalid, err)
	}

	if a.cfg.RequireJTI && claims.ID == "" {
		return nil, fmt.Errorf("%w: %w", ErrTokenInvalid, ErrJTIMissing)
	}

	if claims.Subject == "" {
		return nil, fmt.Errorf("%w: %w", ErrTokenInvalid, ErrSubjectMissing)
	}

	return claims, nil
}

// keyfunc hands the parser the verification key, after re-checking that
// the token's header names exactly the configured algorithm.
func (a *Authenticator) keyfunc(token *jwt.Token) (any, error) {
	if token.Method.Alg() != a.material.Alg() {
		return nil, fmt.Errorf("%w: token alg %q, configured %q", ErrTokenInvalid, token.Method.Alg(), a.material.Alg())
	}

	return a.material.verifyKey(), nil
}
