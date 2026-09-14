package credentials_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/credentials"
	"github.com/chadit/CairnOS/internal/runner/builtins"
)

const (
	credName        = "saas"
	credAlpha       = "alpha"
	credBeta        = "beta"
	clientID        = "cid"
	clientSecretRef = "saas_client_secret"
	refreshTokenRef = "saas_refresh_token"
	mintedToken     = "minted-token"
	sentinelSecret  = "super-secret-client-value"
	sentinelRefresh = "super-secret-refresh-value"
)

// errSecretMissing lets the fake resolver report an unknown ref the way the real
// chain does, so New's wrapping is exercised.
var errSecretMissing = errors.New("secret missing")

// tokenResponse is a successful OAuth2 token body. A concrete type keeps the JSON
// encode safe (errchkjson) and the keys snake-case.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

// errorResponse is an RFC 6749 token error body. The echo server fills its
// description with whatever secret it received, standing in for a hostile
// endpoint.
type errorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// fakeResolver is an in-memory SecretResolver: a ref name maps to its value, and
// a miss is errSecretMissing.
type fakeResolver map[string]string

func (f fakeResolver) Get(_ context.Context, name string) (string, error) {
	value, ok := f[name]
	if !ok {
		return "", fmt.Errorf("%w: %s", errSecretMissing, name)
	}

	return value, nil
}

// directClient is an unguarded client for tests not exercising the egress guard;
// it reaches the loopback httptest servers directly.
func directClient(_ bool) *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
}

// guardedNewClient hands the manager the real shared egress guard, so a loopback
// token_url is refused at dial unless allowPrivate is set.
func guardedNewClient(allowPrivate bool) *http.Client {
	return builtins.GuardedHTTPClient(5*time.Second, allowPrivate)
}

// countingTransport counts every round trip so a test can prove the manager sent
// its token request through the injected client, not http.DefaultClient.
type countingTransport struct {
	base  http.RoundTripper
	count *atomic.Int64
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.count.Add(1)

	resp, err := c.base.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("counting transport: %w", err)
	}

	return resp, nil
}

// writeToken writes an OAuth2 token JSON body with the given expiry.
func writeToken(w http.ResponseWriter, expiresIn int) {
	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(tokenResponse{
		AccessToken: mintedToken,
		TokenType:   "Bearer",
		ExpiresIn:   expiresIn,
	}); err != nil {
		return
	}
}

// mintServer answers every request with a token of the given expiry and counts
// the requests it saw.
func mintServer(t *testing.T, expiresIn int) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	var count atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count.Add(1)
		writeToken(w, expiresIn)
	}))
	t.Cleanup(srv.Close)

	return srv, &count
}

// echoErrorServer reflects every secret it received back into a 401 error body,
// standing in for a broken or hostile token endpoint.
func echoErrorServer(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received := receivedSecrets(r)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(errorResponse{
			Error:            "invalid_client",
			ErrorDescription: "rejected credential " + received,
		})
	}))
	t.Cleanup(srv.Close)

	return srv
}

// receivedSecrets pulls every secret-shaped value out of a token request so the
// echo server can reflect it, whichever auth style the library chose.
func receivedSecrets(r *http.Request) string {
	var parts []string

	if _, secret, ok := r.BasicAuth(); ok && secret != "" {
		parts = append(parts, secret)
	}

	_ = r.ParseForm()

	for _, key := range []string{"client_secret", "refresh_token"} {
		if value := r.PostFormValue(key); value != "" {
			parts = append(parts, value)
		}
	}

	return strings.Join(parts, " ")
}

// clientCredsMap builds a single client_credentials credential map for the tests.
func clientCredsMap(tokenURL string, allowPrivate bool) map[string]config.Credential {
	return map[string]config.Credential{
		credName: {
			Grant:               config.GrantClientCredentials,
			TokenURL:            tokenURL,
			ClientID:            clientID,
			ClientSecretRef:     clientSecretRef,
			AllowPrivateTargets: allowPrivate,
		},
	}
}

// newManager builds a manager for creds or fails the test.
func newManager(
	t *testing.T,
	creds map[string]config.Credential,
	resolver credentials.SecretResolver,
	newClient func(allowPrivate bool) *http.Client,
) *credentials.Manager {
	t.Helper()

	manager, err := credentials.New(t.Context(), creds, resolver, newClient)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return manager
}

