package oauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/oauth2"
)

// Route paths the handler answers on the gateway HTTP mux. They are
// browser-facing, so they live on the HTTP server outside the gRPC
// interceptor, the same place the A2A well-known card endpoint mounts.
const (
	LoginPath    = "/v1/oauth/login"
	CallbackPath = "/v1/oauth/callback"
)

// pkceVerifierBytes sizes the PKCE code verifier's entropy. RFC 7636 allows
// 43 to 128 characters; 32 random bytes base64url-encoded lands at 43, the
// floor, which is plenty of entropy.
const pkceVerifierBytes = 32

// Config wires one Handler.
type Config struct {
	// Verifier checks the ID token against the IdP JWKS.
	Verifier IDTokenVerifier

	// Exchanger builds the authorization URL and swaps the code for a token.
	Exchanger TokenExchanger

	// Minter issues the CairnOS token the SPA receives.
	Minter TokenMinter

	// Members resolves or provisions the user's workspace membership.
	Members Tenancy

	// Now injects the clock for the state store and the login limiter, so tests
	// drive expiry and the rate window without sleeping. Nil means time.Now.
	Now func() time.Time

	// Logger records callback failures server-side; the browser sees a generic
	// message. Nil means slog.Default.
	Logger *slog.Logger

	// DefaultWorkspace and DefaultRole place a first-time login. DefaultRole
	// should be the lowest-privilege role so a fresh sign-in cannot change
	// anything until an admin grants more.
	DefaultWorkspace string
	DefaultRole      string

	// Provision gates first-login auto-provisioning. The zero value
	// is provisioning-off, fail-closed: an unprovisioned subject is refused
	// until an operator turns it on and, when set, clears the allowlists.
	Provision ProvisionPolicy

	// StateTTL bounds how long a started login may sit before its callback
	// must return; zero uses the default.
	StateTTL time.Duration

	// LoginRatePerMinute bounds one source IP's calls to the login route, the
	// DoS guard on the unauthenticated, state-minting endpoint. Zero
	// uses the default.
	LoginRatePerMinute int
}

// Handler serves the two browser-facing OAuth routes. It is safe for
// concurrent use: the state store serializes its own writes and every other
// field is immutable after construction.
type Handler struct {
	cfg     *Config
	states  *stateStore
	limiter *loginLimiter
	logger  *slog.Logger
}

// tokenResponse is the JSON the SPA receives on a successful callback: the
// minted CairnOS token and its expiry. JSON is the simplest delivery for a
// single-page app, which stores the token and sends it as a bearer on later
// API calls; a cookie is an alternative left open.
type tokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
	Workspace string `json:"workspace"`
	Role      string `json:"role"`
}

// New builds a handler over its config.
func New(cfg *Config) *Handler {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Handler{
		cfg:     cfg,
		states:  newStateStore(cfg.StateTTL, cfg.Now),
		limiter: newLoginLimiter(cfg.LoginRatePerMinute, cfg.Now),
		logger:  logger,
	}
}

// Routes returns the HTTP handlers keyed by their pattern, ready to mount on
// the gateway's root mux outside the gRPC interceptor.
func (h *Handler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET " + LoginPath:    h.login,
		"GET " + CallbackPath: h.callback,
	}
}

// login starts the flow: mint a PKCE verifier and a state, store them, and
// redirect the browser to the IdP authorization URL carrying the state, the
// S256 PKCE challenge, and the client id (the Exchanger holds the client id).
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	// The login route is unauthenticated and mints a state-store entry per call,
	// so a per-source rate limit guards both the process and the pending-state
	// map against a flood. A refused call answers 429 before any work.
	if !h.limiter.allow(clientIP(r)) {
		http.Error(w, "too many login attempts", http.StatusTooManyRequests)

		return
	}

	verifier, err := newPKCEVerifier()
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, "start login", err)

		return
	}

	state, err := h.states.issue(verifier)
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, "start login", err)

		return
	}

	url := h.cfg.Exchanger.AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.AccessTypeOnline)

	http.Redirect(w, r, url, http.StatusFound)
}

