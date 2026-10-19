// Token tests: the login exchange, the verify path with every rejection
// the spec demands (expiry, missing claims, wrong algorithm including the
// classic confusion tricks, garbage), revocation, and both signing tiers.
// Expiry tests advance an injected clock; nothing here sleeps.
package auth_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/auth/authtest"
)

// Environment variable names mirrored from the package under test.
const (
	secretEnvName      = "CAIRNOS_AUTH_HS256_SECRET" //nolint:gosec // environment variable name, not a credential
	privateKeyFileName = "CAIRNOS_AUTH_PRIVATE_KEY_FILE"
	publicKeyFileName  = "CAIRNOS_AUTH_PUBLIC_KEY_FILE"
)

// testSecret is exactly the 32-byte floor, proving the boundary passes.
const testSecret = "0123456789abcdef0123456789abcdef"

// Issuer and audience every test authenticator shares.
const (
	testIssuer   = "cairnos-test"
	testAudience = "cairnos"
)

// testTTL keeps expiry math obvious: one hour, like the spec default.
const testTTL = time.Hour

// fakeClock is an advanceable clock injected as Config.Now.
type fakeClock struct {
	now time.Time
	mu  sync.Mutex
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: baseTime}
}

// Now implements the Config.Now contract.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

// Advance moves the clock forward.
func (c *fakeClock) Advance(step time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(step)
}

// fakeEnv builds a getenv over a fixed map, so material loading needs no
// process environment and tests stay parallel-safe.
func fakeEnv(values map[string]string) func(string) string {
	return func(key string) string {
		return values[key]
	}
}

// openTestStore opens an auth store on a throwaway database.
func openTestStore(t *testing.T) *auth.SQLite {
	t.Helper()

	store, err := auth.OpenSQLite(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}

	t.Cleanup(func() { _ = store.Close() })

	return store
}

// testConfig is the secure-default Config every test starts from.
func testConfig(clock *fakeClock) auth.Config {
	return auth.Config{
		Issuer:     testIssuer,
		Audience:   testAudience,
		TTL:        testTTL,
		RequireExp: true,
		RequireJTI: true,
		Now:        clock.Now,
		HashCost:   authtest.FloorHashCost(),
	}
}

// newHS256Authenticator builds the standard test authenticator: sqlite
// store, HS256 material from a fake environment, secure defaults.
func newHS256Authenticator(t *testing.T, clock *fakeClock) (*auth.Authenticator, *auth.SQLite) {
	t.Helper()

	store := openTestStore(t)

	material, err := auth.LoadMaterial(auth.AlgHS256, fakeEnv(map[string]string{secretEnvName: testSecret}))
	if err != nil {
		t.Fatalf("load HS256 material: %v", err)
	}

	return auth.New(store, material, testConfig(clock)), store
}

// seedKey mints and stores one enabled key, returning it with its
// plaintext.
func seedKey(t *testing.T, store *auth.SQLite, scopes []string) (*auth.Key, string) {
	t.Helper()

	key, plaintext, err := auth.NewKey(testLabel, scopes, "", baseTime, authtest.FloorHashCost())
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}

	if err := store.CreateKey(t.Context(), key); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	return key, plaintext
}

// registeredClaims builds the valid claim set the crafting tests mutate.
func registeredClaims(clock *fakeClock) jwt.RegisteredClaims {
	return jwt.RegisteredClaims{
		Subject:   "some-key-id",
		ID:        "some-jti",
		IssuedAt:  jwt.NewNumericDate(clock.Now()),
		ExpiresAt: jwt.NewNumericDate(clock.Now().Add(testTTL)),
		Issuer:    testIssuer,
		Audience:  jwt.ClaimStrings{testAudience},
	}
}

// craftHS256 signs arbitrary claims with the shared test secret, standing
// in for an attacker who somehow learned the secret but sends bad claims,
// or for older tokens minted under different rules.
func craftHS256(t *testing.T, claims jwt.Claims) string {
	t.Helper()

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("craft HS256 token: %v", err)
	}

	return signed
}

