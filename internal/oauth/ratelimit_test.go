package oauth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/oauth"
)

// frozenClock is a fixed clock so the login limiter never refills mid-test,
// making the burst assertion exact without a sleep.
func frozenClock() func() time.Time {
	now := time.Now()

	return func() time.Time { return now }
}

// TestLoginRouteRateLimitsBurst proves the login rate guard: a single source IP that
// bursts past the configured login rate is refused with 429, while calls within
// the rate succeed.
func TestLoginRouteRateLimitsBurst(t *testing.T) {
	t.Parallel()

	const rate = 3

	idp := newFakeIDP(t)
	handler := oauth.New(&oauth.Config{
		Verifier:           idp.verifier,
		Exchanger:          newFakeExchanger(""),
		Minter:             &stubMinter{},
		Members:            newTenancyStore(t),
		DefaultWorkspace:   defaultWS,
		DefaultRole:        defaultRole,
		LoginRatePerMinute: rate,
		Now:                frozenClock(),
	})

	loginRoute := handler.Routes()["GET "+oauth.LoginPath]

	// The first rate calls from one source pass; each starts a login (302).
	for i := range rate {
		if code := callLogin(t, loginRoute); code != http.StatusFound {
			t.Fatalf("login %d/%d status = %d, want 302", i+1, rate, code)
		}
	}

	// The next call from the same source trips the limit.
	if code := callLogin(t, loginRoute); code != http.StatusTooManyRequests {
		t.Fatalf("over-limit login status = %d, want 429", code)
	}
}

// callLogin sends one request to the login route from a fixed source IP and
// returns the status code.
func callLogin(t *testing.T, route http.HandlerFunc) int {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, oauth.LoginPath, http.NoBody)
	req.RemoteAddr = "203.0.113.7:54321"

	route(rec, req)

	return rec.Code
}
