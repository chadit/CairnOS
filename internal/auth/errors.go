package auth

import "errors"

// Sentinel errors for the auth package. Login failures deliberately share
// one user-facing message at the API layer; these sentinels exist so tests
// and logs can still tell the cases apart.
var (
	// ErrKeyMalformed reports a plaintext access key that does not parse as
	// prefix_id_secret.
	ErrKeyMalformed = errors.New("malformed access key")

	// ErrKeyNotFound reports an access key id with no stored row.
	ErrKeyNotFound = errors.New("access key not found")

	// ErrKeyDisabled reports a login attempt with a disabled access key.
	ErrKeyDisabled = errors.New("access key disabled")

	// ErrKeyMismatch reports a key whose secret does not match the stored
	// argon2id hash.
	ErrKeyMismatch = errors.New("access key does not match stored hash")

	// ErrHashMalformed reports a stored hash that does not parse as a PHC
	// argon2id string.
	ErrHashMalformed = errors.New("malformed argon2id hash")

	// ErrTokenInvalid reports a token that failed verification for any
	// reason other than expiry or revocation: bad signature, wrong
	// algorithm, wrong issuer or audience, or missing required claims.
	ErrTokenInvalid = errors.New("token invalid")

	// ErrJTIMissing reports a token with no jti claim while require_jti is
	// on. It wraps alongside ErrTokenInvalid so the API layer still answers
	// one code while callers and tests can name the claim that was absent.
	ErrJTIMissing = errors.New("token is missing the jti claim")

	// ErrSubjectMissing reports a token with no sub claim, which leaves the
	// request with no principal to authorize. It wraps alongside
	// ErrTokenInvalid for the same reason as ErrJTIMissing.
	ErrSubjectMissing = errors.New("token is missing the sub claim")

	// ErrTokenExpired reports a token past its exp; it maps to the
	// canonical auth.token_expired code at the API layer.
	ErrTokenExpired = errors.New("token expired")

	// ErrTokenRevoked reports a token blocked by the denylist, by jti or by
	// the key (sub) it was minted from.
	ErrTokenRevoked = errors.New("token revoked")

	// ErrScopeUnknown reports a scope name outside the known scopes.
	ErrScopeUnknown = errors.New("unknown scope")

	// ErrSecretTooShort reports an HS256 secret under the 32-byte floor; a
	// short HMAC secret undermines the whole token scheme.
	ErrSecretTooShort = errors.New("HS256 secret must be at least 32 bytes")

	// ErrSecretMissing reports a missing HS256 secret environment variable.
	ErrSecretMissing = errors.New("HS256 secret is not set")

	// ErrAlgorithmUnknown reports a signing_algorithm outside HS256|EdDSA.
	ErrAlgorithmUnknown = errors.New("unknown signing algorithm")

	// ErrTooManyLogins reports a Login refused because the credential
	// derivation pool is full or the per-peer rate limit tripped. It maps to
	// the auth.too_many_logins code (RESOURCE_EXHAUSTED) at the API layer so
	// an unauthenticated flood cannot pin memory in argon2id work.
	ErrTooManyLogins = errors.New("too many concurrent or rapid logins")

	// ErrKeyFileMissing reports an unset EdDSA key file environment
	// variable.
	ErrKeyFileMissing = errors.New("EdDSA key file is not set")

	// ErrKeyFileInvalid reports an EdDSA key file that does not parse as a
	// PEM-encoded ed25519 key.
	ErrKeyFileInvalid = errors.New("EdDSA key file is not a PEM-encoded ed25519 key")

	// ErrKeyPairMismatch reports an EdDSA public key that is not the mate of
	// the configured private key; this node would sign tokens its own
	// verifier rejects.
	ErrKeyPairMismatch = errors.New("EdDSA public key does not match the private key")

	// ErrKeyFilePermissive reports an EdDSA private key file readable by
	// group or other; a world-readable signing key is a leak risk.
	ErrKeyFilePermissive = errors.New("EdDSA private key file is group- or world-readable")
)