func TestLoginIssuesVerifiableToken(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	authn, store := newHS256Authenticator(t, clock)
	key, plaintext := seedKey(t, store, []string{auth.ScopeReadRun, auth.ScopeWriteWorkflow})

	token, expiresAt, err := authn.Login(t.Context(), plaintext)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if want := clock.Now().Add(testTTL); !expiresAt.Equal(want) {
		t.Fatalf("expires_at = %v, want %v", expiresAt, want)
	}

	claims, err := authn.Verify(t.Context(), token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if claims.Subject != key.ID {
		t.Fatalf("sub = %q, want the key id %q", claims.Subject, key.ID)
	}

	if claims.ID == "" {
		t.Fatal("token carries no jti")
	}

	if !auth.HasScope(claims.Scopes, auth.ScopeReadRun) || auth.HasScope(claims.Scopes, auth.ScopeAdmin) {
		t.Fatalf("scopes = %v, want exactly the key's grant", claims.Scopes)
	}
}

func TestLoginRejectsBadCredentials(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	authn, store := newHS256Authenticator(t, clock)
	key, plaintext := seedKey(t, store, []string{auth.ScopeReadRun})

	disabledKey, disabledPlaintext := seedKey(t, store, []string{auth.ScopeReadRun})
	if err := store.SetKeyEnabled(t.Context(), disabledKey.ID, false); err != nil {
		t.Fatalf("disable key: %v", err)
	}

	keyID, _, err := auth.SplitKey(plaintext)
	if err != nil {
		t.Fatalf("SplitKey: %v", err)
	}

	cases := []struct {
		wantErr   error
		name      string
		plaintext string
	}{
		{name: "malformed", plaintext: "garbage", wantErr: auth.ErrKeyMalformed},
		{name: "unknown key id", plaintext: "ck_01UNKNOWNID_c2VjcmV0", wantErr: auth.ErrKeyNotFound},
		{name: "wrong secret", plaintext: "ck_" + keyID + "_d3JvbmdzZWNyZXQ", wantErr: auth.ErrKeyMismatch},
		{name: "disabled key", plaintext: disabledPlaintext, wantErr: auth.ErrKeyDisabled},
	}

	_ = key

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, _, err := authn.Login(t.Context(), tt.plaintext); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Login error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestVerifyRejectsExpiredToken(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	authn, store := newHS256Authenticator(t, clock)
	_, plaintext := seedKey(t, store, []string{auth.ScopeReadRun})

	token, _, err := authn.Login(t.Context(), plaintext)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	clock.Advance(testTTL + time.Minute)

	if _, err := authn.Verify(t.Context(), token); !errors.Is(err, auth.ErrTokenExpired) {
		t.Fatalf("Verify error = %v, want ErrTokenExpired", err)
	}
}

func TestVerifyRejectsBadTokens(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	authn, _ := newHS256Authenticator(t, clock)

	noJTI := registeredClaims(clock)
	noJTI.ID = ""

	noExp := registeredClaims(clock)
	noExp.ExpiresAt = nil

	noSub := registeredClaims(clock)
	noSub.Subject = ""

	wrongIssuer := registeredClaims(clock)
	wrongIssuer.Issuer = "someone-else"

	wrongAudience := registeredClaims(clock)
	wrongAudience.Audience = jwt.ClaimStrings{"not-us"}

	noneToken, err := jwt.NewWithClaims(jwt.SigningMethodNone, registeredClaims(clock)).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("craft none token: %v", err)
	}

	// An EdDSA-signed token presented to the HS256 verifier: a valid
	// signature under the wrong algorithm must be rejected by the
	// configured-algorithm pin, never verified opportunistically.
	_, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}

	edToken, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, registeredClaims(clock)).SignedString(edPriv)
	if err != nil {
		t.Fatalf("craft EdDSA token: %v", err)
	}

	// The unmutated claim set verifies, so every rejection below is caused by
	// the one field that case broke and cannot be a signature or baseline
	// failure wearing the same sentinel.
	if _, err := authn.Verify(t.Context(), craftHS256(t, registeredClaims(clock))); err != nil {
		t.Fatalf("baseline crafted token must verify, got %v", err)
	}

	// cause is the specific reason the rejection must carry, so a case cannot
	// pass on somebody else's failure. The parser supplies it for the claims
	// the jwt library validates, auth's own sentinels for the rest.
	cases := []struct {
		cause error
		name  string
		token string
	}{
		{name: "not a token", token: "not-a-token", cause: jwt.ErrTokenMalformed},
		{name: caseEmpty, token: "", cause: jwt.ErrTokenMalformed},
		{name: "alg none", token: noneToken, cause: jwt.ErrTokenSignatureInvalid},
		{name: "alg EdDSA under HS256 config", token: edToken, cause: jwt.ErrTokenSignatureInvalid},
		{name: "missing exp", token: craftHS256(t, noExp), cause: jwt.ErrTokenRequiredClaimMissing},
		{name: "wrong issuer", token: craftHS256(t, wrongIssuer), cause: jwt.ErrTokenInvalidIssuer},
		{name: "wrong audience", token: craftHS256(t, wrongAudience), cause: jwt.ErrTokenInvalidAudience},
		{name: "missing jti", token: craftHS256(t, noJTI), cause: auth.ErrJTIMissing},
		{name: "missing sub", token: craftHS256(t, noSub), cause: auth.ErrSubjectMissing},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := authn.Verify(t.Context(), tt.token)
			if !errors.Is(err, auth.ErrTokenInvalid) {
				t.Fatalf("Verify error = %v, want ErrTokenInvalid", err)
			}

			if !errors.Is(err, tt.cause) {
				t.Fatalf("Verify error = %v, want cause %v", err, tt.cause)
			}
		})
	}
}

