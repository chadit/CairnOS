package oauth_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2"

	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/oauth"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/tenancy"
)

// Fixtures shared across the OAuth handler tests; goconst applies to tests in
// this repo's lint policy.
const (
	testIssuer   = "https://idp.test"
	testClientID = "cairnos-spa"
	testSubject  = "alice@idp.test"
	testCode     = "auth-code-123"
	defaultWS    = "default"
	defaultRole  = "viewer"
	idTokenExtra = "id_token"
	rsaKeyBits   = 2048
	stateParam   = "state"
	tokenTTL     = time.Hour
)

// fakeIDP is a minimal OIDC identity provider for the handler tests: an RSA
// key that signs ID tokens, and a go-oidc verifier built from the matching
// public key through a static key set. The static key set is the JWKS the spec
// calls for, verified by go-oidc the same way a live JWKS would be, without an
// HTTP round trip.
type fakeIDP struct {
	key      *rsa.PrivateKey
	verifier *oidc.IDTokenVerifier
}

// newFakeIDP builds a provider with a fresh RSA key and a verifier that pins
// the issuer and audience the handler expects.
func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, rsaKeyBits)
	if err != nil {
		t.Fatalf("generate idp key: %v", err)
	}

	keySet := &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}}
	verifier := oidc.NewVerifier(testIssuer, keySet, &oidc.Config{
		ClientID:             testClientID,
		SupportedSigningAlgs: []string{oidc.RS256},
	})

	return &fakeIDP{key: key, verifier: verifier}
}

// signIDToken mints an RS256-signed ID token for the test subject, the way the
// IdP would on a successful login, carrying no email claim.
func (m *fakeIDP) signIDToken(t *testing.T) string {
	t.Helper()

	return m.signIDTokenWithEmail(t, testSubject, "", false)
}

// signIDTokenWithEmail mints an ID token carrying a verified or unverified email
// claim, so the auto-provision allowlist tests drive the domain gate off a
// signed claim.
func (m *fakeIDP) signIDTokenWithEmail(t *testing.T, subject, email string, emailVerified bool) string {
	t.Helper()

	now := time.Now()
	claims := jwt.MapClaims{
		"iss": testIssuer,
		"aud": testClientID,
		"sub": subject,
		"iat": now.Unix(),
		"exp": now.Add(tokenTTL).Unix(),
	}

	if email != "" {
		claims["email"] = email
		claims["email_verified"] = emailVerified
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(m.key)
	if err != nil {
		t.Fatalf("sign id token: %v", err)
	}

	return signed
}

// fakeExchanger stands in for *oauth2.Config so the callback runs without a
// live IdP token endpoint. AuthCodeURL delegates to a real oauth2.Config, which
// encodes state, the client id, and the S256 PKCE challenge the way the IdP
// would see them; Exchange returns a token carrying a pre-signed ID token and
// records the PKCE verifier the handler replayed.
type fakeExchanger struct {
	config     oauth2.Config
	idToken    string
	exchangeOK bool

	gotVerifier string
}

// newFakeExchanger wires a real oauth2.Config with the test client id and
// authorization endpoint, so AuthCodeURL produces a genuine authorization URL.
func newFakeExchanger(idToken string) *fakeExchanger {
	return &fakeExchanger{
		config: oauth2.Config{
			ClientID: testClientID,
			Endpoint: oauth2.Endpoint{AuthURL: testIssuer + "/authorize"},
		},
		idToken:    idToken,
		exchangeOK: true,
	}
}

func (f *fakeExchanger) AuthCodeURL(state string, opts ...oauth2.AuthCodeOption) string {
	return f.config.AuthCodeURL(state, opts...)
}

func (f *fakeExchanger) Exchange(_ context.Context, code string, opts ...oauth2.AuthCodeOption) (*oauth2.Token, error) {
	if !f.exchangeOK || code != testCode {
		return nil, errors.New("exchange rejected the code")
	}

	// The verifier option sets code_verifier on the params, so round-tripping
	// it through a real AuthCodeURL recovers the verifier the handler replayed,
	// proving PKCE flows through the exchange. The option type is opaque, so
	// this probe is the supported way to read it back.
	probe, err := url.Parse(f.config.AuthCodeURL("probe", opts...))
	if err != nil {
		return nil, err //nolint:wrapcheck // test helper; the parse never fails on a config-built URL
	}

	f.gotVerifier = probe.Query().Get("code_verifier")

	token := &oauth2.Token{AccessToken: "idp-access-token"}

	return token.WithExtra(map[string]any{idTokenExtra: f.idToken}), nil
}

// stubMinter mints a fixed token and records the subject, workspace, and
// scopes it was asked to mint for, so the callback assertion checks the
// CairnOS token carries the resolved workspace and the role's scopes.
type stubMinter struct {
	gotSubject   string
	gotWorkspace string
	gotScopes    []string
}

func (s *stubMinter) MintForSubject(_ context.Context, subject, workspace string, scopes []string) (string, time.Time, error) {
	s.gotSubject = subject
	s.gotWorkspace = workspace
	s.gotScopes = scopes

	return "cairnos-token-for-" + subject, time.Now().Add(tokenTTL), nil
}

// newTenancyStore opens a fresh sqlite tenancy store seeded with the default
// workspace, so a first login has somewhere to provision into.
func newTenancyStore(t *testing.T) tenancy.Store {
	t.Helper()

	tenants, err := tenancy.OpenSQLite(t.TempDir() + "/tenancy.db")
	if err != nil {
		t.Fatalf("open tenancy store: %v", err)
	}

	t.Cleanup(func() { _ = tenants.Close() })

	return tenants
}

// newHandler wires a handler over the mock IdP, fake exchanger, stub minter,
// and a real sqlite tenancy store, with auto-provisioning on and unrestricted,
// the setup the provisioning happy-path tests assume.
func newHandler(t *testing.T, idp *fakeIDP, exchanger oauth.TokenExchanger, minter oauth.TokenMinter, members tenancy.Store) *oauth.Handler {
	t.Helper()

	return newHandlerWithPolicy(t, idp, exchanger, minter, members, oauth.ProvisionPolicy{Enabled: true})
}

// newHandlerWithPolicy wires a handler with an explicit auto-provision policy,
// so the H1 tests can prove provisioning is refused or gated.
func newHandlerWithPolicy(t *testing.T, idp *fakeIDP, exchanger oauth.TokenExchanger, minter oauth.TokenMinter, members tenancy.Store, policy oauth.ProvisionPolicy) *oauth.Handler {
	t.Helper()

	return oauth.New(&oauth.Config{
		Verifier:         idp.verifier,
		Exchanger:        exchanger,
		Minter:           minter,
		Members:          members,
		DefaultWorkspace: defaultWS,
		DefaultRole:      defaultRole,
		Provision:        policy,
	})
}

// TestLoginRedirectCarriesStateAndPKCE proves the login route redirects to the
// IdP authorization URL carrying state, the client id, and a PKCE challenge.
func TestLoginRedirectCarriesStateAndPKCE(t *testing.T) {
	t.Parallel()

	idp := newFakeIDP(t)
	exchanger := newFakeExchanger("")
	handler := newHandler(t, idp, exchanger, &stubMinter{}, newTenancyStore(t))

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, oauth.LoginPath, http.NoBody)

	handler.Routes()["GET "+oauth.LoginPath](rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("login status = %d, want %d", rec.Code, http.StatusFound)
	}

	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}

	query := location.Query()

	if query.Get(stateParam) == "" {
		t.Error("redirect carried no state")
	}

	if query.Get("client_id") != testClientID {
		t.Errorf("redirect client_id = %q, want %q", query.Get("client_id"), testClientID)
	}

	if query.Get("code_challenge") == "" {
		t.Error("redirect carried no PKCE code_challenge")
	}

	if query.Get("code_challenge_method") != "S256" {
		t.Errorf("PKCE method = %q, want S256", query.Get("code_challenge_method"))
	}
}

