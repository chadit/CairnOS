// Package oauth implements the Phase 5b human-login path
// (.agents/spec/12-ui-and-governance.md, "Multi-tenancy"): a browser user
// signs in at an OIDC identity provider, and CairnOS mints its own JWT
// carrying the user's workspace and the role's scopes. It sits alongside the
// access-key model, not in place of it.
//
// Two browser-facing routes drive the flow. GET /v1/oauth/login redirects to
// the IdP's authorization URL with state and PKCE. GET /v1/oauth/callback
// validates state, exchanges the code, verifies the ID token against the IdP
// JWKS, ensures a workspace membership (auto-provisioning a first-time user),
// and returns a CairnOS token as JSON. The minted token is the same shape an
// access-key login produces, so the gRPC interceptor verifies it identically.
package oauth

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/chadit/CairnOS/internal/tenancy"
)

// IDTokenVerifier checks an ID token's signature against the IdP JWKS and
// returns its parsed claims. *oidc.IDTokenVerifier satisfies it; tests pass a
// fake backed by a test key so no live IdP is needed.
type IDTokenVerifier interface {
	Verify(ctx context.Context, rawIDToken string) (*oidc.IDToken, error)
}

// TokenExchanger builds the authorization URL and swaps a code for a token.
// *oauth2.Config satisfies it; tests pass a fake that returns a pre-signed ID
// token, so the exchange runs without a live IdP.
type TokenExchanger interface {
	// AuthCodeURL builds the IdP authorization URL for one state, with the PKCE
	// challenge among the opts.
	AuthCodeURL(state string, opts ...oauth2.AuthCodeOption) string

	// Exchange swaps an authorization code for a token, with the PKCE verifier
	// among the opts so the IdP binds the code to this client.
	Exchange(ctx context.Context, code string, opts ...oauth2.AuthCodeOption) (*oauth2.Token, error)
}

// TokenMinter issues a CairnOS token for a resolved human subject. The auth
// authenticator satisfies it through MintForSubject. Defined narrow so the
// handler depends only on minting, not the whole authenticator.
type TokenMinter interface {
	// MintForSubject issues a token carrying the subject, workspace, and
	// scopes, returning the token and its expiry.
	MintForSubject(ctx context.Context, subject, workspace string, scopes []string) (string, time.Time, error)
}

// Tenancy is the membership slice the handler needs: resolve an existing
// member's role, or provision a first-time login into the default workspace.
// *tenancy.SQLite and *tenancy.Postgres satisfy it.
type Tenancy interface {
	// GetMemberRole returns one subject's role in a workspace, reporting
	// tenancy.ErrMemberNotFound when the subject is not yet a member.
	GetMemberRole(ctx context.Context, workspaceID, subject string) (string, error)

	// AddMember provisions one subject into a workspace with a role.
	AddMember(ctx context.Context, workspaceID, subject, role string) error
}

// ProvisionPolicy gates first-login auto-provisioning (spec H1). Auto-provision
// is off by default: an IdP-authenticated subject with no pre-existing
// membership is refused unless an operator turns Enabled on, and even then the
// allowlists, when set, decide which subjects may self-provision.
type ProvisionPolicy struct {
	// AllowedEmailDomains gates by the verified email's domain (the part after
	// @). Empty means domain is not checked.
	AllowedEmailDomains []string

	// AllowedSubjects gates by the verified ID token subject. Empty means
	// subject is not checked.
	AllowedSubjects []string

	// Enabled turns auto-provisioning on. With it off, a first-time subject with
	// no membership is refused (errAutoProvisionDisabled) so an admin must
	// pre-create the membership.
	Enabled bool
}

// identity is the verified caller resolveRole acts on: the IdP subject plus the
// email claim from the same verified ID token, so the allowlist check trusts a
// signed value rather than anything the caller could forge.
type identity struct {
	subject string
	email   string
}

// resolveRole returns the scopes for a subject's role in the configured
// workspace. An existing member's stored role wins. A first-time login is
// auto-provisioned only when the policy allows it: provisioning is off by
// default, and the email-domain and subject allowlists gate who may
// self-provision. Cross-workspace OAuth needs a workspace selector at the IdP,
// which is a follow-up.
func resolveRole(ctx context.Context, members Tenancy, policy ProvisionPolicy, workspace, defaultRole string, who identity) (string, []string, error) {
	role, err := members.GetMemberRole(ctx, workspace, who.subject)
	if err == nil {
		scopes, ok := tenancy.ScopesForRole(role)
		if !ok {
			return "", nil, errUnknownStoredRole(role)
		}

		return role, scopes, nil
	}

	if !isMemberNotFound(err) {
		return "", nil, errResolveMember(err)
	}

	if err := checkAutoProvision(policy, who); err != nil {
		return "", nil, err
	}

	// First login passed the gate: provision the subject into the default
	// workspace with the default role, then grant that role's scopes.
	if addErr := members.AddMember(ctx, workspace, who.subject, defaultRole); addErr != nil {
		return "", nil, errProvisionMember(addErr)
	}

	scopes, ok := tenancy.ScopesForRole(defaultRole)
	if !ok {
		return "", nil, errUnknownStoredRole(defaultRole)
	}

	return defaultRole, scopes, nil
}

// checkAutoProvision decides whether a first-time subject may self-provision.
// Provisioning off refuses every unprovisioned subject. With it on, a subject
// must clear every allowlist that is set: the email domain when
// AllowedEmailDomains is non-empty, and the subject when AllowedSubjects is
// non-empty. Both empty under an enabled policy is default-open by the
// operator's explicit choice.
func checkAutoProvision(policy ProvisionPolicy, who identity) error {
	if !policy.Enabled {
		return errAutoProvisionDisabled(who.subject)
	}

	if len(policy.AllowedEmailDomains) > 0 && !emailDomainAllowed(who.email, policy.AllowedEmailDomains) {
		return errSubjectNotAllowed(who.subject)
	}

	if len(policy.AllowedSubjects) > 0 && !slices.Contains(policy.AllowedSubjects, who.subject) {
		return errSubjectNotAllowed(who.subject)
	}

	return nil
}

// emailDomainAllowed reports whether email's domain is in allowed, matched
// case-insensitively on the part after the last @. An email with no domain
// never matches, so a subject with an empty or malformed email cannot slip past
// a domain allowlist.
func emailDomainAllowed(email string, allowed []string) bool {
	_, domain, found := strings.CutLast(email, "@")
	if !found || domain == "" {
		return false
	}

	for _, candidate := range allowed {
		if strings.EqualFold(candidate, domain) {
			return true
		}
	}

	return false
}