// writeEdDSAKeyFiles generates an ed25519 pair and writes both PEM files,
// returning their paths.
func writeEdDSAKeyFiles(t *testing.T) (string, string) {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}

	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}

	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}

	dir := t.TempDir()
	privPath := filepath.Join(dir, "ed25519.key")
	pubPath := filepath.Join(dir, "ed25519.pub")

	writePEM(t, privPath, "PRIVATE KEY", privDER)
	writePEM(t, pubPath, "PUBLIC KEY", pubDER)

	return privPath, pubPath
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()

	data := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// newEdDSAAuthenticator builds an EdDSA-tier authenticator from generated
// PEM files, returning the public key PEM path too for the confusion test.
func newEdDSAAuthenticator(t *testing.T, clock *fakeClock) (*auth.Authenticator, *auth.SQLite, string) {
	t.Helper()

	privPath, pubPath := writeEdDSAKeyFiles(t)

	material, err := auth.LoadMaterial(auth.AlgEdDSA, fakeEnv(map[string]string{
		privateKeyFileName: privPath,
		publicKeyFileName:  pubPath,
	}))
	if err != nil {
		t.Fatalf("load EdDSA material: %v", err)
	}

	store := openTestStore(t)

	return auth.New(store, material, testConfig(clock)), store, pubPath
}