// TestCallbackRejectsBadState proves the callback refuses a state value it
// never issued, the CSRF and replay defense.
func TestCallbackRejectsBadState(t *testing.T) {
	t.Parallel()

	idp := newFakeIDP(t)
	exchanger := newFakeExchanger("")
	handler := newHandler(t, idp, exchanger, &stubMinter{}, newTenancyStore(t))

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		oauth.CallbackPath+"?state=forged&code="+testCode, http.NoBody)

	handler.Routes()["GET "+oauth.CallbackPath](rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("forged-state callback status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// TestCallbackMintsTokenAndProvisions is the happy path: a started login's
// callback exchanges the code, verifies the ID token, auto-provisions the
// first-time subject into the default workspace, and mints a CairnOS token
// carrying that workspace and the default role's scopes.
func TestCallbackMintsTokenAndProvisions(t *testing.T) {
	t.Parallel()

	idp := newFakeIDP(t)
	exchanger := newFakeExchanger(idp.signIDToken(t))
	minter := &stubMinter{}
	members := newTenancyStore(t)
	handler := newHandler(t, idp, exchanger, minter, members)

	state := startLogin(t, handler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		oauth.CallbackPath+"?state="+url.QueryEscape(state)+"&code="+testCode, http.NoBody)

	handler.Routes()["GET "+oauth.CallbackPath](rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("callback status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode token response: %v", err)
	}

	if body["token"] != "cairnos-token-for-"+testSubject {
		t.Errorf("response token = %v, want the minted token", body["token"])
	}

	if body["workspace"] != defaultWS {
		t.Errorf("response workspace = %v, want %q", body["workspace"], defaultWS)
	}

	// The minter saw the resolved workspace and the viewer scopes.
	if minter.gotWorkspace != defaultWS {
		t.Errorf("minted workspace = %q, want %q", minter.gotWorkspace, defaultWS)
	}

	wantScopes, _ := tenancy.ScopesForRole(defaultRole)
	if !slices.Equal(minter.gotScopes, wantScopes) {
		t.Errorf("minted scopes = %v, want the viewer scopes %v", minter.gotScopes, wantScopes)
	}

	// PKCE: the verifier the exchange replayed is non-empty, so the challenge
	// minted at login bound to the exchange.
	if exchanger.gotVerifier == "" {
		t.Error("exchange ran without a PKCE verifier")
	}

	// First login auto-provisioned the subject as a member.
	role, err := members.GetMemberRole(t.Context(), defaultWS, testSubject)
	if err != nil {
		t.Fatalf("subject was not provisioned: %v", err)
	}

	if role != defaultRole {
		t.Errorf("provisioned role = %q, want %q", role, defaultRole)
	}
}

// TestCallbackKeepsExistingMembership proves a returning user keeps their
// stored role rather than being re-provisioned as a viewer: an editor stays an
// editor and the minted token carries the editor scopes.
func TestCallbackKeepsExistingMembership(t *testing.T) {
	t.Parallel()

	idp := newFakeIDP(t)
	exchanger := newFakeExchanger(idp.signIDToken(t))
	minter := &stubMinter{}
	members := newTenancyStore(t)

	if err := members.AddMember(t.Context(), defaultWS, testSubject, tenancy.RoleEditor); err != nil {
		t.Fatalf("seed editor membership: %v", err)
	}

	handler := newHandler(t, idp, exchanger, minter, members)
	state := startLogin(t, handler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		oauth.CallbackPath+"?state="+url.QueryEscape(state)+"&code="+testCode, http.NoBody)

	handler.Routes()["GET "+oauth.CallbackPath](rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("callback status = %d, want %d", rec.Code, http.StatusOK)
	}

	wantScopes, _ := tenancy.ScopesForRole(tenancy.RoleEditor)
	if !slices.Equal(minter.gotScopes, wantScopes) {
		t.Errorf("returning editor minted scopes = %v, want the editor scopes %v", minter.gotScopes, wantScopes)
	}
}

// startLogin runs the login route and returns the state it minted, so a
// callback test can present a state the handler will accept.
func startLogin(t *testing.T, handler *oauth.Handler) string {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, oauth.LoginPath, http.NoBody)

	handler.Routes()["GET "+oauth.LoginPath](rec, req)

	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse login redirect: %v", err)
	}

	state := location.Query().Get(stateParam)
	if state == "" {
		t.Fatal("login minted no state")
	}

	return state
}

