package oauth_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/chadit/CairnOS/internal/oauth"
	"github.com/chadit/CairnOS/internal/tenancy"
)

// allowedDomain and a matching subject email the auto-provision allowlist tests
// gate on.
const (
	allowedDomain   = "allowed.test"
	allowedEmail    = "alice@allowed.test"
	disallowedEmail = "mallory@evil.test"
)

// runCallback drives one started login's callback and returns the recorder, so
// a test can assert the status and provisioning side effect in one place.
func runCallback(t *testing.T, handler *oauth.Handler) *httptest.ResponseRecorder {
	t.Helper()

	state := startLogin(t, handler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		oauth.CallbackPath+"?state="+url.QueryEscape(state)+"&code="+testCode, http.NoBody)

	handler.Routes()["GET "+oauth.CallbackPath](rec, req)

	return rec
}

// TestCallbackRefusesProvisionWhenDisabled proves the H1 fail-closed default: a
// first-time subject with no membership is refused when auto-provisioning is
// off, and no member row is created.
func TestCallbackRefusesProvisionWhenDisabled(t *testing.T) {
	t.Parallel()

	idp := newFakeIDP(t)
	exchanger := newFakeExchanger(idp.signIDToken(t))
	members := newTenancyStore(t)
	handler := newHandlerWithPolicy(t, idp, exchanger, &stubMinter{}, members,
		oauth.ProvisionPolicy{Enabled: false})

	rec := runCallback(t, handler)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("provision-disabled callback status = %d, want %d", rec.Code, http.StatusForbidden)
	}

	if _, err := members.GetMemberRole(t.Context(), defaultWS, testSubject); err == nil {
		t.Fatal("a refused login provisioned a member; the gate must not write")
	}
}

// TestCallbackProvisionsAllowlistedDomain proves an allowlisted email domain
// self-provisions when auto-provisioning is on.
func TestCallbackProvisionsAllowlistedDomain(t *testing.T) {
	t.Parallel()

	idp := newFakeIDP(t)
	exchanger := newFakeExchanger(idp.signIDTokenWithEmail(t, allowedEmail, allowedEmail, true))
	members := newTenancyStore(t)
	handler := newHandlerWithPolicy(t, idp, exchanger, &stubMinter{}, members,
		oauth.ProvisionPolicy{Enabled: true, AllowedEmailDomains: []string{allowedDomain}})

	rec := runCallback(t, handler)
	if rec.Code != http.StatusOK {
		t.Fatalf("allowlisted-domain callback status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	role, err := members.GetMemberRole(t.Context(), defaultWS, allowedEmail)
	if err != nil {
		t.Fatalf("allowlisted subject was not provisioned: %v", err)
	}

	if role != defaultRole {
		t.Errorf("provisioned role = %q, want %q", role, defaultRole)
	}
}

// TestCallbackRefusesUnlistedDomain proves a subject whose email domain is
// outside the allowlist is refused even with auto-provisioning on, and no
// member is created.
func TestCallbackRefusesUnlistedDomain(t *testing.T) {
	t.Parallel()

	idp := newFakeIDP(t)
	exchanger := newFakeExchanger(idp.signIDTokenWithEmail(t, disallowedEmail, disallowedEmail, true))
	members := newTenancyStore(t)
	handler := newHandlerWithPolicy(t, idp, exchanger, &stubMinter{}, members,
		oauth.ProvisionPolicy{Enabled: true, AllowedEmailDomains: []string{allowedDomain}})

	rec := runCallback(t, handler)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unlisted-domain callback status = %d, want %d", rec.Code, http.StatusForbidden)
	}

	if _, err := members.GetMemberRole(t.Context(), defaultWS, disallowedEmail); err == nil {
		t.Fatal("an unlisted-domain login provisioned a member; the allowlist must not write")
	}
}

// TestCallbackRefusesUnverifiedEmailDomain proves an unverified email cannot
// satisfy a domain allowlist: the IdP did not vouch for the address, so the
// claim is dropped and the domain gate fails.
func TestCallbackRefusesUnverifiedEmailDomain(t *testing.T) {
	t.Parallel()

	idp := newFakeIDP(t)
	exchanger := newFakeExchanger(idp.signIDTokenWithEmail(t, allowedEmail, allowedEmail, false))
	members := newTenancyStore(t)
	handler := newHandlerWithPolicy(t, idp, exchanger, &stubMinter{}, members,
		oauth.ProvisionPolicy{Enabled: true, AllowedEmailDomains: []string{allowedDomain}})

	rec := runCallback(t, handler)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unverified-email callback status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// TestCallbackTokenResponseIsNoStore proves the M1 cache headers on the token
// response: the minted bearer token must never be cached, so Cache-Control
// no-store and the Pragma no-cache backstop ride the successful callback.
func TestCallbackTokenResponseIsNoStore(t *testing.T) {
	t.Parallel()

	idp := newFakeIDP(t)
	exchanger := newFakeExchanger(idp.signIDToken(t))
	handler := newHandlerWithPolicy(t, idp, exchanger, &stubMinter{}, newTenancyStore(t),
		oauth.ProvisionPolicy{Enabled: true})

	rec := runCallback(t, handler)
	if rec.Code != http.StatusOK {
		t.Fatalf("callback status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	if got := rec.Header().Get("Pragma"); got != "no-cache" {
		t.Errorf("Pragma = %q, want no-cache", got)
	}
}

// TestCallbackPreExistingMemberLoginsWithoutProvision proves an admin
// pre-created membership lets a subject log in even with auto-provisioning off:
// the gate only guards first-time provisioning, not an existing member.
func TestCallbackPreExistingMemberLoginsWithoutProvision(t *testing.T) {
	t.Parallel()

	idp := newFakeIDP(t)
	exchanger := newFakeExchanger(idp.signIDToken(t))
	members := newTenancyStore(t)

	if err := members.AddMember(t.Context(), defaultWS, testSubject, tenancy.RoleViewer); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	handler := newHandlerWithPolicy(t, idp, exchanger, &stubMinter{}, members,
		oauth.ProvisionPolicy{Enabled: false})

	rec := runCallback(t, handler)
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-existing member callback status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
}