func TestClientCredentialsMintsAndCaches(t *testing.T) {
	t.Parallel()

	srv, count := mintServer(t, 3600)
	manager := newManager(t, clientCredsMap(srv.URL, false), fakeResolver{clientSecretRef: sentinelSecret}, directClient)

	for range 2 {
		token, err := manager.Token(t.Context(), credName)
		if err != nil {
			t.Fatalf("Token: %v", err)
		}

		if token != mintedToken {
			t.Fatalf("token = %q, want %q", token, mintedToken)
		}
	}

	if got := count.Load(); got != 1 {
		t.Fatalf("token endpoint hit %d times for two cached calls, want 1", got)
	}
}

func TestRefreshTokenGrantMints(t *testing.T) {
	t.Parallel()

	srv, count := mintServer(t, 3600)
	creds := map[string]config.Credential{
		credName: {
			Grant:           config.GrantRefreshToken,
			TokenURL:        srv.URL,
			ClientID:        clientID,
			RefreshTokenRef: refreshTokenRef,
		},
	}
	manager := newManager(t, creds, fakeResolver{refreshTokenRef: sentinelRefresh}, directClient)

	token, err := manager.Token(t.Context(), credName)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}

	if token != mintedToken {
		t.Fatalf("token = %q, want %q", token, mintedToken)
	}

	if got := count.Load(); got != 1 {
		t.Fatalf("token endpoint hit %d times, want 1", got)
	}
}

func TestStaleTokenRefreshesExactlyOncePerCall(t *testing.T) {
	t.Parallel()

	// expires_in below the library's expiry buffer makes each cached token stale,
	// so the second call is the single refresh the first mint did not cover.
	srv, count := mintServer(t, 1)
	manager := newManager(t, clientCredsMap(srv.URL, false), fakeResolver{clientSecretRef: sentinelSecret}, directClient)

	for range 2 {
		if _, err := manager.Token(t.Context(), credName); err != nil {
			t.Fatalf("Token: %v", err)
		}
	}

	if got := count.Load(); got != 2 {
		t.Fatalf("token endpoint hit %d times, want 2 (mint plus one refresh)", got)
	}
}

func TestTokenEndpointErrorDropsResponseBody(t *testing.T) {
	t.Parallel()

	srv := echoErrorServer(t)
	manager := newManager(t, clientCredsMap(srv.URL, false), fakeResolver{clientSecretRef: sentinelSecret}, directClient)

	_, err := manager.Token(t.Context(), credName)
	if !errors.Is(err, credentials.ErrTokenEndpoint) {
		t.Fatalf("error = %v, want ErrTokenEndpoint", err)
	}

	message := err.Error()
	if strings.Contains(message, sentinelSecret) {
		t.Fatalf("error leaked the client secret: %q", message)
	}

	if !strings.Contains(message, "401") {
		t.Fatalf("error should carry the status code: %q", message)
	}
}

func TestRefreshTokenEndpointErrorDropsResponseBody(t *testing.T) {
	t.Parallel()

	srv := echoErrorServer(t)
	creds := map[string]config.Credential{
		credName: {
			Grant:           config.GrantRefreshToken,
			TokenURL:        srv.URL,
			ClientID:        clientID,
			RefreshTokenRef: refreshTokenRef,
		},
	}
	manager := newManager(t, creds, fakeResolver{refreshTokenRef: sentinelRefresh}, directClient)

	_, err := manager.Token(t.Context(), credName)
	if err == nil {
		t.Fatal("Token to a refusing endpoint should error")
	}

	if message := err.Error(); strings.Contains(message, sentinelRefresh) {
		t.Fatalf("error leaked the refresh token: %q", message)
	}
}

func TestMissingSecretRefFailsNewNamingRef(t *testing.T) {
	t.Parallel()

	srv, _ := mintServer(t, 3600)

	_, err := credentials.New(t.Context(), clientCredsMap(srv.URL, false), fakeResolver{}, directClient)
	if err == nil {
		t.Fatal("New with an unresolvable ref should fail")
	}

	message := err.Error()
	if !strings.Contains(message, clientSecretRef) {
		t.Fatalf("error should name the ref, got %q", message)
	}

	if strings.Contains(message, sentinelSecret) {
		t.Fatalf("error leaked a secret value: %q", message)
	}
}

func TestTokenRequestsUseInjectedClient(t *testing.T) {
	t.Parallel()

	srv, _ := mintServer(t, 3600)

	var count atomic.Int64

	newClient := func(_ bool) *http.Client {
		return &http.Client{Transport: &countingTransport{base: http.DefaultTransport, count: &count}}
	}

	manager := newManager(t, clientCredsMap(srv.URL, false), fakeResolver{clientSecretRef: sentinelSecret}, newClient)

	if _, err := manager.Token(t.Context(), credName); err != nil {
		t.Fatalf("Token: %v", err)
	}

	if count.Load() == 0 {
		t.Fatal("token request did not ride the injected client")
	}
}