func TestEdDSAIssueAndVerify(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	authn, store, _ := newEdDSAAuthenticator(t, clock)
	key, plaintext := seedKey(t, store, []string{auth.ScopeAdmin})

	token, _, err := authn.Login(t.Context(), plaintext)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	claims, err := authn.Verify(t.Context(), token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if claims.Subject != key.ID {
		t.Fatalf("sub = %q, want %q", claims.Subject, key.ID)
	}
}

// TestEdDSARejectsHS256ConfusionToken is the classic downgrade attack: an
// attacker who holds the (distributable) public key signs an HS256 token
// using the public key PEM bytes as the HMAC secret, hoping the verifier
// follows the token's own alg header. The configured-algorithm pin must
// reject it.
func TestEdDSARejectsHS256ConfusionToken(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	authn, _, pubPath := newEdDSAAuthenticator(t, clock)

	pubPEM, err := os.ReadFile(pubPath) // #nosec G304 -- test-owned temp path
	if err != nil {
		t.Fatalf("read public key PEM: %v", err)
	}

	forged, err := jwt.NewWithClaims(jwt.SigningMethodHS256, registeredClaims(clock)).SignedString(pubPEM)
	if err != nil {
		t.Fatalf("craft confusion token: %v", err)
	}

	if _, err := authn.Verify(t.Context(), forged); !errors.Is(err, auth.ErrTokenInvalid) {
		t.Fatalf("Verify error = %v, want ErrTokenInvalid for the confusion token", err)
	}
}

// TestClockSkewLeewayAcceptsFutureIAT is the L2 fix: a cross-instance token
// whose iat sits a few seconds in the future (a peer whose clock runs ahead)
// verifies when clock_skew leeway is configured. The issuer signs with a
// clock 5s ahead; the verifier's clock is 5s behind but allows 30s of skew.
func TestClockSkewLeewayAcceptsFutureIAT(t *testing.T) {
	t.Parallel()

	const skew = 30 * time.Second

	privPath, pubPath := writeEdDSAKeyFiles(t)

	loadEd := func() *auth.Material {
		material, err := auth.LoadMaterial(auth.AlgEdDSA, fakeEnv(map[string]string{
			privateKeyFileName: privPath,
			publicKeyFileName:  pubPath,
		}))
		if err != nil {
			t.Fatalf("load EdDSA material: %v", err)
		}

		return material
	}

	issuerClock := newFakeClock()
	issuerClock.Advance(5 * time.Second)

	issuerStore := openTestStore(t)
	issuer := auth.New(issuerStore, loadEd(), testConfig(issuerClock))
	_, plaintext := seedKey(t, issuerStore, []string{auth.ScopeReadRun})

	token, _, err := issuer.Login(t.Context(), plaintext)
	if err != nil {
		t.Fatalf("issuer Login: %v", err)
	}

	verifierClock := newFakeClock()

	verifierCfg := testConfig(verifierClock)
	verifierCfg.ClockSkew = skew

	verifier := auth.New(openTestStore(t), loadEd(), verifierCfg)

	if _, err := verifier.Verify(t.Context(), token); err != nil {
		t.Fatalf("Verify a token whose iat is 5s ahead with %s leeway: %v", skew, err)
	}

	// Without leeway the same future-iat token is rejected, proving the
	// leeway is what let it through.
	strictCfg := testConfig(verifierClock)
	strict := auth.New(openTestStore(t), loadEd(), strictCfg)

	if _, err := strict.Verify(t.Context(), token); err == nil {
		t.Fatal("a future-iat token verified with no leeway; the skew test proves nothing")
	}
}

func TestRevokeTokenBlocksImmediately(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	authn, store := newHS256Authenticator(t, clock)
	_, plaintext := seedKey(t, store, []string{auth.ScopeReadRun})

	token, _, err := authn.Login(t.Context(), plaintext)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	claims, err := authn.Verify(t.Context(), token)
	if err != nil {
		t.Fatalf("Verify before revocation: %v", err)
	}

	if err := authn.RevokeToken(t.Context(), claims.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	if _, err := authn.Verify(t.Context(), token); !errors.Is(err, auth.ErrTokenRevoked) {
		t.Fatalf("Verify after revocation = %v, want ErrTokenRevoked", err)
	}
}

func TestRevokeKeyBlocksEveryToken(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	authn, store := newHS256Authenticator(t, clock)
	key, plaintext := seedKey(t, store, []string{auth.ScopeReadRun})

	first, _, err := authn.Login(t.Context(), plaintext)
	if err != nil {
		t.Fatalf("first Login: %v", err)
	}

	second, _, err := authn.Login(t.Context(), plaintext)
	if err != nil {
		t.Fatalf("second Login: %v", err)
	}

	if err := authn.RevokeKey(t.Context(), key.ID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}

	for _, token := range []string{first, second} {
		if _, err := authn.Verify(t.Context(), token); !errors.Is(err, auth.ErrTokenRevoked) {
			t.Fatalf("Verify after key revocation = %v, want ErrTokenRevoked", err)
		}
	}
}

// newAuthenticatorWithTTL builds an HS256 authenticator over one store whose
// only difference from the standard fixture is its token TTL, so the TTL-drop
// tests can mint long and then verify short.
func newAuthenticatorWithTTL(t *testing.T, store *auth.SQLite, clock *fakeClock, ttl time.Duration) *auth.Authenticator {
	t.Helper()

	material, err := auth.LoadMaterial(auth.AlgHS256, fakeEnv(map[string]string{secretEnvName: testSecret}))
	if err != nil {
		t.Fatalf("load HS256 material: %v", err)
	}

	cfg := testConfig(clock)
	cfg.TTL = ttl

	return auth.New(store, material, cfg)
}

// TestRevokeHonorsRealExpAfterTTLDrop is the M2 fix: a token minted under a
// 24h TTL must stay denied past the old 1h window once the operator drops
// access_token_ttl to 1h, because the recorded exp, not the live TTL, sets
// the deny window.
func TestRevokeHonorsRealExpAfterTTLDrop(t *testing.T) {
	t.Parallel()

	const longTTL = 24 * time.Hour

	clock := newFakeClock()
	store := openTestStore(t)
	_, plaintext := seedKey(t, store, []string{auth.ScopeReadRun})

	longLived := newAuthenticatorWithTTL(t, store, clock, longTTL)

	token, _, err := longLived.Login(t.Context(), plaintext)
	if err != nil {
		t.Fatalf("Login under 24h TTL: %v", err)
	}

	claims, err := longLived.Verify(t.Context(), token)
	if err != nil {
		t.Fatalf("Verify fresh token: %v", err)
	}

	// The operator lowers access_token_ttl to 1h and revokes through the
	// short-TTL authenticator over the same store.
	shortLived := newAuthenticatorWithTTL(t, store, clock, time.Hour)
	if err := shortLived.RevokeToken(t.Context(), claims.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	// Past the old 1h window but before the token's real 24h exp, it must
	// still be denied; the pre-fix code would have let it back in here.
	clock.Advance(time.Hour + time.Minute)

	if _, err := shortLived.Verify(t.Context(), token); !errors.Is(err, auth.ErrTokenRevoked) {
		t.Fatalf("Verify past old window = %v, want ErrTokenRevoked through the real exp", err)
	}
}

// TestRevokeKeyKeepsKeyMinting is the M3 fix: revoking by key kills the
// pre-revoke token but the still-enabled key mints a fresh, working token.
func TestRevokeKeyKeepsKeyMinting(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	authn, store := newHS256Authenticator(t, clock)
	key, plaintext := seedKey(t, store, []string{auth.ScopeReadRun})

	preRevoke, _, err := authn.Login(t.Context(), plaintext)
	if err != nil {
		t.Fatalf("pre-revoke Login: %v", err)
	}

	if revokeKeyErr := authn.RevokeKey(t.Context(), key.ID); revokeKeyErr != nil {
		t.Fatalf("RevokeKey: %v", revokeKeyErr)
	}

	// A fresh token minted after the revoke instant must work; the key was
	// never disabled.
	clock.Advance(time.Second)

	fresh, _, err := authn.Login(t.Context(), plaintext)
	if err != nil {
		t.Fatalf("post-revoke Login: %v", err)
	}

	if _, err := authn.Verify(t.Context(), fresh); err != nil {
		t.Fatalf("fresh token after key revocation = %v, want it to work", err)
	}

	if _, err := authn.Verify(t.Context(), preRevoke); !errors.Is(err, auth.ErrTokenRevoked) {
		t.Fatalf("pre-revoke token = %v, want ErrTokenRevoked", err)
	}
}

func TestDenylistPruneDropsExpiredRows(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	authn, store := newHS256Authenticator(t, clock)
	_, plaintext := seedKey(t, store, []string{auth.ScopeReadRun})

	token, _, err := authn.Login(t.Context(), plaintext)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	claims, err := authn.Verify(t.Context(), token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if revokeTokenErr := authn.RevokeToken(t.Context(), claims.ID); revokeTokenErr != nil {
		t.Fatalf("RevokeToken: %v", revokeTokenErr)
	}

	// Before the deny window passes, the row survives a prune.
	pruned, err := store.PruneDenylist(t.Context(), clock.Now())
	if err != nil {
		t.Fatalf("PruneDenylist: %v", err)
	}

	if pruned != 0 {
		t.Fatalf("prune removed %d live rows, want 0", pruned)
	}

	// Past the window the row goes; the token is dead by exp anyway.
	clock.Advance(testTTL + time.Minute)

	pruned, err = store.PruneDenylist(t.Context(), clock.Now())
	if err != nil {
		t.Fatalf("PruneDenylist after expiry: %v", err)
	}

	if pruned != 1 {
		t.Fatalf("prune removed %d rows, want 1", pruned)
	}
}

func TestLoadMaterialFailures(t *testing.T) {
	t.Parallel()

	privPath, pubPath := writeEdDSAKeyFiles(t)

	notPEM := filepath.Join(t.TempDir(), "junk.pem")
	if err := os.WriteFile(notPEM, []byte("no pem here"), 0o600); err != nil {
		t.Fatalf("write junk file: %v", err)
	}

	cases := []struct {
		wantErr error
		env     map[string]string
		name    string
		alg     string
	}{
		{name: "unknown algorithm", alg: "ES256", env: nil, wantErr: auth.ErrAlgorithmUnknown},
		{name: caseMissingSecret, alg: auth.AlgHS256, env: nil, wantErr: auth.ErrSecretMissing},
		{
			name:    "short secret",
			alg:     auth.AlgHS256,
			env:     map[string]string{secretEnvName: "tooshort"},
			wantErr: auth.ErrSecretTooShort,
		},
		{
			name:    "missing private key file",
			alg:     auth.AlgEdDSA,
			env:     map[string]string{publicKeyFileName: pubPath},
			wantErr: auth.ErrKeyFileMissing,
		},
		{
			name:    "missing public key file",
			alg:     auth.AlgEdDSA,
			env:     map[string]string{privateKeyFileName: privPath},
			wantErr: auth.ErrKeyFileMissing,
		},
		{
			name:    "not a pem file",
			alg:     auth.AlgEdDSA,
			env:     map[string]string{privateKeyFileName: notPEM, publicKeyFileName: pubPath},
			wantErr: auth.ErrKeyFileInvalid,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := auth.LoadMaterial(tt.alg, fakeEnv(tt.env)); !errors.Is(err, tt.wantErr) {
				t.Fatalf("LoadMaterial error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestLoadMaterialRejectsMismatchedKeyPair is the L5 fix: a public key from a
// different pair than the private key is refused, since this node would sign
// tokens its own verifier rejects.
func TestLoadMaterialRejectsMismatchedKeyPair(t *testing.T) {
	t.Parallel()

	privPath, _ := writeEdDSAKeyFiles(t)
	_, otherPubPath := writeEdDSAKeyFiles(t)

	_, err := auth.LoadMaterial(auth.AlgEdDSA, fakeEnv(map[string]string{
		privateKeyFileName: privPath,
		publicKeyFileName:  otherPubPath,
	}))
	if !errors.Is(err, auth.ErrKeyPairMismatch) {
		t.Fatalf("LoadMaterial error = %v, want ErrKeyPairMismatch", err)
	}
}

// TestLoadMaterialRejectsWorldReadablePrivateKey is the other half of L5: a
// group- or world-readable private key file is refused so a leaked-permission
// signing key never loads.
func TestLoadMaterialRejectsWorldReadablePrivateKey(t *testing.T) {
	t.Parallel()

	privPath, pubPath := writeEdDSAKeyFiles(t)

	// 0o644 is world- and group-readable; the loader must refuse it.
	if err := os.Chmod(privPath, 0o644); err != nil {
		t.Fatalf("chmod private key: %v", err)
	}

	_, err := auth.LoadMaterial(auth.AlgEdDSA, fakeEnv(map[string]string{
		privateKeyFileName: privPath,
		publicKeyFileName:  pubPath,
	}))
	if !errors.Is(err, auth.ErrKeyFilePermissive) {
		t.Fatalf("LoadMaterial error = %v, want ErrKeyFilePermissive", err)
	}
}

// floorCostParams is the PHC parameter field of a key minted at the floor cost.
const floorCostParams = "m=8,t=1,p=1"

// TestNewKeyStoresTheCostItWasGiven pins the zero cost to RFC 9106's defaults
// (64 MiB, one pass, four lanes), the cost every real key and the decoy pay,
// proves a chosen cost reaches the stored hash, and that a partial cost takes
// the default for each zero field. Each is read back from the PHC parameter
// field, where a key keeps its cost.
func TestNewKeyStoresTheCostItWasGiven(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		want string
		cost auth.HashCost
	}{
		{name: "zero cost is the production default", cost: auth.HashCost{}, want: "m=65536,t=1,p=4"},
		{name: "floor cost is stored as given", cost: authtest.FloorHashCost(), want: floorCostParams},
		{name: "zero passes and lanes take the production values", cost: auth.HashCost{MemoryKiB: 8}, want: "m=8,t=1,p=4"},
		{name: "zero memory takes the production value", cost: auth.HashCost{Time: 2, Threads: 1}, want: "m=65536,t=2,p=1"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			key, plaintext, err := auth.NewKey("cost", []string{auth.ScopeReadRun}, "", baseTime, tt.cost)
			if err != nil {
				t.Fatalf("NewKey: %v", err)
			}

			if got := phcParamField(t, key.Hash); got != tt.want {
				t.Fatalf("stored hash params %s, want %s", got, tt.want)
			}

			_, secret, err := auth.SplitKey(plaintext)
			if err != nil {
				t.Fatalf("SplitKey: %v", err)
			}

			if err := auth.VerifyKey(secret, key.Hash); err != nil {
				t.Fatalf("VerifyKey on the key's own secret: %v", err)
			}
		})
	}
}

// phcParamField returns the m=,t=,p= field of a stored PHC argon2id hash:
// $argon2id$v=19$m=...,t=...,p=...$salt$digest.
func phcParamField(t *testing.T, encoded string) string {
	t.Helper()

	fields := strings.Split(encoded, "$")

	const paramFieldIndex = 3
	if len(fields) <= paramFieldIndex {
		t.Fatalf("hash %q has too few PHC fields", encoded)
	}

	return fields[paramFieldIndex]
}