// TestMintForSubjectShape pins the auth minting the handler relies on: a token
// minted for an OAuth subject verifies under the same authenticator and
// carries the subject, workspace, and scopes, so the gRPC interceptor accepts
// it like an access-key token.
func TestMintForSubjectShape(t *testing.T) {
	t.Parallel()

	authn := newTestAuthenticator(t)

	wantScopes := []string{auth.ScopeReadRun, auth.ScopeReadMemory}

	token, _, err := authn.MintForSubject(t.Context(), testSubject, "acme", wantScopes)
	if err != nil {
		t.Fatalf("MintForSubject: %v", err)
	}

	claims, err := authn.Verify(t.Context(), token)
	if err != nil {
		t.Fatalf("Verify minted token: %v", err)
	}

	if claims.Subject != testSubject {
		t.Errorf("token subject = %q, want %q", claims.Subject, testSubject)
	}

	if claims.Workspace != "acme" {
		t.Errorf("token workspace = %q, want acme", claims.Workspace)
	}

	if !slices.Equal(claims.Scopes, wantScopes) {
		t.Errorf("token scopes = %v, want %v", claims.Scopes, wantScopes)
	}

	// Sanity: the workspace claim lands on the request context the way the
	// interceptor would lift it.
	ctx := store.ContextWithWorkspace(t.Context(), claims.Workspace)
	if got := store.WorkspaceFromContext(ctx); got != "acme" {
		t.Errorf("context workspace = %q, want acme", got)
	}

	if strings.Contains(token, testSubject) {
		t.Error("token is not opaque; it embeds the subject in plaintext")
	}
}

// newTestAuthenticator wires an HS256 authenticator over a sqlite auth store,
// the same construction the API uses, so MintForSubject and Verify share one
// signing key.
func newTestAuthenticator(t *testing.T) *auth.Authenticator {
	t.Helper()

	authStore, err := auth.OpenSQLite(t.TempDir() + "/auth.db")
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}

	t.Cleanup(func() { _ = authStore.Close() })

	material, err := auth.LoadMaterial(auth.AlgHS256, func(string) string {
		return "0123456789abcdef0123456789abcdef"
	})
	if err != nil {
		t.Fatalf("load material: %v", err)
	}

	return auth.New(authStore, material, auth.Config{
		Issuer:     "cairnos-test",
		Audience:   "cairnos",
		TTL:        tokenTTL,
		RequireExp: true,
		RequireJTI: true,
	})
}