func TestGuardBlocksLoopbackUnlessAllowed(t *testing.T) {
	t.Parallel()

	srv, _ := mintServer(t, 3600)
	resolver := fakeResolver{clientSecretRef: sentinelSecret}

	blocked := newManager(t, clientCredsMap(srv.URL, false), resolver, guardedNewClient)
	if _, err := blocked.Token(t.Context(), credName); err == nil {
		t.Fatal("guarded client should refuse a loopback token_url without allow_private_targets")
	}

	allowed := newManager(t, clientCredsMap(srv.URL, true), resolver, guardedNewClient)

	token, err := allowed.Token(t.Context(), credName)
	if err != nil {
		t.Fatalf("allow_private_targets should reach the loopback endpoint: %v", err)
	}

	if token != mintedToken {
		t.Fatalf("token = %q, want %q", token, mintedToken)
	}
}

func TestCanceledCallerReturnsWithoutHittingEndpoint(t *testing.T) {
	t.Parallel()

	srv, count := mintServer(t, 3600)
	manager := newManager(t, clientCredsMap(srv.URL, false), fakeResolver{clientSecretRef: sentinelSecret}, directClient)

	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := manager.Token(canceled, credName); err == nil {
		t.Fatal("a canceled caller ctx should return an error")
	}

	if got := count.Load(); got != 0 {
		t.Fatalf("endpoint hit %d times despite a canceled ctx, want 0", got)
	}

	token, err := manager.Token(t.Context(), credName)
	if err != nil {
		t.Fatalf("a later fresh ctx should mint: %v", err)
	}

	if token != mintedToken {
		t.Fatalf("token = %q, want %q", token, mintedToken)
	}
}

func TestShortDeadlineStillYieldsTokenThenServesCache(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	entered := make(chan struct{}, 1)

	var count atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count.Add(1)

		select {
		case entered <- struct{}{}:
		default:
		}

		<-release
		writeToken(w, 3600)
	}))
	t.Cleanup(srv.Close)

	manager := newManager(t, clientCredsMap(srv.URL, false), fakeResolver{clientSecretRef: sentinelSecret}, directClient)

	short, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	type result struct {
		err   error
		token string
	}

	done := make(chan result, 1)

	go func() {
		token, err := manager.Token(short, credName)
		done <- result{token: token, err: err}
	}()

	<-entered      // the mint reached the endpoint, so Token passed its live-ctx entry check
	<-short.Done() // the caller deadline now expires while the mint is still gated
	close(release) // let the endpoint answer

	res := <-done
	if res.err != nil {
		t.Fatalf("a caller deadline shorter than the endpoint should still yield the token: %v", res.err)
	}

	if res.token != mintedToken {
		t.Fatalf("token = %q, want %q", res.token, mintedToken)
	}

	if _, err := manager.Token(t.Context(), credName); err != nil {
		t.Fatalf("second call: %v", err)
	}

	if got := count.Load(); got != 1 {
		t.Fatalf("endpoint hit %d times, want 1 (second call served from cache)", got)
	}
}

func TestUnknownCredential(t *testing.T) {
	t.Parallel()

	manager, err := credentials.New(t.Context(), map[string]config.Credential{}, fakeResolver{}, directClient)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if manager.Has("missing") {
		t.Fatal("Has reported an unconfigured credential")
	}

	if _, err := manager.Token(t.Context(), "missing"); !errors.Is(err, credentials.ErrUnknownCredential) {
		t.Fatalf("error = %v, want ErrUnknownCredential", err)
	}
}

func TestNamesSortedCopy(t *testing.T) {
	t.Parallel()

	srv, _ := mintServer(t, 3600)
	creds := map[string]config.Credential{
		credBeta:  {Grant: config.GrantClientCredentials, TokenURL: srv.URL, ClientID: clientID, ClientSecretRef: clientSecretRef},
		credAlpha: {Grant: config.GrantClientCredentials, TokenURL: srv.URL, ClientID: clientID, ClientSecretRef: clientSecretRef},
	}
	manager := newManager(t, creds, fakeResolver{clientSecretRef: sentinelSecret}, directClient)

	got := manager.Names()
	if want := []string{credAlpha, credBeta}; !slices.Equal(got, want) {
		t.Fatalf("Names = %v, want %v", got, want)
	}

	got[0] = "mutated"

	if again := manager.Names(); again[0] != credAlpha {
		t.Fatal("Names returned a slice aliasing internal state")
	}
}