// callback finishes the flow: validate the state (CSRF and replay defense),
// exchange the code with the PKCE verifier, verify the ID token against the
// IdP JWKS, resolve or provision the user's membership, and return a CairnOS
// token. Every failure is logged server-side and answered with a generic
// message so the browser never learns which check tripped.
func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	verifier, ok := h.states.consume(r.URL.Query().Get("state"))
	if !ok {
		h.fail(w, r, http.StatusBadRequest, "validate state", ErrStateInvalid)

		return
	}

	who, err := h.verifiedSubject(r.Context(), r.URL.Query().Get("code"), verifier)
	if err != nil {
		h.fail(w, r, http.StatusUnauthorized, "verify identity", err)

		return
	}

	role, scopes, err := resolveRole(r.Context(), h.cfg.Members, h.cfg.Provision,
		h.cfg.DefaultWorkspace, h.cfg.DefaultRole, who)
	if err != nil {
		h.fail(w, r, membershipFailureStatus(err), "resolve membership", err)

		return
	}

	token, expiresAt, err := h.cfg.Minter.MintForSubject(r.Context(), who.subject, h.cfg.DefaultWorkspace, scopes)
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, "mint token", err)

		return
	}

	h.writeToken(w, r, tokenResponse{
		Token:     token,
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
		Workspace: h.cfg.DefaultWorkspace,
		Role:      role,
	})
}

// verifiedSubject exchanges the code and verifies the returned ID token,
// returning the subject and email claim it carries. PKCE binds the code to this
// client, and the verifier checks the signature against the IdP JWKS, so the
// email the auto-provision allowlist trusts is a signed claim, not anything the
// caller could set.
func (h *Handler) verifiedSubject(ctx context.Context, code, verifier string) (identity, error) {
	token, err := h.cfg.Exchanger.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return identity{}, fmt.Errorf("exchange code: %w", err)
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return identity{}, ErrNoIDToken
	}

	idToken, err := h.cfg.Verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return identity{}, fmt.Errorf("verify id token: %w", err)
	}

	if idToken.Subject == "" {
		return identity{}, ErrNoSubject
	}

	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}

	// A missing or malformed claims set is not fatal: the subject is enough to
	// resolve an existing member, and an absent email fails any domain
	// allowlist, which is the safe direction.
	if err := idToken.Claims(&claims); err != nil {
		h.logger.WarnContext(ctx, "id token claims unreadable, continuing without email", "error", err)
	}

	email := claims.Email
	if !claims.EmailVerified {
		// An unverified email must not pass a domain allowlist, so it is dropped
		// rather than trusted; the IdP did not vouch for it.
		email = ""
	}

	return identity{subject: idToken.Subject, email: email}, nil
}

// membershipFailureStatus maps a resolveRole failure to the browser-facing HTTP
// status. A provisioning refusal is the caller's fault (Forbidden); anything
// else is a server-side membership lookup or store failure.
func membershipFailureStatus(err error) int {
	if errors.Is(err, ErrAutoProvisionDisabled) || errors.Is(err, ErrSubjectNotAllowed) {
		return http.StatusForbidden
	}

	return http.StatusInternalServerError
}

// writeToken sends the minted token as JSON. The response carries the CairnOS
// bearer token, so it must never be cached: Cache-Control no-store plus the
// Pragma no-cache backstop keep a proxy or browser from storing it.
func (h *Handler) writeToken(w http.ResponseWriter, r *http.Request, body tokenResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")

	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.logger.ErrorContext(r.Context(), "write oauth token response", "error", err)
	}
}

// fail logs the real cause server-side and answers the browser with a generic
// message, so a caller never learns whether the state, the code, the ID token,
// or the membership lookup tripped.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, status int, stage string, err error) {
	h.logger.WarnContext(r.Context(), "oauth login failed", "stage", stage, "error", err)
	http.Error(w, "oauth login failed", status)
}

// newPKCEVerifier mints a fresh PKCE code verifier. oauth2.GenerateVerifier
// produces an RFC 7636-compliant verifier; this wrapper exists so a crypto/rand
// failure surfaces as an error rather than a panic inside the library.
func newPKCEVerifier() (string, error) {
	raw := make([]byte, pkceVerifierBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate pkce verifier: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(raw), nil
}
